package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/spore-host/spawn/pkg/botsecret"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/testutil"
)

// Every credential in spore-bot-workspaces (botsecret.Fields) is stored
// KMS-encrypted, and an item written before that (plaintext) still reads
// (#868). These run against Substrate's DynamoDB and KMS, and read the item
// back RAW, so they see what is actually at rest rather than what the client
// chooses to return.

// Worded rather than real-looking, so secret scanners do not flag them (see
// lambda/dashboard-api/authz_gates_test.go). Each has its field's legacy shape:
// the signing secret is built, tokens carry '-', the webhook URL "://".
var testCredentials = map[botsecret.Field]string{
	botsecret.SigningSecret:      strings.Repeat("0123456789abcdef", 2), // a Slack signing secret's shape: 32 hex, valid base64
	botsecret.BotToken:           "xoxb-not-a-real-bot-token-fixture",
	botsecret.RefreshToken:       "xoxe-1-not-a-real-refresh-token-fixture",
	botsecret.IncomingWebhookURL: "https://hooks.example.invalid/not-a-real-webhook-fixture",
}

// countingKMS counts the calls that reach KMS.
type countingKMS struct {
	security.KMSAPI
	encrypts, decrypts int
}

func (c *countingKMS) Encrypt(ctx context.Context, in *kms.EncryptInput, o ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	c.encrypts++
	return c.KMSAPI.Encrypt(ctx, in, o...)
}

func (c *countingKMS) Decrypt(ctx context.Context, in *kms.DecryptInput, o ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	c.decrypts++
	return c.KMSAPI.Decrypt(ctx, in, o...)
}

func newKMSTestClient(t *testing.T) (*Client, *dynamodb.Client) {
	c, db, _ := newCountingKMSTestClient(t)
	return c, db
}

func newCountingKMSTestClient(t *testing.T) (*Client, *dynamodb.Client, *countingKMS) {
	t.Helper()
	env := testutil.SubstrateServer(t)
	db := env.DynamoClient()
	setupBotTables(t, db)
	kc := env.KMSClient()
	key, err := kc.CreateKey(context.Background(), &kms.CreateKeyInput{Description: aws.String("bot secrets")})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	counter := &countingKMS{KMSAPI: kc}
	c := NewClientWithTableNames(db, testRegistryTable, testWorkspacesTable).WithSecretsKMS(counter, *key.KeyMetadata.KeyId)
	return c, db, counter
}

func rawItem(t *testing.T, db *dynamodb.Client, key string) map[string]dynamodbtypes.AttributeValue {
	t.Helper()
	out, err := db.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: aws.String(testWorkspacesTable),
		Key:       map[string]dynamodbtypes.AttributeValue{"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: key}},
	})
	if err != nil || out.Item == nil {
		t.Fatalf("raw GetItem %s: %v", key, err)
	}
	return out.Item
}

// assertSealedAtRest fails unless every field in fields is ciphertext in the
// raw item, with no trace of its plaintext.
func assertSealedAtRest(t *testing.T, db *dynamodb.Client, key string, fields map[botsecret.Field]string) {
	t.Helper()
	item := rawItem(t, db, key)
	for f, plain := range fields {
		stored := f.Stored(item)
		if !security.IsEncrypted(stored) || strings.Contains(stored, plain) {
			t.Errorf("%s at rest in %s is %q; want ciphertext", f, key, stored)
		}
	}
	for name, v := range item {
		s, ok := v.(*dynamodbtypes.AttributeValueMemberS)
		if !ok {
			continue
		}
		for f, plain := range fields {
			if strings.Contains(s.Value, plain) {
				t.Errorf("attribute %s of %s holds the %s plaintext", name, key, f)
			}
		}
	}
}

func fullWorkspace(key string) *Workspace {
	return &Workspace{
		WorkspaceKey: key, Platform: "slack",
		SigningSecret:      testCredentials[botsecret.SigningSecret],
		BotToken:           testCredentials[botsecret.BotToken],
		RefreshToken:       testCredentials[botsecret.RefreshToken],
		IncomingWebhookURL: testCredentials[botsecret.IncomingWebhookURL],
		TokenRotation:      true,
	}
}

func TestPutWorkspace_StoresEveryCredentialEncrypted(t *testing.T) {
	c, db := newKMSTestClient(t)
	ctx := context.Background()
	if err := c.PutWorkspace(ctx, fullWorkspace("slack#T7")); err != nil {
		t.Fatalf("PutWorkspace: %v", err)
	}
	assertSealedAtRest(t, db, "slack#T7", testCredentials)

	// Reading back decrypts, through Get and List alike; the struct fields
	// stay empty.
	ws, err := c.GetWorkspace(ctx, "slack", "T7")
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	list, err := c.ListWorkspaces(ctx, "slack")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListWorkspaces = %d items, %v", len(list), err)
	}
	for _, read := range []*Workspace{ws, &list[0]} {
		for f, want := range testCredentials {
			if v := *read.Input(f); v != "" {
				t.Errorf("a read filled %s (%q); the stored form is only reachable through Client.Credential", f, v)
			}
			if !read.HasCredential(f) {
				t.Errorf("HasCredential(%s) = false for an item with one", f)
			}
			if got, err := c.Credential(ctx, read, f); err != nil || got != want {
				t.Errorf("Credential(%s) = %q, %v; want the plaintext", f, got, err)
			}
		}
	}
}

