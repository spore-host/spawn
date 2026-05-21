package compliance

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

func TestGenerateComplianceReport(t *testing.T) {
	tests := []struct {
		name         string
		result       *ValidationResult
		framework    string
		baseline     string
		expectStatus string
	}{
		{
			name: "Fully compliant",
			result: &ValidationResult{
				Compliant:  true,
				Violations: []*ControlViolation{},
				Warnings:   []string{},
			},
			framework:    "NIST 800-171",
			baseline:     "",
			expectStatus: "compliant",
		},
		{
			name: "With violations",
			result: &ValidationResult{
				Compliant: false,
				Violations: []*ControlViolation{
					{
						ControlID:   "SC-28",
						ControlName: "Protection at Rest",
						Description: "EBS encryption required",
						Severity:    "high",
					},
				},
				Warnings: []string{},
			},
			framework:    "NIST 800-171",
			baseline:     "",
			expectStatus: "non-compliant",
		},
		{
			name: "With warnings only",
			result: &ValidationResult{
				Compliant:  true,
				Violations: []*ControlViolation{},
				Warnings:   []string{"Self-hosted infrastructure recommended"},
			},
			framework:    "NIST 800-171",
			baseline:     "",
			expectStatus: "warnings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := GenerateComplianceReport(tt.result, tt.framework, tt.baseline)

			if report == nil {
				t.Fatal("Expected report, got nil")
			}

			if report.Framework != tt.framework {
				t.Errorf("Expected framework %s, got %s", tt.framework, report.Framework)
			}

			if report.Baseline != tt.baseline {
				t.Errorf("Expected baseline %s, got %s", tt.baseline, report.Baseline)
			}

			if report.Summary.Status != tt.expectStatus {
				t.Errorf("Expected status %s, got %s", tt.expectStatus, report.Summary.Status)
			}

			if report.Summary.Compliant != tt.result.Compliant {
				t.Errorf("Expected compliant=%v, got %v", tt.result.Compliant, report.Summary.Compliant)
			}

			if len(report.Violations) != len(tt.result.Violations) {
				t.Errorf("Expected %d violations, got %d", len(tt.result.Violations), len(report.Violations))
			}

			if len(report.Warnings) != len(tt.result.Warnings) {
				t.Errorf("Expected %d warnings, got %d", len(tt.result.Warnings), len(report.Warnings))
			}

			if report.Timestamp.IsZero() {
				t.Error("Expected timestamp to be set")
			}

			if report.Metadata == nil {
				t.Error("Expected metadata to be initialized")
			}
		})
	}
}

func TestGenerateRecommendations(t *testing.T) {
	tests := []struct {
		name           string
		result         *ValidationResult
		framework      string
		baseline       string
		expectKeywords []string
	}{
		{
			name: "EBS encryption issue",
			result: &ValidationResult{
				Compliant: false,
				Violations: []*ControlViolation{
					{
						ControlID:   "SC-28",
						ControlName: "Protection at Rest",
						Description: "EBS encryption required",
					},
				},
				Warnings: []string{},
			},
			framework:      "NIST 800-171",
			baseline:       "",
			expectKeywords: []string{"EBS", "encryption"},
		},
		{
			name: "IMDSv2 issue",
			result: &ValidationResult{
				Compliant: false,
				Violations: []*ControlViolation{
					{
						ControlID:   "AC-17",
						ControlName: "Remote Access",
						Description: "IMDSv2 required",
					},
				},
				Warnings: []string{},
			},
			framework:      "NIST 800-171",
			baseline:       "",
			expectKeywords: []string{"IMDSv2"},
		},
		{
			name: "Customer KMS issue",
			result: &ValidationResult{
				Compliant: false,
				Violations: []*ControlViolation{
					{
						ControlID:   "SC-28(1)",
						ControlName: "Customer-Managed Keys",
						Description: "Customer-managed KMS keys required for High baseline",
					},
				},
				Warnings: []string{},
			},
			framework:      "NIST 800-53",
			baseline:       "high",
			expectKeywords: []string{"KMS", "customer"},
		},
		{
			name: "Self-hosted infrastructure warning",
			result: &ValidationResult{
				Compliant:  true,
				Violations: []*ControlViolation{},
				Warnings:   []string{"Self-hosted infrastructure recommended"},
			},
			framework:      "NIST 800-53",
			baseline:       "moderate",
			expectKeywords: []string{"self-hosted"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recommendations := generateRecommendations(tt.result, tt.framework, tt.baseline)

			if len(recommendations) == 0 {
				t.Error("Expected recommendations, got none")
				return
			}

			// Check that expected keywords are present in recommendations
			allRecommendations := strings.Join(recommendations, " ")
			for _, keyword := range tt.expectKeywords {
				if !strings.Contains(strings.ToLower(allRecommendations), strings.ToLower(keyword)) {
					t.Errorf("Expected keyword '%s' in recommendations: %v", keyword, recommendations)
				}
			}
		})
	}
}

