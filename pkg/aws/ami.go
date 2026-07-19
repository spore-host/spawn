package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// AMI types for Amazon Linux 2023
const (
	AMI_AL2023_X86     = "al2023-x86"
	AMI_AL2023_ARM     = "al2023-arm"
	AMI_AL2023_GPU_X86 = "al2023-gpu-x86"
	AMI_AL2023_GPU_ARM = "al2023-gpu-arm"
)

// SSM Parameter paths for AL2023 AMIs
var amiParameters = map[string]string{
	// Standard AL2023
	AMI_AL2023_X86: "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64",
	AMI_AL2023_ARM: "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64",

	// GPU-enabled AL2023: the "Deep Learning Base OSS Nvidia Driver GPU AMI
	// (Amazon Linux 2023)". AL2023 does NOT publish a GPU variant under the
	// ami-amazon-linux-latest path (the old al2023-ami-kernel-default-gpu-*
	// parameters here never existed → every GPU auto-AMI launch failed with
	// SSM ParameterNotFound, spawn#384). The DL Base AMI ships the NVIDIA driver
	// pre-installed and resolves via the deeplearning SSM namespace.
	AMI_AL2023_GPU_X86: "/aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-amazon-linux-2023/latest/ami-id",
	AMI_AL2023_GPU_ARM: "/aws/service/deeplearning/ami/arm64/base-oss-nvidia-driver-gpu-amazon-linux-2023/latest/ami-id",
}

// GetAL2023AMI returns the latest Amazon Linux 2023 AMI for the specified architecture and GPU support
func (c *Client) GetAL2023AMI(ctx context.Context, region string, arch string, gpu bool) (string, error) {
	// Determine AMI type
	amiType := ""

	if gpu {
		if arch == "arm64" {
			amiType = AMI_AL2023_GPU_ARM
		} else {
			amiType = AMI_AL2023_GPU_X86
		}
	} else {
		if arch == "arm64" {
			amiType = AMI_AL2023_ARM
		} else {
			amiType = AMI_AL2023_X86
		}
	}

	parameterName, ok := amiParameters[amiType]
	if !ok {
		return "", fmt.Errorf("unknown AMI type: %s", amiType)
	}

	// Query SSM Parameter Store (region-specific)
	ssmClient := ssm.NewFromConfig(c.regionalConfig(region))

	output, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(parameterName),
	})

	if err != nil {
		return "", fmt.Errorf("failed to get AMI from SSM: %w", err)
	}

	if output.Parameter == nil || output.Parameter.Value == nil {
		return "", fmt.Errorf("AMI parameter value is nil")
	}

	amiID := *output.Parameter.Value

	return amiID, nil
}

// DetectGPUInstance returns true if the instance type supports GPU
func DetectGPUInstance(instanceType string) bool {
	// NVIDIA GPU instance families. These get the DL Base OSS Nvidia Driver AMI
	// via GetAL2023AMI. Matching is exact-family, so multi-letter families
	// (g6e, g7e, …) must be listed explicitly — a "g6" entry does NOT match "g6e"
	// (spawn#384: g6e/g7e were missing and silently fell back to a CPU AMI).
	// Neuron families (inf*/trn*) are deliberately NOT here: they are AWS
	// accelerators, not NVIDIA GPUs, and the Nvidia driver AMI has no Neuron
	// runtime — auto-AMI gives them the standard AL2023 CPU AMI, and a Neuron
	// workload should pass an explicit --ami (the Neuron DLAMI).
	// Aligned with truffle's NVIDIA GPU families (the suite's capability
	// authority). g4ad is AMD (not here); g5g is Graviton+T4G (arm64).
	gpuFamilies := map[string]bool{
		"p6":   true, // NVIDIA B200/GB200
		"p5e":  true, // NVIDIA H200
		"p5":   true, // NVIDIA H100
		"p4de": true, // NVIDIA A100 80GB
		"p4d":  true, // NVIDIA A100 40GB
		"p3":   true, // NVIDIA V100
		"p2":   true, // NVIDIA K80
		"g7e":  true, // NVIDIA (Blackwell-gen)
		"g7":   true, // NVIDIA (Blackwell-gen)
		"g6e":  true, // NVIDIA L40S
		"g6":   true, // NVIDIA L4
		"g5":   true, // NVIDIA A10G
		"g5g":  true, // NVIDIA T4G (arm64)
		"g4dn": true, // NVIDIA T4
		"g3":   true, // NVIDIA M60
	}

	// Extract family (e.g., "p5" from "p5.48xlarge")
	family := ""
	for i, char := range instanceType {
		if char == '.' {
			family = instanceType[:i]
			break
		}
	}

	return gpuFamilies[family]
}

