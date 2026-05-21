package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gopkg.in/yaml.v3"
)

// Environment variables for self-hosted infrastructure (with fallbacks to shared infrastructure)
const (
	defaultSchedulesTable       = "spawn-schedules"
	defaultHistoryTable         = "spawn-schedule-history"
	defaultSchedulesBucketTmpl  = "spawn-schedules-%s" // %s = region
	defaultOrchestratorFuncName = "sweep-orchestrator"
)

// defaultAccountID is the infra account ID; overridable via SPAWN_INFRA_ACCOUNT_ID.
var defaultAccountID = getEnv("SPAWN_INFRA_ACCOUNT_ID", "966362334030")

// Event is the input from EventBridge Scheduler
type Event struct {
	ScheduleID    string `json:"schedule_id"`
	ExecutionTime string `json:"execution_time"`
}

// ScheduleRecord represents a schedule in DynamoDB
type ScheduleRecord struct {
	ScheduleID         string    `dynamodbav:"schedule_id"`
	UserID             string    `dynamodbav:"user_id"`
	ScheduleName       string    `dynamodbav:"schedule_name"`
	CreatedAt          time.Time `dynamodbav:"created_at"`
	UpdatedAt          time.Time `dynamodbav:"updated_at"`
	ScheduleExpression string    `dynamodbav:"schedule_expression"`
	ScheduleType       string    `dynamodbav:"schedule_type"`
	Timezone           string    `dynamodbav:"timezone"`
	NextExecutionTime  time.Time `dynamodbav:"next_execution_time"`
	S3ParamsKey        string    `dynamodbav:"s3_params_key"`
	SweepName          string    `dynamodbav:"sweep_name"`
	MaxConcurrent      int       `dynamodbav:"max_concurrent"`
	LaunchDelay        string    `dynamodbav:"launch_delay"`
	Region             string    `dynamodbav:"region"`
	Status             string    `dynamodbav:"status"`
	ExecutionCount     int       `dynamodbav:"execution_count"`
	MaxExecutions      int       `dynamodbav:"max_executions,omitempty"`
	EndAfter           time.Time `dynamodbav:"end_after,omitempty"`
	LastSweepID        string    `dynamodbav:"last_sweep_id,omitempty"`
	ScheduleARN        string    `dynamodbav:"schedule_arn,omitempty"`
	TTL                int64     `dynamodbav:"ttl"`
}

// ParamFileFormat matches CLI parameter file structure
type ParamFileFormat struct {
	Defaults map[string]interface{}   `json:"defaults" yaml:"defaults"`
	Params   []map[string]interface{} `json:"params" yaml:"params"`
}

// SweepConfig is the sweep orchestrator input
type SweepConfig struct {
	SweepID       string `json:"sweep_id"`
	SweepName     string `json:"sweep_name"`
	UserID        string `json:"user_id"`
	S3ParamsKey   string `json:"s3_params_key"`
	MaxConcurrent int    `json:"max_concurrent"`
	LaunchDelay   string `json:"launch_delay"`
	Region        string `json:"region"`
	Source        string `json:"source"`
	ScheduleID    string `json:"schedule_id,omitempty"`
}

var (
	dynamoClient         *dynamodb.Client
	lambdaClient         *lambdasvc.Client
	s3Client             *s3.Client
	accountID            string
	schedulesTable       string
	historyTable         string
	schedulesBucketTmpl  string
	orchestratorFuncName string
)