func TestFormatReportText(t *testing.T) {
	result := &ValidationResult{
		Compliant: false,
		Violations: []*ControlViolation{
			{
				ControlID:   "SC-28",
				ControlName: "Protection at Rest",
				Description: "EBS encryption required",
				Severity:    "high",
				Remediation: "Enable EBS encryption",
			},
		},
		Warnings: []string{"Self-hosted infrastructure recommended"},
	}

	report := GenerateComplianceReport(result, "NIST 800-171", "")

	text := FormatReportText(report)

	if text == "" {
		t.Fatal("Expected text report, got empty string")
	}

	// Check for expected sections
	expectedSections := []string{
		"Compliance Validation Report",
		"NIST 800-171",
		"Status:",
		"Control Statistics:",
		"Violations:",
		"Warnings:",
		"Recommendations:",
	}

	for _, section := range expectedSections {
		if !strings.Contains(text, section) {
			t.Errorf("Expected section '%s' in text report", section)
		}
	}

	// Check for control details
	if !strings.Contains(text, "SC-28") {
		t.Error("Expected control ID in text report")
	}

	if !strings.Contains(text, "Protection at Rest") {
		t.Error("Expected control name in text report")
	}

	if !strings.Contains(text, "EBS encryption required") {
		t.Error("Expected violation description in text report")
	}

	if !strings.Contains(text, "Self-hosted") {
		t.Error("Expected warning in text report")
	}
}

func TestFormatReportJSON(t *testing.T) {
	result := &ValidationResult{
		Compliant: false,
		Violations: []*ControlViolation{
			{
				ControlID:   "SC-28",
				ControlName: "Protection at Rest",
				Description: "EBS encryption required",
				Severity:    "high",
			},
		},
		Warnings: []string{"Self-hosted infrastructure recommended"},
	}

	report := GenerateComplianceReport(result, "NIST 800-171", "")

	jsonStr, err := FormatReportJSON(report)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if jsonStr == "" {
		t.Fatal("Expected JSON report, got empty string")
	}

	// Verify it's valid JSON
	var decoded ComplianceReport
	if err := json.Unmarshal([]byte(jsonStr), &decoded); err != nil {
		t.Fatalf("Invalid JSON: %v", err)
	}

	// Verify structure
	if decoded.Framework != "NIST 800-171" {
		t.Errorf("Expected framework 'NIST 800-171', got %s", decoded.Framework)
	}

	if len(decoded.Violations) != 1 {
		t.Errorf("Expected 1 violation, got %d", len(decoded.Violations))
	}

	if len(decoded.Warnings) != 1 {
		t.Errorf("Expected 1 warning, got %d", len(decoded.Warnings))
	}

	if decoded.Summary.Status != "non-compliant" {
		t.Errorf("Expected status 'non-compliant', got %s", decoded.Summary.Status)
	}
}

func TestGenerateInstanceReport(t *testing.T) {
	instance := &aws.InstanceInfo{
		InstanceID:   "i-1234567890abcdef0",
		InstanceType: "t3.micro",
		Region:       "us-east-1",
		State:        "running",
	}

	report := GenerateInstanceReport(instance, "NIST 800-171", "")

	if report == nil {
		t.Fatal("Expected report, got nil")
	}

	if report.Framework != "NIST 800-171" {
		t.Errorf("Expected framework 'NIST 800-171', got %s", report.Framework)
	}

	if report.Metadata == nil {
		t.Fatal("Expected metadata, got nil")
	}

	if report.Metadata["instance_id"] != instance.InstanceID {
		t.Errorf("Expected instance_id %s, got %s", instance.InstanceID, report.Metadata["instance_id"])
	}

	if report.Metadata["instance_type"] != instance.InstanceType {
		t.Errorf("Expected instance_type %s, got %s", instance.InstanceType, report.Metadata["instance_type"])
	}

	if report.Metadata["region"] != instance.Region {
		t.Errorf("Expected region %s, got %s", instance.Region, report.Metadata["region"])
	}

	// Runtime validation not yet implemented, should be compliant placeholder
	if !report.Summary.Compliant {
		t.Error("Expected compliant status for placeholder report")
	}

	if !strings.Contains(report.Summary.Message, "not yet implemented") {
		t.Error("Expected message about runtime validation not being implemented")
	}
}

