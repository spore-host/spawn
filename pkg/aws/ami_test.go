package aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

func TestGetAL2023AMI(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	// Pre-populate SSM parameters with test AMI IDs.
	ssmClient := ssm.NewFromConfig(env.AWSConfig)
	params := map[string]string{
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64":                           "ami-x86-standard",
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64":                            "ami-arm-standard",
		"/aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-amazon-linux-2023/latest/ami-id": "ami-x86-gpu",
		"/aws/service/deeplearning/ami/arm64/base-oss-nvidia-driver-gpu-amazon-linux-2023/latest/ami-id":  "ami-arm-gpu",
	}
	for name, val := range params {
		if _, err := ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
			Name:  aws.String(name),
			Value: aws.String(val),
			Type:  ssmtypes.ParameterTypeString,
		}); err != nil {
			t.Fatalf("PutParameter %s: %v", name, err)
		}
	}

	client := NewClientFromConfig(env.AWSConfig)

	tests := []struct {
		name    string
		region  string
		arch    string
		gpu     bool
		wantAMI string
	}{
		{"x86 standard", "us-east-1", "x86_64", false, "ami-x86-standard"},
		{"arm standard", "us-east-1", "arm64", false, "ami-arm-standard"},
		{"x86 gpu", "us-east-1", "x86_64", true, "ami-x86-gpu"},
		{"arm gpu", "us-east-1", "arm64", true, "ami-arm-gpu"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := client.GetAL2023AMI(ctx, tt.region, tt.arch, tt.gpu)
			if err != nil {
				t.Fatalf("GetAL2023AMI: %v", err)
			}
			if got != tt.wantAMI {
				t.Errorf("GetAL2023AMI = %q, want %q", got, tt.wantAMI)
			}
		})
	}
}

// TestGetRecommendedAMI_GPUFamilyResolvesDLAMI ties the whole resolver together
// (spawn#601): given only an instance TYPE, GetRecommendedAMI must resolve the
// NVIDIA GPU DLAMI SSM param for a GPU family and the standard AL2023 param for a
// CPU family — the exact path the task-run / launch launch uses via
// launcher.Provision. Architecture is derived from the type (resolveArchitecture,
// falling back to the static allow-list here since Substrate need not implement
// DescribeInstanceTypes).
func TestGetRecommendedAMI_GPUFamilyResolvesDLAMI(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	ssmClient := ssm.NewFromConfig(env.AWSConfig)
	params := map[string]string{
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64":                           "ami-x86-standard",
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64":                            "ami-arm-standard",
		"/aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-amazon-linux-2023/latest/ami-id": "ami-x86-gpu",
		"/aws/service/deeplearning/ami/arm64/base-oss-nvidia-driver-gpu-amazon-linux-2023/latest/ami-id":  "ami-arm-gpu",
	}
	for name, val := range params {
		if _, err := ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
			Name:  aws.String(name),
			Value: aws.String(val),
			Type:  ssmtypes.ParameterTypeString,
		}); err != nil {
			t.Fatalf("PutParameter %s: %v", name, err)
		}
	}

	client := NewClientFromConfig(env.AWSConfig)

	tests := []struct {
		name         string
		instanceType string
		wantAMI      string
	}{
		{"g5 x86 GPU → DLAMI", "g5.2xlarge", "ami-x86-gpu"},
		{"g5g arm GPU → arm DLAMI", "g5g.xlarge", "ami-arm-gpu"},
		{"c7i CPU → standard x86", "c7i.4xlarge", "ami-x86-standard"},
		{"c8g arm CPU → standard arm", "c8g.4xlarge", "ami-arm-standard"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := client.GetRecommendedAMI(ctx, "us-east-1", tt.instanceType)
			if err != nil {
				t.Fatalf("GetRecommendedAMI(%q): %v", tt.instanceType, err)
			}
			if got != tt.wantAMI {
				t.Errorf("GetRecommendedAMI(%q) = %q, want %q", tt.instanceType, got, tt.wantAMI)
			}
		})
	}
}

func TestDetectGPUInstance(t *testing.T) {
	tests := []struct {
		instanceType string
		wantGPU      bool
	}{
		{"p3.2xlarge", true},    // p3 family
		{"p5.48xlarge", true},   // p5 family
		{"g4dn.xlarge", true},   // NVIDIA T4
		{"g5.xlarge", true},     // g5 family
		{"g6.xlarge", true},     // g6 family
		{"g6e.2xlarge", true},   // spawn#384: g6e was missing → CPU AMI
		{"g7e.2xlarge", true},   // spawn#384: g7e was missing → CPU AMI
		{"g7.2xlarge", true},    // spawn#384: g7 was missing → CPU AMI
		{"g5g.xlarge", true},    // arm64 NVIDIA T4G
		{"inf2.xlarge", false},  // Neuron, not NVIDIA — no Nvidia-driver AMI
		{"trn1.2xlarge", false}, // Neuron, not NVIDIA
		{"g4ad.xlarge", false},  // AMD Radeon, not NVIDIA
		{"t3.micro", false},
		{"m5.large", false},
		{"c5.xlarge", false},
		{"r5.2xlarge", false},
	}

	for _, tt := range tests {
		t.Run(tt.instanceType, func(t *testing.T) {
			got := DetectGPUInstance(tt.instanceType)
			if got != tt.wantGPU {
				t.Errorf("DetectGPUInstance(%q) = %v, want %v", tt.instanceType, got, tt.wantGPU)
			}
		})
	}
}
