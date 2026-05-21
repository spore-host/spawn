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
	Price    float64 // On-demand hourly price (approximate)
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

	// Sort by price (cheapest first)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Price < candidates[j].Price
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

// getInstanceTypes returns a list of common EC2 instance types with their specs
func getInstanceTypes() []InstanceTypeSpec {
	return []InstanceTypeSpec{
		// General Purpose (t-series)
		{Type: "t3.micro", VCPUs: 2, MemoryMB: 1024, GPUs: 0, Price: 0.0104},
		{Type: "t3.small", VCPUs: 2, MemoryMB: 2048, GPUs: 0, Price: 0.0208},
		{Type: "t3.medium", VCPUs: 2, MemoryMB: 4096, GPUs: 0, Price: 0.0416},
		{Type: "t3.large", VCPUs: 2, MemoryMB: 8192, GPUs: 0, Price: 0.0832},
		{Type: "t3.xlarge", VCPUs: 4, MemoryMB: 16384, GPUs: 0, Price: 0.1664},
		{Type: "t3.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 0, Price: 0.3328},

		// General Purpose (m-series)
		{Type: "m5.large", VCPUs: 2, MemoryMB: 8192, GPUs: 0, Price: 0.096},
		{Type: "m5.xlarge", VCPUs: 4, MemoryMB: 16384, GPUs: 0, Price: 0.192},
		{Type: "m5.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 0, Price: 0.384},
		{Type: "m5.4xlarge", VCPUs: 16, MemoryMB: 65536, GPUs: 0, Price: 0.768},
		{Type: "m5.8xlarge", VCPUs: 32, MemoryMB: 131072, GPUs: 0, Price: 1.536},
		{Type: "m5.12xlarge", VCPUs: 48, MemoryMB: 196608, GPUs: 0, Price: 2.304},
		{Type: "m5.16xlarge", VCPUs: 64, MemoryMB: 262144, GPUs: 0, Price: 3.072},
		{Type: "m5.24xlarge", VCPUs: 96, MemoryMB: 393216, GPUs: 0, Price: 4.608},

		// Compute Optimized (c-series)
		{Type: "c5.large", VCPUs: 2, MemoryMB: 4096, GPUs: 0, Price: 0.085},
		{Type: "c5.xlarge", VCPUs: 4, MemoryMB: 8192, GPUs: 0, Price: 0.17},
		{Type: "c5.2xlarge", VCPUs: 8, MemoryMB: 16384, GPUs: 0, Price: 0.34},
		{Type: "c5.4xlarge", VCPUs: 16, MemoryMB: 32768, GPUs: 0, Price: 0.68},
		{Type: "c5.9xlarge", VCPUs: 36, MemoryMB: 73728, GPUs: 0, Price: 1.53},
		{Type: "c5.12xlarge", VCPUs: 48, MemoryMB: 98304, GPUs: 0, Price: 2.04},
		{Type: "c5.18xlarge", VCPUs: 72, MemoryMB: 147456, GPUs: 0, Price: 3.06},
		{Type: "c5.24xlarge", VCPUs: 96, MemoryMB: 196608, GPUs: 0, Price: 4.08},

		// Compute Optimized (c6i)
		{Type: "c6i.large", VCPUs: 2, MemoryMB: 4096, GPUs: 0, Price: 0.085},
		{Type: "c6i.xlarge", VCPUs: 4, MemoryMB: 8192, GPUs: 0, Price: 0.17},
		{Type: "c6i.2xlarge", VCPUs: 8, MemoryMB: 16384, GPUs: 0, Price: 0.34},
		{Type: "c6i.4xlarge", VCPUs: 16, MemoryMB: 32768, GPUs: 0, Price: 0.68},
		{Type: "c6i.8xlarge", VCPUs: 32, MemoryMB: 65536, GPUs: 0, Price: 1.36},
		{Type: "c6i.12xlarge", VCPUs: 48, MemoryMB: 98304, GPUs: 0, Price: 2.04},
		{Type: "c6i.16xlarge", VCPUs: 64, MemoryMB: 131072, GPUs: 0, Price: 2.72},
		{Type: "c6i.24xlarge", VCPUs: 96, MemoryMB: 196608, GPUs: 0, Price: 4.08},
		{Type: "c6i.32xlarge", VCPUs: 128, MemoryMB: 262144, GPUs: 0, Price: 5.44},

		// Compute Optimized (c7i - latest generation)
		{Type: "c7i.large", VCPUs: 2, MemoryMB: 4096, GPUs: 0, Price: 0.0893},
		{Type: "c7i.xlarge", VCPUs: 4, MemoryMB: 8192, GPUs: 0, Price: 0.1785},
		{Type: "c7i.2xlarge", VCPUs: 8, MemoryMB: 16384, GPUs: 0, Price: 0.357},
		{Type: "c7i.4xlarge", VCPUs: 16, MemoryMB: 32768, GPUs: 0, Price: 0.714},
		{Type: "c7i.8xlarge", VCPUs: 32, MemoryMB: 65536, GPUs: 0, Price: 1.428},
		{Type: "c7i.12xlarge", VCPUs: 48, MemoryMB: 98304, GPUs: 0, Price: 2.142},
		{Type: "c7i.16xlarge", VCPUs: 64, MemoryMB: 131072, GPUs: 0, Price: 2.856},
		{Type: "c7i.24xlarge", VCPUs: 96, MemoryMB: 196608, GPUs: 0, Price: 4.284},
		{Type: "c7i.48xlarge", VCPUs: 192, MemoryMB: 393216, GPUs: 0, Price: 8.568},

		// Memory Optimized (r-series)
		{Type: "r5.large", VCPUs: 2, MemoryMB: 16384, GPUs: 0, Price: 0.126},
		{Type: "r5.xlarge", VCPUs: 4, MemoryMB: 32768, GPUs: 0, Price: 0.252},
		{Type: "r5.2xlarge", VCPUs: 8, MemoryMB: 65536, GPUs: 0, Price: 0.504},
		{Type: "r5.4xlarge", VCPUs: 16, MemoryMB: 131072, GPUs: 0, Price: 1.008},
		{Type: "r5.8xlarge", VCPUs: 32, MemoryMB: 262144, GPUs: 0, Price: 2.016},
		{Type: "r5.12xlarge", VCPUs: 48, MemoryMB: 393216, GPUs: 0, Price: 3.024},
		{Type: "r5.16xlarge", VCPUs: 64, MemoryMB: 524288, GPUs: 0, Price: 4.032},
		{Type: "r5.24xlarge", VCPUs: 96, MemoryMB: 786432, GPUs: 0, Price: 6.048},

		// GPU Instances (g4dn - T4)
		{Type: "g4dn.xlarge", VCPUs: 4, MemoryMB: 16384, GPUs: 1, GPUType: "t4", Price: 0.526},
		{Type: "g4dn.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 1, GPUType: "t4", Price: 0.752},
		{Type: "g4dn.4xlarge", VCPUs: 16, MemoryMB: 65536, GPUs: 1, GPUType: "t4", Price: 1.204},
		{Type: "g4dn.8xlarge", VCPUs: 32, MemoryMB: 131072, GPUs: 1, GPUType: "t4", Price: 2.176},
		{Type: "g4dn.12xlarge", VCPUs: 48, MemoryMB: 196608, GPUs: 4, GPUType: "t4", Price: 3.912},
		{Type: "g4dn.16xlarge", VCPUs: 64, MemoryMB: 262144, GPUs: 1, GPUType: "t4", Price: 4.352},

		// GPU Instances (g5 - A10G)
		{Type: "g5.xlarge", VCPUs: 4, MemoryMB: 16384, GPUs: 1, GPUType: "a10g", Price: 1.006},
		{Type: "g5.2xlarge", VCPUs: 8, MemoryMB: 32768, GPUs: 1, GPUType: "a10g", Price: 1.212},
		{Type: "g5.4xlarge", VCPUs: 16, MemoryMB: 65536, GPUs: 1, GPUType: "a10g", Price: 1.624},
		{Type: "g5.8xlarge", VCPUs: 32, MemoryMB: 131072, GPUs: 1, GPUType: "a10g", Price: 2.448},
		{Type: "g5.12xlarge", VCPUs: 48, MemoryMB: 196608, GPUs: 4, GPUType: "a10g", Price: 5.672},
		{Type: "g5.16xlarge", VCPUs: 64, MemoryMB: 262144, GPUs: 1, GPUType: "a10g", Price: 3.672},
		{Type: "g5.24xlarge", VCPUs: 96, MemoryMB: 393216, GPUs: 4, GPUType: "a10g", Price: 8.144},
		{Type: "g5.48xlarge", VCPUs: 192, MemoryMB: 786432, GPUs: 8, GPUType: "a10g", Price: 16.288},

		// GPU Instances (p3 - V100)
		{Type: "p3.2xlarge", VCPUs: 8, MemoryMB: 61440, GPUs: 1, GPUType: "v100", Price: 3.06},
		{Type: "p3.8xlarge", VCPUs: 32, MemoryMB: 245760, GPUs: 4, GPUType: "v100", Price: 12.24},
		{Type: "p3.16xlarge", VCPUs: 64, MemoryMB: 491520, GPUs: 8, GPUType: "v100", Price: 24.48},

		// GPU Instances (p4d - A100)
		{Type: "p4d.24xlarge", VCPUs: 96, MemoryMB: 1179648, GPUs: 8, GPUType: "a100", Price: 32.77},

		// GPU Instances (p5 - H100)
		{Type: "p5.48xlarge", VCPUs: 192, MemoryMB: 2097152, GPUs: 8, GPUType: "h100", Price: 98.32},
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
