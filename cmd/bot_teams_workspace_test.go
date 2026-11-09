package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/spf13/pflag"
	"github.com/spore-host/spawn/pkg/bot"
	"github.com/spore-host/spawn/pkg/botsecret"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/testutil"
)

// `spawn notify workspace add --platform teams` takes no --signing-secret:
// spore-bot verifies Teams requests with Bot Framework tokens only, and the
// value once documented for the flag (the Azure Bot App ID) is public.

// An App ID's shape; not a credential.
const teamsAppIDFixture = "00000000-0000-0000-0000-000000000000"

// runWorkspaceAdd parses args into `notify workspace add` and runs its RunE,
// then resets every flag it may have set. It runs the command directly rather
// than through rootCmd, so it does not depend on how `notify` is gated.
func runWorkspaceAdd(t *testing.T, args ...string) error {
	t.Helper()
	t.Cleanup(func() {
		botPlatform, botWorkspaceID, botWorkspaceName = "", "", ""
		botBotToken, botSigningSecret, botPublicKey, botWebhookURL = "", "", "", ""
		botWorkspacesTable, botAllowedChannels, botConnectTTLHours = "", nil, 0
		botJSONOutput = false
		botWorkspaceAddCmd.Flags().VisitAll(func(fl *pflag.Flag) { fl.Changed = false })
	})
	if err := botWorkspaceAddCmd.ParseFlags(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	var err error
	_ = captureStdout(t, func() { err = botWorkspaceAddCmd.RunE(botWorkspaceAddCmd, nil) })
	return err
}

// forbidAWS makes any AWS config load fail the test, so a refusal is proven to
// happen before spawn talks to AWS.
func forbidAWS(t *testing.T) {
	t.Helper()
	real := loadAWSConfig
	loadAWSConfig = func(context.Context, string) (aws.Config, error) {
		t.Errorf("the command loaded an AWS config; it must refuse before any AWS call")
		return aws.Config{}, errors.New("no AWS in this test")
	}
	t.Cleanup(func() { loadAWSConfig = real })
}

func TestWorkspaceAdd_TeamsRefusesSigningSecretBeforeAWS(t *testing.T) {
	for name, args := range map[string][]string{
		"app id":       {"--platform", "teams", "--workspace-id", "tenant-1", "--bot-token", "not-a-real-azure-secret", "--signing-secret", teamsAppIDFixture},
		"empty value":  {"--platform", "teams", "--workspace-id", "tenant-1", "--signing-secret="},
		"mixed case":   {"--platform", "Teams", "--workspace-id", "tenant-1", "--signing-secret", "x"},
		"no bot token": {"--platform", "teams", "--workspace-id", "tenant-1", "--signing-secret", teamsAppIDFixture},
	} {
		t.Run(name, func(t *testing.T) {
			forbidAWS(t)
			err := runWorkspaceAdd(t, args...)
			if !errors.Is(err, bot.ErrTeamsSigningSecret) {
				t.Fatalf("workspace add %v = %v; want bot.ErrTeamsSigningSecret", args, err)
			}
			if strings.Contains(err.Error(), teamsAppIDFixture) {
				t.Errorf("the error echoes the value given: %v", err)
			}
		})
	}
}

func TestWorkspaceAdd_SlackStillRequiresSigningSecret(t *testing.T) {
	forbidAWS(t)
	err := runWorkspaceAdd(t, "--platform", "slack", "--workspace-id", "T1", "--bot-token", "xoxb-not-a-real-token")
	if err == nil || !strings.Contains(err.Error(), "--signing-secret is required") {
		t.Fatalf("slack workspace add without --signing-secret = %v; want it required", err)
	}
}

func TestWorkspaceAdd_DiscordStillRequiresPublicKey(t *testing.T) {
	forbidAWS(t)
	err := runWorkspaceAdd(t, "--platform", "discord", "--workspace-id", "G1")
	if err == nil || !strings.Contains(err.Error(), "--public-key is required") {
		t.Fatalf("discord workspace add without --public-key = %v; want it required", err)
	}
}

// teamsWorkspaceEnv points the command at Substrate's STS, DynamoDB and KMS,
// with a fresh workspaces table and credentials key.
func teamsWorkspaceEnv(t *testing.T) (*bot.Client, *dynamodb.Client) {
	t.Helper()
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	if _, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(botCredsTable),
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{{AttributeName: aws.String("workspace_key"), AttributeType: dynamodbtypes.ScalarAttributeTypeS}},
		KeySchema:            []dynamodbtypes.KeySchemaElement{{AttributeName: aws.String("workspace_key"), KeyType: dynamodbtypes.KeyTypeHash}},
		BillingMode:          dynamodbtypes.BillingModePayPerRequest,
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	kc := kms.NewFromConfig(env.AWSConfig)
	key, err := kc.CreateKey(ctx, &kms.CreateKeyInput{Description: aws.String("bot secrets")})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	t.Setenv(botsecret.EnvKeyID, *key.KeyMetadata.KeyId)
	real := loadAWSConfig
	loadAWSConfig = func(context.Context, string) (aws.Config, error) { return env.AWSConfig, nil }
	t.Cleanup(func() { loadAWSConfig = real })
	return bot.NewClientWithTableNames(db, "unused-registry", botCredsTable).WithSecretsKMS(kc, *key.KeyMetadata.KeyId), db
}

func TestWorkspaceAdd_TeamsWithoutSigningSecretSucceeds(t *testing.T) {
	c, db := teamsWorkspaceEnv(t)
	const azureSecret = "not-a-real-azure-bot-client-secret"
	if err := runWorkspaceAdd(t, "--platform", "teams", "--workspace-id", "tenant-2",
		"--bot-token", azureSecret, "--table", botCredsTable); err != nil {
		t.Fatalf("teams workspace add without --signing-secret = %v", err)
	}
	item := rawBotItem(t, db, "teams#tenant-2")
	if _, ok := item["signing_secret"]; ok {
		t.Errorf("the Teams item has a signing_secret attribute")
	}
	if stored := botsecret.BotToken.Stored(item); !security.IsEncrypted(stored) || strings.Contains(stored, azureSecret) {
		t.Errorf("bot token stored as %q; want ciphertext", stored)
	}
	ws, err := c.GetWorkspace(context.Background(), "teams", "tenant-2")
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspace = %v, %v", ws, err)
	}
	if got, err := c.Credential(context.Background(), ws, botsecret.BotToken); err != nil || got != azureSecret {
		t.Errorf("bot token = %q, %v", got, err)
	}
	if len(teamsSigningSecretWarnings([]bot.Workspace{*ws})) != 0 {
		t.Errorf("a clean Teams workspace drew a signing-secret warning")
	}
}

