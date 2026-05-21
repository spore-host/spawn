package config

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// ComplianceMode represents the compliance framework being enforced
type ComplianceMode string

const (
	ComplianceModeNone       ComplianceMode = ""
	ComplianceModeNIST80171  ComplianceMode = "nist-800-171"
	ComplianceModeNIST80053  ComplianceMode = "nist-800-53" // For direct 800-53 reference
	ComplianceModeBaseLow    ComplianceMode = "nist-800-53-low"
	ComplianceModeBaseMod    ComplianceMode = "nist-800-53-moderate"
	ComplianceModeBaseHigh   ComplianceMode = "nist-800-53-high"
	ComplianceModeFedRAMPLow ComplianceMode = "fedramp-low"
	ComplianceModeFedRAMPMod ComplianceMode = "fedramp-moderate"
	ComplianceModeFedRAMPHi  ComplianceMode = "fedramp-high"
)

// ComplianceConfig holds all compliance-related configuration
type ComplianceConfig struct {
	// Mode specifies which compliance framework to enforce
	Mode ComplianceMode `yaml:"mode"`

	// Enforcement settings
	EnforceEncryptedEBS       bool `yaml:"enforce_encrypted_ebs"`
	EnforceIMDSv2             bool `yaml:"enforce_imdsv2"`
	EnforcePrivateSubnets     bool `yaml:"enforce_private_subnets"`
	EnforceNoPublicIP         bool `yaml:"enforce_no_public_ip"`
	EnforceVPCEndpoints       bool `yaml:"enforce_vpc_endpoints"`
	EnforceMultiAZ            bool `yaml:"enforce_multi_az"`
	EnforceCustomerKMS        bool `yaml:"enforce_customer_kms"`
	AuditLoggingRequired      bool `yaml:"audit_logging_required"`
	AllowSharedInfrastructure bool `yaml:"allow_shared_infrastructure"`

	// Validation settings
	StrictMode bool `yaml:"strict_mode"` // Fail on warnings (default: false, only show warnings)
}

