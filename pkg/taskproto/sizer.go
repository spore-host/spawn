package taskproto

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Candidate is one instance type the finder returned, with the facts the sizer
// ranks on. It's a small projection of truffle's InstanceTypeResult so the sizer
// (and its tests) don't depend on truffle directly.
type Candidate struct {
	InstanceType  string
	Family        string // family prefix, e.g. "c7i"
	VCPUs         int
	MemoryGiB     float64
	GPUs          int
	Architecture  string
	OnDemandPrice float64 // $/hr; 0 if unknown
}

// InstanceFinder returns candidate instance types meeting the minimum vCPU /
// memory / architecture constraints. The real implementation wraps truffle's
// SearchInstanceTypes; tests inject a fake so the sizer needs no AWS.
type InstanceFinder interface {
	FindCandidates(ctx context.Context, req ResourceRequest) ([]Candidate, error)
}

// SizeResult is the sizer's choice plus why.
type SizeResult struct {
	InstanceType  string
	Family        string
	VCPUs         int
	MemoryGiB     float64
	OnDemandPrice float64 // $/hr (0 if unknown)
	Considered    int     // how many candidates matched before picking cheapest
}

// Size picks the cheapest instance type satisfying a ResourceRequest. It applies
// the memory headroom, filters to the family allow-list (a capability truffle's
// single-family filter lacks), requires GPUs when asked, and ranks by on-demand
// price (cheapest first; unknown/zero prices sort last so a priced option always
// wins). On a price TIE it prefers the smallest type that fits — vCPUs, then
// memory (spawn#610) — so an equal-priced family like hpc7g, where every size
// costs the same, returns what the request actually asked for instead of the
// largest. Spot-vs-on-demand purchase selection is the caller's concern — Size
// ranks on on-demand price as a stable proxy.
//
// A pinned ResourceRequest.InstanceType bypasses all of this; see below.
func Size(ctx context.Context, finder InstanceFinder, req ResourceRequest) (*SizeResult, error) {
	// An exact instance-type pin bypasses candidate search + price ranking: the
	// caller asked for this specific type (e.g. nf-spawn's ext.instanceType), so
	// honor it verbatim. Family/cpu/memory are ignored when a pin is set.
	if pin := strings.TrimSpace(req.InstanceType); pin != "" {
		return &SizeResult{
			InstanceType: pin,
			Family:       familyOf(pin),
			Considered:   1,
		}, nil
	}

	cands, err := finder.FindCandidates(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("find instance candidates: %w", err)
	}

	allowed := familyAllowSet(req.Families)
	minMem := EffectiveMemoryGiB(req)
	matched := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if len(allowed) > 0 && !allowed[c.Family] {
			continue
		}
		// The finder is asked for at least minMem, but the sizer enforces it too,
		// so a memory floor (ContainerMemoryFloorGiB) holds whatever the finder
		// returns. A candidate with unknown memory (0) is not excluded on a guess.
		if c.MemoryGiB > 0 && c.MemoryGiB < minMem {
			continue
		}
		if req.GPUs > 0 && c.GPUs < req.GPUs {
			continue
		}
		matched = append(matched, c)
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("no instance type matches the request (cpu=%d memory_gib=%.0f gpus=%d arch=%q families=%v)",
			req.CPU, EffectiveMemoryGiB(req), req.GPUs, req.Architecture, req.Families)
	}

	// Cheapest first; a known price always beats an unknown (0) price.
	sort.SliceStable(matched, func(i, j int) bool {
		pi, pj := matched[i].OnDemandPrice, matched[j].OnDemandPrice
		switch {
		case pi <= 0 && pj > 0:
			return false
		case pj <= 0 && pi > 0:
			return true
		case pi != pj:
			return pi < pj
		default:
			// Equal price: prefer the SMALLEST type that still fits (spawn#610).
			// Every candidate already satisfies the cpu/memory request, so on a tie
			// the smallest is what the spec actually asked for and it minimises blast
			// radius — nothing is gained by renting more cores at the same rate.
			//
			// This used to tie-break on the type NAME, which is lexicographic and
			// therefore semantically arbitrary: on hpc7g — where every size costs the
			// same because you rent the socket, not the cores — "hpc7g.16xlarge" sorts
			// before "hpc7g.4xlarge" simply because '1' < '4', so a 16-vCPU request
			// silently got 64 vCPUs. Note the old behaviour was not "prefer the
			// largest": on a 2x/4x/8xlarge family the same comparison picked the
			// smallest and looked correct, which is why it went unnoticed.
			if a, b := matched[i].VCPUs, matched[j].VCPUs; a != b {
				return a < b
			}
			if a, b := matched[i].MemoryGiB, matched[j].MemoryGiB; a != b {
				return a < b
			}
			// Same price, same size: fall back to the name purely so the choice is
			// deterministic across runs.
			return matched[i].InstanceType < matched[j].InstanceType
		}
	})

	best := matched[0]
	return &SizeResult{
		InstanceType:  best.InstanceType,
		Family:        best.Family,
		VCPUs:         best.VCPUs,
		MemoryGiB:     best.MemoryGiB,
		OnDemandPrice: best.OnDemandPrice,
		Considered:    len(matched),
	}, nil
}

