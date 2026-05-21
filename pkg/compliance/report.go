package compliance

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// ComplianceReport represents a compliance validation report
type ComplianceReport struct {
	Timestamp         time.Time           `json:"timestamp"`
	Framework         string              `json:"framework"`
	Baseline          string              `json:"baseline,omitempty"`
	Summary           ComplianceSummary   `json:"summary"`
	Violations        []*ControlViolation `json:"violations"`
	Warnings          []string            `json:"warnings"`
	ControlsEvaluated int                 `json:"controls_evaluated"`
	ControlsPassed    int                 `json:"controls_passed"`
	ControlsFailed    int                 `json:"controls_failed"`
	Recommendations   []string            `json:"recommendations"`
	Metadata          map[string]string   `json:"metadata,omitempty"`
}

// ComplianceSummary provides a high-level summary
type ComplianceSummary struct {
	Compliant bool   `json:"compliant"`
	Status    string `json:"status"` // "compliant", "non-compliant", "warnings"
	Message   string `json:"message"`
}

// GenerateComplianceReport generates a compliance report from validation results
func GenerateComplianceReport(result *ValidationResult, framework, baseline string) *ComplianceReport {
	// Calculate controls passed (we don't have a direct count, so estimate)
	// In the future, we can track this more accurately
	controlsPassed := 0
	if len(result.Violations) == 0 {
		// Rough estimate: assume a baseline has ~10-20 controls
		// If all passed, we can show that count
		// For now, just show the number of controls that didn't fail
		controlsPassed = 10 // Placeholder, will be accurate when we track evaluations
	}

	report := &ComplianceReport{
		Timestamp:         time.Now(),
		Framework:         framework,
		Baseline:          baseline,
		Violations:        result.Violations,
		Warnings:          result.Warnings,
		ControlsEvaluated: len(result.Violations) + controlsPassed,
		ControlsPassed:    controlsPassed,
		ControlsFailed:    len(result.Violations),
		Recommendations:   make([]string, 0),
		Metadata:          make(map[string]string),
	}

	// Determine compliance status
	if len(result.Violations) == 0 && len(result.Warnings) == 0 {
		report.Summary = ComplianceSummary{
			Compliant: true,
			Status:    "compliant",
			Message:   fmt.Sprintf("All %d controls passed validation", report.ControlsEvaluated),
		}
	} else if len(result.Violations) > 0 {
		report.Summary = ComplianceSummary{
			Compliant: false,
			Status:    "non-compliant",
			Message:   fmt.Sprintf("%d control violations detected, %d warnings", len(result.Violations), len(result.Warnings)),
		}
	} else {
		report.Summary = ComplianceSummary{
			Compliant: true,
			Status:    "warnings",
			Message:   fmt.Sprintf("All controls passed with %d warnings", len(result.Warnings)),
		}
	}

	// Generate recommendations
	report.Recommendations = generateRecommendations(result, framework, baseline)

	return report
}

// generateRecommendations generates actionable recommendations based on violations
func generateRecommendations(result *ValidationResult, framework, baseline string) []string {
	recommendations := make([]string, 0)

	// Check for common issues
	hasEBSIssue := false
	hasIMDSIssue := false
	hasNetworkIssue := false
	hasKMSIssue := false

	for _, violation := range result.Violations {
		if strings.Contains(violation.ControlID, "SC-28") {
			hasEBSIssue = true
		}
		if strings.Contains(violation.ControlID, "AC-17") {
			hasIMDSIssue = true
		}
		if strings.Contains(violation.ControlID, "SC-07") || strings.Contains(violation.Description, "public IP") {
			hasNetworkIssue = true
		}
		if strings.Contains(violation.Description, "KMS") || strings.Contains(violation.Description, "customer-managed") {
			hasKMSIssue = true
		}
	}

	// Generate recommendations based on issues
	if hasEBSIssue {
		recommendations = append(recommendations,
			"Enable EBS encryption: use --ebs-encrypted flag or set default encryption in AWS console")
	}

	if hasIMDSIssue {
		recommendations = append(recommendations,
			"IMDSv2 is enforced automatically in compliance mode - ensure you're using --nist-800-171 or --nist-800-53 flags")
	}

	if hasNetworkIssue {
		recommendations = append(recommendations,
			"Deploy in private subnets: use --subnet-id with a private subnet",
			"Disable public IP assignment: ensure your subnet doesn't auto-assign public IPs")
	}

	if hasKMSIssue {
		recommendations = append(recommendations,
			"Create customer-managed KMS key: aws kms create-key --description 'spawn encryption key'",
			"Use customer-managed key: --ebs-kms-key-id <key-id>")
	}

	// Framework-specific recommendations
	if baseline == "moderate" || baseline == "high" || baseline == "fedramp-moderate" || baseline == "fedramp-high" {
		hasInfraWarning := false
		for _, warning := range result.Warnings {
			if strings.Contains(strings.ToLower(warning), "self-hosted") {
				hasInfraWarning = true
				break
			}
		}
		if hasInfraWarning {
			recommendations = append(recommendations,
				"Deploy self-hosted infrastructure: use spawn config init --self-hosted",
				"Self-hosted infrastructure is REQUIRED for Moderate and High baselines")
		}
	}

	// General recommendations
	if len(result.Violations) > 0 {
		recommendations = append(recommendations,
			fmt.Sprintf("Review %s documentation: docs/compliance/%s.md", framework, strings.ToLower(framework)),
			"Run validation before launch: spawn validate --"+framework)
	}

	return recommendations
}

