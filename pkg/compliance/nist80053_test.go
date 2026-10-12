package compliance

import (
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

func TestNIST80053ControlSet(t *testing.T) {
	tests := []struct {
		name        string
		baseline    Baseline
		minControls int
	}{
		{
			name:        "Low baseline",
			baseline:    BaselineLow,
			minControls: 10, // At least NIST 800-171 controls
		},
		{
			name:        "Moderate baseline",
			baseline:    BaselineModerate,
			minControls: 13, // Low + moderate-specific
		},
		{
			name:        "High baseline",
			baseline:    BaselineHigh,
			minControls: 18, // Moderate + high-specific
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controlSet := NIST80053ControlSet(tt.baseline)

			if controlSet == nil {
				t.Fatal("Expected control set, got nil")
			}

			if controlSet.Name == "" {
				t.Error("Expected control set name to be set")
			}

			if controlSet.Description == "" {
				t.Error("Expected control set description to be set")
			}

			if len(controlSet.Controls) < tt.minControls {
				t.Errorf("Expected at least %d controls, got %d", tt.minControls, len(controlSet.Controls))
			}

			// Verify each control has required fields
			for _, control := range controlSet.Controls {
				if control.ID == "" {
					t.Error("Control missing ID")
				}
				if control.Name == "" {
					t.Errorf("Control %s missing name", control.ID)
				}
				if control.Description == "" {
					t.Errorf("Control %s missing description", control.ID)
				}
			}
		})
	}
}

func TestNIST80053_BaselineInheritance(t *testing.T) {
	lowControls := getLowBaselineControls()
	moderateControls := getModerateBaselineControls()
	highControls := getHighBaselineControls()

	// Moderate should have more controls than Low
	if len(moderateControls) <= len(lowControls) {
		t.Errorf("Expected Moderate (%d) > Low (%d) controls", len(moderateControls), len(lowControls))
	}

	// High should have more controls than Moderate
	if len(highControls) <= len(moderateControls) {
		t.Errorf("Expected High (%d) > Moderate (%d) controls", len(highControls), len(moderateControls))
	}

	// Verify Low controls are included in Moderate
	lowControlIDs := make(map[string]bool)
	for _, c := range lowControls {
		lowControlIDs[c.ID] = true
	}

	foundInModerate := 0
	for _, c := range moderateControls {
		if lowControlIDs[c.ID] {
			foundInModerate++
		}
	}

	if foundInModerate != len(lowControls) {
		t.Errorf("Not all Low controls found in Moderate: found %d of %d", foundInModerate, len(lowControls))
	}
}

func TestNIST80053_HighBaseline_CustomerKMS(t *testing.T) {
	controlSet := NIST80053ControlSet(BaselineHigh)

	tests := []struct {
		name      string
		cfg       *aws.LaunchConfig
		wantError bool
	}{
		{
			name: "Customer KMS key ARN",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
				EBSKMSKeyID:    "arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012",
			},
			wantError: false,
		},
		{
			name: "Customer KMS key alias",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
				EBSKMSKeyID:    "alias/my-key",
			},
			wantError: false,
		},
		{
			name: "Customer KMS key UUID",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
				EBSKMSKeyID:    "12345678-1234-1234-1234-123456789012",
			},
			wantError: false,
		},
		{
			name: "No KMS key (High requires customer key)",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
				EBSKMSKeyID:    "",
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controlSet.ValidateLaunchConfig(tt.cfg)

			hasSC28Violation := false
			for _, v := range result.Violations {
				if v.ControlID == "SC-28(1)" {
					hasSC28Violation = true
					break
				}
			}

			if tt.wantError && !hasSC28Violation {
				t.Error("Expected SC-28(1) violation for missing customer KMS key, got none")
			}
			if !tt.wantError && hasSC28Violation {
				t.Error("Expected no SC-28(1) violation with valid customer KMS key")
			}
		})
	}
}

func TestNIST80053_HighBaseline_SecurityGroups(t *testing.T) {
	controlSet := NIST80053ControlSet(BaselineHigh)

	tests := []struct {
		name      string
		cfg       *aws.LaunchConfig
		wantError bool
	}{
		{
			name: "Explicit security groups",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:     true,
				IMDSv2Enforced:   true,
				EBSKMSKeyID:      "arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012",
				SecurityGroupIDs: []string{"sg-12345"},
			},
			wantError: false,
		},
		{
			name: "No security groups (High requires explicit)",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:     true,
				IMDSv2Enforced:   true,
				EBSKMSKeyID:      "arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012",
				SecurityGroupIDs: []string{},
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controlSet.ValidateLaunchConfig(tt.cfg)

			hasSC07Violation := false
			for _, v := range result.Violations {
				if v.ControlID == "SC-07(5)" {
					hasSC07Violation = true
					break
				}
			}

			if tt.wantError && !hasSC07Violation {
				t.Error("Expected SC-07(5) violation for missing security groups, got none")
			}
			if !tt.wantError && hasSC07Violation {
				t.Error("Expected no SC-07(5) violation with explicit security groups")
			}
		})
	}
}
