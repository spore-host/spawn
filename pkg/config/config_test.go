package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// isolateDNSConfigEnv points HOME at a fresh temp dir (no ~/.spawn/config.yaml
// unless the test writes one) and disables AWS credential discovery so
// LoadDNSConfig's SSM lookup fails fast instead of hanging or hitting a real
// account. This is the same pattern cmd/service_test.go uses for AWS-adjacent
// hermetic tests.
func isolateDNSConfigEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(home, "no-aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, "no-aws-creds"))
	t.Setenv("SPAWN_DNS_DOMAIN", "")
	t.Setenv("SPAWN_DNS_API_ENDPOINT", "")
	return home
}

// writeSpawnConfig writes ~/.spawn/config.yaml with the given content.
func writeSpawnConfig(t *testing.T, home, yaml string) {
	t.Helper()
	dir := filepath.Join(home, ".spawn")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .spawn: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// TestDNSConfig_IsEnabled_TriState is the #549 regression: an absent
// `enabled:` key (nil pointer) must resolve to true (the observed
// pre-#549 default), not to the bool zero value.
func TestDNSConfig_IsEnabled_TriState(t *testing.T) {
	tests := []struct {
		name    string
		enabled *bool
		want    bool
	}{
		{"nil (unspecified) defaults to enabled", nil, true},
		{"explicit true", boolPtr(true), true},
		{"explicit false", boolPtr(false), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &DNSConfig{Enabled: tt.enabled}
			if got := cfg.IsEnabled(); got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoadDNSConfig_NoConfigFile_DefaultsEnabled is the core #549 bug: a
// machine with no ~/.spawn/config.yaml at all (the common case) must
// register DNS by default.
func TestLoadDNSConfig_NoConfigFile_DefaultsEnabled(t *testing.T) {
	isolateDNSConfigEnv(t)

	cfg, err := LoadDNSConfig(context.Background(), "", "", false)
	if err != nil {
		t.Fatalf("LoadDNSConfig() error = %v", err)
	}
	if !cfg.IsEnabled() {
		t.Error("IsEnabled() = false, want true (no config file present, no --no-dns)")
	}
}

// TestLoadDNSConfig_ConfigFilePresentButSilentOnDNS_StillEnabled is the exact
// #549 bug report: "a config file present but silent on dns: unmarshals to
// Enabled: false and overwrites the presumed true default." A config file
// that configures something unrelated (compliance mode here) but never
// mentions `dns:` must NOT disable DNS registration.
func TestLoadDNSConfig_ConfigFilePresentButSilentOnDNS_StillEnabled(t *testing.T) {
	home := isolateDNSConfigEnv(t)
	writeSpawnConfig(t, home, "compliance:\n  mode: nist-800-171\n")

	cfg, err := LoadDNSConfig(context.Background(), "", "", false)
	if err != nil {
		t.Fatalf("LoadDNSConfig() error = %v", err)
	}
	if !cfg.IsEnabled() {
		t.Error("IsEnabled() = false, want true — a config file silent on `dns:` must not disable DNS registration (#549)")
	}
}

// TestLoadDNSConfig_ConfigDisabled_NoFlag_Skips: config says disabled, no
// --no-dns flag given → still skip (config file honored).
func TestLoadDNSConfig_ConfigDisabled_NoFlag_Skips(t *testing.T) {
	home := isolateDNSConfigEnv(t)
	writeSpawnConfig(t, home, "dns:\n  enabled: false\n")

	cfg, err := LoadDNSConfig(context.Background(), "", "", false)
	if err != nil {
		t.Fatalf("LoadDNSConfig() error = %v", err)
	}
	if cfg.IsEnabled() {
		t.Error("IsEnabled() = true, want false (dns.enabled: false in config file)")
	}
}

// TestLoadDNSConfig_ConfigEnabled_FlagOverridesToDisabled: config says
// enabled, --no-dns given → --no-dns wins (CLI flag > config file, matching
// the --ttl-vs-config precedence convention this repo uses elsewhere).
func TestLoadDNSConfig_ConfigEnabled_FlagOverridesToDisabled(t *testing.T) {
	home := isolateDNSConfigEnv(t)
	writeSpawnConfig(t, home, "dns:\n  enabled: true\n")

	cfg, err := LoadDNSConfig(context.Background(), "", "", true /* --no-dns */)
	if err != nil {
		t.Fatalf("LoadDNSConfig() error = %v", err)
	}
	if cfg.IsEnabled() {
		t.Error("IsEnabled() = true, want false (--no-dns must override dns.enabled: true)")
	}
}

// TestLoadDNSConfig_NothingSet_DefaultProceeds: no config file, no flag →
// default (enabled) proceeds. This is the explicit "nothing set" case called
// out in the issue's test requirements, distinct from the no-file case above
// in that here we also confirm domain/endpoint fall back to the built-in
// defaults, not just Enabled.
func TestLoadDNSConfig_NothingSet_DefaultProceeds(t *testing.T) {
	isolateDNSConfigEnv(t)

	cfg, err := LoadDNSConfig(context.Background(), "", "", false)
	if err != nil {
		t.Fatalf("LoadDNSConfig() error = %v", err)
	}
	if !cfg.IsEnabled() {
		t.Error("IsEnabled() = false, want true (default)")
	}
	if cfg.Domain != defaultDomain {
		t.Errorf("Domain = %q, want default %q", cfg.Domain, defaultDomain)
	}
	if cfg.APIEndpoint != defaultAPIEndpoint {
		t.Errorf("APIEndpoint = %q, want default %q", cfg.APIEndpoint, defaultAPIEndpoint)
	}
}

// TestLoadDNSConfig_ConfigDisabled_NoDNSAlsoTrue: both the config file and
// the flag agree on disabled — still disabled (no surprise interaction).
func TestLoadDNSConfig_ConfigDisabled_NoDNSAlsoTrue(t *testing.T) {
	home := isolateDNSConfigEnv(t)
	writeSpawnConfig(t, home, "dns:\n  enabled: false\n")

	cfg, err := LoadDNSConfig(context.Background(), "", "", true)
	if err != nil {
		t.Fatalf("LoadDNSConfig() error = %v", err)
	}
	if cfg.IsEnabled() {
		t.Error("IsEnabled() = true, want false")
	}
}
