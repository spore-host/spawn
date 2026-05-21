package scheduler

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
	testLambdaARN = "arn:aws:lambda:us-east-1:123456789012:function:test-handler"
	testRoleARN   = "arn:aws:iam::123456789012:role/test-role"
	testAccountID = "123456789012"
)

// createSchedulesTable creates the spawn-schedules DynamoDB table with the
// required key schema and GSI in the Substrate emulator.
func createSchedulesTable(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String("spawn-schedules"),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("schedule_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("user_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("next_execution_time"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("schedule_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
		GlobalSecondaryIndexes: []dynamodbtypes.GlobalSecondaryIndex{
			{
				IndexName: aws.String("user_id-next_execution_time-index"),
				KeySchema: []dynamodbtypes.KeySchemaElement{
					{AttributeName: aws.String("user_id"), KeyType: dynamodbtypes.KeyTypeHash},
					{AttributeName: aws.String("next_execution_time"), KeyType: dynamodbtypes.KeyTypeRange},
				},
				Projection: &dynamodbtypes.Projection{
					ProjectionType: dynamodbtypes.ProjectionTypeAll,
				},
			},
		},
	})
	if err != nil {
		var resourceInUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &resourceInUse) {
			t.Fatalf("create spawn-schedules table: %v", err)
		}
	}
}

// createScheduleHistoryTable creates the spawn-schedule-history DynamoDB table.
func createScheduleHistoryTable(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String("spawn-schedule-history"),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("schedule_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("execution_time"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("schedule_id"), KeyType: dynamodbtypes.KeyTypeHash},
			{AttributeName: aws.String("execution_time"), KeyType: dynamodbtypes.KeyTypeRange},
		},
	})
	if err != nil {
		var resourceInUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &resourceInUse) {
			t.Fatalf("create spawn-schedule-history table: %v", err)
		}
	}
}

func TestCreateSchedule(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	createSchedulesTable(t, db)

	client := NewClientWithTableName(env.AWSConfig, testLambdaARN, testRoleARN, testAccountID, "spawn-schedules")

	tests := []struct {
		name    string
		record  *ScheduleRecord
		wantErr bool
	}{
		{
			name: "one-time schedule",
			record: &ScheduleRecord{
				ScheduleID:         "sched-test-001",
				UserID:             "user-123",
				ScheduleName:       "test-schedule",
				ScheduleExpression: "at(2026-01-22T15:00:00)",
				ScheduleType:       "one-time",
				Timezone:           "America/New_York",
				S3ParamsKey:        "schedules/sched-test-001/params.yaml",
				SweepName:          "test-sweep",
				Status:             "active",
				MaxConcurrent:      10,
				Region:             "us-east-1",
			},
			wantErr: false,
		},
		{
			name: "recurring schedule with cron",
			record: &ScheduleRecord{
				ScheduleID:         "sched-test-002",
				UserID:             "user-123",
				ScheduleName:       "nightly-training",
				ScheduleExpression: "cron(0 2 * * ? *)",
				ScheduleType:       "recurring",
				Timezone:           "America/New_York",
				S3ParamsKey:        "schedules/sched-test-002/params.yaml",
				SweepName:          "nightly-sweep",
				Status:             "active",
				MaxExecutions:      30,
				Region:             "us-east-1",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arn, err := client.CreateSchedule(ctx, tt.record)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CreateSchedule() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if arn == "" {
					t.Error("CreateSchedule() returned empty ARN")
				}
				// Verify DynamoDB record was saved.
				got, err := client.GetSchedule(ctx, tt.record.ScheduleID)
				if err != nil {
					t.Fatalf("GetSchedule after create: %v", err)
				}
				if got.ScheduleName != tt.record.ScheduleName {
					t.Errorf("ScheduleName = %q, want %q", got.ScheduleName, tt.record.ScheduleName)
				}
			}
		})
	}
}

func TestGetSchedule(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	createSchedulesTable(t, db)

	client := NewClientWithTableName(env.AWSConfig, testLambdaARN, testRoleARN, testAccountID, "spawn-schedules")

	record := &ScheduleRecord{
		ScheduleID:         "sched-get-001",
		UserID:             "user-get",
		ScheduleName:       "get-test",
		ScheduleExpression: "cron(0 2 * * ? *)",
		ScheduleType:       "recurring",
		Status:             "active",
	}
	if err := client.SaveSchedule(ctx, record); err != nil {
		t.Fatalf("setup: SaveSchedule: %v", err)
	}

	t.Run("existing schedule", func(t *testing.T) {
		got, err := client.GetSchedule(ctx, "sched-get-001")
		if err != nil {
			t.Fatalf("GetSchedule: %v", err)
		}
		if got.ScheduleID != record.ScheduleID {
			t.Errorf("ScheduleID = %q, want %q", got.ScheduleID, record.ScheduleID)
		}
		if got.UserID != record.UserID {
			t.Errorf("UserID = %q, want %q", got.UserID, record.UserID)
		}
	})

	t.Run("non-existent schedule", func(t *testing.T) {
		_, err := client.GetSchedule(ctx, "sched-missing")
		if err == nil {
			t.Error("GetSchedule() expected error for missing schedule")
		}
	})
}

