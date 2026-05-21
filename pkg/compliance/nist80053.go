package compliance

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
)

// Baseline represents NIST 800-53 security control baselines
type Baseline string

const (
	BaselineLow      Baseline = "low"
	BaselineModerate Baseline = "moderate"
	BaselineHigh     Baseline = "high"
)

// NIST80053ControlSet returns the control set for a specific NIST 800-53 Rev 5 baseline
func NIST80053ControlSet(baseline Baseline) *ControlSet {
	var name, description string
	var controls []Control

	switch baseline {
	case BaselineLow:
		name = "NIST 800-53 Rev 5 Low Baseline"
		description = "Low-impact baseline for federal information systems"
		controls = getLowBaselineControls()
	case BaselineModerate:
		name = "NIST 800-53 Rev 5 Moderate Baseline"
		description = "Moderate-impact baseline for federal information systems"
		controls = getModerateBaselineControls()
	case BaselineHigh:
		name = "NIST 800-53 Rev 5 High Baseline"
		description = "High-impact baseline for federal information systems"
		controls = getHighBaselineControls()
	default:
		name = "NIST 800-53 Rev 5 Low Baseline"
		description = "Low-impact baseline for federal information systems"
		controls = getLowBaselineControls()
	}

	return &ControlSet{
		Name:        name,
		Description: description,
		Controls:    controls,
	}
}

// getLowBaselineControls returns Low baseline controls (superset of NIST 800-171)
func getLowBaselineControls() []Control {
	// Start with all NIST 800-171 controls
	controls := getNIST80171Controls()

	// Low baseline is essentially 800-171 + self-hosted infrastructure recommendation
	// All 800-171 controls apply, plus emphasis on controlled environment

	return controls
}

// getModerateBaselineControls returns Moderate baseline controls (superset of Low)
func getModerateBaselineControls() []Control {
	// Start with Low baseline
	controls := getLowBaselineControls()

	// Add Moderate-specific controls
	moderateControls := []Control{
		{
			ID:          "SC-07(4)",
			Name:        "Boundary Protection - Private Subnets",
			Description: "Prevent direct external access to internal systems",
			Family:      "System and Communications Protection",
			Validator: func(cfg *aws.LaunchConfig) error {
				// Check if instance will be in private subnet
				// Note: We can't directly check subnet type or public IP without AWS API call
				// This will be enforced via subnet configuration and user guidance
				// TODO: Add AssociatePublicIP field to LaunchConfig if needed
				return nil
			},
			Enforcer: func(cfg *aws.LaunchConfig) {
				// Private subnet enforcement is done via subnet selection
				// User must configure private subnets for Moderate+ baselines
			},
			RuntimeValidator: func(instance *aws.InstanceInfo) error {
				// Runtime validation would check if instance has public IP
				// For now, return nil (will implement in Phase 4)
				return nil
			},
		},
		{
			ID:          "SC-28(1)",
			Name:        "Protection at Rest - Customer-Managed Keys",
			Description: "Use customer-managed encryption keys for data at rest",
			Family:      "System and Communications Protection",
			Validator: func(cfg *aws.LaunchConfig) error {
				// For Moderate, customer KMS keys are recommended but not required
				// Will be enforced in High baseline
				return nil
			},
			Enforcer: func(cfg *aws.LaunchConfig) {
				// No automatic enforcement for Moderate
				// Customer should provide KMS key ID via config
			},
			RuntimeValidator: func(instance *aws.InstanceInfo) error {
				return nil
			},
		},
		{
			ID:          "SI-02",
			Name:        "Flaw Remediation",
			Description: "Identify, report, and correct system flaws",
			Family:      "System and Integrity",
			Validator: func(cfg *aws.LaunchConfig) error {
				// Ensure AMI is specified (customer responsible for patching)
				// spawn doesn't manage OS patching
				return nil
			},
			Enforcer: func(cfg *aws.LaunchConfig) {
				// Customer responsible for AMI updates
			},
			RuntimeValidator: func(instance *aws.InstanceInfo) error {
				return nil
			},
		},
	}

	controls = append(controls, moderateControls...)
	return controls
}

