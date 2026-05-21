package main

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	substrate "github.com/scttfrdmn/substrate"
)

const (
	testRegistryTable   = "test-registry"
	testWorkspacesTable = "test-workspaces"
)

// setupRegistry starts a Substrate DynamoDB emulator, creates the two tables,
// and returns a Registry pointing at them. No AWS credentials required.
func setupRegistry(t *testing.T) *Registry {
	t.Helper()
	ts := substrate.StartTestServer(t)

	cfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithBaseEndpoint(ts.URL),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "test"),
		),
	)
	if err != nil {
		t.Fatalf("build AWS config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg)
	createRegistryTable(t, client)
	createWorkspacesTable(t, client)

	return &Registry{
		client:          client,
		registryTable:   testRegistryTable,
		workspacesTable: testWorkspacesTable,
	}
}

func createRegistryTable(t *testing.T, client *dynamodb.Client) {
	t.Helper()
	_, err := client.CreateTable(context.Background(), &dynamodb.CreateTableInput{
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
	if err != nil {
		t.Fatalf("create registry table: %v", err)
	}
}

func createWorkspacesTable(t *testing.T, client *dynamodb.Client) {
	t.Helper()
	_, err := client.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName:   aws.String(testWorkspacesTable),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("workspace_key"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("workspace_key"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		t.Fatalf("create workspaces table: %v", err)
	}
}

func sampleWorkspace(platform, workspaceID string) *WorkspaceConfig {
	return &WorkspaceConfig{
		WorkspaceKey:  workspaceKey(platform, workspaceID),
		BotToken:      "xoxb-test-token",
		SigningSecret: "test-signing-secret",
		Platform:      platform,
		WorkspaceName: "Test Workspace",
		InstalledBy:   "arn:aws:iam::123456789012:user/test",
		InstalledAt:   time.Now().UTC().Format(time.RFC3339),
	}
}

func sampleReg(platform, workspaceID, userID, nickname, instanceID string) *BotRegistration {
	return &BotRegistration{
		UserKey:        userKey(platform, workspaceID, userID),
		Nickname:       nickname,
		InstanceID:     instanceID,
		AWSAccountID:   "123456789012",
		RoleARN:        "arn:aws:iam::123456789012:role/SpawnBotCrossAccount",
		TagPrefix:      "spawn",
		AllowedActions: []string{"start", "stop", "status"},
		RegisteredBy:   "arn:aws:iam::123456789012:user/test",
		Platform:       platform,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
}

// ── Workspace CRUD ────────────────────────────────────────────────────────────

func TestPutAndGetWorkspace(t *testing.T) {
	reg := setupRegistry(t)
	ctx := context.Background()

	ws := sampleWorkspace("slack", "T03NE3GTY")
	if err := reg.PutWorkspace(ctx, ws); err != nil {
		t.Fatalf("PutWorkspace: %v", err)
	}

	got, err := reg.GetWorkspace(ctx, "slack", "T03NE3GTY")
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.SigningSecret != "test-signing-secret" {
		t.Errorf("SigningSecret = %q, want test-signing-secret", got.SigningSecret)
	}
	if got.BotToken != "xoxb-test-token" {
		t.Errorf("BotToken = %q, want xoxb-test-token", got.BotToken)
	}
	if got.WorkspaceName != "Test Workspace" {
		t.Errorf("WorkspaceName = %q, want Test Workspace", got.WorkspaceName)
	}
}

func TestGetWorkspace_NotFound(t *testing.T) {
	reg := setupRegistry(t)
	_, err := reg.GetWorkspace(context.Background(), "slack", "TNOTEXIST")
	if err == nil {
		t.Error("expected error for nonexistent workspace")
	}
}

func TestDeleteWorkspace(t *testing.T) {
	reg := setupRegistry(t)
	ctx := context.Background()

	_ = reg.PutWorkspace(ctx, sampleWorkspace("slack", "T123"))
	if err := reg.DeleteWorkspace(ctx, "slack", "T123"); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	_, err := reg.GetWorkspace(ctx, "slack", "T123")
	if err == nil {
		t.Error("expected error after deletion")
	}
}

// ── Registration CRUD ─────────────────────────────────────────────────────────

func TestPutAndListUserInstances(t *testing.T) {
	registry := setupRegistry(t)
	ctx := context.Background()

	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U456", "rstudio", "i-0abc1"))
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U456", "jupyter", "i-0abc2"))
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U456", "vscode", "i-0abc3"))

	regs, err := registry.ListUserInstances(ctx, "slack", "T123", "U456")
	if err != nil {
		t.Fatalf("ListUserInstances: %v", err)
	}
	if len(regs) != 3 {
		t.Errorf("got %d registrations, want 3", len(regs))
	}
}

func TestGetInstance_ByNickname(t *testing.T) {
	registry := setupRegistry(t)
	ctx := context.Background()

	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U456", "rstudio", "i-0abc123"))
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U456", "jupyter", "i-0def456"))

	got, err := registry.GetInstance(ctx, "slack", "T123", "U456", "rstudio")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got == nil {
		t.Fatal("GetInstance returned nil")
	}
	if got.InstanceID != "i-0abc123" {
		t.Errorf("InstanceID = %q, want i-0abc123", got.InstanceID)
	}
}

func TestGetInstance_NotFound(t *testing.T) {
	registry := setupRegistry(t)
	ctx := context.Background()

	got, err := registry.GetInstance(ctx, "slack", "T123", "U456", "nonexistent")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent instance, got %+v", got)
	}
}

func TestPutAndDeleteRegistration(t *testing.T) {
	registry := setupRegistry(t)
	ctx := context.Background()

	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U456", "rstudio", "i-0abc123"))

	if err := registry.DeleteRegistration(ctx, "slack", "T123", "U456", "rstudio"); err != nil {
		t.Fatalf("DeleteRegistration: %v", err)
	}

	got, err := registry.GetInstance(ctx, "slack", "T123", "U456", "rstudio")
	if err != nil {
		t.Fatalf("GetInstance after delete: %v", err)
	}
	if got != nil {
		t.Error("expected nil after deletion")
	}
}

func TestListUserInstances_IsolatedByUser(t *testing.T) {
	registry := setupRegistry(t)
	ctx := context.Background()

	// Two users in same workspace
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U111", "rstudio", "i-0abc1"))
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U111", "jupyter", "i-0abc2"))
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U222", "rstudio", "i-0def1"))

	u1Regs, _ := registry.ListUserInstances(ctx, "slack", "T123", "U111")
	u2Regs, _ := registry.ListUserInstances(ctx, "slack", "T123", "U222")

	if len(u1Regs) != 2 {
		t.Errorf("U111 got %d registrations, want 2", len(u1Regs))
	}
	if len(u2Regs) != 1 {
		t.Errorf("U222 got %d registrations, want 1", len(u2Regs))
	}
}

// ── Workspace destroy ─────────────────────────────────────────────────────────

func TestListWorkspaceRegistrations(t *testing.T) {
	registry := setupRegistry(t)
	ctx := context.Background()

	// Registrations in workspace T123
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U111", "rstudio", "i-0abc1"))
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U222", "rstudio", "i-0def1"))
	// Registration in a different workspace — must NOT appear
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T999", "U333", "rstudio", "i-0ghi1"))

	regs, err := registry.ListWorkspaceRegistrations(ctx, "slack", "T123")
	if err != nil {
		t.Fatalf("ListWorkspaceRegistrations: %v", err)
	}
	if len(regs) != 2 {
		t.Errorf("got %d registrations for T123, want 2", len(regs))
	}
	for _, r := range regs {
		if r.UserKey == "slack#T999#U333" {
			t.Error("workspace isolation failed: got T999 registration in T123 query")
		}
	}
}

func TestSetEnabled(t *testing.T) {
	registry := setupRegistry(t)
	ctx := context.Background()

	reg := sampleReg("slack", "T123", "U456", "rstudio", "i-0abc123")
	// Default: enabled = false (Go zero value)
	if reg.Enabled {
		t.Error("new registration should be disabled by default")
	}
	_ = registry.PutRegistration(ctx, reg)

	// Enable
	if err := registry.SetEnabled(ctx, "slack", "T123", "U456", "rstudio", true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	got, _ := registry.GetInstance(ctx, "slack", "T123", "U456", "rstudio")
	if !got.Enabled {
		t.Error("expected enabled = true after SetEnabled(true)")
	}

	// Disable
	if err := registry.SetEnabled(ctx, "slack", "T123", "U456", "rstudio", false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	got, _ = registry.GetInstance(ctx, "slack", "T123", "U456", "rstudio")
	if got.Enabled {
		t.Error("expected enabled = false after SetEnabled(false)")
	}
}

// TestSetEnabled_NotFound verifies the ConditionExpression prevents enabling
// a nonexistent registration. Substrate does not enforce ConditionExpression,
// so this test is skipped in local emulator mode.
func TestSetEnabled_NotFound(t *testing.T) {
	t.Skip("Substrate emulator does not enforce ConditionExpression; verified against real DynamoDB")
}

func TestDestroyWorkspace(t *testing.T) {
	registry := setupRegistry(t)
	ctx := context.Background()

	// Set up workspace + 3 registrations across 2 users
	_ = registry.PutWorkspace(ctx, sampleWorkspace("slack", "T123"))
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U111", "rstudio", "i-0abc1"))
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U111", "jupyter", "i-0abc2"))
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T123", "U222", "rstudio", "i-0def1"))
	// Registration in a different workspace — must survive
	_ = registry.PutRegistration(ctx, sampleReg("slack", "T999", "U333", "rstudio", "i-0ghi1"))

	deleted, err := registry.DestroyWorkspace(ctx, "slack", "T123")
	if err != nil {
		t.Fatalf("DestroyWorkspace: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}

	// Workspace record gone
	if _, err := registry.GetWorkspace(ctx, "slack", "T123"); err == nil {
		t.Error("expected workspace to be gone after destroy")
	}

	// T123 registrations gone
	regs, _ := registry.ListWorkspaceRegistrations(ctx, "slack", "T123")
	if len(regs) != 0 {
		t.Errorf("expected 0 T123 registrations after destroy, got %d", len(regs))
	}

	// T999 registration untouched
	survivor, _ := registry.GetInstance(ctx, "slack", "T999", "U333", "rstudio")
	if survivor == nil {
		t.Error("T999 registration should not be deleted by T123 destroy")
	}
}

// ── Audit log ─────────────────────────────────────────────────────────────────

const testAuditTable = "test-audit"

func createAuditTable(t *testing.T, client *dynamodb.Client) {
	t.Helper()
	_, err := client.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName:   aws.String(testAuditTable),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("audit_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("audit_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		t.Fatalf("create audit table: %v", err)
	}
}

func TestAuditLog_Write(t *testing.T) {
	ts := substrate.StartTestServer(t)
	cfg, _ := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithBaseEndpoint(ts.URL),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "test"),
		),
	)
	client := dynamodb.NewFromConfig(cfg)
	createAuditTable(t, client)

	a := &Auditor{client: client, tableName: testAuditTable}

	event := AuditEvent{
		AuditID:     "test-audit-123",
		UserKey:     "slack#T123#U456",
		TS:          "2026-04-20T12:00:00Z",
		Platform:    "slack",
		WorkspaceID: "T123",
		UserID:      "U456",
		Command:     "stop",
		Nickname:    "rstudio",
		InstanceID:  "i-0abc123",
		AccountID:   "123456789012",
		Result:      AuditResultSuccess,
		TTL:         9999999999,
	}

	// Log is async — call synchronously in test by calling the write directly
	a.Log(context.Background(), event)
	// Give the goroutine a moment
	time.Sleep(100 * time.Millisecond)

	// Verify record exists in DynamoDB
	result, err := client.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: aws.String(testAuditTable),
		Key: map[string]dynamodbtypes.AttributeValue{
			"audit_id": &dynamodbtypes.AttributeValueMemberS{Value: "test-audit-123"},
		},
	})
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if result.Item == nil {
		t.Fatal("expected audit record to be written")
	}
	// Verify key fields
	if v, ok := result.Item["command"].(*dynamodbtypes.AttributeValueMemberS); !ok || v.Value != "stop" {
		t.Errorf("expected command=stop, got %v", result.Item["command"])
	}
	if v, ok := result.Item["result"].(*dynamodbtypes.AttributeValueMemberS); !ok || v.Value != AuditResultSuccess {
		t.Errorf("expected result=success, got %v", result.Item["result"])
	}
}

func TestAuditLog_DefaultsPopulated(t *testing.T) {
	// Verify that missing AuditID and TS are auto-populated
	ts := substrate.StartTestServer(t)
	cfg, _ := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithBaseEndpoint(ts.URL),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "test"),
		),
	)
	client := dynamodb.NewFromConfig(cfg)
	createAuditTable(t, client)

	a := &Auditor{client: client, tableName: testAuditTable}
	// Empty AuditID and TS — should be auto-filled
	event := AuditEvent{Command: "status", Result: AuditResultSuccess, TTL: 9999999999}
	a.Log(context.Background(), event)
	time.Sleep(100 * time.Millisecond)

	// Scan to find the record (we don't know the UUID)
	scan, err := client.Scan(context.Background(), &dynamodb.ScanInput{
		TableName: aws.String(testAuditTable),
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(scan.Items) == 0 {
		t.Fatal("expected at least one audit record")
	}
	item := scan.Items[0]
	if _, ok := item["audit_id"].(*dynamodbtypes.AttributeValueMemberS); !ok {
		t.Error("expected audit_id to be auto-populated")
	}
	if _, ok := item["ts"].(*dynamodbtypes.AttributeValueMemberS); !ok {
		t.Error("expected ts to be auto-populated")
	}
}
