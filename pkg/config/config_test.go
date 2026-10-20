package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
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
		return nil, &ssmtypes.ParameterNotFound{Message: strPtr("not found")}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: &v}}, nil
}

// Each field resolves on its own: flag > env > file > SSM > spore.host's (empty).
func TestResolveDNSTarget_Precedence(t *testing.T) {
	const fileEP = "https://file.lambda-url.eu-west-1.on.aws/"
	ssmAll := fakeSSM{SSMDNSDomainPath: "ssm.example.org", SSMDNSEndpointPath: "https://ssm.lambda-url.us-west-2.on.aws/"}

	home := isolateDNSConfigEnv(t)
	t.Setenv("SPAWN_DNS_DOMAIN", "")
	t.Setenv("SPAWN_DNS_API_ENDPOINT", "")

	if got := mustResolve(t, nil, "", ""); got != (DNSTarget{}) {
		t.Errorf("nothing set = %+v, want empty (spore.host)", got)
	}
	got := mustResolve(t, ssmAll, "", "")
	if got.Domain != "ssm.example.org" || got.DomainSource != "SSM "+SSMDNSDomainPath || got.Endpoint != ssmAll[SSMDNSEndpointPath] {
		t.Errorf("SSM only = %+v", got)
	}

	writeSpawnConfig(t, home, "dns:\n  domain: file.example.org\n  api_endpoint: "+fileEP+"\n")
	got = mustResolve(t, ssmAll, "", "")
	if got.Domain != "file.example.org" || got.Endpoint != fileEP || got.DomainSource != "~/.spawn/config.yaml" {
		t.Errorf("file over SSM = %+v", got)
	}

	t.Setenv("SPAWN_DNS_DOMAIN", "env.example.org")
	got = mustResolve(t, ssmAll, "", "")
	if got.Domain != "env.example.org" || got.DomainSource != "env SPAWN_DNS_DOMAIN" || got.Endpoint != fileEP {
		t.Errorf("env over file, per field = %+v", got)
	}

	got = mustResolve(t, ssmAll, "flag.example.org", "")
	if got.Domain != "flag.example.org" || got.DomainSource != "flag" {
		t.Errorf("flag over env = %+v", got)
	}

	// A missing SSM parameter is "not set", not an error.
	isolateDNSConfigEnv(t)
	t.Setenv("SPAWN_DNS_DOMAIN", "")
	if got := mustResolve(t, fakeSSM{}, "", ""); got != (DNSTarget{}) {
		t.Errorf("SSM without the parameters = %+v, want empty", got)
	}
}

func strPtr(s string) *string { return &s }

func mustResolve(t *testing.T, g SSMGetter, flagDomain, flagEndpoint string) DNSTarget {
	t.Helper()
	got, err := ResolveDNSTarget(context.Background(), g, flagDomain, flagEndpoint, "")
	if err != nil {
		t.Fatalf("ResolveDNSTarget: %v", err)
	}
	return got
}

// failingSSM answers every GetParameter with err, and records the paths asked.
type failingSSM struct {
	err   error
	asked []string
}

func (f *failingSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	f.asked = append(f.asked, *in.Name)
	return nil, f.err
}

