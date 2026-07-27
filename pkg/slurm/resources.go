package slurm

import (
	"fmt"
	"sort"
)

// InstanceTypeSpec represents EC2 instance type specifications
type InstanceTypeSpec struct {
	Type     string
	VCPUs    int
	MemoryMB int
	GPUs     int
	GPUType  string

	// RelativeCost ranks this type against the others for candidate selection
	// ONLY. It is a rough us-east-1 On-Demand $/hr and is NOT a price: it is never
	// multiplied by hours and never shown to a user. Dollar figures come from
	// truffle's live Price List pricer — see [Pricer] and #447, which found this
	// field being used as a real price while p4d was 49% and p5 79% overstated.
	//
	// Only the ordering matters here, so drift is tolerable as long as it doesn't
	// reorder the ladder. Sanity-checked against the live API 2026-07-27.
	RelativeCost float64
}

// SelectInstanceType selects the best EC2 instance type for Slurm job requirements
func SelectInstanceType(job *SlurmJob) (string, error) {
	// If spawn override specified, use it
	if job.SpawnInstanceType != "" {
		return job.SpawnInstanceType, nil
	}

	// Get all available instance types
	types := getInstanceTypes()

	// Filter by requirements
	candidates := []InstanceTypeSpec{}
	for _, t := range types {
		if matches(t, job) {
			candidates = append(candidates, t)
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no instance type found matching requirements (CPUs: %d, Memory: %dMB, GPUs: %d)",
			job.CPUsPerTask, job.MemoryMB, job.GPUs)
	}

	// Cheapest-first by relative cost. Ties break on type name so selection is
	// deterministic — several types can share a rank (c5/c6i are priced
	// identically), and map/slice order must not decide which one a user gets.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].RelativeCost != candidates[j].RelativeCost {
			return candidates[i].RelativeCost < candidates[j].RelativeCost
		}
		return candidates[i].Type < candidates[j].Type
	})

	return candidates[0].Type, nil
}

// matches checks if an instance type matches job requirements
func matches(t InstanceTypeSpec, job *SlurmJob) bool {
	// Check CPU requirement
	if job.CPUsPerTask > 0 && t.VCPUs < job.CPUsPerTask {
		return false
	}

	// Check memory requirement
	if job.MemoryMB > 0 && t.MemoryMB < job.MemoryMB {
		return false
	}

	// Check GPU requirement
	if job.GPUs > 0 {
		if t.GPUs < job.GPUs {
			return false
		}

		// Check GPU type if specified
		if job.GPUType != "" {
			if t.GPUType != job.GPUType && !isCompatibleGPUType(job.GPUType, t.GPUType) {
				return false
			}
		}
	}

	return true
}

// retiredGPUSuccessors maps a GPU a Slurm script may still ask for, but which AWS
// no longer offers, to the current GPUs that can run the same work.
//
// Without this, dropping the retired p3/V100 entries (#447) would turn a
// `--gres=gpu:v100` script into a hard "no instance type found" failure. Erroring
// is not an improvement over the old behavior of picking an instance type that
// cannot launch — the script is still valid, the hardware just moved on. Each
// successor listed has at least as much VRAM as the GPU it replaces (V100 is
// 16-32 GB; A10G/L4 are 24 GB, L40S 48 GB), so a job sized for the old card fits.
// T4 is deliberately excluded: it matches on VRAM but is a large step down in
// compute, and silently making a job much slower is its own kind of wrong answer.
var retiredGPUSuccessors = map[string][]string{
	"v100": {"a10g", "l4", "l40s", "h100", "h200", "b200", "b300"},
}