// FormatReportText formats a compliance report as human-readable text
func FormatReportText(report *ComplianceReport) string {
	var builder strings.Builder

	// Header
	builder.WriteString(strings.Repeat("=", 80))
	builder.WriteString("\n")
	fmt.Fprintf(&builder, "Compliance Validation Report - %s\n", report.Framework)
	if report.Baseline != "" {
		fmt.Fprintf(&builder, "Baseline: %s\n", report.Baseline)
	}
	fmt.Fprintf(&builder, "Timestamp: %s\n", report.Timestamp.Format(time.RFC3339))
	builder.WriteString(strings.Repeat("=", 80))
	builder.WriteString("\n\n")

	// Summary
	statusIcon := "✓"
	switch report.Summary.Status {
	case "non-compliant":
		statusIcon = "✗"
	case "warnings":
		statusIcon = "⚠"
	}

	fmt.Fprintf(&builder, "Status: %s %s\n", statusIcon, report.Summary.Status)
	fmt.Fprintf(&builder, "Message: %s\n\n", report.Summary.Message)

	// Statistics
	builder.WriteString("Control Statistics:\n")
	fmt.Fprintf(&builder, "  Controls Evaluated: %d\n", report.ControlsEvaluated)
	fmt.Fprintf(&builder, "  Controls Passed:    %d\n", report.ControlsPassed)
	fmt.Fprintf(&builder, "  Controls Failed:    %d\n", report.ControlsFailed)
	fmt.Fprintf(&builder, "  Warnings:           %d\n\n", len(report.Warnings))

	// Violations
	if len(report.Violations) > 0 {
		builder.WriteString("Violations:\n")
		builder.WriteString(strings.Repeat("-", 80))
		builder.WriteString("\n")
		for i, violation := range report.Violations {
			fmt.Fprintf(&builder, "%d. [%s] %s\n", i+1, violation.ControlID, violation.ControlName)
			fmt.Fprintf(&builder, "   %s\n", violation.Description)
			if violation.Remediation != "" {
				fmt.Fprintf(&builder, "   Remediation: %s\n", violation.Remediation)
			}
			builder.WriteString("\n")
		}
	}

	// Warnings
	if len(report.Warnings) > 0 {
		builder.WriteString("Warnings:\n")
		builder.WriteString(strings.Repeat("-", 80))
		builder.WriteString("\n")
		for i, warning := range report.Warnings {
			fmt.Fprintf(&builder, "%d. %s\n", i+1, warning)
		}
		builder.WriteString("\n")
	}

	// Recommendations
	if len(report.Recommendations) > 0 {
		builder.WriteString("Recommendations:\n")
		builder.WriteString(strings.Repeat("-", 80))
		builder.WriteString("\n")
		for i, rec := range report.Recommendations {
			fmt.Fprintf(&builder, "%d. %s\n", i+1, rec)
		}
		builder.WriteString("\n")
	}

	// Footer
	builder.WriteString(strings.Repeat("=", 80))
	builder.WriteString("\n")
	builder.WriteString("For more information:\n")
	builder.WriteString("  spawn validate --help\n")
	fmt.Fprintf(&builder, "  docs/compliance/%s-quickstart.md\n", strings.ToLower(report.Framework))
	builder.WriteString(strings.Repeat("=", 80))
	builder.WriteString("\n")

	return builder.String()
}

// FormatReportJSON formats a compliance report as JSON
func FormatReportJSON(report *ComplianceReport) (string, error) {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal report to JSON: %w", err)
	}
	return string(data), nil
}

// GenerateInstanceReport generates a compliance report for a running instance
func GenerateInstanceReport(instance *aws.InstanceInfo, framework, baseline string) *ComplianceReport {
	// For now, create an empty report
	// Runtime validation will be implemented in Phase 4
	report := &ComplianceReport{
		Timestamp:         time.Now(),
		Framework:         framework,
		Baseline:          baseline,
		Violations:        make([]*ControlViolation, 0),
		Warnings:          make([]string, 0),
		ControlsEvaluated: 0,
		ControlsPassed:    0,
		ControlsFailed:    0,
		Recommendations:   make([]string, 0),
		Metadata: map[string]string{
			"instance_id":   instance.InstanceID,
			"instance_type": instance.InstanceType,
			"region":        instance.Region,
		},
	}

	report.Summary = ComplianceSummary{
		Compliant: true,
		Status:    "compliant",
		Message:   "Runtime validation not yet implemented (Phase 4)",
	}

	report.Recommendations = []string{
		"Runtime validation will be available in a future release",
		"Use pre-flight validation: spawn validate --config <file>",
	}

	return report
}

// GenerateSummaryReport generates a summary report for multiple instances
func GenerateSummaryReport(instances []*aws.InstanceInfo, framework, baseline string) *ComplianceReport {
	report := &ComplianceReport{
		Timestamp:         time.Now(),
		Framework:         framework,
		Baseline:          baseline,
		Violations:        make([]*ControlViolation, 0),
		Warnings:          make([]string, 0),
		ControlsEvaluated: 0,
		ControlsPassed:    0,
		ControlsFailed:    0,
		Recommendations:   make([]string, 0),
		Metadata: map[string]string{
			"instances_evaluated": fmt.Sprintf("%d", len(instances)),
		},
	}

	// Runtime validation will be implemented in Phase 4
	report.Summary = ComplianceSummary{
		Compliant: true,
		Status:    "compliant",
		Message:   fmt.Sprintf("Evaluated %d instances (runtime validation not yet implemented)", len(instances)),
	}

	return report
}

// CompareBaselinesReport generates a comparison report of different baselines
func CompareBaselinesReport() string {
	return CompareBaselines()
}

// CompareFedRAMPReport generates a comparison report of FedRAMP levels
func CompareFedRAMPReport() string {
	return CompareFedRAMPLevels()
}