func init() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	dynamoClient = dynamodb.NewFromConfig(cfg)
	lambdaClient = lambdasvc.NewFromConfig(cfg)
	s3Client = s3.NewFromConfig(cfg)

	// Load configuration from environment variables with fallbacks
	accountID = getEnv("SPAWN_ACCOUNT_ID", defaultAccountID)
	schedulesTable = getEnv("SPAWN_SCHEDULES_TABLE", defaultSchedulesTable)
	historyTable = getEnv("SPAWN_SCHEDULE_HISTORY_TABLE", defaultHistoryTable)
	schedulesBucketTmpl = getEnv("SPAWN_SCHEDULES_BUCKET_TEMPLATE", defaultSchedulesBucketTmpl)
	orchestratorFuncName = getEnv("SPAWN_ORCHESTRATOR_FUNCTION_NAME", defaultOrchestratorFuncName)

	log.Printf("Configuration: account=%s, schedules_table=%s, history_table=%s, orchestrator=%s",
		accountID, schedulesTable, historyTable, orchestratorFuncName)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func handler(ctx context.Context, event Event) error {
	log.Printf("Processing schedule: %s at %s", event.ScheduleID, event.ExecutionTime)

	// 1. Load schedule from DynamoDB
	schedule, err := getScheduleRecord(ctx, event.ScheduleID)
	if err != nil {
		return recordError(ctx, event.ScheduleID, "", fmt.Errorf("get schedule: %w", err))
	}

	// 2. Check schedule status
	if schedule.Status != "active" {
		log.Printf("Schedule %s is not active (status: %s), skipping", event.ScheduleID, schedule.Status)
		return nil
	}

	// 3. Check execution limits
	if schedule.MaxExecutions > 0 && schedule.ExecutionCount >= schedule.MaxExecutions {
		log.Printf("Schedule %s reached max executions (%d)", event.ScheduleID, schedule.MaxExecutions)
		return recordError(ctx, event.ScheduleID, "", fmt.Errorf("max executions reached"))
	}

	if !schedule.EndAfter.IsZero() && time.Now().After(schedule.EndAfter) {
		log.Printf("Schedule %s expired (end_after: %s)", event.ScheduleID, schedule.EndAfter)
		return recordError(ctx, event.ScheduleID, "", fmt.Errorf("schedule expired"))
	}

	// 4. Validate parameter file exists in S3 (sweep-orchestrator will download it)
	_, err = downloadParams(ctx, schedule.S3ParamsKey, schedule.Region)
	if err != nil {
		return recordError(ctx, event.ScheduleID, "", fmt.Errorf("validate params: %w", err))
	}

	// 5. Generate sweep ID
	sweepID := fmt.Sprintf("%s-%s", schedule.SweepName, time.Now().Format("20060102-150405"))
	log.Printf("Creating sweep: %s", sweepID)

	// 6. Create sweep configuration
	sweepConfig := SweepConfig{
		SweepID:       sweepID,
		SweepName:     schedule.SweepName,
		UserID:        schedule.UserID,
		S3ParamsKey:   schedule.S3ParamsKey,
		MaxConcurrent: schedule.MaxConcurrent,
		LaunchDelay:   schedule.LaunchDelay,
		Region:        schedule.Region,
		Source:        "scheduled",
		ScheduleID:    event.ScheduleID,
	}

	// 7. Invoke sweep-orchestrator Lambda asynchronously
	sweepPayload, _ := json.Marshal(map[string]interface{}{
		"sweep_id":       sweepConfig.SweepID,
		"force_download": false,
	})

	orchestratorArn := fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", schedule.Region, accountID, orchestratorFuncName)
	_, err = lambdaClient.Invoke(ctx, &lambdasvc.InvokeInput{
		FunctionName:   aws.String(orchestratorArn),
		InvocationType: "Event", // Asynchronous
		Payload:        sweepPayload,
	})
	if err != nil {
		return recordError(ctx, event.ScheduleID, sweepID, fmt.Errorf("invoke orchestrator: %w", err))
	}

	// 8. Record successful execution
	err = recordExecution(ctx, event.ScheduleID, schedule.UserID, sweepID, "success", "")
	if err != nil {
		log.Printf("Warning: failed to record execution: %v", err)
	}

	// 9. Update schedule metadata
	err = updateScheduleMetadata(ctx, event.ScheduleID, sweepID)
	if err != nil {
		log.Printf("Warning: failed to update schedule metadata: %v", err)
	}

	log.Printf("Successfully triggered sweep %s for schedule %s", sweepID, event.ScheduleID)
	return nil
}

func getScheduleRecord(ctx context.Context, scheduleID string) (*ScheduleRecord, error) {
	result, err := dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(schedulesTable),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get item: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("schedule not found: %s", scheduleID)
	}

	var schedule ScheduleRecord
	if err := attributevalue.UnmarshalMap(result.Item, &schedule); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	return &schedule, nil
}

func downloadParams(ctx context.Context, s3Key, region string) (*ParamFileFormat, error) {
	bucket := fmt.Sprintf(schedulesBucketTmpl, region)

	result, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	defer result.Body.Close()

	var params ParamFileFormat
	if err := yaml.NewDecoder(result.Body).Decode(&params); err != nil {
		return nil, fmt.Errorf("decode params: %w", err)
	}

	return &params, nil
}

func recordExecution(ctx context.Context, scheduleID, userID, sweepID, status, errMsg string) error {
	now := time.Now()
	ttl := now.Add(30 * 24 * time.Hour).Unix()

	item := map[string]types.AttributeValue{
		"schedule_id":    &types.AttributeValueMemberS{Value: scheduleID},
		"execution_time": &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
		"sweep_id":       &types.AttributeValueMemberS{Value: sweepID},
		"status":         &types.AttributeValueMemberS{Value: status},
		"user_id":        &types.AttributeValueMemberS{Value: userID},
		"ttl":            &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", ttl)},
	}

	if errMsg != "" {
		item["error_message"] = &types.AttributeValueMemberS{Value: errMsg}
	}

	_, err := dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(historyTable),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put item: %w", err)
	}

	return nil
}

func updateScheduleMetadata(ctx context.Context, scheduleID, sweepID string) error {
	_, err := dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(schedulesTable),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
		UpdateExpression: aws.String("SET execution_count = execution_count + :inc, last_sweep_id = :sweep_id, updated_at = :updated_at"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":inc":        &types.AttributeValueMemberN{Value: "1"},
			":sweep_id":   &types.AttributeValueMemberS{Value: sweepID},
			":updated_at": &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("update item: %w", err)
	}

	return nil
}

func recordError(ctx context.Context, scheduleID, sweepID string, err error) error {
	log.Printf("Error processing schedule %s: %v", scheduleID, err)

	// Try to get schedule for user ID
	schedule, getErr := getScheduleRecord(ctx, scheduleID)
	userID := "unknown"
	if getErr == nil {
		userID = schedule.UserID
	}

	// Record error in history
	_ = recordExecution(ctx, scheduleID, userID, sweepID, "failed", err.Error())

	return err
}

func main() {
	lambda.Start(handler)
}
