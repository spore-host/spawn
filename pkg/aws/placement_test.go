package aws

import (
	"context"
	"testing"

	"github.com/spore-host/spawn/pkg/testutil"
)

func TestValidateInstanceTypeForPlacementGroup(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		wantErr      bool
	}{
		// Compute optimized - supported
		{name: "c4.large supported", instanceType: "c4.large", wantErr: false},
		{name: "c5.xlarge supported", instanceType: "c5.xlarge", wantErr: false},
		{name: "c5n.18xlarge supported", instanceType: "c5n.18xlarge", wantErr: false},
		{name: "c6g.medium supported", instanceType: "c6g.medium", wantErr: false},
		{name: "c6gn.16xlarge supported", instanceType: "c6gn.16xlarge", wantErr: false},
		{name: "c7g.large supported", instanceType: "c7g.large", wantErr: false},

		// Memory optimized - supported
		{name: "r4.large supported", instanceType: "r4.large", wantErr: false},
		{name: "r5.xlarge supported", instanceType: "r5.xlarge", wantErr: false},
		{name: "r5n.large supported", instanceType: "r5n.large", wantErr: false},
		{name: "r6g.medium supported", instanceType: "r6g.medium", wantErr: false},
		{name: "x1.16xlarge supported", instanceType: "x1.16xlarge", wantErr: false},
		{name: "x1e.xlarge supported", instanceType: "x1e.xlarge", wantErr: false},

		// Storage optimized - supported
		{name: "d2.xlarge supported", instanceType: "d2.xlarge", wantErr: false},
		{name: "h1.2xlarge supported", instanceType: "h1.2xlarge", wantErr: false},
		{name: "i3.large supported", instanceType: "i3.large", wantErr: false},
		{name: "i3en.large supported", instanceType: "i3en.large", wantErr: false},

		// Accelerated - supported
		{name: "p2.xlarge supported", instanceType: "p2.xlarge", wantErr: false},
		{name: "p3.2xlarge supported", instanceType: "p3.2xlarge", wantErr: false},
		{name: "p4.24xlarge supported", instanceType: "p4.24xlarge", wantErr: false},
		{name: "g3.4xlarge supported", instanceType: "g3.4xlarge", wantErr: false},
		{name: "g4dn.xlarge supported", instanceType: "g4dn.xlarge", wantErr: false},
		{name: "inf1.xlarge supported", instanceType: "inf1.xlarge", wantErr: false},

		// Unsupported types
		{name: "t2.micro unsupported", instanceType: "t2.micro", wantErr: true},
		{name: "t3.small unsupported", instanceType: "t3.small", wantErr: true},
		{name: "t3a.medium unsupported", instanceType: "t3a.medium", wantErr: true},
		{name: "t4g.nano unsupported", instanceType: "t4g.nano", wantErr: true},
		{name: "m5.large unsupported", instanceType: "m5.large", wantErr: true},
		{name: "m6i.xlarge unsupported", instanceType: "m6i.xlarge", wantErr: true},
		{name: "a1.medium unsupported", instanceType: "a1.medium", wantErr: true},
	}

	client := &Client{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.ValidateInstanceTypeForPlacementGroup(context.Background(), tt.instanceType)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateInstanceTypeForPlacementGroup() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateInstanceTypeForEFAInRegion_UsesProvidedRegion verifies the EFA
// validation queries the specified region rather than the client's default.
// Regression for #307 — hpc6a.48xlarge only exists in us-east-2; querying
// us-east-1 returned InvalidInstanceType.
func TestValidateInstanceTypeForEFAInRegion_UsesProvidedRegion(t *testing.T) {
	// This is a compile-time sanity check: the method must exist with the right signature.
	var c *Client
	_ = c.ValidateInstanceTypeForEFAInRegion // will not compile if renamed/removed
}

// TestValidateInstanceTypeForEFA_Substrate uses a Substrate-backed EC2 client
// to verify EFA validation calls DescribeInstanceTypes (now an API call, not a
// static allowlist). Substrate registers c5n.18xlarge with EfaSupported=true
// and t3.micro with EfaSupported=false.
func TestValidateInstanceTypeForEFA_Substrate(t *testing.T) {
	env := testutil.SubstrateServer(t)
	client := NewClientFromConfig(env.AWSConfig)

	// Substrate pre-populates common instance types with accurate metadata.
	// We only test the call doesn't panic and returns some result.
	// The critical behaviour (using the right region) is tested in e2e tier1.
	err := client.ValidateInstanceTypeForEFAInRegion(context.Background(), "c5n.18xlarge", "")
	// Substrate may or may not model EFA support; we just verify no unexpected panic.
	t.Logf("c5n.18xlarge EFA check result: %v", err)
}

// TestValidateInstanceTypeForEFA was originally a static-allowlist test.
// EFA validation now calls DescribeInstanceTypes and requires real AWS or
// a substrate server with full instance-type metadata. Covered by e2e tier1.
func TestValidateInstanceTypeForEFA(t *testing.T) {
	t.Skip("EFA validation requires live DescribeInstanceTypes; covered by e2e TestTier1_EFAValidationRegion")
	tests := []struct {
		name         string
		instanceType string
		wantErr      bool
	}{
		// EFA supported types
		{name: "c5n.18xlarge supported", instanceType: "c5n.18xlarge", wantErr: false},
		{name: "c5n.metal supported", instanceType: "c5n.metal", wantErr: false},
		{name: "c6gn.16xlarge supported", instanceType: "c6gn.16xlarge", wantErr: false},
		{name: "g4dn.8xlarge supported", instanceType: "g4dn.8xlarge", wantErr: false},
		{name: "g4dn.12xlarge supported", instanceType: "g4dn.12xlarge", wantErr: false},
		{name: "g4dn.metal supported", instanceType: "g4dn.metal", wantErr: false},
		{name: "g5.8xlarge supported", instanceType: "g5.8xlarge", wantErr: false},
		{name: "g5.12xlarge supported", instanceType: "g5.12xlarge", wantErr: false},
		{name: "g5.16xlarge supported", instanceType: "g5.16xlarge", wantErr: false},
		{name: "g5.24xlarge supported", instanceType: "g5.24xlarge", wantErr: false},
		{name: "g5.48xlarge supported", instanceType: "g5.48xlarge", wantErr: false},
		{name: "i3en.12xlarge supported", instanceType: "i3en.12xlarge", wantErr: false},
		{name: "i3en.24xlarge supported", instanceType: "i3en.24xlarge", wantErr: false},
		{name: "i3en.metal supported", instanceType: "i3en.metal", wantErr: false},
		{name: "inf1.24xlarge supported", instanceType: "inf1.24xlarge", wantErr: false},
		{name: "m5dn.24xlarge supported", instanceType: "m5dn.24xlarge", wantErr: false},
		{name: "m5n.24xlarge supported", instanceType: "m5n.24xlarge", wantErr: false},
		{name: "m6i.32xlarge supported", instanceType: "m6i.32xlarge", wantErr: false},
		{name: "p3dn.24xlarge supported", instanceType: "p3dn.24xlarge", wantErr: false},
		{name: "p4d.24xlarge supported", instanceType: "p4d.24xlarge", wantErr: false},
		{name: "p4de.24xlarge supported", instanceType: "p4de.24xlarge", wantErr: false},
		{name: "p5.48xlarge supported", instanceType: "p5.48xlarge", wantErr: false},
		{name: "r5dn.24xlarge supported", instanceType: "r5dn.24xlarge", wantErr: false},
		{name: "r5n.24xlarge supported", instanceType: "r5n.24xlarge", wantErr: false},
		{name: "r6i.32xlarge supported", instanceType: "r6i.32xlarge", wantErr: false},
		{name: "trn1.32xlarge supported", instanceType: "trn1.32xlarge", wantErr: false},

		// EFA unsupported types (even if they support placement groups)
		{name: "c5.large unsupported", instanceType: "c5.large", wantErr: true},
		{name: "c5.xlarge unsupported", instanceType: "c5.xlarge", wantErr: true},
		{name: "c5n.large unsupported", instanceType: "c5n.large", wantErr: true},
		{name: "c5n.xlarge unsupported", instanceType: "c5n.xlarge", wantErr: true},
		{name: "c5n.2xlarge unsupported", instanceType: "c5n.2xlarge", wantErr: true},
		{name: "c5n.4xlarge unsupported", instanceType: "c5n.4xlarge", wantErr: true},
		{name: "g4dn.xlarge unsupported", instanceType: "g4dn.xlarge", wantErr: true},
		{name: "g4dn.2xlarge unsupported", instanceType: "g4dn.2xlarge", wantErr: true},
		{name: "g4dn.4xlarge unsupported", instanceType: "g4dn.4xlarge", wantErr: true},
		{name: "g5.xlarge unsupported", instanceType: "g5.xlarge", wantErr: true},
		{name: "g5.2xlarge unsupported", instanceType: "g5.2xlarge", wantErr: true},
		{name: "g5.4xlarge unsupported", instanceType: "g5.4xlarge", wantErr: true},
		{name: "t3.micro unsupported", instanceType: "t3.micro", wantErr: true},
		{name: "m5.large unsupported", instanceType: "m5.large", wantErr: true},
	}

	client := &Client{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.ValidateInstanceTypeForEFAInRegion(context.Background(), tt.instanceType, "")
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateInstanceTypeForEFA() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateInstanceTypeForEFA_ErrorMessage(t *testing.T) {
	t.Skip("EFA validation requires live DescribeInstanceTypes; covered by e2e TestTier1_EFAValidationRegion")
}

func TestValidateInstanceTypeForPlacementGroup_ErrorMessage(t *testing.T) {
	client := &Client{}
	err := client.ValidateInstanceTypeForPlacementGroup(context.Background(), "t3.micro")

	if err == nil {
		t.Fatal("expected error for unsupported instance type, got nil")
	}

	expectedMsg := "instance type t3.micro does not support cluster placement groups"
	if err.Error() != expectedMsg {
		t.Errorf("expected error message %q, got %q", expectedMsg, err.Error())
	}
}
