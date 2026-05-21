package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
)

// DynamoDBAPI defines the interface for DynamoDB operations
type DynamoDBAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// SchedulerAPI defines the interface for EventBridge Scheduler operations
type SchedulerAPI interface {
	CreateSchedule(ctx context.Context, params *scheduler.CreateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error)
	DeleteSchedule(ctx context.Context, params *scheduler.DeleteScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.DeleteScheduleOutput, error)
	UpdateSchedule(ctx context.Context, params *scheduler.UpdateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.UpdateScheduleOutput, error)
}

// Client handles scheduled execution operations
type Client struct {
	schedulerClient SchedulerAPI
	dynamoClient    DynamoDBAPI
	lambdaARN       string
	roleARN         string
	accountID       string
	tableName       string
}

// NewClient creates a new scheduler client
func NewClient(cfg aws.Config, lambdaARN, roleARN, accountID string) *Client {
	return &Client{
		schedulerClient: scheduler.NewFromConfig(cfg),
		dynamoClient:    dynamodb.NewFromConfig(cfg),
		lambdaARN:       lambdaARN,
		roleARN:         roleARN,
		accountID:       accountID,
		tableName:       "spawn-schedules", // Default for backward compatibility
	}
}

// NewClientWithTableName creates a new scheduler client with custom table name
func NewClientWithTableName(cfg aws.Config, lambdaARN, roleARN, accountID, tableName string) *Client {
	return &Client{
		schedulerClient: scheduler.NewFromConfig(cfg),
		dynamoClient:    dynamodb.NewFromConfig(cfg),
		lambdaARN:       lambdaARN,
		roleARN:         roleARN,
		accountID:       accountID,
		tableName:       tableName,
	}
}

// GenerateScheduleID creates a unique schedule ID
func GenerateScheduleID() string {
	return fmt.Sprintf("sched-%s", time.Now().Format("20060102-150405"))
}

// CreateSchedule creates a new EventBridge schedule and stores metadata in DynamoDB
func (c *Client) CreateSchedule(ctx context.Context, record *ScheduleRecord) (string, error) {
	// Create EventBridge schedule
	scheduleInput := &scheduler.CreateScheduleInput{
		Name:               aws.String(record.ScheduleID),
		Description:        aws.String(fmt.Sprintf("Spawn scheduled execution: %s", record.ScheduleName)),
		ScheduleExpression: aws.String(record.ScheduleExpression),
		FlexibleTimeWindow: &schedulertypes.FlexibleTimeWindow{
			Mode: schedulertypes.FlexibleTimeWindowModeOff,
		},
		Target: &schedulertypes.Target{
			Arn:     aws.String(c.lambdaARN),
			RoleArn: aws.String(c.roleARN),
			Input: aws.String(fmt.Sprintf(`{
				"schedule_id": "%s",
				"execution_time": "<aws.scheduler.scheduled-time>"
			}`, record.ScheduleID)),
			RetryPolicy: &schedulertypes.RetryPolicy{
				MaximumRetryAttempts:     aws.Int32(2),
				MaximumEventAgeInSeconds: aws.Int32(300), // 5 minutes
			},
		},
		State: schedulertypes.ScheduleStateEnabled,
	}

	// Add timezone if specified
	if record.Timezone != "" {
		scheduleInput.ScheduleExpressionTimezone = aws.String(record.Timezone)
	}

	// Create schedule
	result, err := c.schedulerClient.CreateSchedule(ctx, scheduleInput)
	if err != nil {
		return "", fmt.Errorf("create eventbridge schedule: %w", err)
	}

	scheduleARN := *result.ScheduleArn
	record.ScheduleARN = scheduleARN

	// Save metadata to DynamoDB
	if err := c.SaveSchedule(ctx, record); err != nil {
		// Try to cleanup the schedule if DynamoDB save fails
		_ = c.DeleteSchedule(ctx, record.ScheduleID)
		return "", fmt.Errorf("save schedule metadata: %w", err)
	}

	return scheduleARN, nil
}

// SaveSchedule stores schedule metadata in DynamoDB
func (c *Client) SaveSchedule(ctx context.Context, record *ScheduleRecord) error {
	// Set TTL to 90 days from now
	record.TTL = time.Now().Add(90 * 24 * time.Hour).Unix()

	// Convert struct to DynamoDB AttributeValue map
	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("marshal schedule record: %w", err)
	}

	_, err = c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.tableName),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put dynamodb item: %w", err)
	}

	return nil
}

// GetSchedule retrieves schedule metadata from DynamoDB
func (c *Client) GetSchedule(ctx context.Context, scheduleID string) (*ScheduleRecord, error) {
	result, err := c.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: scheduleID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get dynamodb item: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("schedule not found: %s", scheduleID)
	}

	var record ScheduleRecord
	if err := attributevalue.UnmarshalMap(result.Item, &record); err != nil {
		return nil, fmt.Errorf("unmarshal schedule record: %w", err)
	}

	return &record, nil
}

