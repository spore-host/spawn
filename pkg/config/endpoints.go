package config

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// Environment represents a deployment environment (integ or prod).
type Environment string

const (
	EnvInteg Environment = "integ"
	EnvProd  Environment = "prod"
)

// GetEnvironment returns the active deployment environment.
// Reads SPORE_ENV; defaults to prod for backward compatibility.
func GetEnvironment() Environment {
	switch os.Getenv("SPORE_ENV") {
	case "integ", "integration", "staging":
		return EnvInteg
	default:
		return EnvProd
	}
}

// ── AWS profiles ─────────────────────────────────────────────────────────────

// GetInfraProfile returns the AWS named profile for infrastructure operations
// (Lambda, DynamoDB, S3, Route53). Returns empty string to use ambient credentials.
//
// Precedence: SPAWN_INFRA_PROFILE env var → "spore-host-infra" default.
// Set SPAWN_INFRA_PROFILE="" (empty) to use the ambient credential chain instead
// of a named profile — required for Isengard/access-key deployments. When the
// spawn default is explicitly cleared this way but the shared config
// (--profile / SPORE_PROFILE / config.toml) DOES name a profile, that shared
// profile is used, so a suite-wide profile still applies to infra ops.
func GetInfraProfile() string {
	if val, ok := os.LookupEnv("SPAWN_INFRA_PROFILE"); ok {
		if val == "" {
			return SharedProfile() // spawn default cleared → fall to shared (may be "")
		}
		return val
	}
	return "spore-host-infra"
}

// GetComputeProfile returns the AWS named profile for compute/EC2 operations:
// the account instances (and a detached sweep's members) launch in. "" means
// the ambient credential chain. See ResolveComputeProfile for the precedence.
func GetComputeProfile() string { return ResolveComputeProfile().Name }

// ComputeProfile is the resolved compute profile and where it came from.
type ComputeProfile struct {
	// Name is the AWS named profile; "" is the ambient credential chain.
	Name string
	// Source says what chose it: "SPAWN_COMPUTE_PROFILE",
	// ComputeProfileConfigSource, or the shared config layer that named the
	// caller's own profile ("--profile", "SPORE_PROFILE", "AWS_PROFILE", the
	// spore config file). "" with Name "" is the ambient chain.
	Source string
	// Override is true when SPAWN_COMPUTE_PROFILE or compute_profile chose the
	// profile, rather than the caller's own selection. Only an override can
	// put compute in another account than the caller's credentials.
	Override bool
}

// ComputeProfileConfigSource is ComputeProfile.Source for the config key.
const ComputeProfileConfigSource = "compute_profile in ~/.spawn/config.yaml"

// Describe is the profile and its source for one line of output, e.g.
// "profile aws, from AWS_PROFILE" or "ambient credentials, no profile selected".
func (p ComputeProfile) Describe() string {
	if p.Name == "" {
		if p.Source != "" {
			return "ambient credentials, from " + p.Source
		}
		return "ambient credentials, no profile selected"
	}
	return fmt.Sprintf("profile %s, from %s", p.Name, p.Source)
}

// ResolveComputeProfile resolves the compute profile (#1538):
//
//  1. SPAWN_COMPUTE_PROFILE, when set and non-empty. Set but empty means
//     "the caller's own" (step 3), skipping the config key.
//  2. compute_profile under defaults: in ~/.spawn/config.yaml
//     (`spawn defaults set compute-profile <name>`).
//  3. The caller's own: the shared profile (--profile > SPORE_PROFILE >
//     AWS_PROFILE > the spore config file), else the ambient chain.
//
// There is no hardcoded default. Until #1538 it was "spore-host-dev", the
// spore.host team's dev account, so anything on the compute path (a detached
// sweep's members, schedules, batch queues, collect-results) ignored
// AWS_PROFILE/--profile and acted in whatever account a local profile of that
// name pointed at. A team with a split setup sets compute_profile once.
func ResolveComputeProfile() ComputeProfile {
	if val, ok := os.LookupEnv("SPAWN_COMPUTE_PROFILE"); ok {
		if val = strings.TrimSpace(val); val != "" {
			return ComputeProfile{Name: val, Source: "SPAWN_COMPUTE_PROFILE", Override: true}
		}
		return callerComputeProfile()
	}
	if p := configComputeProfile(); p != "" {
		return ComputeProfile{Name: p, Source: ComputeProfileConfigSource, Override: true}
	}
	return callerComputeProfile()
}