func TestDeleteSchedule(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	createSchedulesTable(t, db)

	client := NewClientWithTableName(env.AWSConfig, testLambdaARN, testRoleARN, testAccountID, "spawn-schedules")

	// Create schedule in both EventBridge and DynamoDB.
	record := &ScheduleRecord{
		ScheduleID:         "sched-del-001",
		UserID:             "user-del",
		ScheduleName:       "delete-test",
		ScheduleExpression: "at(2026-01-22T15:00:00)",
		ScheduleType:       "one-time",
		Timezone:           "UTC",
		S3ParamsKey:        "schedules/sched-del-001/params.yaml",
		SweepName:          "del-sweep",
		Status:             "active",
		Region:             "us-east-1",
	}
	if _, err := client.CreateSchedule(ctx, record); err != nil {
		t.Fatalf("setup: CreateSchedule: %v", err)
	}

	if err := client.DeleteSchedule(ctx, "sched-del-001"); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}

	// DynamoDB record should be gone.
	if _, err := client.GetSchedule(ctx, "sched-del-001"); err == nil {
		t.Error("GetSchedule after delete should return error")
	}
}

func TestUpdateScheduleStatus(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	createSchedulesTable(t, db)

	client := NewClientWithTableName(env.AWSConfig, testLambdaARN, testRoleARN, testAccountID, "spawn-schedules")

	// Create schedule (EventBridge + DynamoDB) so UpdateSchedule can find it.
	record := &ScheduleRecord{
		ScheduleID:         "sched-upd-001",
		UserID:             "user-upd",
		ScheduleName:       "update-test",
		ScheduleExpression: "cron(0 2 * * ? *)",
		ScheduleType:       "recurring",
		Timezone:           "UTC",
		S3ParamsKey:        "schedules/sched-upd-001/params.yaml",
		SweepName:          "upd-sweep",
		Status:             "active",
		Region:             "us-east-1",
	}
	if _, err := client.CreateSchedule(ctx, record); err != nil {
		t.Fatalf("setup: CreateSchedule: %v", err)
	}

	tests := []struct {
		name   string
		status ScheduleStatus
	}{
		{"pause schedule", ScheduleStatusPaused},
		{"resume schedule", ScheduleStatusActive},
		{"cancel schedule", ScheduleStatusCancelled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := client.UpdateScheduleStatus(ctx, "sched-upd-001", tt.status); err != nil {
				t.Errorf("UpdateScheduleStatus(%s): %v", tt.status, err)
			}
		})
	}
}

