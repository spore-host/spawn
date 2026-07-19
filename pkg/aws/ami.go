package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
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

// DetectArchitecture returns the CPU architecture for an instance type
func DetectArchitecture(instanceType string) string {
	// Graviton (ARM) instance families
	armFamilies := map[string]bool{
		"t4g": true,
		"m6g": true, "m6gd": true, "m7g": true, "m7gd": true, "m8g": true,
		"c6g": true, "c6gd": true, "c6gn": true, "c7g": true, "c7gd": true, "c7gn": true, "c8g": true,
		"r6g": true, "r6gd": true, "r7g": true, "r7gd": true, "r8g": true,
		"x2gd": true,
		"g5g":  true, // ARM GPU
	}

	// Extract family
	family := ""
	for i, char := range instanceType {
		if char == '.' {
			family = instanceType[:i]
			break
		}
	}

	if armFamilies[family] {
		return "arm64"
	}

	return "x86_64"
}

// GetRecommendedAMI returns the recommended AMI for an instance type in a specific region
func (c *Client) GetRecommendedAMI(ctx context.Context, region string, instanceType string) (string, error) {
	arch := DetectArchitecture(instanceType)
	gpu := DetectGPUInstance(instanceType)

	return c.GetAL2023AMI(ctx, region, arch, gpu)
}