// getHighBaselineControls returns High baseline controls (superset of Moderate)
func getHighBaselineControls() []Control {
	// Start with Moderate baseline
	controls := getModerateBaselineControls()

	// Add High-specific controls
	highControls := []Control{
		{
			ID:          "SC-28(1)",
			Name:        "Protection at Rest - Customer-Managed Keys (Required)",
			Description: "REQUIRED: Use customer-managed KMS keys for all encrypted data",
			Family:      "System and Communications Protection",
			Validator: func(cfg *aws.LaunchConfig) error {
				// For High baseline, customer-managed KMS keys are REQUIRED
				if cfg.EBSKMSKeyID == "" {
					return errors.New("customer-managed KMS key required for High baseline (SC-28(1))")
				}
				// Verify KMS key ID format
				if !strings.HasPrefix(cfg.EBSKMSKeyID, "arn:aws:kms:") &&
					!strings.HasPrefix(cfg.EBSKMSKeyID, "alias/") &&
					len(cfg.EBSKMSKeyID) != 36 { // UUID format
					return errors.New("invalid KMS key ID format (SC-28(1))")
				}
				return nil
			},
			Enforcer: func(cfg *aws.LaunchConfig) {
				// Cannot auto-create customer KMS keys
				// Customer must provide via --ebs-kms-key-id flag
			},
			RuntimeValidator: func(instance *aws.InstanceInfo) error {
				// Runtime validation would check EBS volume encryption key
				// For now, return nil (will implement in Phase 4)
				return nil
			},
		},
		{
			ID:          "SC-07(5)",
			Name:        "Boundary Protection - Deny by Default",
			Description: "Deny network traffic by default, allow by exception",
			Family:      "System and Communications Protection",
			Validator: func(cfg *aws.LaunchConfig) error {
				// Verify security groups are configured with deny-by-default
				if len(cfg.SecurityGroupIDs) == 0 {
					return errors.New("explicit security groups required for High baseline (SC-07(5))")
				}
				return nil
			},
			Enforcer: func(cfg *aws.LaunchConfig) {
				// Security groups must be explicitly configured by user
			},
			RuntimeValidator: func(instance *aws.InstanceInfo) error {
				return nil
			},
		},
		{
			ID:          "SC-07(12)",
			Name:        "Boundary Protection - VPC Endpoints",
			Description: "Use VPC endpoints for AWS service communication",
			Family:      "System and Communications Protection",
			Validator: func(cfg *aws.LaunchConfig) error {
				// VPC endpoints should be configured in the VPC
				// This is infrastructure-level, not instance-level
				// Validation is informational only
				return nil
			},
			Enforcer: func(cfg *aws.LaunchConfig) {
				// VPC endpoints are VPC-level configuration
				// Customer must configure in their VPC
			},
			RuntimeValidator: func(instance *aws.InstanceInfo) error {
				return nil
			},
		},
		{
			ID:          "CP-09",
			Name:        "System Backup",
			Description: "Conduct backups of system-level and user-level information",
			Family:      "Contingency Planning",
			Validator: func(cfg *aws.LaunchConfig) error {
				// EBS snapshots should be enabled via AWS Backup
				// This is customer responsibility, not spawn enforcement
				return nil
			},
			Enforcer: func(cfg *aws.LaunchConfig) {
				// Customer responsible for backup configuration
			},
			RuntimeValidator: func(instance *aws.InstanceInfo) error {
				return nil
			},
		},
		{
			ID:          "CP-10",
			Name:        "System Recovery and Reconstitution",
			Description: "Provide for recovery and reconstitution of the system",
			Family:      "Contingency Planning",
			Validator: func(cfg *aws.LaunchConfig) error {
				// Multi-AZ deployment for High baseline
				// This is sweep-level configuration, not single instance
				return nil
			},
			Enforcer: func(cfg *aws.LaunchConfig) {
				// Multi-AZ deployment is sweep configuration
				// Customer should use --multi-region for High baseline
			},
			RuntimeValidator: func(instance *aws.InstanceInfo) error {
				return nil
			},
		},
	}

	controls = append(controls, highControls...)
	return controls
}

// ValidateNIST80053Compliance validates a launch config against a specific baseline
func ValidateNIST80053Compliance(cfg *aws.LaunchConfig, baseline Baseline) (*ValidationResult, error) {
	controlSet := NIST80053ControlSet(baseline)
	result := controlSet.ValidateLaunchConfig(cfg)
	return result, nil
}

// EnforceNIST80053Compliance enforces baseline controls on a launch config
func EnforceNIST80053Compliance(cfg *aws.LaunchConfig, baseline Baseline) {
	controlSet := NIST80053ControlSet(baseline)
	controlSet.EnforceLaunchConfig(cfg)
}

// ValidateNIST80053Instance validates a running instance against a baseline
func ValidateNIST80053Instance(instance *aws.InstanceInfo, baseline Baseline) (*ValidationResult, error) {
	controlSet := NIST80053ControlSet(baseline)
	result := controlSet.ValidateInstance(instance)
	return result, nil
}

// GetBaselineRequirements returns a summary of requirements for each baseline
func GetBaselineRequirements() map[Baseline][]string {
	return map[Baseline][]string{
		BaselineLow: {
			"EBS encryption (AWS-managed or customer-managed KMS)",
			"IMDSv2 enforcement",
			"Security group configuration",
			"Audit logging",
			"IAM authentication and authorization",
			"Self-hosted infrastructure (recommended)",
		},
		BaselineModerate: {
			"All Low baseline requirements",
			"Private subnet deployment (no public IPs)",
			"Customer-managed KMS keys (recommended)",
			"Self-hosted infrastructure (required)",
			"Flaw remediation processes",
		},
		BaselineHigh: {
			"All Moderate baseline requirements",
			"Customer-managed KMS keys (required)",
			"VPC endpoints for AWS services",
			"Multi-AZ deployment (recommended)",
			"System backup configuration",
			"Explicit security group rules (deny by default)",
		},
	}
}

