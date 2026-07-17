package config

import (
	"context"
	"fmt"
	"os"

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

// GetComputeProfile returns the AWS named profile for compute/EC2 operations.
// Precedence: SPAWN_COMPUTE_PROFILE env var → "spore-host-dev" default.
// Set SPAWN_COMPUTE_PROFILE="" to use the ambient credential chain (or the
// shared config's profile, if one is set — see GetInfraProfile).
func GetComputeProfile() string {
	if val, ok := os.LookupEnv("SPAWN_COMPUTE_PROFILE"); ok {
		if val == "" {
			return SharedProfile()
		}
		return val
	}
	return "spore-host-dev"
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

// botLambdaRoleARNs maps environment → spore-bot Lambda execution role ARN
// that cross-account IAM roles trust for bot operations.
var botLambdaRoleARNs = map[Environment]string{
	EnvProd:  "arn:aws:iam::966362334030:role/prism-bot-PrismBotFunctionRole-U2vZFZXgWBeM",
	EnvInteg: "arn:aws:iam::966362334030:role/prism-bot-PrismBotFunctionRole-U2vZFZXgWBeM", // update when integ stack exists
}

// GetBotLambdaRoleARN returns the ARN of the spore-bot Lambda execution role.
// Precedence: SPORE_BOT_LAMBDA_ROLE_ARN → environment-specific default.
func GetBotLambdaRoleARN() string {
	if v := os.Getenv("SPORE_BOT_LAMBDA_ROLE_ARN"); v != "" {
		return v
	}
	return botLambdaRoleARNs[GetEnvironment()]
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
	if profile := GetInfraProfile(); profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load infra AWS config: %w", err)
	}
	return cfg, nil
}

// LoadComputeAWSConfig loads an AWS SDK config for compute/EC2 operations.
// Uses the profile from GetComputeProfile(); empty profile uses ambient creds.
func LoadComputeAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile := GetComputeProfile(); profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load compute AWS config: %w", err)
	}
	return cfg, nil
}
