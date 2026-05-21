package compliance

import (
	"context"
	"fmt"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/config"
)

// Validator orchestrates compliance validation and enforcement
type Validator struct {
	complianceConfig *config.ComplianceConfig
	infraConfig      *config.InfrastructureConfig
}

// NewValidator creates a new compliance validator
func NewValidator(complianceConfig *config.ComplianceConfig, infraConfig *config.InfrastructureConfig) *Validator {
	return &Validator{
		complianceConfig: complianceConfig,
		infraConfig:      infraConfig,
	}
}

// ValidateLaunchConfig performs pre-flight validation of a launch configuration
// Returns a ValidationResult with any violations or warnings
func (v *Validator) ValidateLaunchConfig(ctx context.Context, cfg *aws.LaunchConfig) (*ValidationResult, error) {
	result := &ValidationResult{Compliant: true}

	// If compliance is not enabled, skip validation
	if !v.complianceConfig.IsComplianceEnabled() {
		return result, nil
	}

	// Get the appropriate control set based on compliance mode
	var controlSet *ControlSet
	switch v.complianceConfig.Mode {
	case config.ComplianceModeNIST80171:
		controlSet = NIST80171ControlSet()
	case config.ComplianceModeNIST80053, config.ComplianceModeBaseLow:
		controlSet = NIST80053ControlSet(BaselineLow)
	case config.ComplianceModeBaseMod:
		controlSet = NIST80053ControlSet(BaselineModerate)
	case config.ComplianceModeBaseHigh:
		controlSet = NIST80053ControlSet(BaselineHigh)
	case config.ComplianceModeFedRAMPLow:
		controlSet = FedRAMPControlSet(FedRAMPLow)
	case config.ComplianceModeFedRAMPMod:
		controlSet = FedRAMPControlSet(FedRAMPModerate)
	case config.ComplianceModeFedRAMPHi:
		controlSet = FedRAMPControlSet(FedRAMPHigh)
	default:
		return nil, fmt.Errorf("unknown compliance mode: %s", v.complianceConfig.Mode)
	}

	// Validate against control set
	validationResult := controlSet.ValidateLaunchConfig(cfg)
	result.Violations = validationResult.Violations
	result.Compliant = validationResult.Compliant

	// Check infrastructure mode compatibility
	if v.complianceConfig.RequiresSelfHosted() && !v.infraConfig.IsSelfHosted() {
		result.AddWarning(fmt.Sprintf(
			"%s requires self-hosted infrastructure, but shared infrastructure is configured. "+
				"This may not meet compliance requirements. "+
				"Run 'spawn config init --self-hosted' to configure self-hosted mode.",
			v.complianceConfig.GetModeDisplayName(),
		))
	}

	// Add warnings for shared infrastructure usage with compliance enabled
	if v.complianceConfig.IsComplianceEnabled() && !v.infraConfig.IsSelfHosted() && v.complianceConfig.AllowSharedInfrastructure {
		result.AddWarning(
			"Using shared infrastructure with compliance mode enabled. " +
				"For full compliance, consider deploying self-hosted infrastructure. " +
				"Run 'spawn config init --self-hosted' to configure.",
		)
	}

	return result, nil
}

// EnforceLaunchConfig enforces compliance controls on a launch configuration
// Modifies the config in-place to make it compliant
func (v *Validator) EnforceLaunchConfig(cfg *aws.LaunchConfig) error {
	// If compliance is not enabled, skip enforcement
	if !v.complianceConfig.IsComplianceEnabled() {
		return nil
	}

	// Get the appropriate control set based on compliance mode
	var controlSet *ControlSet
	switch v.complianceConfig.Mode {
	case config.ComplianceModeNIST80171:
		controlSet = NIST80171ControlSet()
	case config.ComplianceModeNIST80053, config.ComplianceModeBaseLow:
		controlSet = NIST80053ControlSet(BaselineLow)
	case config.ComplianceModeBaseMod:
		controlSet = NIST80053ControlSet(BaselineModerate)
	case config.ComplianceModeBaseHigh:
		controlSet = NIST80053ControlSet(BaselineHigh)
	case config.ComplianceModeFedRAMPLow:
		controlSet = FedRAMPControlSet(FedRAMPLow)
	case config.ComplianceModeFedRAMPMod:
		controlSet = FedRAMPControlSet(FedRAMPModerate)
	case config.ComplianceModeFedRAMPHi:
		controlSet = FedRAMPControlSet(FedRAMPHigh)
	default:
		return fmt.Errorf("unknown compliance mode: %s", v.complianceConfig.Mode)
	}

	// Enforce controls
	controlSet.EnforceLaunchConfig(cfg)

	return nil
}

