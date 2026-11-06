package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spore-host/spawn/pkg/ttl"
	"gopkg.in/yaml.v3"
)

// LaunchDefaults holds user-defined defaults for launch flags.
// Values here are used when the corresponding CLI flag is not explicitly provided.
// Stored under the "defaults" key in ~/.spawn/config.yaml.
type LaunchDefaults struct {
	SlackWorkspace  string `yaml:"slack_workspace,omitempty"`
	ActiveProcesses string `yaml:"active_processes,omitempty"`
	ActivePorts     string `yaml:"active_ports,omitempty"`
	IdleTimeout     string `yaml:"idle_timeout,omitempty"`
	HibernateOnIdle *bool  `yaml:"hibernate_on_idle,omitempty"`
	// BootstrapEgress is --bootstrap-egress's default (#1515): "none" or
	// "proxy". The opt-in to the egress proxy, set once.
	BootstrapEgress string `yaml:"bootstrap_egress,omitempty"`
	// ComputeProfile is the AWS named profile compute operations use when
	// SPAWN_COMPUTE_PROFILE is unset (#1538): where instances and a detached
	// sweep's members launch. Unset, compute uses the caller's own profile
	// (--profile / SPORE_PROFILE / AWS_PROFILE) or the ambient chain. For a
	// split setup that launches in a dedicated account, e.g. the spore.host
	// team's spore-host-dev.
	ComputeProfile string `yaml:"compute_profile,omitempty"`
}

// knownDefaultKeys maps CLI flag names (with hyphens) to their display names.
var knownDefaultKeys = []string{
	"slack-workspace",
	"active-processes",
	"active-ports",
	"idle-timeout",
	"hibernate-on-idle",
	"bootstrap-egress",
	"compute-profile",
}

// KnownDefaultKeys returns the list of valid default key names.
func KnownDefaultKeys() []string {
	return knownDefaultKeys
}

// LoadLaunchDefaults reads the defaults section from ~/.spawn/config.yaml.
// Returns an empty struct (not an error) if the file doesn't exist or has no defaults.
func LoadLaunchDefaults() (*LaunchDefaults, error) {
	cfg, err := loadFromFile()
	if err != nil || cfg == nil {
		return &LaunchDefaults{}, nil
	}
	return &cfg.Defaults, nil
}

// SetLaunchDefault sets a single default by CLI flag name and persists it.
func SetLaunchDefault(key, value string) error {
	cfg, err := loadOrInitFile()
	if err != nil {
		return err
	}
	if err := applyDefaultKey(&cfg.Defaults, key, value); err != nil {
		return err
	}
	return saveToFile(cfg)
}

// UnsetLaunchDefault clears a single default by CLI flag name.
func UnsetLaunchDefault(key string) error {
	cfg, err := loadOrInitFile()
	if err != nil {
		return err
	}
	if err := applyDefaultKey(&cfg.Defaults, key, ""); err != nil {
		return err
	}
	return saveToFile(cfg)
}

// applyDefaultKey sets or clears a field by CLI flag name.
func applyDefaultKey(d *LaunchDefaults, key, value string) error {
	switch key {
	case "slack-workspace":
		d.SlackWorkspace = value
	case "active-processes":
		d.ActiveProcesses = value
	case "active-ports":
		d.ActivePorts = value
	case "idle-timeout":
		// Validated with the shared parser (#990), so a default that spored
		// could not read never reaches a launch. "" clears it.
		if value != "" {
			if _, err := ttl.ParseTTL(value); err != nil {
				return fmt.Errorf("invalid idle-timeout %q: %w (use a duration such as 30m, 2h or 1d)", value, err)
			}
		}
		d.IdleTimeout = value
	case "hibernate-on-idle":
		if value == "" {
			d.HibernateOnIdle = nil
		} else if value == "true" || value == "1" || value == "yes" {
			t := true
			d.HibernateOnIdle = &t
		} else if value == "false" || value == "0" || value == "no" {
			f := false
			d.HibernateOnIdle = &f
		} else {
			return fmt.Errorf("hibernate-on-idle must be true or false")
		}
	case "bootstrap-egress":
		if err := ValidateBootstrapEgress(value); err != nil {
			return err
		}
		d.BootstrapEgress = value
	case "compute-profile":
		value = strings.TrimSpace(value)
		if strings.ContainsAny(value, " \t\n") {
			return fmt.Errorf("invalid compute-profile %q: an AWS profile name has no whitespace", value)
		}
		d.ComputeProfile = value
	default:
		return fmt.Errorf("unknown default key %q — valid keys: %v", key, knownDefaultKeys)
	}
	return nil
}

// GetDefaultValue returns the string representation of a default by CLI flag name.
func GetDefaultValue(d *LaunchDefaults, key string) string {
	switch key {
	case "slack-workspace":
		return d.SlackWorkspace
	case "active-processes":
		return d.ActiveProcesses
	case "active-ports":
		return d.ActivePorts
	case "idle-timeout":
		return d.IdleTimeout
	case "bootstrap-egress":
		return d.BootstrapEgress
	case "hibernate-on-idle":
		if d.HibernateOnIdle == nil {
			return ""
		}
		if *d.HibernateOnIdle {
			return "true"
		}
		return "false"
	case "compute-profile":
		return d.ComputeProfile
	}
	return ""
}

// ValidateBootstrapEgress accepts --bootstrap-egress's values, "none" and
// "proxy", and "" (unset).
func ValidateBootstrapEgress(value string) error {
	switch value {
	case "", "none", "proxy":
		return nil
	}
	return fmt.Errorf("invalid bootstrap-egress %q: use none or proxy", value)
}

func loadOrInitFile() (*Config, error) {
	cfg, err := loadFromFile()
	if err != nil || cfg == nil {
		cfg = &Config{}
	}
	return cfg, nil
}

func saveToFile(cfg *Config) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(homeDir, ".spawn")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