// callerComputeProfile is the caller's own profile, as every non-compute AWS
// client spawn builds resolves it (spawnaws.NewClientWithRegion).
func callerComputeProfile() ComputeProfile {
	name, source := SharedProfileSource()
	return ComputeProfile{Name: name, Source: source}
}

// configComputeProfile is defaults.compute_profile in ~/.spawn/config.yaml,
// or "" when the file, or the key, is absent or unreadable.
func configComputeProfile() string {
	d, err := LoadLaunchDefaults()
	if err != nil || d == nil {
		return ""
	}
	return strings.TrimSpace(d.ComputeProfile)
}

// ── Service endpoint URLs ─────────────────────────────────────────────────────

// dnsEndpointURLs maps environment → hosted DNS Lambda URL.
var dnsEndpointURLs = map[Environment]string{
	EnvProd:  "https://zqonqra6blwh7342ujuxv3bwei0wnpyq.lambda-url.us-east-1.on.aws/",
	EnvInteg: "https://zqonqra6blwh7342ujuxv3bwei0wnpyq.lambda-url.us-east-1.on.aws/", // update when integ stack exists
}

// DefaultDNSFunctionARN is the Lambda function behind spore.host's DNS updater
// endpoint (dnsEndpointURLs). Every spored role spawn writes grants
// lambda:InvokeFunctionUrl on it (#173); a custom updater's function is named by
// SPAWN_DNS_FUNCTION_ARN / dns.function_arn / SSM /spawn/dns/function_arn
// instead (#1158), because a Function URL's hostname does not contain it. The
// region is a wildcard: the grant predates per-region deployments and the
// function name and account already pin it to the one updater.
const DefaultDNSFunctionARN = "arn:aws:lambda:*:966362334030:function:spawn-dns-updater"

// GetDNSEndpointURL returns the DNS Lambda endpoint URL.
// Precedence: SPORE_DNS_URL → environment-specific default.
func GetDNSEndpointURL() string {
	if v := os.Getenv("SPORE_DNS_URL"); v != "" {
		return v
	}
	return dnsEndpointURLs[GetEnvironment()]
}

// ── Bot / notification endpoints (experimental, self-hosted only) ──────────

// spore.host no longer hosts a chat bot (spore-host/spore-host#611): its Slack
// apps, credentials, Function URL and execution role are gone. So the two
// endpoints a chat feature needs have NO built-in default, and are used only
// behind the experimental opt-in (ExperimentalChatEnabled):
//
//   - SPORE_NOTIFY_URL, the self-hosted spore-bot's base URL (the
//     LambdaFunctionUrl output of deployment/cloudformation/spore-bot.yaml),
//     where spored POSTs lifecycle notifications;
//   - SPORE_BOT_LAMBDA_ROLE_ARN, that bot's execution role, which the
//     SpawnBotCrossAccount role `spawn notify register` creates trusts.
//
// Until #611 both fell back to the hosted bot. A value naming the retired
// hosted bot is refused rather than used, so nothing can call it again.
const (
	EnvNotifyURL        = "SPORE_NOTIFY_URL"
	EnvBotLambdaRoleARN = "SPORE_BOT_LAMBDA_ROLE_ARN"
)

// retiredHostedBotURLMarkers are substrings of the retired hosted bot's
// endpoints: its Lambda Function URL id and its API Gateway API id.
var retiredHostedBotURLMarkers = []string{"awdzf7fbbsvqcrnrzusqjsuybm0iiyvf", "g8iytgzrn8"}

