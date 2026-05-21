package compliance

import (
	"fmt"

	"github.com/spore-host/spawn/pkg/aws"
)

// Control represents a single security control requirement
type Control struct {
	// Control identification
	ID          string // E.g., "SC-28", "AC-17"
	Name        string // Human-readable control name
	Description string // What this control does
	Family      string // Control family (e.g., "System and Communications Protection")

	// Validation function checks if a launch config complies
	// Returns nil if compliant, error describing violation otherwise
	Validator func(cfg *aws.LaunchConfig) error

	// Enforcer function modifies a launch config to make it compliant
	// Called when compliance mode is enabled to auto-fix configs
	Enforcer func(cfg *aws.LaunchConfig)

	// Runtime validation checks running instances
	// Used by "spawn validate" command to audit existing resources
	RuntimeValidator func(instance *aws.InstanceInfo) error
}

// ControlViolation represents a compliance control violation
type ControlViolation struct {
	ControlID   string
	ControlName string
	Description string
	Severity    string // "critical", "high", "medium", "low"
	Remediation string // How to fix the violation
	ResourceID  string // Instance ID or resource identifier
}

// Error implements the error interface
func (v *ControlViolation) Error() string {
	return fmt.Sprintf("[%s] %s: %s", v.ControlID, v.ControlName, v.Description)
}

// ValidationResult holds the results of a compliance validation
type ValidationResult struct {
	Compliant  bool
	Violations []*ControlViolation
	Warnings   []string
}

// AddViolation adds a control violation to the validation result
func (r *ValidationResult) AddViolation(v *ControlViolation) {
	r.Compliant = false
	r.Violations = append(r.Violations, v)
}

// AddWarning adds a warning message to the validation result
func (r *ValidationResult) AddWarning(msg string) {
	r.Warnings = append(r.Warnings, msg)
}

// HasViolations returns true if any violations were found
func (r *ValidationResult) HasViolations() bool {
	return len(r.Violations) > 0
}

// HasWarnings returns true if any warnings were found
func (r *ValidationResult) HasWarnings() bool {
	return len(r.Warnings) > 0
}

// GetSummary returns a human-readable summary of the validation result
func (r *ValidationResult) GetSummary() string {
	if r.Compliant {
		return "Compliant"
	}
	return fmt.Sprintf("%d violations found", len(r.Violations))
}

// ControlSet represents a collection of controls for a compliance framework
type ControlSet struct {
	Name        string
	Description string
	Controls    []Control
}

// ValidateLaunchConfig validates a launch configuration against all controls
func (cs *ControlSet) ValidateLaunchConfig(cfg *aws.LaunchConfig) *ValidationResult {
	result := &ValidationResult{Compliant: true}

	for _, control := range cs.Controls {
		if control.Validator != nil {
			if err := control.Validator(cfg); err != nil {
				result.AddViolation(&ControlViolation{
					ControlID:   control.ID,
					ControlName: control.Name,
					Description: err.Error(),
					Severity:    "high",
				})
			}
		}
	}

	return result
}

// EnforceLaunchConfig enforces all controls on a launch configuration
func (cs *ControlSet) EnforceLaunchConfig(cfg *aws.LaunchConfig) {
	for _, control := range cs.Controls {
		if control.Enforcer != nil {
			control.Enforcer(cfg)
		}
	}
}

// ValidateInstance validates a running instance against all controls
func (cs *ControlSet) ValidateInstance(instance *aws.InstanceInfo) *ValidationResult {
	result := &ValidationResult{Compliant: true}

	for _, control := range cs.Controls {
		if control.RuntimeValidator != nil {
			if err := control.RuntimeValidator(instance); err != nil {
				result.AddViolation(&ControlViolation{
					ControlID:   control.ID,
					ControlName: control.Name,
					Description: err.Error(),
					Severity:    "high",
					ResourceID:  instance.InstanceID,
				})
			}
		}
	}

	return result
}