func TestGenerateSummaryReport(t *testing.T) {
	instances := []*aws.InstanceInfo{
		{
			InstanceID:   "i-1234567890abcdef0",
			InstanceType: "t3.micro",
			Region:       "us-east-1",
			State:        "running",
		},
		{
			InstanceID:   "i-0987654321fedcba0",
			InstanceType: "t3.small",
			Region:       "us-west-2",
			State:        "running",
		},
	}

	report := GenerateSummaryReport(instances, "NIST 800-171", "")

	if report == nil {
		t.Fatal("Expected report, got nil")
	}

	if report.Framework != "NIST 800-171" {
		t.Errorf("Expected framework 'NIST 800-171', got %s", report.Framework)
	}

	if report.Metadata == nil {
		t.Fatal("Expected metadata, got nil")
	}

	if report.Metadata["instances_evaluated"] != "2" {
		t.Errorf("Expected instances_evaluated '2', got %s", report.Metadata["instances_evaluated"])
	}

	// Runtime validation not yet implemented
	if !report.Summary.Compliant {
		t.Error("Expected compliant status for placeholder report")
	}

	if !strings.Contains(report.Summary.Message, "2 instances") {
		t.Error("Expected message to mention number of instances")
	}
}

func TestCompareBaselinesReport(t *testing.T) {
	comparison := CompareBaselinesReport()

	if comparison == "" {
		t.Fatal("Expected comparison text, got empty string")
	}

	// Should be same as CompareBaselines()
	expected := CompareBaselines()
	if comparison != expected {
		t.Error("CompareBaselinesReport should return same as CompareBaselines")
	}
}

func TestCompareFedRAMPReport(t *testing.T) {
	comparison := CompareFedRAMPReport()

	if comparison == "" {
		t.Fatal("Expected comparison text, got empty string")
	}

	// Should be same as CompareFedRAMPLevels()
	expected := CompareFedRAMPLevels()
	if comparison != expected {
		t.Error("CompareFedRAMPReport should return same as CompareFedRAMPLevels")
	}
}

func TestReportWithMultipleViolations(t *testing.T) {
	result := &ValidationResult{
		Compliant: false,
		Violations: []*ControlViolation{
			{
				ControlID:   "SC-28",
				ControlName: "Protection at Rest",
				Description: "EBS encryption required",
				Severity:    "high",
				Remediation: "Enable EBS encryption",
			},
			{
				ControlID:   "AC-17",
				ControlName: "Remote Access",
				Description: "IMDSv2 required",
				Severity:    "high",
				Remediation: "Enable IMDSv2",
			},
			{
				ControlID:   "SC-07",
				ControlName: "Boundary Protection",
				Description: "Public IP not allowed",
				Severity:    "medium",
				Remediation: "Deploy in private subnet",
			},
		},
		Warnings: []string{
			"Self-hosted infrastructure recommended",
			"Customer-managed KMS keys recommended",
		},
	}

	report := GenerateComplianceReport(result, "NIST 800-53", "high")

	if report.ControlsFailed != len(result.Violations) {
		t.Errorf("Expected %d failed controls, got %d", len(result.Violations), report.ControlsFailed)
	}

	if len(report.Recommendations) == 0 {
		t.Error("Expected recommendations for multiple violations")
	}

	// Verify recommendations cover multiple issues
	allRecs := strings.Join(report.Recommendations, " ")
	expectedKeywords := []string{"EBS", "IMDSv2", "subnet"}
	for _, keyword := range expectedKeywords {
		if !strings.Contains(strings.ToLower(allRecs), strings.ToLower(keyword)) {
			t.Errorf("Expected keyword '%s' in recommendations", keyword)
		}
	}
}

func TestReportTimestamp(t *testing.T) {
	result := &ValidationResult{
		Compliant:  true,
		Violations: []*ControlViolation{},
		Warnings:   []string{},
	}

	report := GenerateComplianceReport(result, "NIST 800-171", "")

	if report.Timestamp.IsZero() {
		t.Error("Expected timestamp to be set")
	}

	// Timestamp should be recent (within last minute)
	// This is a simple sanity check
	if report.Timestamp.Unix() < 1000000000 {
		t.Error("Timestamp appears to be invalid")
	}
}