// ValidateInstances validates running instances against compliance controls
// Returns a map of instance ID to validation result
func (v *Validator) ValidateInstances(ctx context.Context, instances []aws.InstanceInfo) (map[string]*ValidationResult, error) {
	results := make(map[string]*ValidationResult)

	// If compliance is not enabled, return empty results
	if !v.complianceConfig.IsComplianceEnabled() {
		return results, nil
	}

	// Get the appropriate control set based on compliance mode
	var controlSet *ControlSet
	switch v.complianceConfig.Mode {
	case config.ComplianceModeNIST80171:
		controlSet = NIST80171ControlSet()
	case config.ComplianceModeNIST80053, config.ComplianceModeBaseLow:
		controlSet = NIST80053ControlSet(BaselineLow)
	case config.ComplianceModeBaseMod:
		controlSet = NIST80053ControlSet(BaselineModerate)
	case config.ComplianceModeBaseHigh:
		controlSet = NIST80053ControlSet(BaselineHigh)
	case config.ComplianceModeFedRAMPLow:
		controlSet = FedRAMPControlSet(FedRAMPLow)
	case config.ComplianceModeFedRAMPMod:
		controlSet = FedRAMPControlSet(FedRAMPModerate)
	case config.ComplianceModeFedRAMPHi:
		controlSet = FedRAMPControlSet(FedRAMPHigh)
	default:
		return nil, fmt.Errorf("unknown compliance mode: %s", v.complianceConfig.Mode)
	}

	// Validate each instance
	for _, instance := range instances {
		result := controlSet.ValidateInstance(&instance)
		results[instance.InstanceID] = result
	}

	return results, nil
}

// GetComplianceSummary returns a human-readable summary of compliance status
func (v *Validator) GetComplianceSummary() string {
	if !v.complianceConfig.IsComplianceEnabled() {
		return "Compliance: Disabled"
	}

	summary := fmt.Sprintf("Compliance Mode: %s\n", v.complianceConfig.GetModeDisplayName())
	summary += fmt.Sprintf("Infrastructure: %s\n", v.infraConfig.GetModeDisplayName())

	if v.complianceConfig.RequiresSelfHosted() && !v.infraConfig.IsSelfHosted() {
		summary += "⚠️  Warning: Self-hosted infrastructure required but not configured\n"
	}

	return summary
}

// IsStrictMode returns true if strict mode is enabled (errors instead of warnings)
func (v *Validator) IsStrictMode() bool {
	return v.complianceConfig.StrictMode
}

// GetControlList returns a list of all controls for the current compliance mode
func (v *Validator) GetControlList() ([]Control, error) {
	if !v.complianceConfig.IsComplianceEnabled() {
		return nil, nil
	}

	var controlSet *ControlSet
	switch v.complianceConfig.Mode {
	case config.ComplianceModeNIST80171:
		controlSet = NIST80171ControlSet()
	case config.ComplianceModeNIST80053, config.ComplianceModeBaseLow:
		controlSet = NIST80053ControlSet(BaselineLow)
	case config.ComplianceModeBaseMod:
		controlSet = NIST80053ControlSet(BaselineModerate)
	case config.ComplianceModeBaseHigh:
		controlSet = NIST80053ControlSet(BaselineHigh)
	case config.ComplianceModeFedRAMPLow:
		controlSet = FedRAMPControlSet(FedRAMPLow)
	case config.ComplianceModeFedRAMPMod:
		controlSet = FedRAMPControlSet(FedRAMPModerate)
	case config.ComplianceModeFedRAMPHi:
		controlSet = FedRAMPControlSet(FedRAMPHigh)
	default:
		return nil, fmt.Errorf("unknown compliance mode: %s", v.complianceConfig.Mode)
	}

	return controlSet.Controls, nil
}
