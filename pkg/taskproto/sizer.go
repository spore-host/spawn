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
// wins). Spot-vs-on-demand purchase selection is the caller's concern — Size
// ranks on on-demand price as a stable proxy.
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
	matched := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if len(allowed) > 0 && !allowed[c.Family] {
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
			// Tie-break deterministically by type name.
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
