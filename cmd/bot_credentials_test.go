package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/spore-host/spawn/pkg/bot"
	"github.com/spore-host/spawn/pkg/botsecret"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/testutil"
)

// `spawn notify workspace` keeps workspace credentials sealed at rest and out
// of its output (#868). Against Substrate's DynamoDB and KMS.

const botCredsTable = "spore-bot-workspaces-test"

// Worded so secret scanners do not flag them; legacy shapes per field.
var cmdTestCredentials = map[botsecret.Field]string{
	botsecret.SigningSecret:      "not-a-real-signing-secret-cmd-fixture",
	botsecret.BotToken:           "xoxb-not-a-real-bot-token-cmd-fixture",
	botsecret.RefreshToken:       "xoxe-1-not-a-real-refresh-token-cmd-fixture",
	botsecret.IncomingWebhookURL: "https://hooks.example.invalid/not-a-real-webhook-cmd-fixture",
}

func newBotCredsClient(t *testing.T) (*bot.Client, *dynamodb.Client) {
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
	return bot.NewClientWithTableNames(db, "unused-registry", botCredsTable).WithSecretsKMS(kc, *key.KeyMetadata.KeyId), db
}

func rawBotItem(t *testing.T, db *dynamodb.Client, key string) map[string]dynamodbtypes.AttributeValue {
	t.Helper()
	out, err := db.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: aws.String(botCredsTable),
		Key:       map[string]dynamodbtypes.AttributeValue{"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: key}},
	})
	if err != nil || out.Item == nil {
		t.Fatalf("raw GetItem %s: %v", key, err)
	}
	return out.Item
}

// TestWorkspaceListJSON_RedactsEveryCredential: `notify workspace list --json`
// prints "(redacted)" for every credential field a workspace holds, sealed or
// legacy plaintext, and neither the plaintext nor the ciphertext.
func TestWorkspaceListJSON_RedactsEveryCredential(t *testing.T) {
	c, db := newBotCredsClient(t)
	ctx := context.Background()
	sealed := &bot.Workspace{WorkspaceKey: "slack#TS", Platform: "slack"}
	for f, v := range cmdTestCredentials {
		*sealed.Input(f) = v
	}
	if err := c.PutWorkspace(ctx, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(botCredsTable), Item: map[string]dynamodbtypes.AttributeValue{
		"workspace_key":        &dynamodbtypes.AttributeValueMemberS{Value: "slack#TL"},
		"platform":             &dynamodbtypes.AttributeValueMemberS{Value: "slack"},
		"signing_secret":       &dynamodbtypes.AttributeValueMemberS{Value: cmdTestCredentials[botsecret.SigningSecret]},
		"bot_token":            &dynamodbtypes.AttributeValueMemberS{Value: cmdTestCredentials[botsecret.BotToken]},
		"refresh_token":        &dynamodbtypes.AttributeValueMemberS{Value: cmdTestCredentials[botsecret.RefreshToken]},
		"incoming_webhook_url": &dynamodbtypes.AttributeValueMemberS{Value: cmdTestCredentials[botsecret.IncomingWebhookURL]},
	}}); err != nil {
		t.Fatal(err)
	}
	var ciphertexts []string
	for _, f := range botsecret.Fields() {
		ciphertexts = append(ciphertexts, f.Stored(rawBotItem(t, db, "slack#TS")))
	}

	wss, err := c.ListWorkspaces(ctx, "slack")
	if err != nil || len(wss) != 2 {
		t.Fatalf("ListWorkspaces = %d, %v", len(wss), err)
	}
	var buf bytes.Buffer
	if err := writeWorkspacesJSON(&buf, wss); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for f, plain := range cmdTestCredentials {
		if strings.Contains(out, plain) {
			t.Errorf("list --json prints the %s plaintext", f)
		}
	}
	for _, ct := range ciphertexts {
		if ct == "" || strings.Contains(out, ct) {
			t.Errorf("list --json prints a ciphertext (or the seed has none): %q", ct)
		}
	}
	var decoded []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, ws := range decoded {
		for _, name := range []string{"signing_secret", "bot_token", "refresh_token", "incoming_webhook_url"} {
			if ws[name] != redacted {
				t.Errorf("%v: %s = %v, want %q", ws["workspace_key"], name, ws[name], redacted)
			}
		}
	}
}

// TestFreshBotToken_RefreshWritesBackSealed: a rotating workspace whose token
// is about to expire is refreshed, and the new pair lands sealed, even when
// the item held legacy plaintext tokens.
func TestFreshBotToken_RefreshWritesBackSealed(t *testing.T) {
	c, db := newBotCredsClient(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(botCredsTable), Item: map[string]dynamodbtypes.AttributeValue{
		"workspace_key":    &dynamodbtypes.AttributeValueMemberS{Value: "slack#TR"},
		"platform":         &dynamodbtypes.AttributeValueMemberS{Value: "slack"},
		"token_rotation":   &dynamodbtypes.AttributeValueMemberBOOL{Value: true},
		"token_expires_at": &dynamodbtypes.AttributeValueMemberN{Value: "1800000060"},
		"bot_token":        &dynamodbtypes.AttributeValueMemberS{Value: cmdTestCredentials[botsecret.BotToken]},
		"refresh_token":    &dynamodbtypes.AttributeValueMemberS{Value: cmdTestCredentials[botsecret.RefreshToken]},
	}}); err != nil {
		t.Fatal(err)
	}
	ws, err := c.GetWorkspace(ctx, "slack", "TR")
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	const newBot, newRefresh = "xoxb-not-a-real-rotated-cmd-token", "xoxe-1-not-a-real-rotated-cmd-refresh"
	var exchanged []string
	exchange := func(_ context.Context, refresh string) (string, string, int, error) {
		exchanged = append(exchanged, refresh)
		return newBot, newRefresh, 3600, nil
	}
	got, err := freshBotToken(ctx, c, ws, "slack", "TR", now, exchange)
	if err != nil || got != newBot {
		t.Fatalf("freshBotToken = %q, %v; want the refreshed token", got, err)
	}
	if len(exchanged) != 1 || exchanged[0] != cmdTestCredentials[botsecret.RefreshToken] {
		t.Fatalf("exchange called with %v; want the stored refresh token, once", exchanged)
	}
	item := rawBotItem(t, db, "slack#TR")
	for f, plain := range map[botsecret.Field]string{botsecret.BotToken: newBot, botsecret.RefreshToken: newRefresh} {
		stored := f.Stored(item)
		if !security.IsEncrypted(stored) || strings.Contains(stored, plain) {
			t.Errorf("%s written back as %q; want ciphertext", f, stored)
		}
	}
	// And the next call uses the stored, sealed token without refreshing.
	ws, _ = c.GetWorkspace(ctx, "slack", "TR")
	if got, err := freshBotToken(ctx, c, ws, "slack", "TR", now, exchange); err != nil || got != newBot || len(exchanged) != 1 {
		t.Fatalf("second freshBotToken = %q, %v (exchanges %d)", got, err, len(exchanged))
	}
}
