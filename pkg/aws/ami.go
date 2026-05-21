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

	// GPU-enabled AL2023 (NVIDIA drivers pre-installed)
	AMI_AL2023_GPU_X86: "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-gpu-x86_64",
	AMI_AL2023_GPU_ARM: "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-gpu-arm64",
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
	cfg := c.cfg
	cfg.Region = region
	ssmClient := ssm.NewFromConfig(cfg)

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
	// GPU instance families
	gpuFamilies := map[string]bool{
		"p5":   true, // NVIDIA H100
		"p4":   true, // NVIDIA A100
		"p3":   true, // NVIDIA V100
		"g6":   true, // NVIDIA L4/L40S
		"g5":   true, // NVIDIA A10G
		"g4":   true, // NVIDIA T4
		"g5g":  true, // ARM GPU
		"inf2": true, // Inferentia2
		"inf1": true, // Inferentia
		"trn1": true, // Trainium
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