// retiredHostedBotAccount is the spore.host infra account that ran the hosted
// bot. No self-hosted bot runs there, so any bot role in it is the retired one
// (spore-bot-role, or the prism-bot stack's role before #1046).
const retiredHostedBotAccount = "966362334030"

// GetNotifyURL returns SPORE_NOTIFY_URL, or "" when it is unset. There is no
// default; see ResolveNotifyURL.
func GetNotifyURL() string { return strings.TrimSpace(os.Getenv(EnvNotifyURL)) }

// GetBotLambdaRoleARN returns SPORE_BOT_LAMBDA_ROLE_ARN, or "" when it is
// unset. There is no default; see ResolveBotLambdaRoleARN.
func GetBotLambdaRoleARN() string { return strings.TrimSpace(os.Getenv(EnvBotLambdaRoleARN)) }

// ResolveNotifyURL returns the operator's own spore-bot base URL, or an error
// that says what to set. It is refused when unset, when it is not an https
// URL with a host, and when it names the retired hosted bot.
func ResolveNotifyURL() (string, error) {
	v := GetNotifyURL()
	if v == "" {
		return "", fmt.Errorf("%s is not set: spore.host no longer hosts a chat bot, so chat notifications need your own self-hosted spore-bot; "+
			"set %s to its base URL (the LambdaFunctionUrl output of deployment/cloudformation/spore-bot.yaml)", EnvNotifyURL, EnvNotifyURL)
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("%s must be an https:// URL with a host (your self-hosted spore-bot's base URL)", EnvNotifyURL)
	}
	for _, m := range retiredHostedBotURLMarkers {
		if strings.Contains(strings.ToLower(u.Host), m) {
			return "", fmt.Errorf("%s names spore.host's retired hosted bot (%s), which no longer exists; point it at your own self-hosted spore-bot", EnvNotifyURL, u.Host)
		}
	}
	return v, nil
}

// ResolveBotLambdaRoleARN returns the operator's own spore-bot execution role
// ARN, or an error that says what to set. It is refused when unset, when it is
// not an IAM role ARN, and when it names the retired hosted bot's role.
func ResolveBotLambdaRoleARN() (string, error) {
	v := GetBotLambdaRoleARN()
	if v == "" {
		return "", fmt.Errorf("%s is not set: spore.host no longer hosts a chat bot, so the %s role must trust your own self-hosted spore-bot; "+
			"set %s to its execution role ARN (`aws lambda get-function-configuration --function-name <bot> --query Role`), or pass --role-arn for a role you created", EnvBotLambdaRoleARN, "SpawnBotCrossAccount", EnvBotLambdaRoleARN)
	}
	m := roleARNRe.FindStringSubmatch(v)
	if m == nil {
		return "", fmt.Errorf("%s=%q is not an IAM role ARN (arn:aws:iam::<account>:role/<name>)", EnvBotLambdaRoleARN, v)
	}
	if m[1] == retiredHostedBotAccount && strings.Contains(strings.ToLower(m[2]), "bot") {
		return "", fmt.Errorf("%s names spore.host's retired hosted bot role (%s), which no longer exists; name your own self-hosted spore-bot's execution role", EnvBotLambdaRoleARN, v)
	}
	return v, nil
}