// DetectArchitecture returns the CPU architecture for an instance type from a
// static allow-list of Graviton (ARM) families. This is the OFFLINE fallback:
// it can't know about a family that didn't exist when this list was written
// (spawn#410: m9g/Graviton5 fell through to x86_64 and the launch failed on an
// arch-mismatched AMI). Callers with an AWS client + region should prefer
// (*Client).resolveArchitecture, which asks EC2 authoritatively; this remains
// for the pure/no-client call sites (wizard, sweep grouping key) and as the
// fallback when the API is unreachable.
func DetectArchitecture(instanceType string) string {
	// Graviton (ARM) instance families. New generations are added here as a
	// best-effort default, but the authoritative source is resolveArchitecture.
	armFamilies := map[string]bool{
		"t4g": true,
		"m6g": true, "m6gd": true, "m7g": true, "m7gd": true, "m8g": true, "m8gd": true, "m9g": true,
		"c6g": true, "c6gd": true, "c6gn": true, "c7g": true, "c7gd": true, "c7gn": true, "c8g": true, "c8gd": true, "c8gn": true, "c9g": true,
		"r6g": true, "r6gd": true, "r7g": true, "r7gd": true, "r8g": true, "r8gd": true, "r9g": true,
		"x2gd": true,
		"i8g":  true, "im8g": true, "is8g": true,
		"hpc7g": true,
		"g5g":   true, // ARM GPU
	}

	if armFamilies[instanceFamily(instanceType)] {
		return "arm64"
	}

	return "x86_64"
}

// instanceFamily returns the family prefix of an instance type ("m9g" from
// "m9g.24xlarge"), or the whole string if it has no '.'.
func instanceFamily(instanceType string) string {
	for i, char := range instanceType {
		if char == '.' {
			return instanceType[:i]
		}
	}
	return instanceType
}

// resolveArchitecture returns the CPU architecture for an instance type,
// authoritatively via EC2 DescribeInstanceTypes (ProcessorInfo.SupportedArchitectures)
// so a new family (m9g, r9g, …) needs no code change — the root cause of
// spawn#410. Falls back to the static DetectArchitecture allow-list if the API
// call fails or returns nothing (offline, throttled, or an unknown type), which
// preserves today's behavior for known families. When a type advertises both
// arm64 and x86_64 (rare), arm64 wins — spawn's ARM instance types are ARM.
func (c *Client) resolveArchitecture(ctx context.Context, region, instanceType string) string {
	ec2Client := c.regionalEC2(region)
	out, err := ec2Client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []ec2types.InstanceType{ec2types.InstanceType(instanceType)},
	})
	if err != nil || len(out.InstanceTypes) == 0 {
		return DetectArchitecture(instanceType)
	}
	pi := out.InstanceTypes[0].ProcessorInfo
	if pi == nil || len(pi.SupportedArchitectures) == 0 {
		return DetectArchitecture(instanceType)
	}
	sawX86 := false
	for _, a := range pi.SupportedArchitectures {
		switch a {
		case ec2types.ArchitectureTypeArm64:
			return "arm64"
		case ec2types.ArchitectureTypeX8664:
			sawX86 = true
		}
	}
	if sawX86 {
		return "x86_64"
	}
	// Some other arch (e.g. i386/arm64_mac) — fall back to the static heuristic.
	return DetectArchitecture(instanceType)
}

// GetRecommendedAMI returns the recommended AMI for an instance type in a specific region
func (c *Client) GetRecommendedAMI(ctx context.Context, region string, instanceType string) (string, error) {
	// Resolve arch authoritatively from EC2 so a new Graviton family (m9g, …) gets
	// the right arm64 AMI without a code change (spawn#410); falls back to the
	// static allow-list when the API is unavailable.
	arch := c.resolveArchitecture(ctx, region, instanceType)
	gpu := DetectGPUInstance(instanceType)

	return c.GetAL2023AMI(ctx, region, arch, gpu)
}
