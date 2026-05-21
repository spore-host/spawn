package compliance

import (
	"fmt"

	"github.com/spore-host/spawn/pkg/aws"
)

// FedRAMPLevel represents FedRAMP authorization levels
type FedRAMPLevel string

const (
	FedRAMPLow      FedRAMPLevel = "fedramp-low"
	FedRAMPModerate FedRAMPLevel = "fedramp-moderate"
	FedRAMPHigh     FedRAMPLevel = "fedramp-high"
)

// FedRAMPControlSet returns the control set for a specific FedRAMP level
// FedRAMP is based on NIST 800-53, so we map to baselines
func FedRAMPControlSet(level FedRAMPLevel) *ControlSet {
	var baseline Baseline
	var name, description string

	switch level {
	case FedRAMPLow:
		baseline = BaselineLow
		name = "FedRAMP Low Authorization"
		description = "Cloud service providers offering low-impact SaaS"
	case FedRAMPModerate:
		baseline = BaselineModerate
		name = "FedRAMP Moderate Authorization"
		description = "Cloud service providers offering moderate-impact SaaS"
	case FedRAMPHigh:
		baseline = BaselineHigh
		name = "FedRAMP High Authorization"
		description = "Cloud service providers offering high-impact SaaS"
	default:
		baseline = BaselineLow
		name = "FedRAMP Low Authorization"
		description = "Cloud service providers offering low-impact SaaS"
	}

	// Get the corresponding NIST 800-53 baseline
	controlSet := NIST80053ControlSet(baseline)

	// Override name and description for FedRAMP branding
	controlSet.Name = name
	controlSet.Description = description

	return controlSet
}

// ValidateFedRAMPCompliance validates a launch config against FedRAMP requirements
func ValidateFedRAMPCompliance(cfg *aws.LaunchConfig, level FedRAMPLevel) (*ValidationResult, error) {
	controlSet := FedRAMPControlSet(level)
	result := controlSet.ValidateLaunchConfig(cfg)
	return result, nil
}

// EnforceFedRAMPCompliance enforces FedRAMP controls on a launch config
func EnforceFedRAMPCompliance(cfg *aws.LaunchConfig, level FedRAMPLevel) {
	controlSet := FedRAMPControlSet(level)
	controlSet.EnforceLaunchConfig(cfg)
}

// ValidateFedRAMPInstance validates a running instance against FedRAMP requirements
func ValidateFedRAMPInstance(instance *aws.InstanceInfo, level FedRAMPLevel) (*ValidationResult, error) {
	controlSet := FedRAMPControlSet(level)
	result := controlSet.ValidateInstance(instance)
	return result, nil
}

// GetFedRAMPRequirements returns FedRAMP-specific requirements for each level
func GetFedRAMPRequirements() map[FedRAMPLevel][]string {
	return map[FedRAMPLevel][]string{
		FedRAMPLow: {
			"FIPS 140-2 validated encryption",
			"Continuous monitoring",
			"Incident response procedures",
			"System security plans (SSP)",
			"Annual assessment by 3PAO",
			"All NIST 800-53 Low baseline controls",
		},
		FedRAMPModerate: {
			"All FedRAMP Low requirements",
			"Annual assessment by 3PAO",
			"Continuous monitoring",
			"Enhanced incident response (24/7)",
			"Vulnerability scanning",
			"Penetration testing (annual)",
			"All NIST 800-53 Moderate baseline controls",
		},
		FedRAMPHigh: {
			"All FedRAMP Moderate requirements",
			"More frequent assessment by 3PAO",
			"Enhanced continuous monitoring and alerting",
			"Insider threat program",
			"Supply chain risk management",
			"Penetration testing (more frequent)",
			"All NIST 800-53 High baseline controls",
		},
	}
}

