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

// DNSConfig represents DNS configuration
type DNSConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Domain      string `yaml:"domain"`
	APIEndpoint string `yaml:"api_endpoint"`
}

// LoadDNSConfig loads DNS configuration with precedence:
// 1. CLI flags (passed as parameters)
// 2. Environment variables
// 3. Config file
// 4. SSM Parameter Store
// 5. Defaults
func LoadDNSConfig(ctx context.Context, flagDomain, flagAPIEndpoint string) (*DNSConfig, error) {
	cfg := &DNSConfig{
		Enabled:     true,
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
		cfg.Enabled = fileConfig.DNS.Enabled
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