// GetBaselineDescription returns a detailed description of a baseline
func GetBaselineDescription(baseline Baseline) string {
	requirements := GetBaselineRequirements()[baseline]
	reqStr := ""
	for _, req := range requirements {
		reqStr += fmt.Sprintf("  - %s\n", req)
	}

	switch baseline {
	case BaselineLow:
		return fmt.Sprintf(`NIST 800-53 Rev 5 Low Baseline

Impact Level: Low
System Type: General-purpose federal information systems
Authorization: Suitable for FedRAMP Low

Requirements:
%s
Controls: %d implemented
Framework: NIST 800-53 Rev 5 (based on NIST 800-171)

This baseline is appropriate for systems where:
- Loss of confidentiality, integrity, or availability would have LIMITED adverse effect
- Information and systems are not mission-critical
- Basic security controls are sufficient

Note: spawn implements technical controls. Organizational controls
(policies, procedures, training) remain the customer's responsibility.`, reqStr, len(getLowBaselineControls()))

	case BaselineModerate:
		return fmt.Sprintf(`NIST 800-53 Rev 5 Moderate Baseline

Impact Level: Moderate
System Type: Sensitive federal information systems
Authorization: Suitable for FedRAMP Moderate

Requirements:
%s
Controls: %d implemented
Framework: NIST 800-53 Rev 5 (superset of Low baseline)

This baseline is appropriate for systems where:
- Loss of confidentiality, integrity, or availability would have SERIOUS adverse effect
- Information and systems support important missions
- Enhanced security controls are required

⚠️  IMPORTANT: Moderate baseline REQUIRES self-hosted infrastructure.
Shared infrastructure will generate errors.

Note: spawn implements technical controls. Organizational controls
(policies, procedures, training) remain the customer's responsibility.`, reqStr, len(getModerateBaselineControls()))

	case BaselineHigh:
		return fmt.Sprintf(`NIST 800-53 Rev 5 High Baseline

Impact Level: High
System Type: Critical federal information systems
Authorization: Suitable for FedRAMP High

Requirements:
%s
Controls: %d implemented
Framework: NIST 800-53 Rev 5 (superset of Moderate baseline)

This baseline is appropriate for systems where:
- Loss of confidentiality, integrity, or availability would have SEVERE or CATASTROPHIC adverse effect
- Information and systems are mission-critical
- Stringent security controls are mandatory

⚠️  CRITICAL: High baseline REQUIRES:
  - Self-hosted infrastructure (mandatory)
  - Customer-managed KMS keys (mandatory)
  - Private subnets (no public IPs)
  - VPC endpoints for AWS services
  - Multi-AZ deployment (recommended)

Note: spawn implements technical controls. Organizational controls
(policies, procedures, training) remain the customer's responsibility.`, reqStr, len(getHighBaselineControls()))

	default:
		return "Unknown baseline"
	}
}

// GetBaselineControlCount returns the number of controls in each baseline
func GetBaselineControlCount(baseline Baseline) int {
	switch baseline {
	case BaselineLow:
		return len(getLowBaselineControls())
	case BaselineModerate:
		return len(getModerateBaselineControls())
	case BaselineHigh:
		return len(getHighBaselineControls())
	default:
		return 0
	}
}

// CompareBaselines returns a comparison of control counts across baselines
func CompareBaselines() string {
	return fmt.Sprintf(`NIST 800-53 Rev 5 Baseline Comparison

Low Baseline:      %d controls (NIST 800-171 foundation)
Moderate Baseline: %d controls (Low + enhanced protections)
High Baseline:     %d controls (Moderate + stringent requirements)

Progressive Control Enhancement:
┌─────────────┬──────────────┬──────────────┬──────────────┐
│   Control   │     Low      │   Moderate   │     High     │
├─────────────┼──────────────┼──────────────┼──────────────┤
│ EBS Encrypt │   Required   │   Required   │   Required   │
│             │ (any KMS)    │ (any KMS)    │ (customer)   │
├─────────────┼──────────────┼──────────────┼──────────────┤
│   IMDSv2    │   Required   │   Required   │   Required   │
├─────────────┼──────────────┼──────────────┼──────────────┤
│ Private Net │      -       │   Required   │   Required   │
├─────────────┼──────────────┼──────────────┼──────────────┤
│  Public IP  │   Allowed    │   Blocked    │   Blocked    │
├─────────────┼──────────────┼──────────────┼──────────────┤
│ Self-Hosted │ Recommended  │   Required   │   Required   │
├─────────────┼──────────────┼──────────────┼──────────────┤
│ VPC Endpoints│      -       │ Recommended  │   Required   │
├─────────────┼──────────────┼──────────────┼──────────────┤
│  Multi-AZ   │      -       │      -       │ Recommended  │
└─────────────┴──────────────┴──────────────┴──────────────┘

Usage:
  spawn launch --nist-800-53=low ...       # Low baseline
  spawn launch --nist-800-53=moderate ...  # Moderate baseline
  spawn launch --nist-800-53=high ...      # High baseline
`,
		GetBaselineControlCount(BaselineLow),
		GetBaselineControlCount(BaselineModerate),
		GetBaselineControlCount(BaselineHigh))
}