// ListSchedulesByUser retrieves all schedules for a user
func (c *Client) ListSchedulesByUser(ctx context.Context, userID string) ([]ScheduleRecord, error) {
	result, err := c.dynamoClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(c.tableName),
		IndexName:              aws.String("user_id-next_execution_time-index"),
		KeyConditionExpression: aws.String("user_id = :user_id"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":user_id": &dynamodbtypes.AttributeValueMemberS{Value: userID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("query dynamodb: %w", err)
	}

	var schedules []ScheduleRecord
	for _, item := range result.Items {
		var record ScheduleRecord
		if err := attributevalue.UnmarshalMap(item, &record); err != nil {
			continue // Skip malformed records
		}
		schedules = append(schedules, record)
	}

	return schedules, nil
}

// UpdateScheduleStatus updates the status of a schedule
func (c *Client) UpdateScheduleStatus(ctx context.Context, scheduleID string, status ScheduleStatus) error {
	_, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: scheduleID},
		},
		UpdateExpression: aws.String("SET #status = :status, updated_at = :updated_at"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":status":     &dynamodbtypes.AttributeValueMemberS{Value: string(status)},
			":updated_at": &dynamodbtypes.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("update schedule status: %w", err)
	}

	// Determine new EventBridge schedule state.
	var ebState schedulertypes.ScheduleState
	switch status {
	case ScheduleStatusActive:
		ebState = schedulertypes.ScheduleStateEnabled
	case ScheduleStatusPaused, ScheduleStatusCancelled:
		ebState = schedulertypes.ScheduleStateDisabled
	}

	// Load DynamoDB record to reconstruct required EventBridge fields.
	record, err := c.GetSchedule(ctx, scheduleID)
	if err != nil {
		return fmt.Errorf("load schedule for eventbridge update: %w", err)
	}

	updateInput := &scheduler.UpdateScheduleInput{
		Name:               aws.String(scheduleID),
		ScheduleExpression: aws.String(record.ScheduleExpression),
		FlexibleTimeWindow: &schedulertypes.FlexibleTimeWindow{
			Mode: schedulertypes.FlexibleTimeWindowModeOff,
		},
		Target: &schedulertypes.Target{
			Arn:     aws.String(c.lambdaARN),
			RoleArn: aws.String(c.roleARN),
		},
		State: ebState,
	}
	if record.Timezone != "" {
		updateInput.ScheduleExpressionTimezone = aws.String(record.Timezone)
	}

	_, err = c.schedulerClient.UpdateSchedule(ctx, updateInput)
	if err != nil {
		return fmt.Errorf("update eventbridge schedule state: %w", err)
	}

	return nil
}

// DeleteSchedule removes a schedule from both EventBridge and DynamoDB
func (c *Client) DeleteSchedule(ctx context.Context, scheduleID string) error {
	// Delete EventBridge schedule
	_, err := c.schedulerClient.DeleteSchedule(ctx, &scheduler.DeleteScheduleInput{
		Name: aws.String(scheduleID),
	})
	if err != nil {
		// Continue with DynamoDB deletion even if EventBridge fails
		fmt.Printf("Warning: failed to delete EventBridge schedule: %v\n", err)
	}

	// Delete DynamoDB record
	_, err = c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: scheduleID},
		},
	})
	if err != nil {
		return fmt.Errorf("delete schedule metadata: %w", err)
	}

	return nil
}

// RecordExecution stores execution history in DynamoDB
func (c *Client) RecordExecution(ctx context.Context, history *ExecutionHistory) error {
	// Set TTL to 30 days from now
	history.TTL = time.Now().Add(30 * 24 * time.Hour).Unix()

	item, err := attributevalue.MarshalMap(history)
	if err != nil {
		return fmt.Errorf("marshal execution history: %w", err)
	}

	_, err = c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("spawn-schedule-history"),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put execution history: %w", err)
	}

	return nil
}

// GetExecutionHistory retrieves execution history for a schedule
func (c *Client) GetExecutionHistory(ctx context.Context, scheduleID string, limit int) ([]ExecutionHistory, error) {
	input := &dynamodb.QueryInput{
		TableName:              aws.String("spawn-schedule-history"),
		KeyConditionExpression: aws.String("schedule_id = :schedule_id"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: scheduleID},
		},
		ScanIndexForward: aws.Bool(false), // Most recent first
	}

	if limit > 0 {
		input.Limit = aws.Int32(int32(limit))
	}

	result, err := c.dynamoClient.Query(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("query execution history: %w", err)
	}

	var history []ExecutionHistory
	for _, item := range result.Items {
		var h ExecutionHistory
		if err := attributevalue.UnmarshalMap(item, &h); err != nil {
			continue
		}
		history = append(history, h)
	}

	return history, nil
}

// UpdateScheduleMetadata updates schedule metadata after an execution
func (c *Client) UpdateScheduleMetadata(ctx context.Context, scheduleID, sweepID string, nextExecutionTime *time.Time) error {
	updateExpr := "SET execution_count = execution_count + :inc, last_sweep_id = :sweep_id, updated_at = :updated_at"
	exprValues := map[string]dynamodbtypes.AttributeValue{
		":inc":        &dynamodbtypes.AttributeValueMemberN{Value: "1"},
		":sweep_id":   &dynamodbtypes.AttributeValueMemberS{Value: sweepID},
		":updated_at": &dynamodbtypes.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
	}

	if nextExecutionTime != nil {
		updateExpr += ", next_execution_time = :next_exec"
		exprValues[":next_exec"] = &dynamodbtypes.AttributeValueMemberS{
			Value: nextExecutionTime.Format(time.RFC3339),
		}
	}

	_, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: scheduleID},
		},
		UpdateExpression:          aws.String(updateExpr),
		ExpressionAttributeValues: exprValues,
	})
	if err != nil {
		return fmt.Errorf("update schedule metadata: %w", err)
	}

	return nil
}
