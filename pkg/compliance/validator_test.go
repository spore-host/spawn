package compliance

import (
	"context"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/config"
)

func TestNewValidator(t *testing.T) {
	complianceConfig := &config.ComplianceConfig{
		Mode:       config.ComplianceModeNIST80171,
		StrictMode: false,
	}
	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	validator := NewValidator(complianceConfig, infraConfig)

	if validator == nil {
		t.Fatal("Expected validator, got nil")
	}

	if validator.complianceConfig != complianceConfig {
		t.Error("Validator not initialized with correct compliance config")
	}

	if validator.infraConfig != infraConfig {
		t.Error("Validator not initialized with correct infrastructure config")
	}
}

func TestValidator_ValidateLaunchConfig_DisabledCompliance(t *testing.T) {
	complianceConfig := &config.ComplianceConfig{
		Mode: config.ComplianceModeNone,
	}
	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	validator := NewValidator(complianceConfig, infraConfig)

	cfg := &aws.LaunchConfig{
		EBSEncrypted:   false,
		IMDSv2Enforced: false,
	}

	result, err := validator.ValidateLaunchConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !result.Compliant {
		t.Error("Expected compliant when compliance is disabled")
	}

	if len(result.Violations) > 0 {
		t.Error("Expected no violations when compliance is disabled")
	}
}

func TestValidator_ValidateLaunchConfig_NIST80171(t *testing.T) {
	complianceConfig := &config.ComplianceConfig{
		Mode:       config.ComplianceModeNIST80171,
		StrictMode: false,
	}
	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	validator := NewValidator(complianceConfig, infraConfig)

	tests := []struct {
		name            string
		cfg             *aws.LaunchConfig
		expectCompliant bool
		expectWarnings  bool
	}{
		{
			name: "Compliant config",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
			},
			expectCompliant: true,
			expectWarnings:  true, // Warning about shared infrastructure
		},
		{
			name: "Non-compliant config",
			cfg: &aws.LaunchConfig{
				EBSEncrypted:   false,
				IMDSv2Enforced: false,
			},
			expectCompliant: false,
			expectWarnings:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := validator.ValidateLaunchConfig(context.Background(), tt.cfg)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if result.Compliant != tt.expectCompliant {
				t.Errorf("Expected compliant=%v, got %v", tt.expectCompliant, result.Compliant)
			}

			if tt.expectWarnings && len(result.Warnings) == 0 {
				t.Error("Expected warnings about shared infrastructure")
			}
		})
	}
}

func TestValidator_ValidateLaunchConfig_AllBaselines(t *testing.T) {
	modes := []config.ComplianceMode{
		config.ComplianceModeNIST80171,
		config.ComplianceModeBaseLow,
		config.ComplianceModeBaseMod,
		config.ComplianceModeBaseHigh,
		config.ComplianceModeFedRAMPLow,
		config.ComplianceModeFedRAMPMod,
		config.ComplianceModeFedRAMPHi,
	}

	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			complianceConfig := &config.ComplianceConfig{
				Mode:       mode,
				StrictMode: false,
			}

			validator := NewValidator(complianceConfig, infraConfig)

			cfg := &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
			}

			result, err := validator.ValidateLaunchConfig(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Unexpected error for mode %s: %v", mode, err)
			}

			if result == nil {
				t.Fatal("Expected result, got nil")
			}
		})
	}
}