// #1121: an SSM read that fails for any reason but ParameterNotFound must never
// resolve to spore.host's default. The issue's own words: "A test should pin that
// an AccessDenied from the SSM getter never resolves to the spore.host default
// silently." Each case is the error for one of the two parameters a launch reads
// first, with the other absent.
func TestResolveDNSTarget_UnreadableSSMIsAnError(t *testing.T) {
	isolateDNSConfigEnv(t)
	t.Setenv("SPAWN_DNS_DOMAIN", "")
	t.Setenv("SPAWN_DNS_API_ENDPOINT", "")
	t.Setenv("SPAWN_DNS_FUNCTION_ARN", "")
	denied := &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "User is not authorized to perform: ssm:GetParameter"}
	throttled := &smithy.GenericAPIError{Code: "ThrottlingException", Message: "Rate exceeded"}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"AccessDenied", denied},
		{"throttled", throttled},
		{"network", errors.New("dial tcp: i/o timeout")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &failingSSM{err: tc.err}
			got, err := ResolveDNSTarget(context.Background(), g, "", "", "")
			if err == nil {
				t.Fatalf("an SSM %s resolved to %+v with no error: the launch would register under spore.host", tc.name, got)
			}
			if got != (DNSTarget{}) {
				t.Errorf("an error came back with a target %+v", got)
			}
			var u *DNSSettingUnreadableError
			if !errors.As(err, &u) || u.Path != SSMDNSDomainPath {
				t.Fatalf("err = %v, want a *DNSSettingUnreadableError for %s", err, SSMDNSDomainPath)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("err does not wrap the SSM error: %v", err)
			}
			msg := err.Error()
			for _, want := range []string{SSMDNSDomainPath, "ssm:GetParameter", "--dns-domain", "SPAWN_DNS_DOMAIN"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message does not name %q:\n%s", want, msg)
				}
			}
		})
	}

	// The endpoint parameter too, once the domain is set elsewhere.
	t.Setenv("SPAWN_DNS_DOMAIN", "spore.example.edu")
	g := &failingSSM{err: denied}
	_, err := ResolveDNSTarget(context.Background(), g, "", "", "")
	var u *DNSSettingUnreadableError
	if !errors.As(err, &u) || u.Path != SSMDNSEndpointPath {
		t.Fatalf("endpoint: err = %v, want one for %s", err, SSMDNSEndpointPath)
	}

	// And the function ARN, read only once an endpoint was found.
	t.Setenv("SPAWN_DNS_API_ENDPOINT", "https://abc.lambda-url.eu-west-1.on.aws/")
	g = &failingSSM{err: denied}
	_, err = ResolveDNSTarget(context.Background(), g, "", "", "")
	if !errors.As(err, &u) || u.Path != SSMDNSFunctionARNPath {
		t.Fatalf("function ARN: err = %v, want one for %s", err, SSMDNSFunctionARNPath)
	}
}

// ParameterNotFound is "not set": spore.host's default, no error — as before.
// And the function ARN is not looked up at all without an endpoint, so a launch
// against spore.host's updater makes no third SSM call.
func TestResolveDNSTarget_ParameterNotFoundIsTheDefault(t *testing.T) {
	isolateDNSConfigEnv(t)
	t.Setenv("SPAWN_DNS_DOMAIN", "")
	t.Setenv("SPAWN_DNS_API_ENDPOINT", "")
	t.Setenv("SPAWN_DNS_FUNCTION_ARN", "")
	g := &failingSSM{err: &ssmtypes.ParameterNotFound{Message: strPtr("nope")}}
	got, err := ResolveDNSTarget(context.Background(), g, "", "", "")
	if err != nil || got != (DNSTarget{}) {
		t.Fatalf("ParameterNotFound = (%+v, %v), want spore.host's default and no error", got, err)
	}
	if strings.Join(g.asked, ",") != SSMDNSDomainPath+","+SSMDNSEndpointPath {
		t.Errorf("asked SSM for %v, want only the domain and endpoint", g.asked)
	}
}

// The function ARN resolves through the same chain as the other two.
func TestResolveDNSTarget_FunctionARN(t *testing.T) {
	isolateDNSConfigEnv(t)
	t.Setenv("SPAWN_DNS_DOMAIN", "")
	t.Setenv("SPAWN_DNS_API_ENDPOINT", "")
	t.Setenv("SPAWN_DNS_FUNCTION_ARN", "")
	const arn = "arn:aws:lambda:us-west-2:111122223333:function:spawn-dns-updater"
	ssmAll := fakeSSM{SSMDNSEndpointPath: "https://ssm.lambda-url.us-west-2.on.aws/", SSMDNSFunctionARNPath: arn}
	got, err := ResolveDNSTarget(context.Background(), ssmAll, "", "", "")
	if err != nil || got.FunctionARN != arn || got.FunctionARNSource != "SSM "+SSMDNSFunctionARNPath {
		t.Errorf("SSM = (%+v, %v)", got, err)
	}
	t.Setenv("SPAWN_DNS_FUNCTION_ARN", "arn:aws:lambda:us-west-2:444455556666:function:x")
	got, _ = ResolveDNSTarget(context.Background(), ssmAll, "", "", "")
	if got.FunctionARNSource != "env SPAWN_DNS_FUNCTION_ARN" {
		t.Errorf("env over SSM = %+v", got)
	}
	got, _ = ResolveDNSTarget(context.Background(), ssmAll, "", "", arn)
	if got.FunctionARN != arn || got.FunctionARNSource != "flag" {
		t.Errorf("flag over env = %+v", got)
	}
}
