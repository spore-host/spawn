package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

const (
	testRegistryTable   = "spore-bot-registry"
	testWorkspacesTable = "spore-bot-workspaces"
)

func setupBotTables(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()

	// Registry: hash user_key + range nickname.
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(testRegistryTable),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("user_key"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("nickname"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("user_key"), KeyType: dynamodbtypes.KeyTypeHash},
			{AttributeName: aws.String("nickname"), KeyType: dynamodbtypes.KeyTypeRange},
		},
	})
	tolerateInUse(t, err, testRegistryTable)

	// Workspaces: hash workspace_key.
	_, err = db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(testWorkspacesTable),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("workspace_key"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("workspace_key"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	tolerateInUse(t, err, testWorkspacesTable)
}

func tolerateInUse(t *testing.T, err error, table string) {
	t.Helper()
	if err != nil {
		var inUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &inUse) {
			t.Fatalf("create %s: %v", table, err)
		}
	}
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	env := testutil.SubstrateServer(t)
	db := env.DynamoClient()
	setupBotTables(t, db)
	return NewClientWithTableNames(db, testRegistryTable, testWorkspacesTable)
}

func TestBotStore_RegistrationLifecycle(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	userKey := "slack#T1#U1"

	reg := &Registration{
		UserKey: userKey, Nickname: "rstudio", InstanceID: "i-1",
		AWSAccountID: "111122223333", TagPrefix: "spawn",
		AllowedActions: []string{"start", "stop"}, RegisteredBy: "arn:me",
		Platform: "slack", CreatedAt: "2026-07-11T00:00:00Z",
	}
	if err := c.UpsertRegistration(ctx, reg); err != nil {
		t.Fatalf("UpsertRegistration: %v", err)
	}

	// Enabled defaults to false (not set on upsert); enable it.
	if err := c.SetRegistrationEnabled(ctx, userKey, "rstudio", true); err != nil {
		t.Fatalf("SetRegistrationEnabled: %v", err)
	}

	// Re-upsert must NOT reset enabled.
	if err := c.UpsertRegistration(ctx, reg); err != nil {
		t.Fatalf("re-UpsertRegistration: %v", err)
	}
	regs, err := c.ListRegistrationsByWorkspace(ctx, "slack", "T1")
	if err != nil {
		t.Fatalf("ListRegistrationsByWorkspace: %v", err)
	}
	if len(regs) != 1 {
		t.Fatalf("expected 1 registration, got %d", len(regs))
	}
	if !regs[0].Enabled {
		t.Error("re-upsert reset enabled to false (should be preserved)")
	}
	if regs[0].InstanceID != "i-1" || len(regs[0].AllowedActions) != 2 {
		t.Errorf("registration round-trip mismatch: %+v", regs[0])
	}
	// Note: SetRegistrationEnabled carries a ConditionExpression
	// (attribute_exists(user_key)) so it fails on a missing item against real
	// DynamoDB; the Substrate emulator doesn't enforce conditions, so that path
	// isn't asserted here.

	// Delete.
	if err := c.DeleteRegistration(ctx, userKey, "rstudio"); err != nil {
		t.Fatalf("DeleteRegistration: %v", err)
	}
	regs, _ = c.ListRegistrationsByWorkspace(ctx, "slack", "T1")
	if len(regs) != 0 {
		t.Errorf("registration still present after delete: %+v", regs)
	}
}

func TestBotStore_BatchDeleteRegistrations(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	// Seed 30 registrations (forces 2 BatchWriteItem chunks of 25 + 5).
	for i := 0; i < 30; i++ {
		nick := "n" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if err := c.UpsertRegistration(ctx, &Registration{
			UserKey: "slack#T2#U", Nickname: nick, InstanceID: "i", Platform: "slack",
			CreatedAt: "t", TagPrefix: "spawn", RegisteredBy: "me", AllowedActions: []string{"status"},
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	regs, err := c.ListRegistrationsByWorkspace(ctx, "slack", "T2")
	if err != nil || len(regs) != 30 {
		t.Fatalf("expected 30 seeded, got %d (%v)", len(regs), err)
	}
	deleted, err := c.BatchDeleteRegistrations(ctx, regs)
	if err != nil {
		t.Fatalf("BatchDeleteRegistrations: %v", err)
	}
	if deleted != 30 {
		t.Errorf("deleted %d, want 30", deleted)
	}
	regs, _ = c.ListRegistrationsByWorkspace(ctx, "slack", "T2")
	if len(regs) != 0 {
		t.Errorf("%d registrations remain after batch delete", len(regs))
	}
}

func TestBotStore_WorkspaceAndConnectCode(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()

	ws := &Workspace{
		WorkspaceKey: "slack#T3", BotToken: "xoxb-1", Platform: "slack",
		WorkspaceName: "acme", InstalledBy: "arn:me", InstalledAt: "t",
	}
	if err := c.PutWorkspace(ctx, ws); err != nil {
		t.Fatalf("PutWorkspace: %v", err)
	}
	got, err := c.GetWorkspace(ctx, "slack", "T3")
	if err != nil || got == nil || got.BotToken != "xoxb-1" {
		t.Fatalf("GetWorkspace: %+v %v", got, err)
	}
	if missing, _ := c.GetWorkspace(ctx, "slack", "nope"); missing != nil {
		t.Errorf("expected nil for missing workspace, got %+v", missing)
	}

	if err := c.UpdateWorkspaceTokens(ctx, "slack", "T3", "xoxb-2", "refresh", 999); err != nil {
		t.Fatalf("UpdateWorkspaceTokens: %v", err)
	}
	if got, _ := c.GetWorkspace(ctx, "slack", "T3"); got.BotToken != "xoxb-2" || got.TokenExpiresAt != 999 {
		t.Errorf("token update not persisted: %+v", got)
	}

	list, err := c.ListWorkspaces(ctx, "slack")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListWorkspaces = %+v (%v)", list, err)
	}

	// Connect code: seed one, then redeem (atomic delete + return).
	future := time.Now().Add(time.Hour).Unix()
	cc := ConnectCode{CodeKey: "connect#ABC123", Platform: "slack", WorkspaceID: "T3", UserID: "U9", TTL: future}
	item, err := attributevalue.MarshalMap(cc)
	if err != nil {
		t.Fatalf("marshal connect code: %v", err)
	}
	if _, err := c.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(testWorkspacesTable), Item: item}); err != nil {
		t.Fatalf("seed connect code: %v", err)
	}
	// Redeem accepts the "SPORE-" prefix + lowercase.
	rec, err := c.RedeemConnectCode(ctx, "spore-abc123")
	if err != nil || rec == nil {
		t.Fatalf("RedeemConnectCode: %+v %v", rec, err)
	}
	if rec.UserID != "U9" || rec.WorkspaceID != "T3" {
		t.Errorf("redeemed code mismatch: %+v", rec)
	}
	// Second redeem finds nothing (it was deleted).
	if again, _ := c.RedeemConnectCode(ctx, "ABC123"); again != nil {
		t.Errorf("connect code not consumed: %+v", again)
	}

	if err := c.DeleteWorkspace(ctx, "slack", "T3"); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if got, _ := c.GetWorkspace(ctx, "slack", "T3"); got != nil {
		t.Errorf("workspace still present after delete: %+v", got)
	}
}