func TestValidator_EnforceLaunchConfig(t *testing.T) {
	complianceConfig := &config.ComplianceConfig{
		Mode:       config.ComplianceModeNIST80171,
		StrictMode: false,
	}
	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	validator := NewValidator(complianceConfig, infraConfig)

	cfg := &aws.LaunchConfig{
		EBSEncrypted:   false,
		IMDSv2Enforced: false,
	}

	err := validator.EnforceLaunchConfig(cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !cfg.EBSEncrypted {
		t.Error("Expected EBS encryption to be enforced")
	}

	if !cfg.IMDSv2Enforced {
		t.Error("Expected IMDSv2 to be enforced")
	}
}

func TestValidator_EnforceLaunchConfig_DisabledCompliance(t *testing.T) {
	complianceConfig := &config.ComplianceConfig{
		Mode: config.ComplianceModeNone,
	}
	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	validator := NewValidator(complianceConfig, infraConfig)

	cfg := &aws.LaunchConfig{
		EBSEncrypted:   false,
		IMDSv2Enforced: false,
	}

	err := validator.EnforceLaunchConfig(cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Should not enforce when compliance is disabled
	if cfg.EBSEncrypted {
		t.Error("Should not enforce when compliance is disabled")
	}

	if cfg.IMDSv2Enforced {
		t.Error("Should not enforce when compliance is disabled")
	}
}

func TestValidator_ValidateInstances(t *testing.T) {
	complianceConfig := &config.ComplianceConfig{
		Mode:       config.ComplianceModeNIST80171,
		StrictMode: false,
	}
	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	validator := NewValidator(complianceConfig, infraConfig)

	instances := []aws.InstanceInfo{
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

	results, err := validator.ValidateInstances(context.Background(), instances)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(results) != len(instances) {
		t.Errorf("Expected results for %d instances, got %d", len(instances), len(results))
	}

	for instanceID, result := range results {
		if result == nil {
			t.Errorf("Expected result for instance %s, got nil", instanceID)
		}
	}
}

func TestValidator_ValidateInstances_DisabledCompliance(t *testing.T) {
	complianceConfig := &config.ComplianceConfig{
		Mode: config.ComplianceModeNone,
	}
	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	validator := NewValidator(complianceConfig, infraConfig)

	instances := []aws.InstanceInfo{
		{
			InstanceID:   "i-1234567890abcdef0",
			InstanceType: "t3.micro",
			Region:       "us-east-1",
			State:        "running",
		},
	}

	results, err := validator.ValidateInstances(context.Background(), instances)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(results) != 0 {
		t.Error("Expected empty results when compliance is disabled")
	}
}

func TestValidator_GetComplianceSummary(t *testing.T) {
	tests := []struct {
		name           string
		complianceMode config.ComplianceMode
		infraMode      config.InfrastructureMode
		expectDisabled bool
		expectWarning  bool
	}{
		{
			name:           "Compliance disabled",
			complianceMode: config.ComplianceModeNone,
			infraMode:      config.InfrastructureModeShared,
			expectDisabled: true,
			expectWarning:  false,
		},
		{
			name:           "NIST 800-171 with shared",
			complianceMode: config.ComplianceModeNIST80171,
			infraMode:      config.InfrastructureModeShared,
			expectDisabled: false,
			expectWarning:  false, // Warning only shown with AllowSharedInfrastructure=true
		},
		{
			name:           "NIST 800-53 Moderate with shared",
			complianceMode: config.ComplianceModeBaseMod,
			infraMode:      config.InfrastructureModeShared,
			expectDisabled: false,
			expectWarning:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			complianceConfig := &config.ComplianceConfig{
				Mode:       tt.complianceMode,
				StrictMode: false,
			}
			infraConfig := &config.InfrastructureConfig{
				Mode: tt.infraMode,
			}

			validator := NewValidator(complianceConfig, infraConfig)
			summary := validator.GetComplianceSummary()

			if summary == "" {
				t.Error("Expected summary, got empty string")
			}

			if tt.expectDisabled && !strings.Contains(summary, "Disabled") {
				t.Error("Expected 'Disabled' in summary")
			}

			if !tt.expectDisabled && strings.Contains(summary, "Disabled") {
				t.Error("Did not expect 'Disabled' in summary")
			}

			if tt.expectWarning && !strings.Contains(summary, "Warning") {
				t.Error("Expected warning in summary")
			}
		})
	}
}

func TestValidator_IsStrictMode(t *testing.T) {
	tests := []struct {
		name       string
		strictMode bool
	}{
		{name: "Strict mode enabled", strictMode: true},
		{name: "Strict mode disabled", strictMode: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			complianceConfig := &config.ComplianceConfig{
				Mode:       config.ComplianceModeNIST80171,
				StrictMode: tt.strictMode,
			}
			infraConfig := &config.InfrastructureConfig{
				Mode: config.InfrastructureModeShared,
			}

			validator := NewValidator(complianceConfig, infraConfig)

			if validator.IsStrictMode() != tt.strictMode {
				t.Errorf("Expected strict mode=%v, got %v", tt.strictMode, validator.IsStrictMode())
			}
		})
	}
}

func TestValidator_GetControlList(t *testing.T) {
	modes := []config.ComplianceMode{
		config.ComplianceModeNIST80171,
		config.ComplianceModeBaseLow,
		config.ComplianceModeBaseMod,
		config.ComplianceModeBaseHigh,
	}

	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			complianceConfig := &config.ComplianceConfig{
				Mode:       mode,
				StrictMode: false,
			}

			validator := NewValidator(complianceConfig, infraConfig)

			controls, err := validator.GetControlList()
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if len(controls) == 0 {
				t.Error("Expected controls to be returned")
			}

			// Verify control structure
			for _, control := range controls {
				if control.ID == "" {
					t.Error("Control missing ID")
				}
				if control.Name == "" {
					t.Errorf("Control %s missing name", control.ID)
				}
			}
		})
	}
}