// isCompatibleGPUType checks if a GPU type is compatible with the requested type
func isCompatibleGPUType(requested, available string) bool {
	// Normalize names
	requested = normalizeGPUType(requested)
	available = normalizeGPUType(available)

	// Exact match
	if requested == available {
		return true
	}

	// Family match (e.g., v100 matches any v100 variant)
	if requested == "v100" && (available == "v100" || available == "v100-sxm2" || available == "v100-16gb") {
		return true
	}
	if requested == "a100" && (available == "a100" || available == "a100-80gb") {
		return true
	}

	// Retired GPU: accept a current successor rather than failing to match at all.
	for _, successor := range retiredGPUSuccessors[requested] {
		if available == successor {
			return true
		}
	}

	return false
}

// normalizeGPUType normalizes GPU type names
func normalizeGPUType(gpuType string) string {
	gpuType = toLower(gpuType)

	// Handle common variants
	switch gpuType {
	case "tesla_v100", "v100-sxm2-16gb", "v100-sxm2":
		return "v100"
	case "tesla_a100", "a100-sxm4-40gb":
		return "a100"
	case "tesla_a100_80gb", "a100-sxm4-80gb":
		return "a100-80gb"
	case "tesla_t4":
		return "t4"
	case "nvidia_a10g", "a10g":
		return "a10g"
	case "nvidia_l4":
		return "l4"
	case "nvidia_l40s", "l40":
		return "l40s"
	case "nvidia_h100", "h100-80gb", "h100-sxm5-80gb":
		return "h100"
	case "nvidia_h200", "h200-141gb":
		return "h200"
	case "nvidia_b200":
		return "b200"
	case "nvidia_b300":
		return "b300"
	// The g7 families' GPUs, as Slurm gres names would plausibly spell them.
	case "rtx_pro_4500", "rtx-pro-4500-blackwell", "rtx4500":
		return "rtx-pro-4500"
	case "rtx_pro_6000", "rtx-pro-6000-blackwell-server", "rtx6000":
		return "rtx-pro-6000"
	}

	return gpuType
}

func toLower(s string) string {
	// Simple lowercase conversion
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			result[i] = c + 32
		} else {
			result[i] = c
		}
	}
	return string(result)
}