// GetFedRAMPDescription returns a detailed description of a FedRAMP level
func GetFedRAMPDescription(level FedRAMPLevel) string {
	requirements := GetFedRAMPRequirements()[level]
	reqStr := ""
	for _, req := range requirements {
		reqStr += fmt.Sprintf("  - %s\n", req)
	}

	switch level {
	case FedRAMPLow:
		return fmt.Sprintf(`FedRAMP Low Authorization

Impact Level: Low
Use Case: Cloud service providers offering low-impact SaaS to federal agencies
Authorization: FedRAMP Low Authorization to Operate (ATO)

Requirements:
%s
Based on: NIST 800-53 Rev 5 Low Baseline
Controls: %d implemented (technical controls only)

Authorization Process:
1. Prepare System Security Plan (SSP)
2. Third-Party Assessment Organization (3PAO) assessment
3. Authorization by federal agency or FedRAMP PMO
4. Continuous monitoring and annual assessment

⚠️  IMPORTANT: spawn provides technical control implementation.
Organizational controls (policies, procedures, 3PAO assessment) are
the customer's responsibility.

Self-hosted infrastructure recommended but not required for FedRAMP Low.`, reqStr, GetBaselineControlCount(BaselineLow))

	case FedRAMPModerate:
		return fmt.Sprintf(`FedRAMP Moderate Authorization

Impact Level: Moderate
Use Case: Cloud service providers offering moderate-impact SaaS to federal agencies
Authorization: FedRAMP Moderate Authorization to Operate (ATO)

Requirements:
%s
Based on: NIST 800-53 Rev 5 Moderate Baseline
Controls: %d implemented (technical controls only)

Authorization Process:
1. Prepare System Security Plan (SSP)
2. Third-Party Assessment Organization (3PAO) assessment
3. Authorization by federal agency or FedRAMP PMO
4. Continuous monitoring and annual assessment

⚠️  CRITICAL: FedRAMP Moderate REQUIRES self-hosted infrastructure.
spawn will enforce this requirement.

Organizational controls (policies, procedures, 3PAO assessment) are
the customer's responsibility.`, reqStr, GetBaselineControlCount(BaselineModerate))

	case FedRAMPHigh:
		return fmt.Sprintf(`FedRAMP High Authorization

Impact Level: High
Use Case: Cloud service providers offering high-impact SaaS to federal agencies
Authorization: FedRAMP High Authorization to Operate (ATO)

Requirements:
%s
Based on: NIST 800-53 Rev 5 High Baseline
Controls: %d implemented (technical controls only)

Authorization Process:
1. Prepare System Security Plan (SSP)
2. Rigorous Third-Party Assessment Organization (3PAO) assessment
3. Authorization by federal agency or FedRAMP PMO
4. Enhanced continuous monitoring and more frequent assessments

⚠️  CRITICAL: FedRAMP High REQUIRES:
  - Self-hosted infrastructure (mandatory)
  - Customer-managed KMS keys (mandatory)
  - Private subnets (no public IPs)
  - VPC endpoints for AWS services
  - Multi-AZ deployment
  - Enhanced monitoring and incident response

Organizational controls (policies, procedures, 3PAO assessment) are
the customer's responsibility.

Note: FedRAMP High authorization is significantly more rigorous and
time-consuming than Moderate. Plan for 12-18 months for initial ATO.`, reqStr, GetBaselineControlCount(BaselineHigh))

	default:
		return "Unknown FedRAMP level"
	}
}

// MapFedRAMPToBaseline maps FedRAMP levels to NIST 800-53 baselines
func MapFedRAMPToBaseline(level FedRAMPLevel) Baseline {
	switch level {
	case FedRAMPLow:
		return BaselineLow
	case FedRAMPModerate:
		return BaselineModerate
	case FedRAMPHigh:
		return BaselineHigh
	default:
		return BaselineLow
	}
}

// GetFedRAMPControlCount returns the number of controls in each FedRAMP level
func GetFedRAMPControlCount(level FedRAMPLevel) int {
	baseline := MapFedRAMPToBaseline(level)
	return GetBaselineControlCount(baseline)
}

// CompareFedRAMPLevels returns a comparison of FedRAMP authorization levels
func CompareFedRAMPLevels() string {
	return fmt.Sprintf(`FedRAMP Authorization Level Comparison

Low:      %d controls (low-impact systems)
Moderate: %d controls (moderate-impact systems)
High:     %d controls (high-impact systems)

Authorization Timeline (typical):
┌─────────────┬──────────────┬──────────────┬──────────────┐
│   Aspect    │     Low      │   Moderate   │     High     │
├─────────────┼──────────────┼──────────────┼──────────────┤
│  Timeline   │  6-9 months  │  9-12 months │ 12-18 months │
├─────────────┼──────────────┼──────────────┼──────────────┤
│ 3PAO Cost   │  $50k-100k   │  $100k-200k  │  $200k-400k  │
├─────────────┼──────────────┼──────────────┼──────────────┤
│  Monitoring │    Annual    │   Quarterly  │   Monthly    │
├─────────────┼──────────────┼──────────────┼──────────────┤
│   PenTest   │    Annual    │    Annual    │  Semi-annual │
├─────────────┼──────────────┼──────────────┼──────────────┤
│ Infrastructure│  Shared OK  │  Self-Hosted │  Self-Hosted │
├─────────────┼──────────────┼──────────────┼──────────────┤
│  Public IP  │   Allowed    │   Blocked    │   Blocked    │
└─────────────┴──────────────┴──────────────┴──────────────┘

Usage:
  spawn launch --fedramp-low ...       # FedRAMP Low
  spawn launch --fedramp-moderate ...  # FedRAMP Moderate
  spawn launch --fedramp-high ...      # FedRAMP High

Note: FedRAMP authorization requires:
1. System Security Plan (SSP) - customer responsibility
2. Third-Party Assessment (3PAO) - customer contracts separately
3. Authorization decision by federal agency
4. Continuous monitoring - customer + spawn technical controls

spawn provides technical control implementation only.
`,
		GetFedRAMPControlCount(FedRAMPLow),
		GetFedRAMPControlCount(FedRAMPModerate),
		GetFedRAMPControlCount(FedRAMPHigh))
}
