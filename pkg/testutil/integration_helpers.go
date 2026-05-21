//go:build integration

package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
)

const (
	TestSchedulesTable          = "spawn-schedules"
	TestScheduleHistoryTable    = "spawn-schedule-history"
	TestSweepOrchestrationTable = "spawn-sweep-orchestration"
	TestScheduleBucket          = "spawn-schedules-us-east-1"
	TestQueueResultsBucket      = "spawn-queue-results-test"
	TestRegion                  = "us-east-1"
)

// AWS Client Helpers

// NewTestSchedulerClient creates EventBridge Scheduler client for testing
func NewTestSchedulerClient(ctx context.Context) *scheduler.Client {
	cfg := loadTestAWSConfig(ctx)
	return scheduler.NewFromConfig(cfg)
}

// NewTestDynamoDBClient creates DynamoDB client for testing
func NewTestDynamoDBClient(ctx context.Context) *dynamodb.Client {
	cfg := loadTestAWSConfig(ctx)
	return dynamodb.NewFromConfig(cfg)
}

// NewTestS3Client creates S3 client for testing
func NewTestS3Client(ctx context.Context) *s3.Client {
	cfg := loadTestAWSConfig(ctx)
	return s3.NewFromConfig(cfg)
}

// NewTestEC2Client creates EC2 client for testing
func NewTestEC2Client(ctx context.Context, region string) *ec2.Client {
	cfg := loadTestAWSConfig(ctx)
	if region != "" {
		cfg.Region = region
	}
	return ec2.NewFromConfig(cfg)
}

func loadTestAWSConfig(ctx context.Context) aws.Config {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(TestRegion),
		config.WithSharedConfigProfile("spore-host-dev"),
	)
	if err != nil {
		panic(fmt.Sprintf("failed to load AWS config: %v", err))
	}
	return cfg
}

// Schedule Test Helpers

// ScheduleTestConfig holds configuration for creating test schedules
type ScheduleTestConfig struct {
	ScheduleID       string
	ParameterFileKey string
	ScheduleType     string // "one-time" or "recurring"
	ExecutionTime    time.Time
	CronExpression   string
	MaxExecutions    int
}

// ExecutionHistory represents a schedule execution record
type ExecutionHistory struct {
	ScheduleID    string
	ExecutionTime time.Time
	SweepID       string
	Status        string
	ErrorMessage  string
}

// CreateTestSchedule creates a schedule for testing
func CreateTestSchedule(t *testing.T, ctx context.Context, config ScheduleTestConfig) string {
	t.Helper()

	client := NewTestDynamoDBClient(ctx)

	item := map[string]types.AttributeValue{
		"schedule_id":        &types.AttributeValueMemberS{Value: config.ScheduleID},
		"parameter_file_key": &types.AttributeValueMemberS{Value: config.ParameterFileKey},
		"schedule_type":      &types.AttributeValueMemberS{Value: config.ScheduleType},
		"status":             &types.AttributeValueMemberS{Value: "active"},
		"execution_count":    &types.AttributeValueMemberN{Value: "0"},
		"created_at":         &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
	}

	if config.ScheduleType == "one-time" {
		item["execution_time"] = &types.AttributeValueMemberS{Value: config.ExecutionTime.Format(time.RFC3339)}
	} else if config.ScheduleType == "recurring" {
		item["cron_expression"] = &types.AttributeValueMemberS{Value: config.CronExpression}
		item["next_execution_time"] = &types.AttributeValueMemberS{Value: config.ExecutionTime.Format(time.RFC3339)}
		if config.MaxExecutions > 0 {
			item["max_executions"] = &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", config.MaxExecutions)}
		}
	}

	_, err := client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(TestSchedulesTable),
		Item:      item,
	})
	if err != nil {
		t.Fatalf("failed to create test schedule: %v", err)
	}

	return config.ScheduleID
}

// WaitForScheduleExecution waits for a schedule to execute and returns the sweep ID
func WaitForScheduleExecution(t *testing.T, ctx context.Context, scheduleID string, timeout time.Duration) string {
	t.Helper()

	var sweepID string
	err := PollUntil(t, timeout, 5*time.Second, func() bool {
		history := GetScheduleHistory(t, ctx, scheduleID)
		if len(history) > 0 {
			sweepID = history[0].SweepID
			return sweepID != ""
		}
		return false
	})

	if err != nil {
		t.Fatalf("schedule did not execute within timeout: %v", err)
	}

	return sweepID
}