func TestGetWorkspace_LegacyPlaintextCredentialsStillRead(t *testing.T) {
	c, db, counter := newCountingKMSTestClient(t)
	ctx := context.Background()
	// An item as written before encryption: every credential in plaintext.
	item := map[string]dynamodbtypes.AttributeValue{
		"workspace_key":        &dynamodbtypes.AttributeValueMemberS{Value: "slack#T8"},
		"platform":             &dynamodbtypes.AttributeValueMemberS{Value: "slack"},
		"token_rotation":       &dynamodbtypes.AttributeValueMemberBOOL{Value: true},
		"signing_secret":       &dynamodbtypes.AttributeValueMemberS{Value: testCredentials[botsecret.SigningSecret]},
		"bot_token":            &dynamodbtypes.AttributeValueMemberS{Value: testCredentials[botsecret.BotToken]},
		"refresh_token":        &dynamodbtypes.AttributeValueMemberS{Value: testCredentials[botsecret.RefreshToken]},
		"incoming_webhook_url": &dynamodbtypes.AttributeValueMemberS{Value: testCredentials[botsecret.IncomingWebhookURL]},
	}
	if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(testWorkspacesTable), Item: item}); err != nil {
		t.Fatal(err)
	}
	if got := rawItem(t, db, "slack#T8"); botsecret.BotToken.Stored(got) != testCredentials[botsecret.BotToken] {
		t.Fatalf("legacy seed is not plaintext: %v", got)
	}

	ws, err := c.GetWorkspace(ctx, "slack", "T8")
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspace(legacy): %v", err)
	}
	for f, want := range testCredentials {
		got, err := c.Credential(ctx, ws, f)
		if err != nil || got != want {
			t.Fatalf("Credential(legacy %s) = %q, %v; want the stored plaintext", f, got, err)
		}
		*ws.Input(f) = got
	}
	if counter.decrypts != 0 || counter.encrypts != 0 {
		t.Fatalf("reading legacy plaintext made %d Decrypt and %d Encrypt calls, want 0", counter.decrypts, counter.encrypts)
	}
	// Reading does not rewrite it; the next write does, sealed.
	if raw := rawItem(t, db, "slack#T8"); botsecret.SigningSecret.Stored(raw) != testCredentials[botsecret.SigningSecret] {
		t.Fatalf("a read changed the stored item: %v", raw)
	}
	if err := c.PutWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	assertSealedAtRest(t, db, "slack#T8", testCredentials)
}

func TestUpdateWorkspaceTokens_WritesTheRotatedPairSealed(t *testing.T) {
	c, db := newKMSTestClient(t)
	ctx := context.Background()
	// A legacy rotating workspace: plaintext tokens.
	if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(testWorkspacesTable),
		Item: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: "slack#T10"},
			"platform":      &dynamodbtypes.AttributeValueMemberS{Value: "slack"},
			"bot_token":     &dynamodbtypes.AttributeValueMemberS{Value: testCredentials[botsecret.BotToken]},
			"refresh_token": &dynamodbtypes.AttributeValueMemberS{Value: testCredentials[botsecret.RefreshToken]},
		},
	}); err != nil {
		t.Fatal(err)
	}
	newBot, newRefresh := "xoxb-not-a-real-rotated-token", "xoxe-1-not-a-real-rotated-refresh"
	if err := c.UpdateWorkspaceTokens(ctx, "slack", "T10", newBot, newRefresh, 4242); err != nil {
		t.Fatalf("UpdateWorkspaceTokens: %v", err)
	}
	assertSealedAtRest(t, db, "slack#T10", map[botsecret.Field]string{botsecret.BotToken: newBot, botsecret.RefreshToken: newRefresh})
	ws, _ := c.GetWorkspace(ctx, "slack", "T10")
	if ws == nil || ws.TokenExpiresAt != 4242 {
		t.Fatalf("expiry not stored: %+v", ws)
	}
	for f, want := range map[botsecret.Field]string{botsecret.BotToken: newBot, botsecret.RefreshToken: newRefresh} {
		if got, err := c.Credential(ctx, ws, f); err != nil || got != want {
			t.Fatalf("Credential(%s) after rotation = %q, %v", f, got, err)
		}
	}
	// Without KMS nothing is written.
	if err := newTestClient(t).UpdateWorkspaceTokens(ctx, "slack", "T10", newBot, newRefresh, 1); err == nil {
		t.Fatal("UpdateWorkspaceTokens wrote tokens with no KMS client")
	}
}

func TestCredential_WrongWorkspaceKeyFailsToOpen(t *testing.T) {
	c, db := newKMSTestClient(t)
	ctx := context.Background()
	if err := c.PutWorkspace(ctx, fullWorkspace("slack#T11")); err != nil {
		t.Fatal(err)
	}
	// Copy the sealed item, ciphertexts and all, under another key.
	item := rawItem(t, db, "slack#T11")
	item["workspace_key"] = &dynamodbtypes.AttributeValueMemberS{Value: "slack#T12"}
	if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(testWorkspacesTable), Item: item}); err != nil {
		t.Fatal(err)
	}
	ws, err := c.GetWorkspace(ctx, "slack", "T12")
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	for f := range testCredentials {
		if got, err := c.Credential(ctx, ws, f); err == nil {
			t.Errorf("%s sealed for slack#T11 opened under slack#T12 as %q", f, got)
		}
	}
}

func TestPutWorkspace_WithoutKMSRefusesEveryCredential(t *testing.T) {
	c := newTestClient(t) // no WithSecretsKMS
	ctx := context.Background()
	for f, plain := range testCredentials {
		ws := &Workspace{WorkspaceKey: "slack#T9", Platform: "slack"}
		*ws.Input(f) = plain
		if err := c.PutWorkspace(ctx, ws); err == nil {
			t.Fatalf("PutWorkspace stored a %s with no KMS client", f)
		}
		if got, _ := c.GetWorkspace(ctx, "slack", "T9"); got != nil {
			t.Fatalf("a failed PutWorkspace left an item: %+v", got)
		}
	}
}
