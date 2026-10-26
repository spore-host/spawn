package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/testutil"
)

// The signing secret in spore-bot-workspaces is stored KMS-encrypted, and an
// item written before that (plaintext) still reads (#868). These run against
// Substrate's DynamoDB and KMS, and read the item back RAW, so they see what is
// actually at rest rather than what the client chooses to return.

// Worded rather than a hex blob so secret scanners do not flag it (see
// lambda/dashboard-api/authz_gates_test.go).
const testSigningSecret = "not-a-real-signing-secret-bot-store-fixture"

func newKMSTestClient(t *testing.T) (*Client, *dynamodb.Client) {
	t.Helper()
	env := testutil.SubstrateServer(t)
	db := env.DynamoClient()
	setupBotTables(t, db)
	kc := env.KMSClient()
	key, err := kc.CreateKey(context.Background(), &kms.CreateKeyInput{Description: aws.String("bot secrets")})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	c := NewClientWithTableNames(db, testRegistryTable, testWorkspacesTable).WithSecretsKMS(kc, *key.KeyMetadata.KeyId)
	return c, db
}

func rawSigningSecret(t *testing.T, db *dynamodb.Client, key string) string {
	t.Helper()
	out, err := db.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: aws.String(testWorkspacesTable),
		Key:       map[string]dynamodbtypes.AttributeValue{"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: key}},
	})
	if err != nil || out.Item == nil {
		t.Fatalf("raw GetItem %s: %v", key, err)
	}
	v, _ := out.Item["signing_secret"].(*dynamodbtypes.AttributeValueMemberS)
	if v == nil {
		t.Fatalf("item %s has no signing_secret attribute", key)
	}
	return v.Value
}

func TestPutWorkspace_StoresSigningSecretEncrypted(t *testing.T) {
	c, db := newKMSTestClient(t)
	ctx := context.Background()
	if err := c.PutWorkspace(ctx, &Workspace{WorkspaceKey: "slack#T7", Platform: "slack", BotToken: "xoxb-t", SigningSecret: testSigningSecret}); err != nil {
		t.Fatalf("PutWorkspace: %v", err)
	}
	stored := rawSigningSecret(t, db, "slack#T7")
	if !security.IsEncrypted(stored) || strings.Contains(stored, testSigningSecret) {
		t.Fatalf("signing_secret at rest is %q; want ciphertext", stored)
	}

	// Reading it back decrypts.
	ws, err := c.GetWorkspace(ctx, "slack", "T7")
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if ws.SigningSecret != "" {
		t.Errorf("GetWorkspace filled SigningSecret (%q); the stored form is only reachable through Client.SigningSecret", ws.SigningSecret)
	}
	if !ws.HasSigningSecret() {
		t.Error("HasSigningSecret = false for an item with a secret")
	}
	got, err := c.SigningSecret(ctx, ws)
	if err != nil || got != testSigningSecret {
		t.Fatalf("SigningSecret = %q, %v; want the plaintext", got, err)
	}

	// Same through ListWorkspaces.
	list, err := c.ListWorkspaces(ctx, "slack")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListWorkspaces = %d items, %v", len(list), err)
	}
	if got, err := c.SigningSecret(ctx, &list[0]); err != nil || got != testSigningSecret {
		t.Fatalf("SigningSecret(listed) = %q, %v", got, err)
	}
}

func TestGetWorkspace_LegacyPlaintextSigningSecretStillReads(t *testing.T) {
	c, db := newKMSTestClient(t)
	ctx := context.Background()
	// An item as written before encryption: the plaintext in signing_secret.
	if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(testWorkspacesTable),
		Item: map[string]dynamodbtypes.AttributeValue{
			"workspace_key":  &dynamodbtypes.AttributeValueMemberS{Value: "slack#T8"},
			"platform":       &dynamodbtypes.AttributeValueMemberS{Value: "slack"},
			"bot_token":      &dynamodbtypes.AttributeValueMemberS{Value: "xoxb-l"},
			"signing_secret": &dynamodbtypes.AttributeValueMemberS{Value: testSigningSecret},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ws, err := c.GetWorkspace(ctx, "slack", "T8")
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspace(legacy): %v", err)
	}
	got, err := c.SigningSecret(ctx, ws)
	if err != nil || got != testSigningSecret {
		t.Fatalf("SigningSecret(legacy) = %q, %v; want the stored plaintext", got, err)
	}
	// Reading does not rewrite it; the next write does, sealed.
	if raw := rawSigningSecret(t, db, "slack#T8"); raw != testSigningSecret {
		t.Fatalf("a read changed the stored value to %q", raw)
	}
	ws.SigningSecret = got
	if err := c.PutWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if raw := rawSigningSecret(t, db, "slack#T8"); !security.IsEncrypted(raw) {
		t.Fatalf("after re-write, signing_secret at rest is %q; want ciphertext", raw)
	}
}

func TestPutWorkspace_WithoutKMSRefusesSecret(t *testing.T) {
	c := newTestClient(t) // no WithSecretsKMS
	ctx := context.Background()
	if err := c.PutWorkspace(ctx, &Workspace{WorkspaceKey: "slack#T9", Platform: "slack", SigningSecret: testSigningSecret}); err == nil {
		t.Fatal("PutWorkspace stored a signing secret with no KMS client")
	}
	if ws, _ := c.GetWorkspace(ctx, "slack", "T9"); ws != nil {
		t.Fatalf("a failed PutWorkspace left an item: %+v", ws)
	}
}
