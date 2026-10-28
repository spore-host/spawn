package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/aws/smithy-go"
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

// ParamsDeleteAPI is the S3 call that deletes a schedule's params object.
type ParamsDeleteAPI interface {
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// Client handles scheduled execution operations
type Client struct {
	schedulerClient SchedulerAPI
	dynamoClient    DynamoDBAPI
	// paramsS3 returns an S3 client for a params bucket in region: the
	// buckets are per region (the record's Region), and a client in another
	// region gets a 301 for them.
	paramsS3  func(region string) ParamsDeleteAPI
	lambdaARN string
	roleARN   string
	accountID string
	tableName string
}

// NewClient creates a new scheduler client
func NewClient(cfg aws.Config, lambdaARN, roleARN, accountID string) *Client {
	return NewClientWithTableName(cfg, lambdaARN, roleARN, accountID, TableName)
}

// NewClientWithTableName creates a new scheduler client with custom table name
func NewClientWithTableName(cfg aws.Config, lambdaARN, roleARN, accountID, tableName string) *Client {
	return &Client{
		schedulerClient: scheduler.NewFromConfig(cfg),
		dynamoClient:    dynamodb.NewFromConfig(cfg),
		paramsS3: func(region string) ParamsDeleteAPI {
			return s3.NewFromConfig(cfg, func(o *s3.Options) { o.Region = region })
		},
		lambdaARN: lambdaARN,
		roleARN:   roleARN,
		accountID: accountID,
		tableName: tableName,
	}
}

// ErrScheduleNotFound is GetSchedule's error for a schedule with no record.
var ErrScheduleNotFound = errors.New("schedule not found")

// target is the EventBridge target of schedule id. CreateSchedule and
// UpdateScheduleStatus both send it: UpdateSchedule replaces the whole
// schedule, so a field one of them left out was dropped by the other.
func (c *Client) target(id string) *schedulertypes.Target {
	return &schedulertypes.Target{
		Arn:     aws.String(c.lambdaARN),
		RoleArn: aws.String(c.roleARN),
		Input: aws.String(fmt.Sprintf(`{
				"schedule_id": "%s",
				"execution_time": "<aws.scheduler.scheduled-time>"
			}`, id)),
		RetryPolicy: &schedulertypes.RetryPolicy{
			MaximumRetryAttempts:     aws.Int32(2),
			MaximumEventAgeInSeconds: aws.Int32(300), // 5 minutes
		},
	}
}

// scheduleActionAfterCompletion: a schedule that completes (a one-time
// at() schedule after its firing) deletes itself, so a finished schedule
// leaves no EventBridge schedule behind (#1382). Recurring schedules do not
// complete on their own; scheduler-handler's CompleteSchedule deletes them
// when they run out, and `spawn schedule cancel` at any time.
const scheduleActionAfterCompletion = schedulertypes.ActionAfterCompletionDelete

// ScheduleDescriptionPrefix opens the Description of every EventBridge
// Scheduler schedule CreateSchedule makes. `spawn footprint` identifies a
// schedule as spawn's by it when the target does not say so (#1219).
const ScheduleDescriptionPrefix = "Spawn scheduled execution: "

// GenerateScheduleID creates a unique schedule ID
func GenerateScheduleID() string {
	return fmt.Sprintf("sched-%s", time.Now().Format("20060102-150405"))
}

// CreateSchedule creates a new EventBridge schedule and stores metadata in DynamoDB
func (c *Client) CreateSchedule(ctx context.Context, record *ScheduleRecord) (string, error) {
	// A one-time schedule is one execution; saying so lets FiringAt (and so
	// the reaper's role sweep, #1378) see it has run out after its firing.
	if record.ScheduleType == string(ScheduleTypeOneTime) && record.MaxExecutions == 0 {
		record.MaxExecutions = 1
	}

	// Create EventBridge schedule
	scheduleInput := &scheduler.CreateScheduleInput{
		Name:               aws.String(record.ScheduleID),
		Description:        aws.String(ScheduleDescriptionPrefix + record.ScheduleName),
		ScheduleExpression: aws.String(record.ScheduleExpression),
		FlexibleTimeWindow: &schedulertypes.FlexibleTimeWindow{
			Mode: schedulertypes.FlexibleTimeWindowModeOff,
		},
		Target:                c.target(record.ScheduleID),
		ActionAfterCompletion: scheduleActionAfterCompletion,
		State:                 schedulertypes.ScheduleStateEnabled,
	}

	// Add timezone if specified
	if record.Timezone != "" {
		scheduleInput.ScheduleExpressionTimezone = aws.String(record.Timezone)
	}
	setEndDate(&scheduleInput.EndDate, record)

	// Create schedule
	result, err := c.schedulerClient.CreateSchedule(ctx, scheduleInput)
	if err != nil {
		// The caller uploaded the params first; nothing else would delete them.
		_ = c.deleteParams(ctx, record)
		return "", fmt.Errorf("create eventbridge schedule: %w", err)
	}

	scheduleARN := *result.ScheduleArn
	record.ScheduleARN = scheduleARN

	// Save metadata to DynamoDB
	if err := c.SaveSchedule(ctx, record); err != nil {
		// Clean up the schedule and params if DynamoDB save fails. There is
		// no record yet, so this goes by the one in hand.
		_ = c.deleteResources(ctx, record)
		return "", fmt.Errorf("save schedule metadata: %w", err)
	}

	return scheduleARN, nil
}

// setEndDate gives a recurring schedule with EndAfter an EventBridge EndDate
// of EndAfter+RecordTTLMargin, its record's TTL: with ActionAfterCompletion it
// then deletes itself when its record goes. Not EndAfter itself: the first
// firing after EndAfter is the one on which scheduler-handler sees it has run
// out and deletes its params (CompleteSchedule).
func setEndDate(dst **time.Time, record *ScheduleRecord) {
	if record.EndAfter.IsZero() || record.ScheduleType == string(ScheduleTypeOneTime) {
		return
	}
	end := record.EndAfter.Add(RecordTTLMargin)
	*dst = &end
}

// SaveSchedule stores schedule metadata in DynamoDB, with the record TTL
// RecordTTL gives it (none for a schedule with no last firing).
func (c *Client) SaveSchedule(ctx context.Context, record *ScheduleRecord) error {
	record.TTL = record.RecordTTL()

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
		return nil, fmt.Errorf("%w: %s", ErrScheduleNotFound, scheduleID)
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
//
// Resuming (ScheduleStatusActive) is allowed only from paused or active: a
// cancelled or completed schedule's EventBridge schedule and params may be
// gone, and re-activating its record would make it look like it could launch
// again (FiringAt) when it cannot.
func (c *Client) UpdateScheduleStatus(ctx context.Context, scheduleID string, status ScheduleStatus) error {
	in := &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: scheduleID},
		},
		UpdateExpression:    aws.String("SET #status = :status, updated_at = :updated_at"),
		ConditionExpression: aws.String("attribute_exists(schedule_id)"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":status":     &dynamodbtypes.AttributeValueMemberS{Value: string(status)},
			":updated_at": &dynamodbtypes.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		},
	}
	if status == ScheduleStatusActive {
		in.ConditionExpression = aws.String("#status IN (:paused, :active)")
		in.ExpressionAttributeValues[":paused"] = &dynamodbtypes.AttributeValueMemberS{Value: string(ScheduleStatusPaused)}
		in.ExpressionAttributeValues[":active"] = &dynamodbtypes.AttributeValueMemberS{Value: string(ScheduleStatusActive)}
	}
	if _, err := c.dynamoClient.UpdateItem(ctx, in); err != nil {
		var ccf *dynamodbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			if status == ScheduleStatusActive {
				return fmt.Errorf("schedule %s is not paused (only a paused schedule can be resumed; "+
					"a cancelled or completed one cannot run again — create a new schedule)", scheduleID)
			}
			return fmt.Errorf("%w: %s", ErrScheduleNotFound, scheduleID)
		}
		return fmt.Errorf("update schedule status: %w", err)
	}

	// Determine new EventBridge schedule state.
	var ebState schedulertypes.ScheduleState
	switch status {
	case ScheduleStatusActive:
		ebState = schedulertypes.ScheduleStateEnabled
	case ScheduleStatusPaused, ScheduleStatusCancelled, ScheduleStatusCompleted:
		ebState = schedulertypes.ScheduleStateDisabled
	}

	// Load DynamoDB record to reconstruct required EventBridge fields.
	record, err := c.GetSchedule(ctx, scheduleID)
	if err != nil {
		return fmt.Errorf("load schedule for eventbridge update: %w", err)
	}

	updateInput := &scheduler.UpdateScheduleInput{
		Name:               aws.String(scheduleID),
		Description:        aws.String(ScheduleDescriptionPrefix + record.ScheduleName),
		ScheduleExpression: aws.String(record.ScheduleExpression),
		FlexibleTimeWindow: &schedulertypes.FlexibleTimeWindow{
			Mode: schedulertypes.FlexibleTimeWindowModeOff,
		},
		Target:                c.target(scheduleID),
		ActionAfterCompletion: scheduleActionAfterCompletion,
		State:                 ebState,
	}
	if record.Timezone != "" {
		updateInput.ScheduleExpressionTimezone = aws.String(record.Timezone)
	}
	setEndDate(&updateInput.EndDate, record)

	_, err = c.schedulerClient.UpdateSchedule(ctx, updateInput)
	if err != nil {
		return fmt.Errorf("update eventbridge schedule state: %w", err)
	}

	return nil
}