// getInstanceTypes returns the candidate instance types Slurm job requirements
// are matched against, with the specs used for matching (vCPUs, memory, GPUs) and
// a RelativeCost used only to rank them.
//
// Specs are from DescribeInstanceTypes and RelativeCost from the Price List API,
// both us-east-1, verified 2026-07-27 (#447). The costs are ranking inputs, not
// prices — a user-visible dollar figure always comes from truffle's live pricer.
func getInstanceTypes() []InstanceTypeSpec {
	return []InstanceTypeSpec{
		// General Purpose (t-series)
		{Type: "t3.micro", VCPUs: 2, MemoryMB: 1024, GPUs: 0, RelativeCost: 0.0104},
		{Type: "t3.small", VCPUs: 2, MemoryMB: 2048, GPUs: 0, RelativeCost: 0.0208},
		{Type: "t3.medium", VCPUs: 2, MemoryMB: 4096, GPUs: 0, RelativeCost: 0.0416},
		{Type: "t3.large", VCPUs: 2, MemoryMB: 8192, GPUs: 0, RelativeCost: 0.0832},
		{Type: "t3.xlarge", VCPUs: 4, MemoryMB: 16384, GPUs: 0, RelativeCost: 0.1664},
		{Type: "t3.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 0, RelativeCost: 0.3328},

		// General Purpose (m-series)
		{Type: "m5.large", VCPUs: 2, MemoryMB: 8192, GPUs: 0, RelativeCost: 0.096},
		{Type: "m5.xlarge", VCPUs: 4, MemoryMB: 16384, GPUs: 0, RelativeCost: 0.192},
		{Type: "m5.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 0, RelativeCost: 0.384},
		{Type: "m5.4xlarge", VCPUs: 16, MemoryMB: 65536, GPUs: 0, RelativeCost: 0.768},
		{Type: "m5.8xlarge", VCPUs: 32, MemoryMB: 131072, GPUs: 0, RelativeCost: 1.536},
		{Type: "m5.12xlarge", VCPUs: 48, MemoryMB: 196608, GPUs: 0, RelativeCost: 2.304},
		{Type: "m5.16xlarge", VCPUs: 64, MemoryMB: 262144, GPUs: 0, RelativeCost: 3.072},
		{Type: "m5.24xlarge", VCPUs: 96, MemoryMB: 393216, GPUs: 0, RelativeCost: 4.608},

		// Compute Optimized (c-series)
		{Type: "c5.large", VCPUs: 2, MemoryMB: 4096, GPUs: 0, RelativeCost: 0.085},
		{Type: "c5.xlarge", VCPUs: 4, MemoryMB: 8192, GPUs: 0, RelativeCost: 0.17},
		{Type: "c5.2xlarge", VCPUs: 8, MemoryMB: 16384, GPUs: 0, RelativeCost: 0.34},
		{Type: "c5.4xlarge", VCPUs: 16, MemoryMB: 32768, GPUs: 0, RelativeCost: 0.68},
		{Type: "c5.9xlarge", VCPUs: 36, MemoryMB: 73728, GPUs: 0, RelativeCost: 1.53},
		{Type: "c5.12xlarge", VCPUs: 48, MemoryMB: 98304, GPUs: 0, RelativeCost: 2.04},
		{Type: "c5.18xlarge", VCPUs: 72, MemoryMB: 147456, GPUs: 0, RelativeCost: 3.06},
		{Type: "c5.24xlarge", VCPUs: 96, MemoryMB: 196608, GPUs: 0, RelativeCost: 4.08},

		// Compute Optimized (c6i)
		{Type: "c6i.large", VCPUs: 2, MemoryMB: 4096, GPUs: 0, RelativeCost: 0.085},
		{Type: "c6i.xlarge", VCPUs: 4, MemoryMB: 8192, GPUs: 0, RelativeCost: 0.17},
		{Type: "c6i.2xlarge", VCPUs: 8, MemoryMB: 16384, GPUs: 0, RelativeCost: 0.34},
		{Type: "c6i.4xlarge", VCPUs: 16, MemoryMB: 32768, GPUs: 0, RelativeCost: 0.68},
		{Type: "c6i.8xlarge", VCPUs: 32, MemoryMB: 65536, GPUs: 0, RelativeCost: 1.36},
		{Type: "c6i.12xlarge", VCPUs: 48, MemoryMB: 98304, GPUs: 0, RelativeCost: 2.04},
		{Type: "c6i.16xlarge", VCPUs: 64, MemoryMB: 131072, GPUs: 0, RelativeCost: 2.72},
		{Type: "c6i.24xlarge", VCPUs: 96, MemoryMB: 196608, GPUs: 0, RelativeCost: 4.08},
		{Type: "c6i.32xlarge", VCPUs: 128, MemoryMB: 262144, GPUs: 0, RelativeCost: 5.44},

		// Compute Optimized (c7i - latest generation)
		{Type: "c7i.large", VCPUs: 2, MemoryMB: 4096, GPUs: 0, RelativeCost: 0.08925},
		{Type: "c7i.xlarge", VCPUs: 4, MemoryMB: 8192, GPUs: 0, RelativeCost: 0.1785},
		{Type: "c7i.2xlarge", VCPUs: 8, MemoryMB: 16384, GPUs: 0, RelativeCost: 0.357},
		{Type: "c7i.4xlarge", VCPUs: 16, MemoryMB: 32768, GPUs: 0, RelativeCost: 0.714},
		{Type: "c7i.8xlarge", VCPUs: 32, MemoryMB: 65536, GPUs: 0, RelativeCost: 1.428},
		{Type: "c7i.12xlarge", VCPUs: 48, MemoryMB: 98304, GPUs: 0, RelativeCost: 2.142},
		{Type: "c7i.16xlarge", VCPUs: 64, MemoryMB: 131072, GPUs: 0, RelativeCost: 2.856},
		{Type: "c7i.24xlarge", VCPUs: 96, MemoryMB: 196608, GPUs: 0, RelativeCost: 4.284},
		{Type: "c7i.48xlarge", VCPUs: 192, MemoryMB: 393216, GPUs: 0, RelativeCost: 8.568},

		// Memory Optimized (r-series)
		{Type: "r5.large", VCPUs: 2, MemoryMB: 16384, GPUs: 0, RelativeCost: 0.126},
		{Type: "r5.xlarge", VCPUs: 4, MemoryMB: 32768, GPUs: 0, RelativeCost: 0.252},
		{Type: "r5.2xlarge", VCPUs: 8, MemoryMB: 65536, GPUs: 0, RelativeCost: 0.504},
		{Type: "r5.4xlarge", VCPUs: 16, MemoryMB: 131072, GPUs: 0, RelativeCost: 1.008},
		{Type: "r5.8xlarge", VCPUs: 32, MemoryMB: 262144, GPUs: 0, RelativeCost: 2.016},
		{Type: "r5.12xlarge", VCPUs: 48, MemoryMB: 393216, GPUs: 0, RelativeCost: 3.024},
		{Type: "r5.16xlarge", VCPUs: 64, MemoryMB: 524288, GPUs: 0, RelativeCost: 4.032},
		{Type: "r5.24xlarge", VCPUs: 96, MemoryMB: 786432, GPUs: 0, RelativeCost: 6.048},

		// GPU Instances (g4dn - T4, 16 GB/GPU)
		{Type: "g4dn.xlarge", VCPUs: 4, MemoryMB: 16384, GPUs: 1, GPUType: "t4", RelativeCost: 0.526},
		{Type: "g4dn.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 1, GPUType: "t4", RelativeCost: 0.752},
		{Type: "g4dn.4xlarge", VCPUs: 16, MemoryMB: 65536, GPUs: 1, GPUType: "t4", RelativeCost: 1.204},
		{Type: "g4dn.8xlarge", VCPUs: 32, MemoryMB: 131072, GPUs: 1, GPUType: "t4", RelativeCost: 2.176},
		{Type: "g4dn.12xlarge", VCPUs: 48, MemoryMB: 196608, GPUs: 4, GPUType: "t4", RelativeCost: 3.912},
		{Type: "g4dn.16xlarge", VCPUs: 64, MemoryMB: 262144, GPUs: 1, GPUType: "t4", RelativeCost: 4.352},

		// GPU Instances (g5 - A10G, 24 GB/GPU)
		{Type: "g5.xlarge", VCPUs: 4, MemoryMB: 16384, GPUs: 1, GPUType: "a10g", RelativeCost: 1.006},
		{Type: "g5.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 1, GPUType: "a10g", RelativeCost: 1.212},
		{Type: "g5.4xlarge", VCPUs: 16, MemoryMB: 65536, GPUs: 1, GPUType: "a10g", RelativeCost: 1.624},
		{Type: "g5.8xlarge", VCPUs: 32, MemoryMB: 131072, GPUs: 1, GPUType: "a10g", RelativeCost: 2.448},
		{Type: "g5.12xlarge", VCPUs: 48, MemoryMB: 196608, GPUs: 4, GPUType: "a10g", RelativeCost: 5.672},
		{Type: "g5.16xlarge", VCPUs: 64, MemoryMB: 262144, GPUs: 1, GPUType: "a10g", RelativeCost: 4.096},
		{Type: "g5.24xlarge", VCPUs: 96, MemoryMB: 393216, GPUs: 4, GPUType: "a10g", RelativeCost: 8.144},
		{Type: "g5.48xlarge", VCPUs: 192, MemoryMB: 786432, GPUs: 8, GPUType: "a10g", RelativeCost: 16.288},

		// GPU Instances (g6 - L4, 24 GB/GPU)
		{Type: "g6.xlarge", VCPUs: 4, MemoryMB: 16384, GPUs: 1, GPUType: "l4", RelativeCost: 0.8048},
		{Type: "g6.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 1, GPUType: "l4", RelativeCost: 0.9776},
		{Type: "g6.4xlarge", VCPUs: 16, MemoryMB: 65536, GPUs: 1, GPUType: "l4", RelativeCost: 1.3232},
		{Type: "g6.8xlarge", VCPUs: 32, MemoryMB: 131072, GPUs: 1, GPUType: "l4", RelativeCost: 2.0144},
		{Type: "g6.12xlarge", VCPUs: 48, MemoryMB: 196608, GPUs: 4, GPUType: "l4", RelativeCost: 4.6016},
		{Type: "g6.16xlarge", VCPUs: 64, MemoryMB: 262144, GPUs: 1, GPUType: "l4", RelativeCost: 3.3968},
		{Type: "g6.24xlarge", VCPUs: 96, MemoryMB: 393216, GPUs: 4, GPUType: "l4", RelativeCost: 6.6752},
		{Type: "g6.48xlarge", VCPUs: 192, MemoryMB: 786432, GPUs: 8, GPUType: "l4", RelativeCost: 13.3504},

		// GPU Instances (g6e - L40S, 48 GB/GPU)
		{Type: "g6e.xlarge", VCPUs: 4, MemoryMB: 32768, GPUs: 1, GPUType: "l40s", RelativeCost: 1.861},
		{Type: "g6e.2xlarge", VCPUs: 8, MemoryMB: 65536, GPUs: 1, GPUType: "l40s", RelativeCost: 2.24208},
		{Type: "g6e.4xlarge", VCPUs: 16, MemoryMB: 131072, GPUs: 1, GPUType: "l40s", RelativeCost: 3.00424},
		{Type: "g6e.8xlarge", VCPUs: 32, MemoryMB: 262144, GPUs: 1, GPUType: "l40s", RelativeCost: 4.52856},
		{Type: "g6e.12xlarge", VCPUs: 48, MemoryMB: 393216, GPUs: 4, GPUType: "l40s", RelativeCost: 10.49264},
		{Type: "g6e.16xlarge", VCPUs: 64, MemoryMB: 524288, GPUs: 1, GPUType: "l40s", RelativeCost: 7.57719},
		{Type: "g6e.24xlarge", VCPUs: 96, MemoryMB: 786432, GPUs: 4, GPUType: "l40s", RelativeCost: 15.06559},
		{Type: "g6e.48xlarge", VCPUs: 192, MemoryMB: 1572864, GPUs: 8, GPUType: "l40s", RelativeCost: 30.13118},

		// GPU Instances (g7 - RTX PRO 4500 Blackwell, 32 GB/GPU)
		{Type: "g7.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 1, GPUType: "rtx-pro-4500", RelativeCost: 2.52},
		{Type: "g7.4xlarge", VCPUs: 16, MemoryMB: 65536, GPUs: 1, GPUType: "rtx-pro-4500", RelativeCost: 3.04208},
		{Type: "g7.8xlarge", VCPUs: 32, MemoryMB: 131072, GPUs: 1, GPUType: "rtx-pro-4500", RelativeCost: 4.08624},
		{Type: "g7.12xlarge", VCPUs: 48, MemoryMB: 196608, GPUs: 2, GPUType: "rtx-pro-4500", RelativeCost: 7.12832},
		{Type: "g7.24xlarge", VCPUs: 96, MemoryMB: 393216, GPUs: 4, GPUType: "rtx-pro-4500", RelativeCost: 14.25664},
		{Type: "g7.48xlarge", VCPUs: 192, MemoryMB: 786432, GPUs: 8, GPUType: "rtx-pro-4500", RelativeCost: 28.51328},

		// GPU Instances (g7e - RTX PRO 6000 Blackwell Server, 96 GB/GPU)
		{Type: "g7e.2xlarge", VCPUs: 8, MemoryMB: 65536, GPUs: 1, GPUType: "rtx-pro-6000", RelativeCost: 3.36312},
		{Type: "g7e.4xlarge", VCPUs: 16, MemoryMB: 131072, GPUs: 1, GPUType: "rtx-pro-6000", RelativeCost: 3.99816},
		{Type: "g7e.8xlarge", VCPUs: 32, MemoryMB: 262144, GPUs: 1, GPUType: "rtx-pro-6000", RelativeCost: 5.26824},
		{Type: "g7e.12xlarge", VCPUs: 48, MemoryMB: 524288, GPUs: 2, GPUType: "rtx-pro-6000", RelativeCost: 8.28608},
		{Type: "g7e.24xlarge", VCPUs: 96, MemoryMB: 1048576, GPUs: 4, GPUType: "rtx-pro-6000", RelativeCost: 16.57216},
		{Type: "g7e.48xlarge", VCPUs: 192, MemoryMB: 2097152, GPUs: 8, GPUType: "rtx-pro-6000", RelativeCost: 33.14432},

		// GPU Instances (p4d/p4de - A100, 40/80 GB per GPU)
		{Type: "p4d.24xlarge", VCPUs: 96, MemoryMB: 1179648, GPUs: 8, GPUType: "a100", RelativeCost: 21.957642},
		{Type: "p4de.24xlarge", VCPUs: 96, MemoryMB: 1179648, GPUs: 8, GPUType: "a100-80gb", RelativeCost: 27.44705},

		// GPU Instances (p5 - H100, 80 GB/GPU). p5.4xlarge is the only H100 size
		// that doesn't require renting all 8 GPUs, so it matters for single-GPU work.
		{Type: "p5.4xlarge", VCPUs: 16, MemoryMB: 262144, GPUs: 1, GPUType: "h100", RelativeCost: 6.88},
		{Type: "p5.48xlarge", VCPUs: 192, MemoryMB: 2097152, GPUs: 8, GPUType: "h100", RelativeCost: 55.04},

		// GPU Instances (p5e/p5en - H200, 141 GB/GPU). p5e has no published
		// On-Demand rate (Capacity Block / reservation only), so it is ranked at the
		// p5en rate — near enough for ordering, and the dollar figure never comes
		// from here.
		{Type: "p5e.48xlarge", VCPUs: 192, MemoryMB: 2097152, GPUs: 8, GPUType: "h200", RelativeCost: 63.296},
		{Type: "p5en.48xlarge", VCPUs: 192, MemoryMB: 2097152, GPUs: 8, GPUType: "h200", RelativeCost: 63.296},

		// GPU Instances (p6 - Blackwell, 180/288 GB per GPU)
		{Type: "p6-b200.48xlarge", VCPUs: 192, MemoryMB: 2097152, GPUs: 8, GPUType: "b200", RelativeCost: 113.9328},
		{Type: "p6-b300.48xlarge", VCPUs: 192, MemoryMB: 4194304, GPUs: 8, GPUType: "b300", RelativeCost: 142.416},

		// NOT listed: p3 (V100). Retired — DescribeInstanceTypeOfferings returns no
		// p3 offering in us-east-1, us-east-2, us-west-2 or eu-west-1 (checked
		// 2026-07-27), so selecting it produced an instance type that cannot launch.
		// A --gres=gpu:v100 request now resolves through isCompatibleGPUType to a
		// current type instead of a dead one (#447).
	}
}

// GetInstanceTypeInfo returns specs for a specific instance type
func GetInstanceTypeInfo(instanceType string) (InstanceTypeSpec, bool) {
	types := getInstanceTypes()
	for _, t := range types {
		if t.Type == instanceType {
			return t, true
		}
	}
	return InstanceTypeSpec{}, false
}