// LoadComplianceConfig loads compliance configuration with precedence:
// 1. CLI flags (passed as parameters)
// 2. Environment variables
// 3. Config file
// 4. Defaults (no compliance enforcement)
func LoadComplianceConfig(ctx context.Context, flagMode string, flagStrict bool) (*ComplianceConfig, error) {
	cfg := &ComplianceConfig{
		Mode:                      ComplianceModeNone,
		EnforceEncryptedEBS:       false,
		EnforceIMDSv2:             false,
		EnforcePrivateSubnets:     false,
		EnforceNoPublicIP:         false,
		EnforceVPCEndpoints:       false,
		EnforceMultiAZ:            false,
		EnforceCustomerKMS:        false,
		AuditLoggingRequired:      true, // Always enabled by default
		AllowSharedInfrastructure: true, // Allow shared infra by default
		StrictMode:                false,
	}

	// 3. Try config file
	fileConfig, err := loadFromFile()
	if err == nil && fileConfig != nil {
		if fileConfig.Compliance.Mode != "" {
			cfg.Mode = ComplianceMode(fileConfig.Compliance.Mode)
		}
		cfg.EnforceEncryptedEBS = fileConfig.Compliance.EnforceEncryptedEBS
		cfg.EnforceIMDSv2 = fileConfig.Compliance.EnforceIMDSv2
		cfg.EnforcePrivateSubnets = fileConfig.Compliance.EnforcePrivateSubnets
		cfg.EnforceNoPublicIP = fileConfig.Compliance.EnforceNoPublicIP
		cfg.EnforceVPCEndpoints = fileConfig.Compliance.EnforceVPCEndpoints
		cfg.EnforceMultiAZ = fileConfig.Compliance.EnforceMultiAZ
		cfg.EnforceCustomerKMS = fileConfig.Compliance.EnforceCustomerKMS
		cfg.AuditLoggingRequired = fileConfig.Compliance.AuditLoggingRequired
		cfg.AllowSharedInfrastructure = fileConfig.Compliance.AllowSharedInfrastructure
		cfg.StrictMode = fileConfig.Compliance.StrictMode
	}

	// 2. Environment variables
	if envMode := os.Getenv("SPAWN_COMPLIANCE_MODE"); envMode != "" {
		cfg.Mode = ComplianceMode(strings.ToLower(envMode))
	}
	if envEBS := os.Getenv("SPAWN_COMPLIANCE_ENFORCE_ENCRYPTED_EBS"); envEBS != "" {
		cfg.EnforceEncryptedEBS = parseBool(envEBS)
	}
	if envIMDS := os.Getenv("SPAWN_COMPLIANCE_ENFORCE_IMDSV2"); envIMDS != "" {
		cfg.EnforceIMDSv2 = parseBool(envIMDS)
	}
	if envPrivate := os.Getenv("SPAWN_COMPLIANCE_ENFORCE_PRIVATE_SUBNETS"); envPrivate != "" {
		cfg.EnforcePrivateSubnets = parseBool(envPrivate)
	}
	if envNoPublicIP := os.Getenv("SPAWN_COMPLIANCE_ENFORCE_NO_PUBLIC_IP"); envNoPublicIP != "" {
		cfg.EnforceNoPublicIP = parseBool(envNoPublicIP)
	}
	if envVPCE := os.Getenv("SPAWN_COMPLIANCE_ENFORCE_VPC_ENDPOINTS"); envVPCE != "" {
		cfg.EnforceVPCEndpoints = parseBool(envVPCE)
	}
	if envMultiAZ := os.Getenv("SPAWN_COMPLIANCE_ENFORCE_MULTI_AZ"); envMultiAZ != "" {
		cfg.EnforceMultiAZ = parseBool(envMultiAZ)
	}
	if envKMS := os.Getenv("SPAWN_COMPLIANCE_ENFORCE_CUSTOMER_KMS"); envKMS != "" {
		cfg.EnforceCustomerKMS = parseBool(envKMS)
	}
	if envAudit := os.Getenv("SPAWN_COMPLIANCE_AUDIT_LOGGING"); envAudit != "" {
		cfg.AuditLoggingRequired = parseBool(envAudit)
	}
	if envShared := os.Getenv("SPAWN_COMPLIANCE_ALLOW_SHARED_INFRASTRUCTURE"); envShared != "" {
		cfg.AllowSharedInfrastructure = parseBool(envShared)
	}
	if envStrict := os.Getenv("SPAWN_COMPLIANCE_STRICT_MODE"); envStrict != "" {
		cfg.StrictMode = parseBool(envStrict)
	}

	// 1. CLI flags (highest priority)
	if flagMode != "" {
		cfg.Mode = ComplianceMode(strings.ToLower(flagMode))
	}
	if flagStrict {
		cfg.StrictMode = true
	}

	// Apply mode-specific defaults
	if cfg.Mode != ComplianceModeNone {
		applyModeDefaults(cfg)
	}

	return cfg, nil
}

// applyModeDefaults applies control enforcement based on compliance mode
func applyModeDefaults(cfg *ComplianceConfig) {
	switch cfg.Mode {
	case ComplianceModeNIST80171:
		// NIST 800-171 Rev 3 requirements
		cfg.EnforceEncryptedEBS = true
		cfg.EnforceIMDSv2 = true
		cfg.AuditLoggingRequired = true
		// Other controls recommended but not required

	case ComplianceModeNIST80053, ComplianceModeBaseLow, ComplianceModeFedRAMPLow:
		// FedRAMP Low / NIST 800-53 Low baseline
		cfg.EnforceEncryptedEBS = true
		cfg.EnforceIMDSv2 = true
		cfg.AuditLoggingRequired = true

	case ComplianceModeBaseMod, ComplianceModeFedRAMPMod:
		// FedRAMP Moderate / NIST 800-53 Moderate baseline
		cfg.EnforceEncryptedEBS = true
		cfg.EnforceIMDSv2 = true
		cfg.EnforcePrivateSubnets = true
		cfg.EnforceNoPublicIP = true
		cfg.AuditLoggingRequired = true
		cfg.AllowSharedInfrastructure = false // Self-hosted required

	case ComplianceModeBaseHigh, ComplianceModeFedRAMPHi:
		// FedRAMP High / NIST 800-53 High baseline
		cfg.EnforceEncryptedEBS = true
		cfg.EnforceIMDSv2 = true
		cfg.EnforcePrivateSubnets = true
		cfg.EnforceNoPublicIP = true
		cfg.EnforceVPCEndpoints = true
		cfg.EnforceMultiAZ = true
		cfg.EnforceCustomerKMS = true
		cfg.AuditLoggingRequired = true
		cfg.AllowSharedInfrastructure = false // Self-hosted required
	}
}

