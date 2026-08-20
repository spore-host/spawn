package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"gopkg.in/yaml.v3"
)

const (
	// Default DNS configuration
	defaultDomain      = "spore.host"
	defaultAPIEndpoint = "https://f4gm19tl70.execute-api.us-east-1.amazonaws.com/prod/update-dns"

	// SSM parameter paths
	ssmDomainPath      = "/spawn/dns/domain"
	ssmAPIEndpointPath = "/spawn/dns/api_endpoint"

	// Config file path
	configFileName = ".spawn/config.yaml"
)

// Config represents the spawn configuration
type Config struct {
	DNS            DNSConfig            `yaml:"dns"`
	Compliance     ComplianceConfig     `yaml:"compliance"`
	Infrastructure InfrastructureConfig `yaml:"infrastructure"`
	Defaults       LaunchDefaults       `yaml:"defaults"`
}

// DNSConfig represents DNS configuration.
//
// Enabled is a tri-state pointer rather than a plain bool (#549): a
// ~/.spawn/config.yaml that exists but doesn't mention a `dns:` section (or
// mentions `dns:` but not `enabled:`) must NOT be treated the same as an
// explicit `dns: { enabled: false }` — yaml.v3 leaves an absent bool key at
// its zero value (false), which previously silently disabled DNS registration
// for anyone with an unrelated config file. nil means "not specified at this
// layer"; a non-nil pointer means the layer explicitly said so.
type DNSConfig struct {
	Enabled     *bool  `yaml:"enabled"`
	Domain      string `yaml:"domain"`
	APIEndpoint string `yaml:"api_endpoint"`
}

// IsEnabled resolves the tri-state Enabled field to a concrete bool. A nil
// Enabled (nothing ever said otherwise) defaults to true — DNS registration
// is on by default, matching the observed pre-#549 behavior for anyone not
// using --no-dns or dns.enabled: false.
func (c *DNSConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func boolPtr(b bool) *bool { return &b }

// LoadDNSConfig loads DNS configuration with precedence:
// 1. CLI flags (passed as parameters, including flagNoDNS)
// 2. Environment variables
// 3. Config file
// 4. SSM Parameter Store
// 5. Defaults
//
// flagNoDNS is the --no-dns flag (#549): when true it forces Enabled to
// false regardless of what the config file says, since a CLI flag on this
// specific invocation should always win over a persisted default.
func LoadDNSConfig(ctx context.Context, flagDomain, flagAPIEndpoint string, flagNoDNS bool) (*DNSConfig, error) {
	cfg := &DNSConfig{
		Enabled:     boolPtr(true),
		Domain:      defaultDomain,
		APIEndpoint: defaultAPIEndpoint,
	}

	// 5. Start with defaults (already set above)

	// 4. Try SSM Parameter Store
	ssmConfig, err := loadFromSSM(ctx)
	if err == nil && ssmConfig != nil {
		if ssmConfig.Domain != "" {
			cfg.Domain = ssmConfig.Domain
		}
		if ssmConfig.APIEndpoint != "" {
			cfg.APIEndpoint = ssmConfig.APIEndpoint
		}
	}

	// 3. Try config file
	fileConfig, err := loadFromFile()
	if err == nil && fileConfig != nil {
		if fileConfig.DNS.Domain != "" {
			cfg.Domain = fileConfig.DNS.Domain
		}
		if fileConfig.DNS.APIEndpoint != "" {
			cfg.APIEndpoint = fileConfig.DNS.APIEndpoint
		}
		// Only override the default when the file actually specified `enabled:`
		// (fileConfig.DNS.Enabled != nil) — an absent key must not disable DNS.
		if fileConfig.DNS.Enabled != nil {
			cfg.Enabled = fileConfig.DNS.Enabled
		}
	}

	// 2. Environment variables
	if envDomain := os.Getenv("SPAWN_DNS_DOMAIN"); envDomain != "" {
		cfg.Domain = envDomain
	}
	if envEndpoint := os.Getenv("SPAWN_DNS_API_ENDPOINT"); envEndpoint != "" {
		cfg.APIEndpoint = envEndpoint
	}

	// 1. CLI flags (highest priority)
	if flagDomain != "" {
		cfg.Domain = flagDomain
	}
	if flagAPIEndpoint != "" {
		cfg.APIEndpoint = flagAPIEndpoint
	}
	if flagNoDNS {
		cfg.Enabled = boolPtr(false)
	}

	return cfg, nil
}

// loadFromFile loads configuration from ~/.spawn/config.yaml
func loadFromFile() (*Config, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	configPath := filepath.Join(homeDir, configFileName)

	// Check if file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file not found")
	}

	// Read file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse YAML
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return &cfg, nil
}

// loadFromSSM loads configuration from AWS SSM Parameter Store
func loadFromSSM(ctx context.Context) (*DNSConfig, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return loadFromSSMWithClient(ctx, ssm.NewFromConfig(awsCfg))
}

// loadFromSSMWithClient loads DNS configuration using the provided SSM client.
// This allows injection of a pre-configured client for testing.
func loadFromSSMWithClient(ctx context.Context, ssmClient *ssm.Client) (*DNSConfig, error) {
	cfg := &DNSConfig{}

	// Try to get domain parameter
	domainParam, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: stringPtr(ssmDomainPath),
	})
	if err == nil && domainParam.Parameter != nil && domainParam.Parameter.Value != nil {
		cfg.Domain = *domainParam.Parameter.Value
	}

	// Try to get API endpoint parameter
	endpointParam, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name: stringPtr(ssmAPIEndpointPath),
	})
	if err == nil && endpointParam.Parameter != nil && endpointParam.Parameter.Value != nil {
		cfg.APIEndpoint = *endpointParam.Parameter.Value
	}

	// Return nil if no parameters were found
	if cfg.Domain == "" && cfg.APIEndpoint == "" {
		return nil, fmt.Errorf("no SSM parameters found")
	}

	return cfg, nil
}

// GetConfigSource returns a human-readable description of where the config came from
func GetConfigSource(ctx context.Context, flagDomain, flagAPIEndpoint string) string {
	if flagDomain != "" || flagAPIEndpoint != "" {
		return "CLI flags"
	}

	if os.Getenv("SPAWN_DNS_DOMAIN") != "" || os.Getenv("SPAWN_DNS_API_ENDPOINT") != "" {
		return "environment variables"
	}

	homeDir, _ := os.UserHomeDir()
	configPath := filepath.Join(homeDir, configFileName)
	if _, err := os.Stat(configPath); err == nil {
		return "config file (~/.spawn/config.yaml)"
	}

	// Check SSM
	ssmConfig, err := loadFromSSM(ctx)
	if err == nil && ssmConfig != nil {
		return "SSM Parameter Store (auto-discovery)"
	}

	return "default (spore.host)"
}

func stringPtr(s string) *string {
	return &s
}