// GetScheduleHistory retrieves execution history for a schedule
func GetScheduleHistory(t *testing.T, ctx context.Context, scheduleID string) []ExecutionHistory {
	t.Helper()

	client := NewTestDynamoDBClient(ctx)

	result, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(TestScheduleHistoryTable),
		KeyConditionExpression: aws.String("schedule_id = :sid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sid": &types.AttributeValueMemberS{Value: scheduleID},
		},
		ScanIndexForward: aws.Bool(false), // Most recent first
	})
	if err != nil {
		t.Logf("Warning: failed to query schedule history: %v", err)
		return nil
	}

	var history []ExecutionHistory
	for _, item := range result.Items {
		h := ExecutionHistory{
			ScheduleID: scheduleID,
		}
		if v, ok := item["execution_time"].(*types.AttributeValueMemberS); ok {
			h.ExecutionTime, _ = time.Parse(time.RFC3339, v.Value)
		}
		if v, ok := item["sweep_id"].(*types.AttributeValueMemberS); ok {
			h.SweepID = v.Value
		}
		if v, ok := item["status"].(*types.AttributeValueMemberS); ok {
			h.Status = v.Value
		}
		if v, ok := item["error_message"].(*types.AttributeValueMemberS); ok {
			h.ErrorMessage = v.Value
		}
		history = append(history, h)
	}

	return history
}

// UpdateScheduleNextExecution modifies next execution time for testing
func UpdateScheduleNextExecution(t *testing.T, ctx context.Context, scheduleID string, nextTime time.Time) {
	t.Helper()

	client := NewTestDynamoDBClient(ctx)

	_, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(TestSchedulesTable),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
		UpdateExpression: aws.String("SET next_execution_time = :time"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":time": &types.AttributeValueMemberS{Value: nextTime.Format(time.RFC3339)},
		},
	})
	if err != nil {
		t.Fatalf("failed to update schedule execution time: %v", err)
	}
}

// UpdateScheduleStatus updates schedule status (pause/resume/cancel)
func UpdateScheduleStatus(t *testing.T, ctx context.Context, scheduleID, status string) {
	t.Helper()

	client := NewTestDynamoDBClient(ctx)

	_, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(TestSchedulesTable),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
		UpdateExpression: aws.String("SET #status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":status": &types.AttributeValueMemberS{Value: status},
		},
	})
	if err != nil {
		t.Fatalf("failed to update schedule status: %v", err)
	}
}

// GetScheduleRecord retrieves schedule record from DynamoDB
func GetScheduleRecord(t *testing.T, ctx context.Context, scheduleID string) map[string]types.AttributeValue {
	t.Helper()

	client := NewTestDynamoDBClient(ctx)

	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(TestSchedulesTable),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
	})
	if err != nil {
		t.Fatalf("failed to get schedule record: %v", err)
	}

	return result.Item
}

// CleanupSchedule deletes a schedule and its history
func CleanupSchedule(t *testing.T, ctx context.Context, scheduleID string) {
	t.Helper()

	client := NewTestDynamoDBClient(ctx)

	// Delete from schedules table
	_, err := client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(TestSchedulesTable),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
	})
	if err != nil {
		t.Logf("Warning: failed to delete schedule: %v", err)
	}

	// Delete EventBridge Scheduler schedule if exists
	schedulerClient := NewTestSchedulerClient(ctx)
	_, err = schedulerClient.DeleteSchedule(ctx, &scheduler.DeleteScheduleInput{
		Name: aws.String(scheduleID),
	})
	if err != nil {
		t.Logf("Warning: failed to delete EventBridge schedule: %v", err)
	}
}

// Queue Test Helpers

