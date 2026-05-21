package compliance

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

func TestFedRAMPControlSet(t *testing.T) {
	tests := []struct {
		name             string
		level            FedRAMPLevel
		expectedBaseline Baseline
	}{
		{
			name:             "FedRAMP Low",
			level:            FedRAMPLow,
			expectedBaseline: BaselineLow,
		},
		{
			name:             "FedRAMP Moderate",
			level:            FedRAMPModerate,
			expectedBaseline: BaselineModerate,
		},
		{
			name:             "FedRAMP High",
			level:            FedRAMPHigh,
			expectedBaseline: BaselineHigh,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controlSet := FedRAMPControlSet(tt.level)

			if controlSet == nil {
				t.Fatal("Expected control set, got nil")
			}

			if controlSet.Name == "" {
				t.Error("Expected control set name to be set")
			}

			if !strings.Contains(controlSet.Name, "FedRAMP") {
				t.Errorf("Expected FedRAMP branding in name, got: %s", controlSet.Name)
			}

			if controlSet.Description == "" {
				t.Error("Expected control set description to be set")
			}

			if len(controlSet.Controls) == 0 {
				t.Error("Expected controls to be defined")
			}

			// Verify control count matches the baseline
			expectedCount := GetBaselineControlCount(tt.expectedBaseline)
			if len(controlSet.Controls) != expectedCount {
				t.Errorf("Expected %d controls (from %s baseline), got %d",
					expectedCount, tt.expectedBaseline, len(controlSet.Controls))
			}
		})
	}
}

func TestMapFedRAMPToBaseline(t *testing.T) {
	tests := []struct {
		level    FedRAMPLevel
		expected Baseline
	}{
		{FedRAMPLow, BaselineLow},
		{FedRAMPModerate, BaselineModerate},
		{FedRAMPHigh, BaselineHigh},
	}

	for _, tt := range tests {
		result := MapFedRAMPToBaseline(tt.level)
		if result != tt.expected {
			t.Errorf("MapFedRAMPToBaseline(%s) = %s, want %s", tt.level, result, tt.expected)
		}
	}
}

func TestGetFedRAMPControlCount(t *testing.T) {
	lowCount := GetFedRAMPControlCount(FedRAMPLow)
	moderateCount := GetFedRAMPControlCount(FedRAMPModerate)
	highCount := GetFedRAMPControlCount(FedRAMPHigh)

	if lowCount == 0 {
		t.Error("FedRAMP Low should have controls")
	}

	if moderateCount <= lowCount {
		t.Errorf("FedRAMP Moderate (%d) should have more controls than Low (%d)", moderateCount, lowCount)
	}

	if highCount <= moderateCount {
		t.Errorf("FedRAMP High (%d) should have more controls than Moderate (%d)", highCount, moderateCount)
	}

	// Should match baseline counts
	if lowCount != GetBaselineControlCount(BaselineLow) {
		t.Error("FedRAMP Low count should match Low baseline count")
	}
	if moderateCount != GetBaselineControlCount(BaselineModerate) {
		t.Error("FedRAMP Moderate count should match Moderate baseline count")
	}
	if highCount != GetBaselineControlCount(BaselineHigh) {
		t.Error("FedRAMP High count should match High baseline count")
	}
}

func TestValidateFedRAMPCompliance(t *testing.T) {
	tests := []struct {
		name    string
		level   FedRAMPLevel
		cfg     *aws.LaunchConfig
		wantErr bool
	}{
		{
			name:  "FedRAMP Low - compliant",
			level: FedRAMPLow,
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
			},
			wantErr: false,
		},
		{
			name:  "FedRAMP Low - non-compliant",
			level: FedRAMPLow,
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   false,
				IMDSv2Enforced: false,
			},
			wantErr: true,
		},
		{
			name:  "FedRAMP High - missing customer KMS",
			level: FedRAMPHigh,
			cfg: &aws.LaunchConfig{
				EBSEncrypted:     true,
				IMDSv2Enforced:   true,
				SecurityGroupIDs: []string{"sg-12345"},
			},
			wantErr: true,
		},
		{
			name:  "FedRAMP High - fully compliant",
			level: FedRAMPHigh,
			cfg: &aws.LaunchConfig{
				EBSEncrypted:     true,
				IMDSv2Enforced:   true,
				EBSKMSKeyID:      "arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012",
				SecurityGroupIDs: []string{"sg-12345"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ValidateFedRAMPCompliance(tt.cfg, tt.level)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			hasViolations := len(result.Violations) > 0

			if tt.wantErr && !hasViolations {
				t.Error("Expected violations, got none")
			}
			if !tt.wantErr && hasViolations {
				t.Errorf("Expected no violations, got %d", len(result.Violations))
				for _, v := range result.Violations {
					t.Logf("  Violation: [%s] %s", v.ControlID, v.Description)
				}
			}
		})
	}
}

func TestEnforceFedRAMPCompliance(t *testing.T) {
	levels := []FedRAMPLevel{FedRAMPLow, FedRAMPModerate, FedRAMPHigh}

	for _, level := range levels {
		t.Run(string(level), func(t *testing.T) {
			cfg := &aws.LaunchConfig{
				EBSEncrypted:   false,
				IMDSv2Enforced: false,
			}

			EnforceFedRAMPCompliance(cfg, level)

			// All levels should enforce these
			if !cfg.EBSEncrypted {
				t.Error("Expected EBS encryption to be enforced")
			}
			if !cfg.IMDSv2Enforced {
				t.Error("Expected IMDSv2 to be enforced")
			}
		})
	}
}

