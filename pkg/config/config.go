package config

import (
	"fmt"
	"os"
	"path/filepath"

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
// Only `enabled` remains (#872). The record is registered by spored on the
// instance, from the spawn:dns-name tag, against the endpoint compiled into it;
// the CLI's own SSH-driven registration, which is what `domain` and
// `api_endpoint` (and their env, SSM and flag equivalents) configured, posted to
// an API Gateway deleted long ago and never sent the domain at all. Those keys
// are now ignored rather than read and silently disregarded.
//
// Enabled is a tri-state pointer rather than a plain bool (#549): a
// ~/.spawn/config.yaml that exists but doesn't mention a `dns:` section (or
// mentions `dns:` but not `enabled:`) must NOT be treated the same as an
// explicit `dns: { enabled: false }` — yaml.v3 leaves an absent bool key at
// its zero value (false), which previously silently disabled DNS registration
// for anyone with an unrelated config file. nil means "not specified at this
// layer"; a non-nil pointer means the layer explicitly said so.
type DNSConfig struct {
	Enabled *bool `yaml:"enabled"`
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