// roleARNRe is an IAM role ARN: the account and the role name (with path).
var roleARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:iam::(\d{12}):role/(.+)$`)

// ── Portal BYOA onboarding (phone-home) ──────────────────────────────────────

// portalPhoneHomeRoleARNs maps environment → the portal phone-home Lambda
// execution role ARN. The cross-account onboarding role created by
// `spawn onboard` trusts THIS principal (under a per-account ExternalId).
var portalPhoneHomeRoleARNs = map[Environment]string{
	EnvProd:  "arn:aws:iam::966362334030:role/PortalPhoneHomeLambdaRole",
	EnvInteg: "arn:aws:iam::966362334030:role/PortalPhoneHomeLambdaRole",
}

// GetPortalPhoneHomeRoleARN returns the phone-home Lambda role ARN the onboarding
// role trusts. Precedence: SPORE_PORTAL_PHONE_HOME_ROLE_ARN → env default.
func GetPortalPhoneHomeRoleARN() string {
	if v := os.Getenv("SPORE_PORTAL_PHONE_HOME_ROLE_ARN"); v != "" {
		return v
	}
	return portalPhoneHomeRoleARNs[GetEnvironment()]
}

// portalPhoneHomeURLs maps environment → the deployed phone-home Function URL
// (function_url output of infra/tofu/portal-phone-home).
var portalPhoneHomeURLs = map[Environment]string{
	EnvProd:  "https://omnvm7osdhhnr2f7qm4xghz2xq0txwjl.lambda-url.us-east-1.on.aws/",
	EnvInteg: "https://omnvm7osdhhnr2f7qm4xghz2xq0txwjl.lambda-url.us-east-1.on.aws/",
}

// GetPortalPhoneHomeURL returns the phone-home Function URL the onboarding role
// POSTs its registration to. Precedence: SPORE_PORTAL_PHONE_HOME_URL → env default.
func GetPortalPhoneHomeURL() string {
	if v := os.Getenv("SPORE_PORTAL_PHONE_HOME_URL"); v != "" {
		return v
	}
	return portalPhoneHomeURLs[GetEnvironment()]
}

// GetBotRegistryTable returns the DynamoDB table name for bot registrations.
// Precedence: SPORE_BOT_REGISTRY_TABLE → "spore-bot-registry".
func GetBotRegistryTable() string {
	if v := os.Getenv("SPORE_BOT_REGISTRY_TABLE"); v != "" {
		return v
	}
	return "spore-bot-registry"
}

// GetBotWorkspacesTable returns the DynamoDB table name for bot workspaces.
// Precedence: SPORE_BOT_WORKSPACES_TABLE → "spore-bot-workspaces".
func GetBotWorkspacesTable() string {
	if v := os.Getenv("SPORE_BOT_WORKSPACES_TABLE"); v != "" {
		return v
	}
	return "spore-bot-workspaces"
}

// ── AWS config loaders ────────────────────────────────────────────────────────

// LoadInfraAWSConfig loads an AWS SDK config for infrastructure operations.
// Uses the profile from GetInfraProfile(); if that returns "", ambient
// credentials are used (correct for Isengard/access-key deployments).
func LoadInfraAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	profile := GetInfraProfile()
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load infra AWS config: %w", err)
	}
	// The account guard (#1118) names the account spawn acts in for you. A
	// dedicated infra profile (SPAWN_INFRA_PROFILE, default spore-host-infra)
	// is spawn's control-plane account, a different account by design, so
	// guarding it would refuse every sweep/schedule/pipeline command that pins
	// --account. When infra resolves through the same profile as everything
	// else (SPAWN_INFRA_PROFILE="" or equal to it), it IS the user's account.
	if profile == SharedProfile() {
		if err := CheckAccount(ctx, cfg); err != nil {
			return aws.Config{}, err
		}
	}
	return cfg, nil
}

// LoadComputeAWSConfig loads an AWS SDK config for compute/EC2 operations.
// Uses the profile ResolveComputeProfile resolves; an empty profile uses the
// ambient chain. When an override (SPAWN_COMPUTE_PROFILE or compute_profile)
// names another profile than the caller's own, it prints a notice once if
// the two resolve to different accounts (#1538).
func LoadComputeAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cp := ResolveComputeProfile()
	if cp.Name != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(cp.Name))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load compute AWS config (%s): %w", cp.Describe(), err)
	}
	// Compute is where instances launch: always subject to the account guard.
	if err := CheckAccount(ctx, cfg); err != nil {
		return aws.Config{}, err
	}
	noteComputeOverride(ctx, cfg, cp)
	return cfg, nil
}
