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
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64":     "ami-x86-standard",
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64":      "ami-arm-standard",
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-gpu-x86_64": "ami-x86-gpu",
		"/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-gpu-arm64":  "ami-arm-gpu",
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

func TestDetectGPUInstance(t *testing.T) {
	tests := []struct {
		instanceType string
		wantGPU      bool
	}{
		{"p3.2xlarge", true},  // p3 family
		{"p5.48xlarge", true}, // p5 family
		{"g4.xlarge", true},   // g4 family (prefix match)
		{"g5.xlarge", true},   // g5 family
		{"g6.xlarge", true},   // g6 family
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
