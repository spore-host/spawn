package config

import (
	"context"
	"fmt"
	"os"
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

// notifyURLs maps environment → hosted notification Lambda URL.
var notifyURLs = map[Environment]string{
	EnvProd:  "https://awdzf7fbbsvqcrnrzusqjsuybm0iiyvf.lambda-url.us-east-1.on.aws",
	EnvInteg: "https://awdzf7fbbsvqcrnrzusqjsuybm0iiyvf.lambda-url.us-east-1.on.aws", // update when integ stack exists
}

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

// GetNotifyURL returns the notification Lambda callback URL.
// Precedence: SPORE_NOTIFY_URL → environment-specific default.
func GetNotifyURL() string {
	if v := os.Getenv("SPORE_NOTIFY_URL"); v != "" {
		return v
	}
	return notifyURLs[GetEnvironment()]
}

// GetDNSEndpointURL returns the DNS Lambda endpoint URL.
// Precedence: SPORE_DNS_URL → environment-specific default.
func GetDNSEndpointURL() string {
	if v := os.Getenv("SPORE_DNS_URL"); v != "" {
		return v
	}
	return dnsEndpointURLs[GetEnvironment()]
}

// ── Bot / notification registration ──────────────────────────────────────────

// SporeBotRoleName is the execution role of the hosted spore-bot Lambda. It is
// a fixed name, created by spore-host's infra/tofu/spore-bot
// (aws_iam_role.spore_bot), not one CloudFormation generated with a random
// suffix, so recreating a stack cannot change it (#476, #1046).
const SporeBotRoleName = "spore-bot-role"

// DefaultBotLambdaRoleARN is the principal that SpawnBotCrossAccount roles
// trust: the hosted spore-bot Lambda's execution role. It is the single source
// for that ARN; deployment/cloudformation/bot-cross-account-role.yaml's
// BotLambdaRoleArn default must equal it, and bot_principal_test.go fails the
// build when any file in the repo names a different bot principal.
//
// It was the prism-bot stack's auto-generated function role until #1046, which
// spore-bot no longer runs as.
const DefaultBotLambdaRoleARN = "arn:aws:iam::966362334030:role/" + SporeBotRoleName

// botLambdaRoleARNs maps environment → spore-bot Lambda execution role ARN
// that cross-account IAM roles trust for bot operations.
var botLambdaRoleARNs = map[Environment]string{
	EnvProd:  DefaultBotLambdaRoleARN,
	EnvInteg: DefaultBotLambdaRoleARN, // update when an integ stack exists
}

// GetBotLambdaRoleARN returns the ARN of the spore-bot Lambda execution role.
// Precedence: SPORE_BOT_LAMBDA_ROLE_ARN → environment-specific default.
func GetBotLambdaRoleARN() string {
	if v := os.Getenv("SPORE_BOT_LAMBDA_ROLE_ARN"); v != "" {
		return v
	}
	return botLambdaRoleARNs[GetEnvironment()]
}

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