// DeleteSchedule is `spawn schedule cancel`: the named deleter of everything
// `spawn schedule create` made for a schedule (#1382) — its params object,
// its EventBridge schedule and its record, in that order, so the record (the
// only index of the other two) goes last and a failed step can be retried.
// Idempotent: a schedule already gone, wholly or in part, is not an error.
func (c *Client) DeleteSchedule(ctx context.Context, scheduleID string) error {
	record, err := c.GetSchedule(ctx, scheduleID)
	if errors.Is(err, ErrScheduleNotFound) {
		// No record says where the params are. The EventBridge schedule has
		// the same name, so it can still go.
		return c.deleteEventBridgeSchedule(ctx, scheduleID)
	}
	if err != nil {
		return err
	}
	return c.deleteResources(ctx, record)
}

// deleteResources deletes the params, EventBridge schedule and record of
// record.
func (c *Client) deleteResources(ctx context.Context, record *ScheduleRecord) error {
	if err := c.deleteParams(ctx, record); err != nil {
		return err
	}
	if err := c.deleteEventBridgeSchedule(ctx, record.ScheduleID); err != nil {
		return err
	}
	_, err := c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: record.ScheduleID},
		},
	})
	if err != nil {
		return fmt.Errorf("delete schedule metadata: %w", err)
	}
	return nil
}