// IsComplianceEnabled returns true if any compliance mode is active
func (c *ComplianceConfig) IsComplianceEnabled() bool {
	return c.Mode != ComplianceModeNone
}

// RequiresSelfHosted returns true if the compliance mode requires self-hosted infrastructure
func (c *ComplianceConfig) RequiresSelfHosted() bool {
	// No compliance mode = no requirement
	if c.Mode == ComplianceModeNone {
		return false
	}

	switch c.Mode {
	case ComplianceModeBaseMod, ComplianceModeFedRAMPMod:
		return true // Always required for Moderate+
	case ComplianceModeBaseHigh, ComplianceModeFedRAMPHi:
		return true // Always required for High
	default:
		// For other modes (800-171, Low baseline), check AllowSharedInfrastructure flag
		return !c.AllowSharedInfrastructure
	}
}

// GetModeDisplayName returns a human-readable name for the compliance mode
func (c *ComplianceConfig) GetModeDisplayName() string {
	switch c.Mode {
	case ComplianceModeNone:
		return "None"
	case ComplianceModeNIST80171:
		return "NIST 800-171 Rev 3"
	case ComplianceModeNIST80053:
		return "NIST 800-53 Rev 5"
	case ComplianceModeBaseLow:
		return "NIST 800-53 Low Baseline"
	case ComplianceModeBaseMod:
		return "NIST 800-53 Moderate Baseline"
	case ComplianceModeBaseHigh:
		return "NIST 800-53 High Baseline"
	case ComplianceModeFedRAMPLow:
		return "FedRAMP Low"
	case ComplianceModeFedRAMPMod:
		return "FedRAMP Moderate"
	case ComplianceModeFedRAMPHi:
		return "FedRAMP High"
	default:
		return string(c.Mode)
	}
}

// GetConfigSource returns a human-readable description of where the compliance config came from
func GetComplianceConfigSource(ctx context.Context, flagMode string) string {
	if flagMode != "" {
		return "CLI flags"
	}

	if os.Getenv("SPAWN_COMPLIANCE_MODE") != "" {
		return "environment variables"
	}

	fileConfig, err := loadFromFile()
	if err == nil && fileConfig != nil && fileConfig.Compliance.Mode != "" {
		return "config file (~/.spawn/config.yaml)"
	}

	return "default (no compliance enforcement)"
}

// parseBool parses a string as a boolean (case-insensitive)
func parseBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1" || s == "yes" || s == "on" || s == "enabled"
}

// ValidateMode checks if the specified compliance mode is valid
func ValidateMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return nil // Empty is valid (means no compliance)
	}

	validModes := []string{
		string(ComplianceModeNIST80171),
		string(ComplianceModeNIST80053),
		string(ComplianceModeBaseLow),
		string(ComplianceModeBaseMod),
		string(ComplianceModeBaseHigh),
		string(ComplianceModeFedRAMPLow),
		string(ComplianceModeFedRAMPMod),
		string(ComplianceModeFedRAMPHi),
	}

	for _, valid := range validModes {
		if mode == valid {
			return nil
		}
	}

	return fmt.Errorf("invalid compliance mode %q, valid modes: %s", mode, strings.Join(validModes, ", "))
}