func TestGetFedRAMPRequirements(t *testing.T) {
	requirements := GetFedRAMPRequirements()

	if len(requirements) == 0 {
		t.Fatal("Expected requirements map to be populated")
	}

	// Check each level has requirements
	for _, level := range []FedRAMPLevel{FedRAMPLow, FedRAMPModerate, FedRAMPHigh} {
		reqs, ok := requirements[level]
		if !ok {
			t.Errorf("No requirements found for level %s", level)
			continue
		}

		if len(reqs) == 0 {
			t.Errorf("Empty requirements for level %s", level)
		}

		// Check for FedRAMP-specific requirements
		found3PAO := false
		foundMonitoring := false
		for _, req := range reqs {
			if strings.Contains(req, "3PAO") {
				found3PAO = true
			}
			if strings.Contains(req, "monitoring") || strings.Contains(req, "Continuous") {
				foundMonitoring = true
			}
		}

		if !found3PAO {
			t.Errorf("FedRAMP %s should mention 3PAO assessment", level)
		}
		if !foundMonitoring {
			t.Errorf("FedRAMP %s should mention continuous monitoring", level)
		}
	}

	// Moderate and High should reference lower levels
	moderateReqs := requirements[FedRAMPModerate]
	foundLowRef := false
	for _, req := range moderateReqs {
		if strings.Contains(req, "FedRAMP Low") {
			foundLowRef = true
			break
		}
	}
	if !foundLowRef {
		t.Error("FedRAMP Moderate requirements should reference FedRAMP Low")
	}
}

func TestGetFedRAMPDescription(t *testing.T) {
	levels := []FedRAMPLevel{FedRAMPLow, FedRAMPModerate, FedRAMPHigh}

	for _, level := range levels {
		t.Run(string(level), func(t *testing.T) {
			desc := GetFedRAMPDescription(level)

			if desc == "" {
				t.Error("Expected description, got empty string")
				return
			}

			// Should contain FedRAMP-specific content
			if !strings.Contains(desc, "FedRAMP") {
				t.Error("Description should mention FedRAMP")
			}

			if !strings.Contains(desc, "Impact Level") {
				t.Error("Description should mention Impact Level")
			}

			if !strings.Contains(desc, "Authorization") {
				t.Error("Description should mention Authorization")
			}

			if !strings.Contains(desc, "3PAO") {
				t.Error("Description should mention 3PAO assessment")
			}

			if !strings.Contains(desc, "SSP") || !strings.Contains(desc, "System Security Plan") {
				t.Error("Description should mention System Security Plan (SSP)")
			}

			if !strings.Contains(desc, "NIST 800-53") {
				t.Error("Description should reference NIST 800-53 baseline")
			}
		})
	}

	// High should mention more stringent requirements
	highDesc := GetFedRAMPDescription(FedRAMPHigh)
	highDescLower := strings.ToLower(highDesc)
	if !strings.Contains(highDescLower, "customer-managed kms") {
		t.Error("FedRAMP High should mention customer-managed KMS requirement")
	}

	if !strings.Contains(highDescLower, "self-hosted") {
		t.Error("FedRAMP High should mention self-hosted infrastructure requirement")
	}

	// Check for timeline mention
	if !strings.Contains(highDesc, "months") {
		t.Error("FedRAMP High should mention authorization timeline")
	}
}

func TestCompareFedRAMPLevels(t *testing.T) {
	comparison := CompareFedRAMPLevels()

	if comparison == "" {
		t.Fatal("Expected comparison text, got empty string")
	}

	// Should contain all levels
	if !strings.Contains(comparison, "Low") {
		t.Error("Comparison should mention Low level")
	}
	if !strings.Contains(comparison, "Moderate") {
		t.Error("Comparison should mention Moderate level")
	}
	if !strings.Contains(comparison, "High") {
		t.Error("Comparison should mention High level")
	}

	// Should contain control counts
	if !strings.Contains(comparison, "controls") {
		t.Error("Comparison should mention control counts")
	}

	// Should contain timeline information
	if !strings.Contains(comparison, "Timeline") {
		t.Error("Comparison should contain timeline information")
	}

	if !strings.Contains(comparison, "months") {
		t.Error("Comparison should mention authorization timelines in months")
	}

	// Should mention 3PAO cost
	if !strings.Contains(comparison, "3PAO") {
		t.Error("Comparison should mention 3PAO assessment")
	}

	// Should contain usage examples
	if !strings.Contains(comparison, "spawn launch") {
		t.Error("Comparison should contain usage examples")
	}

	if !strings.Contains(comparison, "--fedramp") {
		t.Error("Comparison should show FedRAMP flag usage")
	}

	// Should mention infrastructure requirements
	if !strings.Contains(comparison, "Self-Hosted") || !strings.Contains(comparison, "Shared") {
		t.Error("Comparison should mention infrastructure requirements")
	}
}

func TestValidateFedRAMPInstance(t *testing.T) {
	instance := &aws.InstanceInfo{
		InstanceID:   "i-1234567890abcdef0",
		InstanceType: "t3.micro",
		Region:       "us-east-1",
		State:        "running",
	}

	levels := []FedRAMPLevel{FedRAMPLow, FedRAMPModerate, FedRAMPHigh}

	for _, level := range levels {
		t.Run(string(level), func(t *testing.T) {
			result, err := ValidateFedRAMPInstance(instance, level)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if result == nil {
				t.Fatal("Expected result, got nil")
			}

			// Runtime validation not fully implemented yet, so just check structure
			if result.Compliant && len(result.Violations) > 0 {
				t.Error("Result marked compliant but has violations")
			}
		})
	}
}
