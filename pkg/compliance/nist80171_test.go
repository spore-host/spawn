package compliance

import (
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

func TestNIST80171ControlSet(t *testing.T) {
	controlSet := NIST80171ControlSet()

	if controlSet == nil {
		t.Fatal("Expected control set, got nil")
	}

	if controlSet.Name == "" {
		t.Error("Expected control set name to be set")
	}

	if controlSet.Description == "" {
		t.Error("Expected control set description to be set")
	}

	if len(controlSet.Controls) == 0 {
		t.Error("Expected controls to be defined")
	}

	// Verify we have at least the key controls
	expectedControls := []string{"SC-28", "AC-17", "AC-06", "AU-02", "IA-05", "SC-12", "SC-13"}
	foundControls := make(map[string]bool)

	for _, control := range controlSet.Controls {
		foundControls[control.ID] = true

		// Verify each control has required fields
		if control.ID == "" {
			t.Error("Control missing ID")
		}
		if control.Name == "" {
			t.Errorf("Control %s missing name", control.ID)
		}
		if control.Description == "" {
			t.Errorf("Control %s missing description", control.ID)
		}
		if control.Family == "" {
			t.Errorf("Control %s missing family", control.ID)
		}
	}

	for _, expectedID := range expectedControls {
		if !foundControls[expectedID] {
			t.Errorf("Expected control %s not found", expectedID)
		}
	}
}

func TestNIST80171_SC28_EBSEncryption(t *testing.T) {
	controlSet := NIST80171ControlSet()

	tests := []struct {
		name      string
		cfg       *aws.LaunchConfig
		wantError bool
	}{
		{
			name: "EBS encryption enabled",
			cfg: &aws.LaunchConfig{
				EBSEncrypted: true,
			},
			wantError: false,
		},
		{
			name: "EBS encryption disabled",
			cfg: &aws.LaunchConfig{
				EBSEncrypted: false,
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controlSet.ValidateLaunchConfig(tt.cfg)

			hasViolation := false
			for _, v := range result.Violations {
				if v.ControlID == "SC-28" {
					hasViolation = true
					break
				}
			}

			if tt.wantError && !hasViolation {
				t.Error("Expected SC-28 violation, got none")
			}
			if !tt.wantError && hasViolation {
				t.Error("Expected no SC-28 violation, got one")
			}
		})
	}
}

func TestNIST80171_AC17_IMDSv2(t *testing.T) {
	controlSet := NIST80171ControlSet()

	tests := []struct {
		name      string
		cfg       *aws.LaunchConfig
		wantError bool
	}{
		{
			name: "IMDSv2 enforced",
			cfg: &aws.LaunchConfig{
				IMDSv2Enforced: true,
			},
			wantError: false,
		},
		{
			name: "IMDSv2 not enforced",
			cfg: &aws.LaunchConfig{
				IMDSv2Enforced: false,
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controlSet.ValidateLaunchConfig(tt.cfg)

			hasViolation := false
			for _, v := range result.Violations {
				if v.ControlID == "AC-17" {
					hasViolation = true
					break
				}
			}

			if tt.wantError && !hasViolation {
				t.Error("Expected AC-17 violation, got none")
			}
			if !tt.wantError && hasViolation {
				t.Error("Expected no AC-17 violation, got one")
			}
		})
	}
}

func TestNIST80171_EnforceLaunchConfig(t *testing.T) {
	controlSet := NIST80171ControlSet()

	cfg := &aws.LaunchConfig{
		EBSEncrypted:   false,
		IMDSv2Enforced: false,
	}

	// Enforce controls
	controlSet.EnforceLaunchConfig(cfg)

	// Verify enforcement
	if !cfg.EBSEncrypted {
		t.Error("Expected EBS encryption to be enforced")
	}

	if !cfg.IMDSv2Enforced {
		t.Error("Expected IMDSv2 to be enforced")
	}
}

func TestNIST80171_CompliantConfig(t *testing.T) {
	controlSet := NIST80171ControlSet()

	cfg := &aws.LaunchConfig{
		EBSEncrypted:   true,
		IMDSv2Enforced: true,
		EBSKMSKeyID:    "arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012",
	}

	result := controlSet.ValidateLaunchConfig(cfg)

	if !result.Compliant {
		t.Errorf("Expected compliant config, got %d violations", len(result.Violations))
		for _, v := range result.Violations {
			t.Logf("  Violation: [%s] %s", v.ControlID, v.Description)
		}
	}

	if len(result.Violations) > 0 {
		t.Errorf("Expected no violations, got %d", len(result.Violations))
	}
}

func TestNIST80171_NonCompliantConfig(t *testing.T) {
	controlSet := NIST80171ControlSet()

	cfg := &aws.LaunchConfig{
		EBSEncrypted:   false,
		IMDSv2Enforced: false,
	}

	result := controlSet.ValidateLaunchConfig(cfg)

	if result.Compliant {
		t.Error("Expected non-compliant config, got compliant")
	}

	if len(result.Violations) == 0 {
		t.Error("Expected violations, got none")
	}

	// Should have at least SC-28 and AC-17 violations
	foundSC28 := false
	foundAC17 := false
	for _, v := range result.Violations {
		if v.ControlID == "SC-28" {
			foundSC28 = true
		}
		if v.ControlID == "AC-17" {
			foundAC17 = true
		}
	}

	if !foundSC28 {
		t.Error("Expected SC-28 violation not found")
	}
	if !foundAC17 {
		t.Error("Expected AC-17 violation not found")
	}
}

func TestValidateNIST80171Compliance(t *testing.T) {
	tests := []struct {
		name             string
		cfg              *aws.LaunchConfig
		expectCompliant  bool
		expectViolations int
	}{
		{
			name: "Fully compliant",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
			},
			expectCompliant:  true,
			expectViolations: 0,
		},
		{
			name: "Missing EBS encryption",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   false,
				IMDSv2Enforced: true,
			},
			expectCompliant:  false,
			expectViolations: 1,
		},
		{
			name: "Missing IMDSv2",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: false,
			},
			expectCompliant:  false,
			expectViolations: 1,
		},
		{
			name: "Missing both",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   false,
				IMDSv2Enforced: false,
			},
			expectCompliant:  false,
			expectViolations: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ValidateNIST80171Compliance(tt.cfg)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if result.Compliant != tt.expectCompliant {
				t.Errorf("Expected compliant=%v, got %v", tt.expectCompliant, result.Compliant)
			}

			if len(result.Violations) < tt.expectViolations {
				t.Errorf("Expected at least %d violations, got %d", tt.expectViolations, len(result.Violations))
			}
		})
	}
}

func TestEnforceNIST80171Compliance(t *testing.T) {
	cfg := &aws.LaunchConfig{
		EBSEncrypted:   false,
		IMDSv2Enforced: false,
	}

	EnforceNIST80171Compliance(cfg)

	if !cfg.EBSEncrypted {
		t.Error("Expected EBS encryption to be enforced")
	}

	if !cfg.IMDSv2Enforced {
		t.Error("Expected IMDSv2 to be enforced")
	}
}
