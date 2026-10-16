package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"gopkg.in/yaml.v3"
)

const (
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
// Domain and APIEndpoint name a custom DNS deployment (#891) — see
// ResolveDNSTarget. They were removed in #872 because the only thing that read
// them was the CLI's own SSH registration, which never sent the domain; they now
// reach spored, which is what registers.
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

// LoadDNSConfig resolves whether this launch registers DNS: the --no-dns flag
// (flagNoDNS) wins, then `dns.enabled` in ~/.spawn/config.yaml, then on.
func LoadDNSConfig(flagNoDNS bool) (*DNSConfig, error) {
	cfg := &DNSConfig{Enabled: boolPtr(true)}

	// Only override the default when the file actually specified `enabled:`
	// (fileConfig.DNS.Enabled != nil) — an absent key must not disable DNS.
	if fileConfig, err := loadFromFile(); err == nil && fileConfig != nil && fileConfig.DNS.Enabled != nil {
		cfg.Enabled = fileConfig.DNS.Enabled
	}
	if flagNoDNS {
		cfg.Enabled = boolPtr(false)
	}
	return cfg, nil
}

// SSM parameters a custom DNS deployment can publish for every launch in an
// account and region (#891).
const (
	SSMDNSDomainPath   = "/spawn/dns/domain"
	SSMDNSEndpointPath = "/spawn/dns/api_endpoint"
)

// DNSTarget is a resolved custom DNS target. Empty fields mean spore.host's.
// The sources say where each value came from, for the launch preview and for
// error messages. Unvalidated: pkg/dns owns validation (and imports this
// package, so it cannot be called from here).
type DNSTarget struct {
	Domain, DomainSource     string
	Endpoint, EndpointSource string
}

// SSMGetter is the one SSM call ResolveDNSTarget makes; *ssm.Client satisfies it.
type SSMGetter interface {
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// ResolveDNSTarget resolves the custom DNS target for a launch, each field
// independently: the flag, then SPAWN_DNS_DOMAIN / SPAWN_DNS_API_ENDPOINT, then
// ~/.spawn/config.yaml (dns.domain / dns.api_endpoint), then the SSM parameters
// above, then spore.host's (empty). ssmClient may be nil to skip SSM; a missing
// parameter, or any SSM error, means "not set there".
func ResolveDNSTarget(ctx context.Context, ssmClient SSMGetter, flagDomain, flagEndpoint string) DNSTarget {
	var file DNSConfig
	if fc, err := loadFromFile(); err == nil && fc != nil {
		file = fc.DNS
	}
	pick := func(flag, envKey, fileVal, ssmPath string) (string, string) {
		if v := strings.TrimSpace(flag); v != "" {
			return v, "flag"
		}
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			return v, "env " + envKey
		}
		if v := strings.TrimSpace(fileVal); v != "" {
			return v, "~/.spawn/config.yaml"
		}
		if ssmClient != nil {
			out, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{Name: &ssmPath})
			if err == nil && out.Parameter != nil && out.Parameter.Value != nil {
				if v := strings.TrimSpace(*out.Parameter.Value); v != "" {
					return v, "SSM " + ssmPath
				}
			}
		}
		return "", ""
	}
	var t DNSTarget
	t.Domain, t.DomainSource = pick(flagDomain, "SPAWN_DNS_DOMAIN", file.Domain, SSMDNSDomainPath)
	t.Endpoint, t.EndpointSource = pick(flagEndpoint, "SPAWN_DNS_API_ENDPOINT", file.APIEndpoint, SSMDNSEndpointPath)
	return t
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
