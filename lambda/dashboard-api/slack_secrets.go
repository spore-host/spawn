package main

// Slack app secrets accessor.
//
// SECURITY: the Slack client_secret and signing_secret used to live as PLAINTEXT
// Lambda environment variables (readable by anyone with lambda:GetFunctionConfig,
// and surfaced in the console/CLI). They now live in a single Secrets Manager
// secret whose ARN is passed via SLACK_SECRETS_ARN; the exec role gets
// secretsmanager:GetSecretValue on just that ARN. The value is a small JSON blob:
//
//	{"client_secret": "...", "signing_secret": "..."}
//
// The secret is fetched once and cached for the life of the (warm) Lambda — these
// are app-level, effectively static credentials, so a per-cold-start fetch is
// enough and avoids a Secrets Manager call on every request.
//
// Backward-compatible: if SLACK_SECRETS_ARN is unset, or a field is absent from
// the secret JSON, we fall back to the legacy SLACK_CLIENT_SECRET /
// SLACK_SIGNING_SECRET env vars. This lets the code deploy before the secret is
// wired, and the env vars be removed after — no flag-day.

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type slackSecrets struct {
	ClientSecret  string `json:"client_secret"`
	SigningSecret string `json:"signing_secret"`
}

var (
	slackSecretsOnce   sync.Once
	slackSecretsCached slackSecrets
)

// loadSlackSecrets resolves the Slack secrets once (cached). It reads the
// Secrets Manager secret named by SLACK_SECRETS_ARN when set, then layers the
// legacy env vars as a fallback for any field the secret didn't provide.
func loadSlackSecrets() slackSecrets {
	slackSecretsOnce.Do(func() {
		// Legacy env fallback first — always available, overridden by the secret.
		slackSecretsCached = slackSecrets{
			ClientSecret:  os.Getenv("SLACK_CLIENT_SECRET"),
			SigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		}

		arn := os.Getenv("SLACK_SECRETS_ARN")
		if arn == "" {
			return // no secret configured — use env fallback
		}

		ctx := context.Background()
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			log.Printf("slack secrets: load AWS config: %v (using env fallback)", err)
			return
		}
		out, err := secretsmanager.NewFromConfig(cfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
			SecretId: aws.String(arn),
		})
		if err != nil || out.SecretString == nil {
			log.Printf("slack secrets: GetSecretValue: %v (using env fallback)", err)
			return
		}
		merged, err := mergeSlackSecrets(*out.SecretString, slackSecretsCached)
		if err != nil {
			log.Printf("slack secrets: parse secret JSON: %v (using env fallback)", err)
			return
		}
		slackSecretsCached = merged
	})
	return slackSecretsCached
}

// mergeSlackSecrets parses a Secrets Manager JSON blob and layers it over the
// env fallback: a field present (non-empty) in the secret wins; anything absent
// keeps the fallback value. Pure — unit-tested without AWS.
func mergeSlackSecrets(secretJSON string, fallback slackSecrets) (slackSecrets, error) {
	var fromSecret slackSecrets
	if err := json.Unmarshal([]byte(secretJSON), &fromSecret); err != nil {
		return fallback, err
	}
	out := fallback
	if fromSecret.ClientSecret != "" {
		out.ClientSecret = fromSecret.ClientSecret
	}
	if fromSecret.SigningSecret != "" {
		out.SigningSecret = fromSecret.SigningSecret
	}
	return out, nil
}

// slackClientSecret returns the Slack OAuth client secret (Secrets Manager or env).
func slackClientSecret() string { return loadSlackSecrets().ClientSecret }

// slackSigningSecret returns the Slack request-signing secret (Secrets Manager or env).
func slackSigningSecret() string { return loadSlackSecrets().SigningSecret }