// `notify workspace list` warns about a Teams item that still stores a
// signing secret, sealed or legacy plaintext, by name and never by value.
func TestTeamsSigningSecretWarnings(t *testing.T) {
	c, db := teamsWorkspaceEnv(t)
	ctx := context.Background()
	put := func(key, platform, secret string) {
		item := map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: key},
			"platform":      &dynamodbtypes.AttributeValueMemberS{Value: platform},
		}
		if secret != "" {
			item["signing_secret"] = &dynamodbtypes.AttributeValueMemberS{Value: secret}
		}
		if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(botCredsTable), Item: item}); err != nil {
			t.Fatal(err)
		}
	}
	put("teams#legacy", "teams", teamsAppIDFixture) // what the old guide stored
	put("teams#clean", "teams", "")
	put("slack#TSLACK", "slack", cmdTestCredentials[botsecret.SigningSecret])

	wss, err := c.ListWorkspaces(ctx, "")
	if err != nil || len(wss) != 3 {
		t.Fatalf("ListWorkspaces = %d, %v", len(wss), err)
	}
	warnings := teamsSigningSecretWarnings(wss)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q; want one, for teams#legacy", warnings)
	}
	w := warnings[0]
	if !strings.Contains(w, "teams#legacy") || !strings.Contains(w, teamsSetupGuideURL) {
		t.Errorf("warning %q does not name the workspace and the guide", w)
	}
	for _, value := range []string{teamsAppIDFixture, cmdTestCredentials[botsecret.SigningSecret]} {
		if strings.Contains(w, value) {
			t.Errorf("warning prints a stored value: %q", w)
		}
	}
}
