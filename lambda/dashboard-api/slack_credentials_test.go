package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/spore-host/spawn/pkg/botsecret"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/testutil"
)

// The dashboard's Slack install and token refresh store workspace credentials
// sealed (#868). Against Substrate's DynamoDB and KMS, reading items back raw.

const credsTable = "spore-bot-workspaces"

// Worded so secret scanners do not flag them; legacy shapes per field.
const (
	fixtureBotToken     = "xoxb-not-a-real-bot-token-dashboard-fixture"
	fixtureRefreshToken = "xoxe-1-not-a-real-refresh-token-dashboard-fixture"
	fixtureWebhookURL   = "https://hooks.example.invalid/not-a-real-webhook-dashboard-fixture"
	fixtureRotatedBot   = "xoxb-not-a-real-rotated-dashboard-token"
	fixtureRotatedRef   = "xoxe-1-not-a-real-rotated-dashboard-refresh"
)

// countingKMS counts the calls that reach KMS.
type countingKMS struct {
	security.KMSAPI
	decrypts int
}

func (c *countingKMS) Decrypt(ctx context.Context, in *kms.DecryptInput, o ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	c.decrypts++
	return c.KMSAPI.Decrypt(ctx, in, o...)
}

type credsEnv struct {
	cfg aws.Config
	db  *dynamodb.Client
	kms *countingKMS
}

func newCredsEnv(t *testing.T) credsEnv {
	t.Helper()
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	if _, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(credsTable),
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
	return credsEnv{cfg: env.AWSConfig, db: db, kms: &countingKMS{KMSAPI: kc}}
}

func (e credsEnv) raw(t *testing.T, key string) map[string]dynamodbtypes.AttributeValue {
	t.Helper()
	out, err := e.db.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: aws.String(credsTable),
		Key:       map[string]dynamodbtypes.AttributeValue{"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: key}},
	})
	if err != nil || out.Item == nil {
		t.Fatalf("raw GetItem %s: %v", key, err)
	}
	return out.Item
}

func (e credsEnv) assertSealed(t *testing.T, key string, want map[botsecret.Field]string) {
	t.Helper()
	item := e.raw(t, key)
	for f, plain := range want {
		stored := f.Stored(item)
		if !security.IsEncrypted(stored) || strings.Contains(stored, plain) {
			t.Errorf("%s at rest in %s is %q; want ciphertext", f, key, stored)
			continue
		}
		if got, err := f.Open(context.Background(), e.kms, key, stored); err != nil || got != plain {
			t.Errorf("%s at rest opens to %q, %v; want the plaintext", f, got, err)
		}
	}
}

// putLegacy writes a pre-#868 rotating workspace: plaintext tokens, expired.
func (e credsEnv) putLegacy(t *testing.T, key string) {
	t.Helper()
	if _, err := e.db.PutItem(context.Background(), &dynamodb.PutItemInput{TableName: aws.String(credsTable), Item: map[string]dynamodbtypes.AttributeValue{
		"workspace_key":    &dynamodbtypes.AttributeValueMemberS{Value: key},
		"platform":         &dynamodbtypes.AttributeValueMemberS{Value: "slack"},
		"token_rotation":   &dynamodbtypes.AttributeValueMemberBOOL{Value: true},
		"token_expires_at": &dynamodbtypes.AttributeValueMemberN{Value: "1"},
		"bot_token":        &dynamodbtypes.AttributeValueMemberS{Value: fixtureBotToken},
		"refresh_token":    &dynamodbtypes.AttributeValueMemberS{Value: fixtureRefreshToken},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreSlackWorkspace_SealsEveryCredential(t *testing.T) {
	e := newCredsEnv(t)
	withSigningSecret(t, testSigningSecret)
	tok := &slackOAuthTokenResponse{AppID: "A1", BotToken: fixtureBotToken, RefreshToken: fixtureRefreshToken, ExpiresIn: 3600}
	tok.Team.ID = "T1"
	tok.IncomingWebhook.URL = fixtureWebhookURL
	if err := storeSlackWorkspace(context.Background(), e.cfg, credsTable, tok); err != nil {
		t.Fatalf("storeSlackWorkspace: %v", err)
	}
	e.assertSealed(t, "slack#T1#A1", map[botsecret.Field]string{
		botsecret.SigningSecret:      testSigningSecret,
		botsecret.BotToken:           fixtureBotToken,
		botsecret.RefreshToken:       fixtureRefreshToken,
		botsecret.IncomingWebhookURL: fixtureWebhookURL,
	})
}

func TestRotateTokenWith_MatchesLegacyWithoutDecryptAndWritesBackSealed(t *testing.T) {
	e := newCredsEnv(t)
	ctx := context.Background()
	e.putLegacy(t, "slack#T2")
	var exchanged []string
	exchange := func(refresh string) (string, string, int, error) {
		exchanged = append(exchanged, refresh)
		return fixtureRotatedBot, fixtureRotatedRef, 3600, nil
	}
	got, err := rotateTokenWith(ctx, e.db, e.kms, credsTable, fixtureBotToken, exchange)
	if err != nil || got != fixtureRotatedBot {
		t.Fatalf("rotateTokenWith = %q, %v", got, err)
	}
	if e.kms.decrypts != 0 {
		t.Fatalf("matching a legacy plaintext token made %d Decrypt calls, want 0", e.kms.decrypts)
	}
	if len(exchanged) != 1 || exchanged[0] != fixtureRefreshToken {
		t.Fatalf("exchange called with %v", exchanged)
	}
	e.assertSealed(t, "slack#T2", map[botsecret.Field]string{botsecret.BotToken: fixtureRotatedBot, botsecret.RefreshToken: fixtureRotatedRef})

	// The rotated, sealed token is still found by its plaintext.
	if got, err := rotateTokenWith(ctx, e.db, e.kms, credsTable, fixtureRotatedBot, exchange); err != nil || got != fixtureRotatedBot {
		t.Fatalf("rotate by a sealed token = %q, %v", got, err)
	}
	// An unknown token matches nothing.
	if _, err := rotateTokenWith(ctx, e.db, e.kms, credsTable, "xoxb-not-a-stored-token", exchange); err == nil {
		t.Fatal("an unknown token was rotated")
	}
}

func TestFreshBotTokenWith_RefreshWritesBackSealed(t *testing.T) {
	e := newCredsEnv(t)
	ctx := context.Background()
	e.putLegacy(t, "slack#T3")
	exchange := func(refresh string) (string, string, int, error) {
		if refresh != fixtureRefreshToken {
			t.Errorf("exchange got %q", refresh)
		}
		return fixtureRotatedBot, fixtureRotatedRef, 3600, nil
	}
	got, err := freshBotTokenWith(ctx, e.db, e.kms, credsTable, "slack#T3", time.Now(), exchange)
	if err != nil || got != fixtureRotatedBot {
		t.Fatalf("freshBotTokenWith = %q, %v", got, err)
	}
	e.assertSealed(t, "slack#T3", map[botsecret.Field]string{botsecret.BotToken: fixtureRotatedBot, botsecret.RefreshToken: fixtureRotatedRef})
	// A sealed, unexpired token is opened and returned as is.
	if got, err := freshBotTokenWith(ctx, e.db, e.kms, credsTable, "slack#T3", time.Now(), exchange); err != nil || got != fixtureRotatedBot {
		t.Fatalf("second freshBotTokenWith = %q, %v", got, err)
	}
}