// QueueState represents the queue execution state
type QueueState struct {
	QueueID     string     `json:"queue_id"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt time.Time  `json:"completed_at"`
	Jobs        []JobState `json:"jobs"`
}

// JobState represents individual job state
type JobState struct {
	JobID        string    `json:"job_id"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
	ExitCode     int       `json:"exit_code"`
	Attempt      int       `json:"attempt"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

// UploadQueueConfig uploads queue config to S3
func UploadQueueConfig(t *testing.T, ctx context.Context, queueConfig, s3Bucket, s3Key string) {
	t.Helper()

	client := NewTestS3Client(ctx)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(s3Key),
		Body:   bytes.NewReader([]byte(queueConfig)),
	})
	if err != nil {
		t.Fatalf("failed to upload queue config: %v", err)
	}
}

// WaitForQueueCompletion waits for queue to complete and returns final state
func WaitForQueueCompletion(t *testing.T, ctx context.Context, s3Bucket, s3Key string, timeout time.Duration) QueueState {
	t.Helper()

	var queueState QueueState
	err := PollUntil(t, timeout, 10*time.Second, func() bool {
		state := DownloadQueueState(t, ctx, s3Bucket, s3Key)
		if state.Status == "completed" || state.Status == "failed" {
			queueState = state
			return true
		}
		return false
	})

	if err != nil {
		t.Fatalf("queue did not complete within timeout: %v", err)
	}

	return queueState
}

// DownloadQueueState downloads queue state from S3
func DownloadQueueState(t *testing.T, ctx context.Context, s3Bucket, s3Key string) QueueState {
	t.Helper()

	client := NewTestS3Client(ctx)

	result, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		// Object may not exist yet
		return QueueState{}
	}
	defer result.Body.Close()

	var state QueueState
	if err := json.NewDecoder(result.Body).Decode(&state); err != nil {
		t.Logf("Warning: failed to decode queue state: %v", err)
		return QueueState{}
	}

	return state
}

// S3 Helpers

// UploadParamFile uploads parameter file to S3
func UploadParamFile(t *testing.T, ctx context.Context, content, bucket, key string) {
	t.Helper()

	client := NewTestS3Client(ctx)

	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte(content)),
	})
	if err != nil {
		t.Fatalf("failed to upload parameter file: %v", err)
	}
}

// DownloadS3File downloads file from S3
func DownloadS3File(t *testing.T, ctx context.Context, bucket, key string) []byte {
	t.Helper()

	client := NewTestS3Client(ctx)

	result, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("failed to download S3 file: %v", err)
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("failed to read S3 file: %v", err)
	}

	return data
}

// CleanupS3Prefix deletes all objects with prefix
func CleanupS3Prefix(t *testing.T, ctx context.Context, bucket, prefix string) {
	t.Helper()

	client := NewTestS3Client(ctx)

	result, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		t.Logf("Warning: failed to list S3 objects: %v", err)
		return
	}

	for _, obj := range result.Contents {
		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    obj.Key,
		})
		if err != nil {
			t.Logf("Warning: failed to delete S3 object %s: %v", *obj.Key, err)
		}
	}
}

// Sweep Helpers

// SweepStatus represents sweep orchestration status
type SweepStatus struct {
	SweepID     string
	Status      string
	InstanceIDs []string
}

// WaitForSweepCompletion waits for sweep to complete
func WaitForSweepCompletion(t *testing.T, ctx context.Context, sweepID string, timeout time.Duration) SweepStatus {
	t.Helper()

	var sweepStatus SweepStatus
	err := PollUntil(t, timeout, 10*time.Second, func() bool {
		status := GetSweepStatus(t, ctx, sweepID)
		if status.Status == "COMPLETED" || status.Status == "FAILED" {
			sweepStatus = status
			return true
		}
		return false
	})

	if err != nil {
		t.Fatalf("sweep did not complete within timeout: %v", err)
	}

	return sweepStatus
}

// GetSweepStatus retrieves sweep status from DynamoDB
func GetSweepStatus(t *testing.T, ctx context.Context, sweepID string) SweepStatus {
	t.Helper()

	client := NewTestDynamoDBClient(ctx)

	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(TestSweepOrchestrationTable),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
	})
	if err != nil {
		t.Logf("Warning: failed to get sweep status: %v", err)
		return SweepStatus{}
	}

	status := SweepStatus{SweepID: sweepID}
	if v, ok := result.Item["status"].(*types.AttributeValueMemberS); ok {
		status.Status = v.Value
	}

	return status
}

// TerminateSweepInstances terminates all instances for a sweep
func TerminateSweepInstances(t *testing.T, ctx context.Context, sweepID string) {
	t.Helper()

	client := NewTestEC2Client(ctx, TestRegion)

	result, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:spawn:sweep-id"),
				Values: []string{sweepID},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running", "stopping", "stopped"},
			},
		},
	})
	if err != nil {
		t.Logf("Warning: failed to describe instances: %v", err)
		return
	}

	var instanceIDs []string
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			instanceIDs = append(instanceIDs, *instance.InstanceId)
		}
	}

	if len(instanceIDs) > 0 {
		_, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: instanceIDs,
		})
		if err != nil {
			t.Logf("Warning: failed to terminate instances: %v", err)
		}
	}
}

// Polling Utilities

// PollUntil polls a condition until it's true or timeout
func PollUntil(t *testing.T, timeout time.Duration, interval time.Duration, condition func() bool) error {
	t.Helper()

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if condition() {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %v", timeout)
		}

		select {
		case <-ticker.C:
			// Continue polling
		}
	}
}
