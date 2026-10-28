package main

import (
	"bytes"
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
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/sweep"
	"gopkg.in/yaml.v3"
)

// Environment variables for self-hosted infrastructure (with fallbacks to shared infrastructure)
const (
	defaultSchedulesTable      = scheduler.TableName
	defaultHistoryTable        = "spawn-schedule-history"
	defaultSchedulesBucketTmpl = scheduler.ParamsBucketTemplate // %s = region
	// The deployed function is spawn-sweep-orchestrator — scripts/deploy-sweep-orchestrator.sh
	// creates exactly that name. This constant said "sweep-orchestrator" and so
	// every scheduled sweep invoked an ARN for a function that has never existed,
	// failing with ResourceNotFoundException (#812). Verified against the live
	// account: `sweep-orchestrator` does not exist, `spawn-sweep-orchestrator`
	// does, and the deployed scheduler-handler had no environment override.
	//
	// #790 was the same class in the other direction — a name with an extra
	// prefix. Both are now gated by lambda_names_test.go.
	defaultOrchestratorFuncName = "spawn-sweep-orchestrator"
	defaultOrchestratorRegion   = "us-east-1" // where the single orchestrator is deployed
	// The orchestrator loads its state from this table by sweep ID (#1026).
	defaultSweepTable = "spawn-sweep-orchestration"
)

// defaultAccountID is the infra account ID; overridable via SPAWN_INFRA_ACCOUNT_ID.
var defaultAccountID = getEnv("SPAWN_INFRA_ACCOUNT_ID", "966362334030")

// Event is the input from EventBridge Scheduler
type Event struct {
	ScheduleID    string `json:"schedule_id"`
	ExecutionTime string `json:"execution_time"`
}

// ScheduleRecord is the record `spawn schedule create` writes. Shared with the
// CLI rather than copied, so a field the CLI adds (aws_account_id, #1026) cannot
// be silently dropped on read here.
type ScheduleRecord = scheduler.ScheduleRecord

// ParamFileFormat matches CLI parameter file structure
type ParamFileFormat struct {
	Defaults map[string]interface{}   `json:"defaults" yaml:"defaults"`
	Params   []map[string]interface{} `json:"params" yaml:"params"`
}

var (
	dynamoClient         *dynamodb.Client
	lambdaClient         *lambdasvc.Client
	awsCfg               aws.Config
	schedules            *scheduler.Client
	accountID            string
	schedulesTable       string
	historyTable         string
	schedulesBucketTmpl  string
	orchestratorFuncName string
	orchestratorRegion   string

	// The orchestrator's table and params bucket live in ITS region, which is
	// not necessarily this function's (#1015's latent note), so they get their
	// own clients.
	orchestratorDynamo *dynamodb.Client
	orchestratorS3     *s3.Client
	sweepTable         string
	sweepsBucket       string
)

