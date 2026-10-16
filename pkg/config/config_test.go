package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// isolateDNSConfigEnv points HOME at a fresh temp dir (no ~/.spawn/config.yaml
// unless the test writes one) and disables AWS credential discovery, so no
// test here can reach a real account even if LoadDNSConfig regains a lookup.
func isolateDNSConfigEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(home, "no-aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, "no-aws-creds"))
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

	cfg, err := LoadDNSConfig(false)
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

	cfg, err := LoadDNSConfig(false)
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

	cfg, err := LoadDNSConfig(false)
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

	cfg, err := LoadDNSConfig(true /* --no-dns */)
	if err != nil {
		t.Fatalf("LoadDNSConfig() error = %v", err)
	}
	if cfg.IsEnabled() {
		t.Error("IsEnabled() = true, want false (--no-dns must override dns.enabled: true)")
	}
}

// TestLoadDNSConfig_ConfigDisabled_NoDNSAlsoTrue: both the config file and
// the flag agree on disabled — still disabled (no surprise interaction).
func TestLoadDNSConfig_ConfigDisabled_NoDNSAlsoTrue(t *testing.T) {
	home := isolateDNSConfigEnv(t)
	writeSpawnConfig(t, home, "dns:\n  enabled: false\n")

	cfg, err := LoadDNSConfig(true)
	if err != nil {
		t.Fatalf("LoadDNSConfig() error = %v", err)
	}
	if cfg.IsEnabled() {
		t.Error("IsEnabled() = true, want false")
	}
}

// dns.enabled beside the custom-target keys (#891) is still honoured: the keys
// are read by ResolveDNSTarget and must not disturb LoadDNSConfig.
func TestLoadDNSConfig_EnabledBesideTargetKeys(t *testing.T) {
	home := isolateDNSConfigEnv(t)
	writeSpawnConfig(t, home, "dns:\n  enabled: false\n  domain: spore.example.edu\n  api_endpoint: https://abc.lambda-url.eu-west-1.on.aws/\n")
	cfg, err := LoadDNSConfig(false)
	if err != nil {
		t.Fatalf("LoadDNSConfig() error = %v", err)
	}
	if cfg.IsEnabled() {
		t.Error("dns.enabled: false beside domain/api_endpoint was not honoured")
	}
}

type fakeSSM map[string]string

func (f fakeSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	v, ok := f[*in.Name]
	if !ok {
		return nil, errors.New("ParameterNotFound")
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: &v}}, nil
}

// Each field resolves on its own: flag > env > file > SSM > spore.host's (empty).
func TestResolveDNSTarget_Precedence(t *testing.T) {
	const fileEP = "https://file.lambda-url.eu-west-1.on.aws/"
	ssmAll := fakeSSM{SSMDNSDomainPath: "ssm.example.org", SSMDNSEndpointPath: "https://ssm.lambda-url.us-west-2.on.aws/"}
	ctx := context.Background()

	home := isolateDNSConfigEnv(t)
	t.Setenv("SPAWN_DNS_DOMAIN", "")
	t.Setenv("SPAWN_DNS_API_ENDPOINT", "")

	if got := ResolveDNSTarget(ctx, nil, "", ""); got != (DNSTarget{}) {
		t.Errorf("nothing set = %+v, want empty (spore.host)", got)
	}
	got := ResolveDNSTarget(ctx, ssmAll, "", "")
	if got.Domain != "ssm.example.org" || got.DomainSource != "SSM "+SSMDNSDomainPath || got.Endpoint != ssmAll[SSMDNSEndpointPath] {
		t.Errorf("SSM only = %+v", got)
	}

	writeSpawnConfig(t, home, "dns:\n  domain: file.example.org\n  api_endpoint: "+fileEP+"\n")
	got = ResolveDNSTarget(ctx, ssmAll, "", "")
	if got.Domain != "file.example.org" || got.Endpoint != fileEP || got.DomainSource != "~/.spawn/config.yaml" {
		t.Errorf("file over SSM = %+v", got)
	}

	t.Setenv("SPAWN_DNS_DOMAIN", "env.example.org")
	got = ResolveDNSTarget(ctx, ssmAll, "", "")
	if got.Domain != "env.example.org" || got.DomainSource != "env SPAWN_DNS_DOMAIN" || got.Endpoint != fileEP {
		t.Errorf("env over file, per field = %+v", got)
	}

	got = ResolveDNSTarget(ctx, ssmAll, "flag.example.org", "")
	if got.Domain != "flag.example.org" || got.DomainSource != "flag" {
		t.Errorf("flag over env = %+v", got)
	}

	// A missing SSM parameter is "not set", not an error.
	isolateDNSConfigEnv(t)
	t.Setenv("SPAWN_DNS_DOMAIN", "")
	if got := ResolveDNSTarget(ctx, fakeSSM{}, "", ""); got != (DNSTarget{}) {
		t.Errorf("SSM without the parameters = %+v, want empty", got)
	}
}
