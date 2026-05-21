package alerts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

const (
	testAlertsTable  = "spawn-alerts"
	testHistoryTable = "spawn-alert-history"
)

func setupAlertsTables(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()

	// Create alerts table with GSIs
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(testAlertsTable),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("alert_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("user_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("sweep_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("alert_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
		GlobalSecondaryIndexes: []dynamodbtypes.GlobalSecondaryIndex{
			{
				IndexName: aws.String("user_id-index"),
				KeySchema: []dynamodbtypes.KeySchemaElement{
					{AttributeName: aws.String("user_id"), KeyType: dynamodbtypes.KeyTypeHash},
				},
				Projection: &dynamodbtypes.Projection{ProjectionType: dynamodbtypes.ProjectionTypeAll},
			},
			{
				IndexName: aws.String("sweep_id-index"),
				KeySchema: []dynamodbtypes.KeySchemaElement{
					{AttributeName: aws.String("sweep_id"), KeyType: dynamodbtypes.KeyTypeHash},
				},
				Projection: &dynamodbtypes.Projection{ProjectionType: dynamodbtypes.ProjectionTypeAll},
			},
		},
	})
	if err != nil {
		var resourceInUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &resourceInUse) {
			t.Fatalf("create %s table: %v", testAlertsTable, err)
		}
	}

	// Create alert history table
	_, err = db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(testHistoryTable),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("alert_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("timestamp"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("alert_id"), KeyType: dynamodbtypes.KeyTypeHash},
			{AttributeName: aws.String("timestamp"), KeyType: dynamodbtypes.KeyTypeRange},
		},
	})
	if err != nil {
		var resourceInUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &resourceInUse) {
			t.Fatalf("create %s table: %v", testHistoryTable, err)
		}
	}
}

func newTestAlert(userID, sweepID string) *AlertConfig {
	return &AlertConfig{
		UserID:  userID,
		SweepID: sweepID,
		Triggers: []TriggerType{
			TriggerComplete,
		},
		Destinations: []Destination{
			{Type: DestinationEmail, Target: "user@example.com"},
		},
	}
}

func TestAlertsCreateAndGet(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupAlertsTables(t, env.DynamoClient())

	client := NewClientWithTableNames(env.DynamoClient(), testAlertsTable, testHistoryTable)

	cfg := newTestAlert("user-001", "sweep-001")
	if err := client.CreateAlert(ctx, cfg); err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	got, err := client.GetAlert(ctx, cfg.AlertID)
	if err != nil {
		t.Fatalf("GetAlert: %v", err)
	}

	if got.AlertID != cfg.AlertID {
		t.Errorf("AlertID = %q, want %q", got.AlertID, cfg.AlertID)
	}
	if got.UserID != cfg.UserID {
		t.Errorf("UserID = %q, want %q", got.UserID, cfg.UserID)
	}
	if got.SweepID != cfg.SweepID {
		t.Errorf("SweepID = %q, want %q", got.SweepID, cfg.SweepID)
	}
	if len(got.Triggers) != 1 || got.Triggers[0] != TriggerComplete {
		t.Errorf("Triggers = %v, want [complete]", got.Triggers)
	}
	if got.TTL == 0 {
		t.Error("TTL should be set")
	}
}

func TestAlertsListByUser(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupAlertsTables(t, env.DynamoClient())

	client := NewClientWithTableNames(env.DynamoClient(), testAlertsTable, testHistoryTable)

	userID := "user-list-001"
	for _, sweepID := range []string{"sweep-a", "sweep-b"} {
		cfg := newTestAlert(userID, sweepID)
		if err := client.CreateAlert(ctx, cfg); err != nil {
			t.Fatalf("CreateAlert: %v", err)
		}
	}

	alerts, err := client.ListAlerts(ctx, userID, "")
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}

	if len(alerts) != 2 {
		t.Errorf("got %d alerts, want 2", len(alerts))
	}
	for _, a := range alerts {
		if a.UserID != userID {
			t.Errorf("UserID = %q, want %q", a.UserID, userID)
		}
	}
}

func TestAlertsListBySweep(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupAlertsTables(t, env.DynamoClient())

	client := NewClientWithTableNames(env.DynamoClient(), testAlertsTable, testHistoryTable)

	userID := "user-sweep-001"
	sweepA := "sweep-filter-a"
	sweepB := "sweep-filter-b"

	for _, sweepID := range []string{sweepA, sweepB} {
		cfg := newTestAlert(userID, sweepID)
		if err := client.CreateAlert(ctx, cfg); err != nil {
			t.Fatalf("CreateAlert: %v", err)
		}
	}

	alerts, err := client.ListAlerts(ctx, userID, sweepA)
	if err != nil {
		t.Fatalf("ListAlerts by sweep: %v", err)
	}

	if len(alerts) != 1 {
		t.Errorf("got %d alerts, want 1", len(alerts))
	}
	if len(alerts) > 0 && alerts[0].SweepID != sweepA {
		t.Errorf("SweepID = %q, want %q", alerts[0].SweepID, sweepA)
	}
}

func TestAlertsDelete(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupAlertsTables(t, env.DynamoClient())

	client := NewClientWithTableNames(env.DynamoClient(), testAlertsTable, testHistoryTable)

	cfg := newTestAlert("user-del-001", "sweep-del-001")
	if err := client.CreateAlert(ctx, cfg); err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}

	if err := client.DeleteAlert(ctx, cfg.AlertID); err != nil {
		t.Fatalf("DeleteAlert: %v", err)
	}

	if _, err := client.GetAlert(ctx, cfg.AlertID); err == nil {
		t.Error("GetAlert after delete should return error")
	}
}

func TestAlertsRecordAndListHistory(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupAlertsTables(t, env.DynamoClient())

	client := NewClientWithTableNames(env.DynamoClient(), testAlertsTable, testHistoryTable)

	alertID := "alert-hist-001"
	entry := &AlertHistory{
		AlertID:   alertID,
		Timestamp: time.Now(),
		UserID:    "user-hist-001",
		SweepID:   "sweep-hist-001",
		Trigger:   TriggerComplete,
		Message:   "sweep completed",
		Success:   true,
	}

	if err := client.RecordAlertHistory(ctx, entry); err != nil {
		t.Fatalf("RecordAlertHistory: %v", err)
	}

	history, err := client.ListAlertHistory(ctx, alertID)
	if err != nil {
		t.Fatalf("ListAlertHistory: %v", err)
	}

	if len(history) != 1 {
		t.Fatalf("got %d history entries, want 1", len(history))
	}
	if history[0].Message != entry.Message {
		t.Errorf("Message = %q, want %q", history[0].Message, entry.Message)
	}
	if !history[0].Success {
		t.Error("Success should be true")
	}
}