func init() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	awsCfg = cfg
	dynamoClient = dynamodb.NewFromConfig(cfg)
	lambdaClient = lambdasvc.NewFromConfig(cfg)

	// Load configuration from environment variables with fallbacks
	accountID = getEnv("SPAWN_ACCOUNT_ID", defaultAccountID)
	schedulesTable = getEnv("SPAWN_SCHEDULES_TABLE", defaultSchedulesTable)
	// Completes run-out schedules (#1382): this function's own region, where
	// the table and the EventBridge schedules are.
	schedules = scheduler.NewClientWithTableName(cfg, "", "", accountID, schedulesTable)
	historyTable = getEnv("SPAWN_SCHEDULE_HISTORY_TABLE", defaultHistoryTable)
	schedulesBucketTmpl = getEnv("SPAWN_SCHEDULES_BUCKET_TEMPLATE", defaultSchedulesBucketTmpl)
	orchestratorFuncName = getEnv("SPAWN_ORCHESTRATOR_FUNCTION_NAME", defaultOrchestratorFuncName)
	orchestratorRegion = getEnv("SPAWN_ORCHESTRATOR_REGION", defaultOrchestratorRegion)
	sweepTable = getEnv("SPAWN_SWEEP_ORCHESTRATION_TABLE", defaultSweepTable)
	sweepsBucket = getEnv("SPAWN_SWEEPS_BUCKET", sweep.SweepsBucket(orchestratorRegion))

	orchCfg := cfg.Copy()
	orchCfg.Region = orchestratorRegion
	orchestratorDynamo = dynamodb.NewFromConfig(orchCfg)
	orchestratorS3 = s3.NewFromConfig(orchCfg)

	log.Printf("Configuration: account=%s, schedules_table=%s, history_table=%s, orchestrator=%s in %s",
		accountID, schedulesTable, historyTable, orchestratorFuncName, orchestratorRegion)
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

	// 2-3. Status and execution limits. scheduler.FiringAt is this decision,
	// shared with pkg/instancerole, which keeps the roles of exactly the
	// schedules that would launch here (#1378).
	switch schedule.FiringAt(time.Now()) {
	case scheduler.FiringNotActive:
		log.Printf("Schedule %s is not active (status: %s), skipping", event.ScheduleID, schedule.Status)
		return nil
	case scheduler.FiringMaxExecutions:
		log.Printf("Schedule %s reached max executions (%d)", event.ScheduleID, schedule.MaxExecutions)
		_ = recordError(ctx, event.ScheduleID, "", fmt.Errorf("max executions reached"))
		return completeRunOut(ctx, schedules, schedule, time.Now())
	case scheduler.FiringExpired:
		log.Printf("Schedule %s expired (end_after: %s)", event.ScheduleID, schedule.EndAfter)
		_ = recordError(ctx, event.ScheduleID, "", fmt.Errorf("schedule expired"))
		return completeRunOut(ctx, schedules, schedule, time.Now())
	case scheduler.FiringLaunches:
	}

	// 4. Load the parameter file the CLI uploaded.
	params, err := downloadParams(ctx, schedule.S3ParamsBucket, schedule.S3ParamsKey, schedule.Region)
	if err != nil {
		return recordError(ctx, event.ScheduleID, "", fmt.Errorf("load params: %w", err))
	}

	// 5. Generate sweep ID
	sweepID := fmt.Sprintf("%s-%s", schedule.SweepName, time.Now().Format("20060102-150405"))
	log.Printf("Creating sweep: %s", sweepID)

	// 6. Build what the orchestrator consumes. This used to build a SweepConfig
	// and then send only its sweep ID, writing no record — so the orchestrator's
	// first step, loading the record by that ID, failed for every scheduled
	// sweep (#1026).
	prepared, err := prepareSweep(schedule, params, sweepID, sweepsBucket, time.Now())
	if err != nil {
		return recordError(ctx, event.ScheduleID, sweepID, err)
	}

	// 7. Stage the params, write the record, then invoke.
	orchestratorArn := fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", orchestratorRegion, accountID, orchestratorFuncName)
	if err := startSweep(ctx, sweepStarter{
		s3:              orchestratorS3,
		dynamo:          orchestratorDynamo,
		lambda:          lambdaClient,
		sweepTable:      sweepTable,
		orchestratorArn: orchestratorArn,
	}, prepared); err != nil {
		return recordError(ctx, event.ScheduleID, sweepID, err)
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
	} else {
		schedule.ExecutionCount++
	}

	// 10. If that was its last run, the schedule is done: delete what it
	// leaves behind now rather than on a firing that may never come.
	if lastRun(schedule, time.Now()) {
		if err := completeRunOut(ctx, schedules, schedule, time.Now()); err != nil {
			log.Printf("Warning: %v", err)
		}
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

// downloadParams fetches the schedule's parameter file.
//
// recordedBucket is the bucket the CLI actually wrote to, persisted on the record
// since #804. Before that the record carried only a key and this function rebuilt
// the name from the region — but resolveSchedulesBucket falls back to an
// account-scoped `spawn-schedules-<account>-<region>` when the shared name is
// taken by another account, so in any account that took the fallback every
// scheduled execution read a bucket it had never written to.
//
// Empty means a record written before the field existed. Falling back to the
// template is right for those: it is what they were created under, and it is the
// shared name, since an account on the fallback path would have been failing
// already.
func downloadParams(ctx context.Context, recordedBucket, s3Key, region string) (*ParamFileFormat, error) {
	bucket := paramsBucket(recordedBucket, region)

	// A client in the bucket's region: the bucket is spawn-schedules-<the
	// schedule's Region>, and from this function's region S3 answers a bucket
	// elsewhere with a 301 (#1383).
	result, err := paramsS3Client(awsCfg, region).GetObject(ctx, &s3.GetObjectInput{
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

// paramsS3Client is an S3 client for a params bucket in region.
func paramsS3Client(cfg aws.Config, region string) *s3.Client {
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.Region = region })
}

// completer is scheduler.Client.CompleteSchedule, behind an interface so the
// decision to call it is testable without AWS.
type completer interface {
	CompleteSchedule(ctx context.Context, record *scheduler.ScheduleRecord, now time.Time) error
}

// completeRunOut deletes a run-out schedule's EventBridge schedule and params
// and gives its record a TTL (#1382). Before this, a schedule that hit
// max_executions or end_after kept firing into this handler forever, and its
// params and record stayed until someone cancelled it.
func completeRunOut(ctx context.Context, c completer, schedule *ScheduleRecord, now time.Time) error {
	if err := c.CompleteSchedule(ctx, schedule, now); err != nil {
		return fmt.Errorf("complete run-out schedule %s: %w", schedule.ScheduleID, err)
	}
	log.Printf("Schedule %s has run out: deleted its EventBridge schedule and params; its record expires in %s",
		schedule.ScheduleID, scheduler.RecordTTLMargin)
	return nil
}

// lastRun reports whether a schedule that has just launched (with
// ExecutionCount already counting that launch) will launch no more: a one-time
// schedule, or one FiringAt now says has reached MaxExecutions. EndAfter is
// left to the first firing after it, which FiringAt reports as expired: until
// then the next firing may still launch.
func lastRun(schedule *ScheduleRecord, now time.Time) bool {
	return schedule.ScheduleType == string(scheduler.ScheduleTypeOneTime) ||
		schedule.FiringAt(now) == scheduler.FiringMaxExecutions
}

// paramsBucket decides which bucket holds a schedule's parameter file.
//
// Pure, and separate from downloadParams, so the decision can be tested without
// an S3 client — the thing that matters is "a recorded bucket is used", and a
// gate reading the source for the ORDER of two expressions was checking style
// rather than mechanism: an implementation that computes the template first and
// then overrides it is equally correct, and passed that gate while a broken one
// could too.
func paramsBucket(recordedBucket, region string) string {
	rec := scheduler.ScheduleRecord{S3ParamsBucket: recordedBucket, Region: region}
	if recordedBucket != "" {
		return rec.ParamsBucketName(schedulesBucketTmpl)
	}
	fallback := rec.ParamsBucketName(schedulesBucketTmpl)
	log.Printf("schedule params: record carries no s3_params_bucket (pre-#804); "+
		"falling back to the region template %q", fallback)
	return fallback
}

// preparedSweep is everything the handler sends the orchestrator's way.
type preparedSweep struct {
	Record       *sweep.SweepRecord
	ParamsBucket string
	ParamsKey    string
	ParamsBody   []byte
	Payload      []byte
}

// prepareSweep turns a firing schedule into the staged params, sweep record and
// invoke payload the orchestrator consumes — all built by pkg/sweep, the code
// `spawn launch --param-file` uses, so the two callers cannot drift apart again.
// Pure, so the contract is testable without AWS.
func prepareSweep(schedule *ScheduleRecord, params *ParamFileFormat, sweepID, paramsBucket string, now time.Time) (*preparedSweep, error) {
	sp := &sweep.ParamFileFormat{Defaults: params.Defaults, Params: params.Params}
	record, err := sweep.NewScheduledSweepRecord(sweep.ScheduledSweep{
		SweepID:       sweepID,
		SweepName:     schedule.SweepName,
		UserID:        schedule.UserID,
		ScheduleID:    schedule.ScheduleID,
		AccountID:     schedule.AWSAccountID,
		Region:        schedule.Region,
		MaxConcurrent: schedule.MaxConcurrent,
		LaunchDelay:   schedule.LaunchDelay,
		ParamsBucket:  paramsBucket,
		Params:        sp,
	}, now)
	if err != nil {
		return nil, err
	}
	body, err := sweep.EncodeParams(sp)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	payload, err := json.Marshal(sweep.OrchestratorEvent{SweepID: sweepID})
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	return &preparedSweep{
		Record:       record,
		ParamsBucket: paramsBucket,
		ParamsKey:    sweep.ParamsObjectKey(sweepID),
		ParamsBody:   body,
		Payload:      payload,
	}, nil
}

type paramsPutter interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type recordPutter interface {
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

type invoker interface {
	Invoke(context.Context, *lambdasvc.InvokeInput, ...func(*lambdasvc.Options)) (*lambdasvc.InvokeOutput, error)
}

// sweepStarter is the three calls that start a sweep, behind interfaces so the
// order and content can be tested without AWS.
type sweepStarter struct {
	s3              paramsPutter
	dynamo          recordPutter
	lambda          invoker
	sweepTable      string
	orchestratorArn string
}

// startSweep stages the params, writes the sweep record, then invokes the
// orchestrator — the order `spawn launch --param-file` uses, and the only one in
// which the orchestrator finds both when it starts. The handler used to do only
// the last step (#1026).
//
// orchestratorArn is the orchestrator's OWN region, not the schedule's. The
// orchestrator is deployed once, in the infra account's us-east-1;
// schedule.Region is where the sweep's INSTANCES go, which is a different axis.
// Interpolating it meant a schedule targeting any other region invoked an ARN in
// a region the orchestrator has never been deployed to (#812).
func startSweep(ctx context.Context, d sweepStarter, p *preparedSweep) error {
	if _, err := d.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(p.ParamsBucket),
		Key:         aws.String(p.ParamsKey),
		Body:        bytes.NewReader(p.ParamsBody),
		ContentType: aws.String("application/json"),
	}); err != nil {
		return fmt.Errorf("stage params: %w", err)
	}
	item, err := attributevalue.MarshalMap(p.Record)
	if err != nil {
		return fmt.Errorf("marshal sweep record: %w", err)
	}
	if _, err := d.dynamo.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.sweepTable),
		Item:      item,
	}); err != nil {
		return fmt.Errorf("write sweep record: %w", err)
	}
	if _, err := d.lambda.Invoke(ctx, &lambdasvc.InvokeInput{
		FunctionName:   aws.String(d.orchestratorArn),
		InvocationType: "Event", // Asynchronous
		Payload:        p.Payload,
	}); err != nil {
		return fmt.Errorf("invoke orchestrator: %w", err)
	}
	return nil
}