func TestRecordExecution(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	createScheduleHistoryTable(t, db)

	client := NewClientWithTableName(env.AWSConfig, testLambdaARN, testRoleARN, testAccountID, "spawn-schedules")

	tests := []struct {
		name    string
		history *ExecutionHistory
		wantErr bool
	}{
		{
			name: "successful execution",
			history: &ExecutionHistory{
				ScheduleID:    "sched-exec-001",
				ExecutionTime: time.Date(2026, 1, 22, 14, 5, 30, 0, time.UTC),
				SweepID:       "sweep-20260122-140530",
				Status:        "success",
			},
			wantErr: false,
		},
		{
			name: "failed execution",
			history: &ExecutionHistory{
				ScheduleID:    "sched-exec-001",
				ExecutionTime: time.Date(2026, 1, 23, 14, 5, 30, 0, time.UTC),
				SweepID:       "sweep-20260123-140530",
				Status:        "failed",
				ErrorMessage:  "instance launch failed",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.RecordExecution(ctx, tt.history)
			if (err != nil) != tt.wantErr {
				t.Errorf("RecordExecution() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.history.TTL == 0 {
				t.Error("RecordExecution() did not set TTL")
			}
		})
	}
}

func TestSaveSchedule(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	createSchedulesTable(t, db)

	client := NewClientWithTableName(env.AWSConfig, testLambdaARN, testRoleARN, testAccountID, "spawn-schedules")

	record := &ScheduleRecord{
		ScheduleID:         "sched-save-001",
		UserID:             "user-123",
		ScheduleName:       "save-test",
		ScheduleExpression: "cron(0 2 * * ? *)",
		ScheduleType:       "recurring",
		Status:             "active",
	}

	if err := client.SaveSchedule(ctx, record); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	if record.TTL == 0 {
		t.Error("SaveSchedule() did not set TTL")
	}

	// Verify persisted.
	got, err := client.GetSchedule(ctx, record.ScheduleID)
	if err != nil {
		t.Fatalf("GetSchedule after save: %v", err)
	}
	if got.ScheduleName != record.ScheduleName {
		t.Errorf("ScheduleName = %q, want %q", got.ScheduleName, record.ScheduleName)
	}
}

func TestListSchedulesByUser(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	createSchedulesTable(t, db)

	client := NewClientWithTableName(env.AWSConfig, testLambdaARN, testRoleARN, testAccountID, "spawn-schedules")

	next := time.Now().Add(24 * time.Hour)
	records := []*ScheduleRecord{
		{
			ScheduleID:         "sched-list-001",
			UserID:             "user-list",
			ScheduleName:       "list-schedule-1",
			ScheduleExpression: "cron(0 2 * * ? *)",
			ScheduleType:       "recurring",
			Status:             "active",
			NextExecutionTime:  next,
		},
		{
			ScheduleID:         "sched-list-002",
			UserID:             "user-list",
			ScheduleName:       "list-schedule-2",
			ScheduleExpression: "at(2026-01-22T15:00:00)",
			ScheduleType:       "one-time",
			Status:             "active",
			NextExecutionTime:  next.Add(time.Hour),
		},
	}
	for _, r := range records {
		if err := client.SaveSchedule(ctx, r); err != nil {
			t.Fatalf("setup: SaveSchedule %s: %v", r.ScheduleID, err)
		}
	}

	t.Run("user with schedules", func(t *testing.T) {
		got, err := client.ListSchedulesByUser(ctx, "user-list")
		if err != nil {
			t.Fatalf("ListSchedulesByUser: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d schedules, want 2", len(got))
		}
	})

	t.Run("user with no schedules", func(t *testing.T) {
		got, err := client.ListSchedulesByUser(ctx, "user-empty")
		if err != nil {
			t.Fatalf("ListSchedulesByUser: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d schedules, want 0", len(got))
		}
	})
}

func TestGetExecutionHistory(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	createScheduleHistoryTable(t, db)

	client := NewClientWithTableName(env.AWSConfig, testLambdaARN, testRoleARN, testAccountID, "spawn-schedules")

	// Write two history entries.
	entries := []*ExecutionHistory{
		{
			ScheduleID:    "sched-hist-001",
			ExecutionTime: time.Date(2026, 1, 22, 14, 0, 0, 0, time.UTC),
			SweepID:       "sweep-001",
			Status:        "success",
		},
		{
			ScheduleID:    "sched-hist-001",
			ExecutionTime: time.Date(2026, 1, 23, 14, 0, 0, 0, time.UTC),
			SweepID:       "sweep-002",
			Status:        "success",
		},
	}
	for _, e := range entries {
		if err := client.RecordExecution(ctx, e); err != nil {
			t.Fatalf("setup: RecordExecution: %v", err)
		}
	}

	t.Run("schedule with history", func(t *testing.T) {
		got, err := client.GetExecutionHistory(ctx, "sched-hist-001", 10)
		if err != nil {
			t.Fatalf("GetExecutionHistory: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d history entries, want 2", len(got))
		}
	})

	t.Run("schedule with no history", func(t *testing.T) {
		got, err := client.GetExecutionHistory(ctx, "sched-hist-empty", 10)
		if err != nil {
			t.Fatalf("GetExecutionHistory: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d history entries, want 0", len(got))
		}
	})
}

func TestGenerateScheduleID(t *testing.T) {
	id := GenerateScheduleID()
	if len(id) == 0 {
		t.Error("GenerateScheduleID() returned empty string")
	}
	if id[:6] != "sched-" {
		t.Errorf("GenerateScheduleID() should start with 'sched-', got %s", id)
	}
}

func TestScheduleRecordValidation(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name   string
		record *ScheduleRecord
	}{
		{
			name: "valid one-time schedule",
			record: &ScheduleRecord{
				ScheduleID:         "sched-test-001",
				ScheduleType:       "one-time",
				ScheduleExpression: "at(2026-01-22T15:00:00)",
				Status:             "active",
			},
		},
		{
			name: "valid recurring schedule",
			record: &ScheduleRecord{
				ScheduleID:         "sched-test-002",
				ScheduleType:       "recurring",
				ScheduleExpression: "cron(0 2 * * ? *)",
				Status:             "active",
			},
		},
		{
			name: "recurring with max executions",
			record: &ScheduleRecord{
				ScheduleID:         "sched-test-003",
				ScheduleType:       "recurring",
				ScheduleExpression: "cron(0 2 * * ? *)",
				Status:             "active",
				MaxExecutions:      30,
			},
		},
		{
			name: "recurring with end date",
			record: &ScheduleRecord{
				ScheduleID:         "sched-test-004",
				ScheduleType:       "recurring",
				ScheduleExpression: "cron(0 2 * * ? *)",
				Status:             "active",
				EndAfter:           now.Add(30 * 24 * time.Hour),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.record.ScheduleID == "" {
				t.Error("ScheduleID should not be empty")
			}
			if tt.record.ScheduleType != "one-time" && tt.record.ScheduleType != "recurring" {
				t.Error("ScheduleType must be one-time or recurring")
			}
			if tt.record.Status != "active" && tt.record.Status != "paused" && tt.record.Status != "cancelled" {
				t.Error("invalid status")
			}
		})
	}
}
