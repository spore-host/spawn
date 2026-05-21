package compliance

import (
	"strings"
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

func TestValidateNIST80053Compliance(t *testing.T) {
	tests := []struct {
		name            string
		baseline        Baseline
		cfg             *aws.LaunchConfig
		expectCompliant bool
	}{
		{
			name:     "Low - compliant",
			baseline: BaselineLow,
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
			},
			expectCompliant: true,
		},
		{
			name:     "Moderate - missing controls",
			baseline: BaselineModerate,
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
			},
			expectCompliant: true, // Moderate doesn't enforce public IP check yet
		},
		{
			name:     "High - missing customer KMS",
			baseline: BaselineHigh,
			cfg: &aws.LaunchConfig{
				EBSEncrypted:     true,
				IMDSv2Enforced:   true,
				SecurityGroupIDs: []string{"sg-12345"},
			},
			expectCompliant: false,
		},
		{
			name:     "High - fully compliant",
			baseline: BaselineHigh,
			cfg: &aws.LaunchConfig{
				EBSEncrypted:     true,
				IMDSv2Enforced:   true,
				EBSKMSKeyID:      "arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012",
				SecurityGroupIDs: []string{"sg-12345"},
			},
			expectCompliant: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ValidateNIST80053Compliance(tt.cfg, tt.baseline)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if result.Compliant != tt.expectCompliant {
				t.Errorf("Expected compliant=%v, got %v", tt.expectCompliant, result.Compliant)
				for _, v := range result.Violations {
					t.Logf("  Violation: [%s] %s", v.ControlID, v.Description)
				}
			}
		})
	}
}

func TestEnforceNIST80053Compliance(t *testing.T) {
	tests := []struct {
		name     string
		baseline Baseline
	}{
		{name: "Low", baseline: BaselineLow},
		{name: "Moderate", baseline: BaselineModerate},
		{name: "High", baseline: BaselineHigh},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &aws.LaunchConfig{
				EBSEncrypted:   false,
				IMDSv2Enforced: false,
			}

			EnforceNIST80053Compliance(cfg, tt.baseline)

			// All baselines should enforce these
			if !cfg.EBSEncrypted {
				t.Error("Expected EBS encryption to be enforced")
			}
			if !cfg.IMDSv2Enforced {
				t.Error("Expected IMDSv2 to be enforced")
			}
		})
	}
}

func TestGetBaselineRequirements(t *testing.T) {
	requirements := GetBaselineRequirements()

	if len(requirements) == 0 {
		t.Fatal("Expected requirements map to be populated")
	}

	// Check each baseline has requirements
	for _, baseline := range []Baseline{BaselineLow, BaselineModerate, BaselineHigh} {
		reqs, ok := requirements[baseline]
		if !ok {
			t.Errorf("No requirements found for baseline %s", baseline)
			continue
		}

		if len(reqs) == 0 {
			t.Errorf("Empty requirements for baseline %s", baseline)
		}
	}

	// Moderate should mention "Moderate baseline requirements"
	moderateReqs := requirements[BaselineModerate]
	foundLowRef := false
	for _, req := range moderateReqs {
		if strings.Contains(req, "Low") {
			foundLowRef = true
			break
		}
	}
	if !foundLowRef {
		t.Error("Moderate requirements should reference Low baseline")
	}
}

func TestGetBaselineDescription(t *testing.T) {
	baselines := []Baseline{BaselineLow, BaselineModerate, BaselineHigh}

	for _, baseline := range baselines {
		desc := GetBaselineDescription(baseline)

		if desc == "" {
			t.Errorf("Empty description for baseline %s", baseline)
			continue
		}

		// Should contain impact level
		if !strings.Contains(desc, "Impact Level") {
			t.Errorf("Description for %s should contain 'Impact Level'", baseline)
		}

		// Should contain requirements section
		if !strings.Contains(desc, "Requirements:") {
			t.Errorf("Description for %s should contain 'Requirements:'", baseline)
		}
	}

	// High should mention customer-managed KMS requirement
	highDesc := GetBaselineDescription(BaselineHigh)
	if !strings.Contains(strings.ToLower(highDesc), "customer-managed kms") {
		t.Error("High baseline description should mention customer-managed KMS requirement")
	}

	// Moderate should mention self-hosted requirement
	moderateDesc := GetBaselineDescription(BaselineModerate)
	if !strings.Contains(strings.ToLower(moderateDesc), "self-hosted") {
		t.Error("Moderate baseline description should mention self-hosted requirement")
	}
}

func TestGetBaselineControlCount(t *testing.T) {
	lowCount := GetBaselineControlCount(BaselineLow)
	moderateCount := GetBaselineControlCount(BaselineModerate)
	highCount := GetBaselineControlCount(BaselineHigh)

	if lowCount == 0 {
		t.Error("Low baseline should have controls")
	}

	if moderateCount <= lowCount {
		t.Errorf("Moderate (%d) should have more controls than Low (%d)", moderateCount, lowCount)
	}

	if highCount <= moderateCount {
		t.Errorf("High (%d) should have more controls than Moderate (%d)", highCount, moderateCount)
	}
}

func TestCompareBaselines(t *testing.T) {
	comparison := CompareBaselines()

	if comparison == "" {
		t.Fatal("Expected comparison text, got empty string")
	}

	// Should contain all baselines
	if !strings.Contains(comparison, "Low") {
		t.Error("Comparison should mention Low baseline")
	}
	if !strings.Contains(comparison, "Moderate") {
		t.Error("Comparison should mention Moderate baseline")
	}
	if !strings.Contains(comparison, "High") {
		t.Error("Comparison should mention High baseline")
	}

	// Should contain control counts
	if !strings.Contains(comparison, "controls") {
		t.Error("Comparison should mention control counts")
	}

	// Should contain usage examples
	if !strings.Contains(comparison, "spawn launch") {
		t.Error("Comparison should contain usage examples")
	}
}