// ContainerMemoryFloorGiB is the least memory a container task is sized to
// (#1180). A container task installs Docker at boot (`dnf install docker`, then
// dockerd + containerd, then the image pull) before the user's command runs.
//
// Measured in experiment E1 (spawn#1171, us-east-1, 2026-10-19): a spec with only
// resources.cpu sized to t4g.nano (0.5 GiB), where the Docker install was
// OOM-killed 3 of 3 times; with 4 GiB (t4g.medium) all 3 runs succeeded, the
// install taking ~66 s. Nothing between 0.5 and 4 GiB has been measured.
//
// 2 GiB rather than 1 GiB: 1 GiB is already the point below which spawn warns
// that AL2023's own package transactions are unreliable (aws.SporedMemoryFloorMiB,
// spawn#682), and a container task runs that transaction PLUS a resident Docker
// daemon PLUS the workload, so sizing a container task at exactly the bare-dnf
// line would leave the daemon and the workload no headroom. Not 4 GiB, because
// the floor applies only when the spec asked for less, and it doubles the
// cheapest container box for every such task on an unmeasured margin; 2 GiB is
// the smallest standard size (t4g.small, t3.small) above that line. Raise it if
// a measurement at 2 GiB fails.
const ContainerMemoryFloorGiB = 2.0

// SizingRequest is the ResourceRequest a spec is sized against: its Resources,
// with the container memory floor applied. Callers size with
// Size(ctx, finder, spec.SizingRequest()), never spec.Resources directly.
//
// The floor applies whenever spec.Container is set and the effective memory
// request (memory_gib with headroom) is below ContainerMemoryFloorGiB, including
// an explicit memory_gib lower than the floor: memory_gib describes what the
// workload needs, and the Docker install needs its own on top, which the spec
// author cannot be expected to know. It replaces memory_gib with the floor and
// drops the headroom, so a floored request is exactly the floor, not floor plus
// headroom. A pinned resources.instance_type is returned unchanged; Size honours
// a pin verbatim, and the caller warns if the pin is below the floor.
func (s *TaskSpec) SizingRequest() ResourceRequest {
	req := s.Resources
	if s.Container == "" || strings.TrimSpace(req.InstanceType) != "" {
		return req
	}
	if EffectiveMemoryGiB(req) < ContainerMemoryFloorGiB {
		req.MemoryGiB = ContainerMemoryFloorGiB
		req.MemoryHeadroomPercent = 0
	}
	return req
}

// EffectiveMemoryGiB applies the headroom percentage to the requested memory, so
// a job asking for 32 GiB with 20% headroom is sized against 38.4 GiB. Exported
// so the truffle adapter sizes its MinMemory query the same way the sizer filters.
func EffectiveMemoryGiB(req ResourceRequest) float64 {
	if req.MemoryHeadroomPercent <= 0 {
		return req.MemoryGiB
	}
	return req.MemoryGiB * (1 + float64(req.MemoryHeadroomPercent)/100)
}

// familyOf returns the family prefix of an instance type ("c7i" from
// "c7i.4xlarge"), or "" if it has no "." separator.
func familyOf(instanceType string) string {
	if i := strings.IndexByte(instanceType, '.'); i > 0 {
		return instanceType[:i]
	}
	return ""
}

func familyAllowSet(families []string) map[string]bool {
	if len(families) == 0 {
		return nil
	}
	set := make(map[string]bool, len(families))
	for _, f := range families {
		if f = strings.TrimSpace(f); f != "" {
			set[f] = true
		}
	}
	return set
}