// deleteParams deletes record's params object. DeleteObject of a missing key
// succeeds, and a missing bucket holds no params, so this is idempotent.
func (c *Client) deleteParams(ctx context.Context, record *ScheduleRecord) error {
	if record.S3ParamsKey == "" || record.Region == "" {
		return nil
	}
	bucket := record.ParamsBucketName(ParamsBucketTemplate)
	_, err := c.paramsS3(record.Region).DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(record.S3ParamsKey),
	})
	var apiErr smithy.APIError
	if err != nil && !(errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket") {
		return fmt.Errorf("delete schedule params s3://%s/%s: %w", bucket, record.S3ParamsKey, err)
	}
	return nil
}

// deleteEventBridgeSchedule deletes the EventBridge schedule named id; one
// already gone (cancelled before, or deleted by ActionAfterCompletion) is not
// an error.
func (c *Client) deleteEventBridgeSchedule(ctx context.Context, id string) error {
	_, err := c.schedulerClient.DeleteSchedule(ctx, &scheduler.DeleteScheduleInput{Name: aws.String(id)})
	var nf *schedulertypes.ResourceNotFoundException
	if err != nil && !errors.As(err, &nf) {
		return fmt.Errorf("delete eventbridge schedule %s: %w", id, err)
	}
	return nil
}

// CompleteSchedule is what scheduler-handler does when a schedule has run
// out (FiringAt says MaxExecutions or Expired, or a one-time schedule has
// fired): it marks the record completed with a TTL of now+RecordTTLMargin,
// then deletes the EventBridge schedule and the params (#1382). The record
// goes first, so nothing reading the table — FiringAt, the reaper's role
// sweep (#1378) — sees an active schedule whose params are gone.
func (c *Client) CompleteSchedule(ctx context.Context, record *ScheduleRecord, now time.Time) error {
	_, err := c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: record.ScheduleID},
		},
		UpdateExpression:    aws.String("SET #status = :status, updated_at = :updated_at, #ttl = :ttl"),
		ConditionExpression: aws.String("attribute_exists(schedule_id)"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
			"#ttl":    "ttl",
		},
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":status":     &dynamodbtypes.AttributeValueMemberS{Value: string(ScheduleStatusCompleted)},
			":updated_at": &dynamodbtypes.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			":ttl":        &dynamodbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Add(RecordTTLMargin).Unix())},
		},
	})
	var ccf *dynamodbtypes.ConditionalCheckFailedException
	if err != nil && !errors.As(err, &ccf) {
		return fmt.Errorf("mark schedule %s completed: %w", record.ScheduleID, err)
	}
	if err := c.deleteEventBridgeSchedule(ctx, record.ScheduleID); err != nil {
		return err
	}
	return c.deleteParams(ctx, record)
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
