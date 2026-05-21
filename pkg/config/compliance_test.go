package config

import (
	"context"
	"os"
	"testing"
)

func TestLoadComplianceConfig(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		flagMode   string
		flagStrict bool
		envMode    string
		envEBS     string
		envIMDS    string
		wantMode   ComplianceMode
		wantEBS    bool
		wantIMDS   bool
		wantStrict bool
	}{
		{
			name:       "default no compliance",
			flagMode:   "",
			flagStrict: false,
			wantMode:   ComplianceModeNone,
			wantEBS:    false,
			wantIMDS:   false,
			wantStrict: false,
		},
		{
			name:       "NIST 800-171 from flag",
			flagMode:   "nist-800-171",
			flagStrict: false,
			wantMode:   ComplianceModeNIST80171,
			wantEBS:    true, // Auto-enabled for 800-171
			wantIMDS:   true, // Auto-enabled for 800-171
			wantStrict: false,
		},
		{
			name:       "NIST 800-171 strict mode",
			flagMode:   "nist-800-171",
			flagStrict: true,
			wantMode:   ComplianceModeNIST80171,
			wantEBS:    true,
			wantIMDS:   true,
			wantStrict: true,
		},
		{
			name:       "FedRAMP Moderate baseline",
			flagMode:   "fedramp-moderate",
			flagStrict: false,
			wantMode:   ComplianceModeFedRAMPMod,
			wantEBS:    true,
			wantIMDS:   true,
			wantStrict: false,
		},
		{
			name:     "env var override",
			flagMode: "",
			envMode:  "nist-800-171",
			envEBS:   "true",
			envIMDS:  "true",
			wantMode: ComplianceModeNIST80171,
			wantEBS:  true,
			wantIMDS: true,
		},
		{
			name:     "flag overrides env",
			flagMode: "nist-800-53-low",
			envMode:  "nist-800-171",
			wantMode: ComplianceModeBaseLow,
			wantEBS:  true,
			wantIMDS: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set env vars
			if tt.envMode != "" {
				if err := os.Setenv("SPAWN_COMPLIANCE_MODE", tt.envMode); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Unsetenv("SPAWN_COMPLIANCE_MODE") }()
			}
			if tt.envEBS != "" {
				if err := os.Setenv("SPAWN_COMPLIANCE_ENFORCE_ENCRYPTED_EBS", tt.envEBS); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Unsetenv("SPAWN_COMPLIANCE_ENFORCE_ENCRYPTED_EBS") }()
			}
			if tt.envIMDS != "" {
				if err := os.Setenv("SPAWN_COMPLIANCE_ENFORCE_IMDSV2", tt.envIMDS); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Unsetenv("SPAWN_COMPLIANCE_ENFORCE_IMDSV2") }()
			}

			cfg, err := LoadComplianceConfig(ctx, tt.flagMode, tt.flagStrict)
			if err != nil {
				t.Fatalf("LoadComplianceConfig() error = %v", err)
			}

			if cfg.Mode != tt.wantMode {
				t.Errorf("Mode = %v, want %v", cfg.Mode, tt.wantMode)
			}
			if cfg.EnforceEncryptedEBS != tt.wantEBS {
				t.Errorf("EnforceEncryptedEBS = %v, want %v", cfg.EnforceEncryptedEBS, tt.wantEBS)
			}
			if cfg.EnforceIMDSv2 != tt.wantIMDS {
				t.Errorf("EnforceIMDSv2 = %v, want %v", cfg.EnforceIMDSv2, tt.wantIMDS)
			}
			if cfg.StrictMode != tt.wantStrict {
				t.Errorf("StrictMode = %v, want %v", cfg.StrictMode, tt.wantStrict)
			}
		})
	}
}

func TestComplianceConfigHelpers(t *testing.T) {
	tests := []struct {
		name            string
		mode            ComplianceMode
		wantEnabled     bool
		wantSelfHosted  bool
		wantDisplayName string
	}{
		{
			name:            "no compliance",
			mode:            ComplianceModeNone,
			wantEnabled:     false,
			wantSelfHosted:  false,
			wantDisplayName: "None",
		},
		{
			name:            "NIST 800-171",
			mode:            ComplianceModeNIST80171,
			wantEnabled:     true,
			wantSelfHosted:  false, // Not required, but recommended
			wantDisplayName: "NIST 800-171 Rev 3",
		},
		{
			name:            "FedRAMP Moderate",
			mode:            ComplianceModeFedRAMPMod,
			wantEnabled:     true,
			wantSelfHosted:  true, // Required for Moderate+
			wantDisplayName: "FedRAMP Moderate",
		},
		{
			name:            "FedRAMP High",
			mode:            ComplianceModeFedRAMPHi,
			wantEnabled:     true,
			wantSelfHosted:  true, // Required for High
			wantDisplayName: "FedRAMP High",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ComplianceConfig{
				Mode:                      tt.mode,
				AllowSharedInfrastructure: true, // Default
			}

			if got := cfg.IsComplianceEnabled(); got != tt.wantEnabled {
				t.Errorf("IsComplianceEnabled() = %v, want %v", got, tt.wantEnabled)
			}

			// RequiresSelfHosted needs to apply mode defaults first
			applyModeDefaults(cfg)
			if got := cfg.RequiresSelfHosted(); got != tt.wantSelfHosted {
				t.Errorf("RequiresSelfHosted() = %v, want %v", got, tt.wantSelfHosted)
			}

			if got := cfg.GetModeDisplayName(); got != tt.wantDisplayName {
				t.Errorf("GetModeDisplayName() = %v, want %v", got, tt.wantDisplayName)
			}
		})
	}
}

func TestValidateMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{"empty valid", "", false},
		{"NIST 800-171 valid", "nist-800-171", false},
		{"NIST 800-53 Low valid", "nist-800-53-low", false},
		{"FedRAMP Moderate valid", "fedramp-moderate", false},
		{"invalid mode", "invalid-mode", true},
		{"case insensitive", "NIST-800-171", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMode(tt.mode)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMode() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"YES", true},
		{"on", true},
		{"enabled", true},
		{"false", false},
		{"0", false},
		{"no", false},
		{"off", false},
		{"", false},
		{"invalid", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := parseBool(tt.input); got != tt.want {
				t.Errorf("parseBool(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