func TestValidator_GetControlList_DisabledCompliance(t *testing.T) {
	complianceConfig := &config.ComplianceConfig{
		Mode: config.ComplianceModeNone,
	}
	infraConfig := &config.InfrastructureConfig{
		Mode: config.InfrastructureModeShared,
	}

	validator := NewValidator(complianceConfig, infraConfig)

	controls, err := validator.GetControlList()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(controls) != 0 {
		t.Error("Expected no controls when compliance is disabled")
	}
}

func TestValidator_SelfHostedInfrastructureWarnings(t *testing.T) {
	tests := []struct {
		name                      string
		complianceMode            config.ComplianceMode
		infraMode                 config.InfrastructureMode
		allowSharedInfrastructure bool
		expectWarning             bool
	}{
		{
			name:                      "Moderate requires self-hosted",
			complianceMode:            config.ComplianceModeBaseMod,
			infraMode:                 config.InfrastructureModeShared,
			allowSharedInfrastructure: false,
			expectWarning:             true,
		},
		{
			name:                      "Moderate with self-hosted",
			complianceMode:            config.ComplianceModeBaseMod,
			infraMode:                 config.InfrastructureModeSelfHosted,
			allowSharedInfrastructure: false,
			expectWarning:             false,
		},
		{
			name:                      "Low with shared (allowed)",
			complianceMode:            config.ComplianceModeBaseLow,
			infraMode:                 config.InfrastructureModeShared,
			allowSharedInfrastructure: true,
			expectWarning:             true, // Still warns even if allowed
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			complianceConfig := &config.ComplianceConfig{
				Mode:                      tt.complianceMode,
				StrictMode:                false,
				AllowSharedInfrastructure: tt.allowSharedInfrastructure,
			}
			infraConfig := &config.InfrastructureConfig{
				Mode: tt.infraMode,
			}

			validator := NewValidator(complianceConfig, infraConfig)

			cfg := &aws.LaunchConfig{
				EBSEncrypted:   true,
				IMDSv2Enforced: true,
			}

			result, err := validator.ValidateLaunchConfig(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			hasWarning := len(result.Warnings) > 0

			if tt.expectWarning && !hasWarning {
				t.Error("Expected warning about infrastructure mode, got none")
			}
			if !tt.expectWarning && hasWarning {
				t.Errorf("Did not expect warning, got: %v", result.Warnings)
			}
		})
	}
}
