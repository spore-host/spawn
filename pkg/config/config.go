package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
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
	FunctionARN string `yaml:"function_arn"`
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
// account and region (#891). SSMDNSFunctionARNPath names the updater's Lambda
// function (#1158): a Function URL's hostname carries an opaque id, not the
// function's name or account, so the instance role's lambda:InvokeFunctionUrl
// grant cannot be derived from the endpoint alone.
const (
	SSMDNSDomainPath      = "/spawn/dns/domain"
	SSMDNSEndpointPath    = "/spawn/dns/api_endpoint"
	SSMDNSFunctionARNPath = "/spawn/dns/function_arn"
)

// DNSTarget is a resolved custom DNS target. Empty fields mean spore.host's.
// The sources say where each value came from, for the launch preview and for
// error messages. Unvalidated: pkg/dns owns validation (and imports this
// package, so it cannot be called from here).
//
// FunctionARN is resolved only when Endpoint is: it names the function behind a
// custom endpoint, and means nothing for spore.host's.
type DNSTarget struct {
	Domain, DomainSource           string
	Endpoint, EndpointSource       string
	FunctionARN, FunctionARNSource string
}

// SSMGetter is the one SSM call ResolveDNSTarget makes; *ssm.Client satisfies it.
type SSMGetter interface {
	GetParameter(ctx context.Context, in *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// DNSSettingUnreadableError is an SSM read of a /spawn/dns/* parameter that
// failed for a reason other than the parameter not existing — AccessDenied,
// throttling, the network (#1121).
//
// It is an error and not "not set" because the two are not the same thing: an
// account that publishes /spawn/dns/* does so to keep its spores' names and DNS
// traffic off spore.host, and a launcher who cannot read the setting would
// otherwise be sent, silently, to exactly the updater the setting exists to avoid.
type DNSSettingUnreadableError struct {
	Path string // the SSM parameter
	// Flag, Env and FileKey are the other ways to supply the value, any of which
	// skips the SSM lookup.
	Flag, Env, FileKey string
	Err                error
}

func (e *DNSSettingUnreadableError) Error() string {
	return fmt.Sprintf("cannot read SSM parameter %s (%v).\n"+
		"  spawn will not fall back to spore.host's DNS when the account's DNS setting is unreadable.\n"+
		"  Grant ssm:GetParameter on arn:aws:ssm:<region>:<account>:parameter/spawn/dns/*, "+
		"or set the value with %s, %s or %s in ~/.spawn/config.yaml, which skips the lookup",
		e.Path, e.Err, e.Flag, e.Env, e.FileKey)
}

func (e *DNSSettingUnreadableError) Unwrap() error { return e.Err }

// ResolveDNSTarget resolves the custom DNS target for a launch, each field
// independently: the flag, then SPAWN_DNS_DOMAIN / SPAWN_DNS_API_ENDPOINT /
// SPAWN_DNS_FUNCTION_ARN, then ~/.spawn/config.yaml (dns.domain /
// dns.api_endpoint / dns.function_arn), then the SSM parameters above, then
// spore.host's (empty). The function ARN is looked up only when an endpoint was
// found.
//
// ssmClient may be nil to skip SSM. A parameter that does not exist
// (ParameterNotFound) means "not set there"; any other SSM error is returned as a
// *DNSSettingUnreadableError, and the caller must not launch (#1121).
func ResolveDNSTarget(ctx context.Context, ssmClient SSMGetter, flagDomain, flagEndpoint, flagFunctionARN string) (DNSTarget, error) {
	var file DNSConfig
	if fc, err := loadFromFile(); err == nil && fc != nil {
		file = fc.DNS
	}
	pick := func(flag, flagName, envKey, fileVal, fileKey, ssmPath string) (string, string, error) {
		if v := strings.TrimSpace(flag); v != "" {
			return v, "flag", nil
		}
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			return v, "env " + envKey, nil
		}
		if v := strings.TrimSpace(fileVal); v != "" {
			return v, "~/.spawn/config.yaml", nil
		}
		if ssmClient == nil {
			return "", "", nil
		}
		out, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{Name: &ssmPath})
		if err != nil {
			var nf *ssmtypes.ParameterNotFound
			if errors.As(err, &nf) {
				return "", "", nil
			}
			return "", "", &DNSSettingUnreadableError{Path: ssmPath, Flag: flagName, Env: envKey, FileKey: fileKey, Err: err}
		}
		if out.Parameter != nil && out.Parameter.Value != nil {
			if v := strings.TrimSpace(*out.Parameter.Value); v != "" {
				return v, "SSM " + ssmPath, nil
			}
		}
		return "", "", nil
	}
	var t DNSTarget
	var err error
	if t.Domain, t.DomainSource, err = pick(flagDomain, "--dns-domain", "SPAWN_DNS_DOMAIN", file.Domain, "dns.domain", SSMDNSDomainPath); err != nil {
		return DNSTarget{}, err
	}
	if t.Endpoint, t.EndpointSource, err = pick(flagEndpoint, "--dns-api-endpoint", "SPAWN_DNS_API_ENDPOINT", file.APIEndpoint, "dns.api_endpoint", SSMDNSEndpointPath); err != nil {
		return DNSTarget{}, err
	}
	if t.Endpoint != "" {
		if t.FunctionARN, t.FunctionARNSource, err = pick(flagFunctionARN, "--dns-function-arn", "SPAWN_DNS_FUNCTION_ARN", file.FunctionARN, "dns.function_arn", SSMDNSFunctionARNPath); err != nil {
			return DNSTarget{}, err
		}
	}
	return t, nil
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
