package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/smithy-go"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/ttl"
)

const (
	defaultTableName = "spawn-pipeline-orchestration"
	maxExecutionDur  = 13 * time.Minute
	pollInterval     = 10 * time.Second

	// defaultStageRegion is where a stage with no region launches. The launch,
	// the user data and the cleanup all go through stageRegion so they agree;
	// cleanup used to hardcode it while launch used the Lambda's own region.
	defaultStageRegion = "us-east-1"

	// defaultStageTimeout bounds a stage command that declares no timeout.
	defaultStageTimeout = time.Hour

	// stageBootstrapAllowance is the time a stage instance needs on top of its
	// command's timeout: downloading spored, installing the AWS CLI, writing the
	// marker and the 60s linger before it terminates itself. A stage's TTL is
	// its timeout plus this, so the reaper never reclaims a stage that is still
	// inside its own timeout (#912).
	stageBootstrapAllowance = 30 * time.Minute

	// pipelineDeadlineSlack is added to the sum of every stage's TTL to give
	// the whole pipeline's deadline. Past it the orchestrator fails the
	// pipeline, terminates what is left and stops re-invoking itself (#912).
	pipelineDeadlineSlack = 30 * time.Minute
)

// markerAPI is the S3 call the orchestrator uses to read a stage's
// COMPLETE/FAILED marker. An interface so tests can supply the markers.
type markerAPI interface {
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// stageEC2API is what the orchestrator needs from EC2 in a stage's region to
// watch and reclaim that stage's instances.
type stageEC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// Seams over the AWS side effects, replaced in tests.
var (
	markers      markerAPI
	ec2ForRegion = func(ctx context.Context, region string) (stageEC2API, error) {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			return nil, err
		}
		return ec2.NewFromConfig(cfg), nil
	}
	loadStateFn  = loadPipelineState
	saveStateFn  = savePipelineState
	reinvokeFn   = reinvokeSelf
	notifyFn     = sendNotification
	cleanupNetFn = cleanupNetworkResources
)

// PipelineEvent is the Lambda input event
type PipelineEvent struct {
	PipelineID string `json:"pipeline_id"`
}

var (
	awsCfg         aws.Config
	dynamodbClient *dynamodb.Client
	s3Client       *s3.Client
	lambdaClient   *lambdasvc.Client
	snsClient      *sns.Client
	tableName      string
)

func init() {
	var err error
	awsCfg, err = config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	dynamodbClient = dynamodb.NewFromConfig(awsCfg)
	s3Client = s3.NewFromConfig(awsCfg)
	lambdaClient = lambdasvc.NewFromConfig(awsCfg)
	snsClient = sns.NewFromConfig(awsCfg)
	markers = s3Client

	tableName = getEnv("SPAWN_PIPELINE_ORCHESTRATION_TABLE", defaultTableName)
	log.Printf("Configuration: table=%s", tableName)
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context, event PipelineEvent) error {
	log.Printf("Starting pipeline orchestration for pipeline_id=%s", event.PipelineID)

	// Load state from DynamoDB
	state, err := loadPipelineState(ctx, event.PipelineID)
	if err != nil {
		return fmt.Errorf("failed to load pipeline state: %w", err)
	}

	// Download pipeline definition from S3
	pipelineDef, err := downloadPipelineDefinition(ctx, state.S3ConfigKey)
	if err != nil {
		return fmt.Errorf("failed to download pipeline definition: %w", err)
	}

	// Check if pipeline is already in terminal state
	if state.Status == pipeline.StatusCompleted || state.Status == pipeline.StatusFailed || state.Status == pipeline.StatusCancelled {
		log.Printf("Pipeline already in terminal state: %s. Exiting without reinvocation.", state.Status)
		return nil
	}

	// Setup resources if INITIALIZING
	if state.Status == pipeline.StatusInitializing {
		log.Println("Setting up pipeline resources...")
		if err := setupPipelineResources(ctx, state, pipelineDef); err != nil {
			state.Status = pipeline.StatusFailed
			state.CompletedAt = timePtr(time.Now())
			_ = savePipelineState(ctx, state)
			return fmt.Errorf("failed to setup resources: %w", err)
		}
		state.Status = pipeline.StatusRunning
		if err := savePipelineState(ctx, state); err != nil {
			return fmt.Errorf("failed to update status to RUNNING: %w", err)
		}
	}

	// Run polling loop
	return runPipelinePollingLoop(ctx, state, pipelineDef, event.PipelineID)
}

func loadPipelineState(ctx context.Context, pipelineID string) (*pipeline.PipelineState, error) {
	result, err := dynamodbClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"pipeline_id": &types.AttributeValueMemberS{Value: pipelineID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dynamodb get failed: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("pipeline %s not found", pipelineID)
	}

	var state pipeline.PipelineState
	if err := attributevalue.UnmarshalMap(result.Item, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	return &state, nil
}

func savePipelineState(ctx context.Context, state *pipeline.PipelineState) error {
	state.UpdatedAt = time.Now()

	item, err := attributevalue.MarshalMap(state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Retry logic with exponential backoff
	for retries := 0; retries < 3; retries++ {
		_, err = dynamodbClient.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(tableName),
			Item:      item,
		})
		if err == nil {
			return nil
		}
		log.Printf("DynamoDB write failed (attempt %d/3): %v", retries+1, err)
		time.Sleep(time.Duration(retries+1) * time.Second)
	}

	return fmt.Errorf("failed to save state after 3 retries: %w", err)
}

func downloadPipelineDefinition(ctx context.Context, s3Key string) (*pipeline.Pipeline, error) {
	// Parse S3 key (format: s3://bucket/key)
	parts := strings.SplitN(strings.TrimPrefix(s3Key, "s3://"), "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid S3 key format: %s", s3Key)
	}
	bucket, key := parts[0], parts[1]

	// Check cache in /tmp
	cacheFile := fmt.Sprintf("/tmp/%s", strings.ReplaceAll(key, "/", "_"))
	if data, err := os.ReadFile(cacheFile); err == nil {
		log.Printf("Using cached pipeline definition from %s", cacheFile)
		var p pipeline.Pipeline
		if err := json.Unmarshal(data, &p); err == nil {
			return &p, nil
		}
	}

	// Download from S3
	log.Printf("Downloading pipeline definition from s3://%s/%s", bucket, key)
	result, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get failed: %w", err)
	}
	defer func() { _ = result.Body.Close() }()

	var p pipeline.Pipeline
	if err := json.NewDecoder(result.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("failed to decode pipeline: %w", err)
	}

	// Cache to /tmp
	if data, err := json.Marshal(p); err == nil {
		_ = os.WriteFile(cacheFile, data, 0644)
	}

	return &p, nil
}

func setupPipelineResources(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline) error {
	// Initialize stages from pipeline definition
	log.Printf("Initializing %d stages from pipeline definition", len(pipelineDef.Stages))
	state.Stages = make([]pipeline.StageState, len(pipelineDef.Stages))

	// Get topological order for stage indices
	topoOrder, err := pipelineDef.GetTopologicalOrder()
	if err != nil {
		return fmt.Errorf("get topological order: %w", err)
	}

	// Create index map
	indexMap := make(map[string]int)
	for i, stageID := range topoOrder {
		indexMap[stageID] = i
	}

	// Initialize each stage with pending status
	for i, stageSpec := range pipelineDef.Stages {
		state.Stages[i] = pipeline.StageState{
			StageID:       stageSpec.StageID,
			StageIndex:    indexMap[stageSpec.StageID],
			Status:        pipeline.StageStatusPending,
			Instances:     []pipeline.InstanceInfo{},
			InstanceHours: 0,
			StageCostUSD:  0,
		}
		log.Printf("Initialized stage %s (index=%d, status=pending)", stageSpec.StageID, indexMap[stageSpec.StageID])
	}

	// Create security group if streaming stages exist
	if pipelineDef.HasStreamingStages() {
		log.Println("Pipeline has streaming stages, creating security group...")
		sgID, err := createStreamingSecurityGroup(ctx, state.PipelineID)
		if err != nil {
			return fmt.Errorf("create security group: %w", err)
		}
		state.SecurityGroupID = sgID
		log.Printf("Created security group: %s", sgID)
	}

	// Create placement group if EFA stages exist
	if pipelineDef.HasEFAStages() {
		log.Println("Pipeline has EFA stages, creating placement group...")
		pgID, err := createPlacementGroup(ctx, state.PipelineID)
		if err != nil {
			return fmt.Errorf("create placement group: %w", err)
		}
		state.PlacementGroupID = pgID
		log.Printf("Created placement group: %s", pgID)
	}

	return nil
}

func createStreamingSecurityGroup(ctx context.Context, pipelineID string) (string, error) {
	// Get default region EC2 client
	ec2Client := ec2.NewFromConfig(awsCfg)

	sgName := fmt.Sprintf("spawn-pipeline-%s", pipelineID)
	description := fmt.Sprintf("Security group for spawn pipeline %s", pipelineID)

	// Create security group
	createResult, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String(description),
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeSecurityGroup,
				// These carried NO spawn:managed tag, so the reaper was not even
				// permitted to touch them — its terminate/delete grants are
				// conditioned on it — and spawn:created was absent too. Both are
				// supplied by LifecycleTags now.
				Tags: append(spawnaws.LifecycleTags(sgName, "pipeline", time.Now()),
					ec2types.Tag{Key: aws.String("spawn:pipeline-id"), Value: aws.String(pipelineID)}),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create security group failed: %w", err)
	}

	sgID := *createResult.GroupId

	// Add ingress rules: allow all TCP 50000-60000 between pipeline instances (self-referential)
	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []ec2types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(50000),
				ToPort:     aws.Int32(60000),
				UserIdGroupPairs: []ec2types.UserIdGroupPair{
					{GroupId: aws.String(sgID)}, // Self-referential
				},
			},
			// SSH access
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges: []ec2types.IpRange{
					{CidrIp: aws.String("0.0.0.0/0")},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("authorize ingress failed: %w", err)
	}

	return sgID, nil
}

func createPlacementGroup(ctx context.Context, pipelineID string) (string, error) {
	ec2Client := ec2.NewFromConfig(awsCfg)

	pgName := fmt.Sprintf("spawn-pipeline-%s", pipelineID)

	_, err := ec2Client.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
		GroupName: aws.String(pgName),
		Strategy:  ec2types.PlacementStrategyCluster,
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypePlacementGroup,
				Tags: append(spawnaws.LifecycleTags(pgName, "pipeline", time.Now()),
					ec2types.Tag{Key: aws.String("spawn:pipeline-id"), Value: aws.String(pipelineID)}),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create placement group failed: %w", err)
	}

	return pgName, nil
}

func runPipelinePollingLoop(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline, pipelineID string) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	invocationDeadline := time.Now().Add(maxExecutionDur)

	// Get topological order
	order, err := pipelineDef.GetTopologicalOrder()
	if err != nil {
		return fmt.Errorf("get topological order: %w", err)
	}

	// The pipeline deadline is anchored to CreatedAt, which `spawn pipeline
	// launch` always writes. Should a record lack it, anchor it now rather than
	// leave the pipeline unbounded.
	if state.CreatedAt.IsZero() {
		state.CreatedAt = time.Now()
		if err := saveStateFn(ctx, state); err != nil {
			log.Printf("Failed to save anchored created_at: %v", err)
		}
	}

	for {
		done, err := pollOnce(ctx, state, pipelineDef, order, time.Now())
		if err != nil || done {
			return err
		}

		// Re-invoke before the Lambda's own timeout. pollOnce has already
		// failed the pipeline if it is past its deadline, so this chain is
		// bounded (#912): it used to continue every ~13 minutes indefinitely.
		if time.Now().After(invocationDeadline) {
			log.Println("Approaching Lambda timeout, re-invoking...")
			if err := saveStateFn(ctx, state); err != nil {
				log.Printf("Failed to save state before re-invocation: %v", err)
			}
			return reinvokeFn(ctx, pipelineID)
		}

		// Wait for next poll
		<-ticker.C
	}
}

// pollOnce runs one orchestration pass. It returns done=true once the pipeline
// has reached a terminal state, after its instances have been terminated.
func pollOnce(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline, order []string, now time.Time) (bool, error) {
	// Reload state to check for cancellation
	currentState, err := loadStateFn(ctx, state.PipelineID)
	if err != nil {
		log.Printf("Failed to reload pipeline state: %v", err)
	} else if currentState.CancelRequested {
		log.Println("Cancellation requested, stopping orchestration")
		finishPipeline(ctx, state, pipelineDef, pipeline.StatusCancelled, "pipeline cancelled")
		return true, nil
	}

	// The whole pipeline's deadline (#912). Without it a stage that never
	// reported anything kept the orchestrator re-invoking itself forever.
	if deadline := pipelineDeadline(state, pipelineDef); now.After(deadline) {
		reason := fmt.Sprintf("pipeline exceeded its deadline of %s (created %s)",
			deadline.UTC().Format(time.RFC3339), state.CreatedAt.UTC().Format(time.RFC3339))
		log.Printf("Pipeline %s: %s; failing it and terminating its instances", state.PipelineID, reason)
		finishPipeline(ctx, state, pipelineDef, pipeline.StatusFailed, reason)
		msg := fmt.Sprintf("Pipeline %s (%s) failed: %s.\n\nIts remaining instances were terminated.",
			state.PipelineName, state.PipelineID, reason)
		if err := notifyFn(ctx, state, "Spawn Pipeline: FAILED (deadline exceeded)", msg); err != nil {
			log.Printf("Warning: Failed to send deadline notification: %v", err)
		}
		return true, nil
	}

	// Process stages in topological order
	madeProgress := false
	for _, stageID := range order {
		stageState := getStageState(state, stageID)
		if stageState == nil {
			log.Printf("Warning: stage %s not found in state", stageID)
			continue
		}

		stageDef := pipelineDef.GetStage(stageID)
		if stageDef == nil {
			log.Printf("Warning: stage %s not found in definition", stageID)
			continue
		}

		switch stageState.Status {
		case pipeline.StageStatusPending:
			// A stage whose dependency failed or was skipped can never run.
			// It used to stay pending forever, so on_failure=continue never
			// let the pipeline finish (#912).
			if dep := blockingDependency(state, stageDef); dep != "" {
				log.Printf("Stage %s skipped: dependency %s did not complete", stageID, dep)
				stageState.Status = pipeline.StageStatusSkipped
				stageState.ErrorMessage = fmt.Sprintf("skipped: dependency %s did not complete", dep)
				madeProgress = true
			} else if dependenciesMet(state, stageDef) {
				log.Printf("Stage %s dependencies met, marking as ready", stageID)
				stageState.Status = pipeline.StageStatusReady
				madeProgress = true
			}

		case pipeline.StageStatusReady:
			// Launch stage
			log.Printf("Launching stage %s", stageID)
			if err := launchStage(ctx, state, stageDef, stageState); err != nil {
				log.Printf("Failed to launch stage %s: %v", stageID, err)
				// Instances launched before the failing one are terminated
				// by failStage rather than left running.
				stageState.LaunchedAt = timePtr(now)
				failStage(ctx, state, pipelineDef, stageState, err.Error())
			} else {
				stageState.Status = pipeline.StageStatusLaunching
				stageState.LaunchedAt = timePtr(now)
			}
			madeProgress = true

		case pipeline.StageStatusLaunching:
			// Check if instances are running
			if allInstancesRunning(ctx, stageState) {
				log.Printf("Stage %s instances all running", stageID)
				stageState.Status = pipeline.StageStatusRunning
				madeProgress = true
			}

		case pipeline.StageStatusRunning:
			outcome, reason, err := checkStageOutcome(ctx, state, stageDef, stageState, now)
			if err != nil {
				log.Printf("Error checking stage %s outcome: %v", stageID, err)
				continue
			}
			switch outcome {
			case stageFailed:
				log.Printf("Stage %s failed: %s", stageID, reason)
				failStage(ctx, state, pipelineDef, stageState, reason)
				madeProgress = true

			case stageCompleted:
				log.Printf("Stage %s completed", stageID)
				stageState.Status = pipeline.StageStatusCompleted
				stageState.CompletedAt = timePtr(now)
				state.CompletedStages++

				// Calculate cost
				calculateStageCost(stageState)
				state.CurrentCostUSD += stageState.StageCostUSD
				madeProgress = true

				// Check budget
				if state.MaxCostUSD != nil && state.CurrentCostUSD > *state.MaxCostUSD {
					log.Printf("Budget exceeded: $%.2f > $%.2f", state.CurrentCostUSD, *state.MaxCostUSD)
					// This used to set CANCELLED and `break` out of the
					// switch only: the loop carried on, the status was never
					// saved, and the next invocation boundary returned
					// without terminating anything. Finish the pipeline here.
					finishPipeline(ctx, state, pipelineDef, pipeline.StatusCancelled, "budget exceeded")

					// Send budget exceeded notification
					msg := fmt.Sprintf("Pipeline %s (%s) cancelled due to budget exceeded.\n\nCurrent cost: $%.2f\nBudget limit: $%.2f\nStages completed: %d/%d",
						state.PipelineID, state.PipelineName, state.CurrentCostUSD, *state.MaxCostUSD,
						state.CompletedStages, state.TotalStages)
					if err := notifyFn(ctx, state, "Spawn Pipeline: Budget Exceeded", msg); err != nil {
						log.Printf("Warning: Failed to send budget notification: %v", err)
					}
					return true, nil
				}
			}
		}
	}

	// Save state if progress made
	if madeProgress {
		if err := saveStateFn(ctx, state); err != nil {
			log.Printf("Failed to save state: %v", err)
		}
	}

	// Check for completion
	if allStagesComplete(state) {
		log.Println("All stages completed")
		status := pipeline.StatusCompleted
		if state.FailedStages > 0 {
			status = pipeline.StatusFailed
		}
		finishPipeline(ctx, state, pipelineDef, status, "")

		// Send completion notification
		duration := state.CompletedAt.Sub(state.CreatedAt)
		subject := fmt.Sprintf("Spawn Pipeline: %s", state.Status)
		msg := fmt.Sprintf("Pipeline %s (%s) %s.\n\nStages completed: %d\nStages failed: %d\nTotal cost: $%.2f\nDuration: %v\n\nPipeline ID: %s",
			state.PipelineName, state.PipelineID, strings.ToLower(string(state.Status)),
			state.CompletedStages, state.FailedStages, state.CurrentCostUSD, duration.Round(time.Second),
			state.PipelineID)
		if err := notifyFn(ctx, state, subject, msg); err != nil {
			log.Printf("Warning: Failed to send completion notification: %v", err)
		}
		return true, nil
	}

	return false, nil
}

// finishPipeline puts the pipeline in a terminal status: every stage that has
// not finished is marked skipped, every instance still alive is terminated, the
// state is saved and the pipeline's network resources are deleted. Every
// terminal path goes through it, so none can return with instances running.
func finishPipeline(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline, status pipeline.PipelineStatus, reason string) {
	now := time.Now()
	for i := range state.Stages {
		st := &state.Stages[i]
		switch st.Status {
		case pipeline.StageStatusCompleted, pipeline.StageStatusFailed, pipeline.StageStatusSkipped:
			continue
		}
		wasRunning := st.Status == pipeline.StageStatusLaunching || st.Status == pipeline.StageStatusRunning
		st.Status = pipeline.StageStatusSkipped
		if reason != "" {
			st.ErrorMessage = "skipped: " + reason
		}
		if wasRunning {
			st.CompletedAt = timePtr(now)
			calculateStageCost(st)
			state.CurrentCostUSD += st.StageCostUSD
		}
	}

	terminateAllInstances(ctx, state, pipelineDef)

	state.Status = status
	state.CompletedAt = timePtr(now)
	if err := saveStateFn(ctx, state); err != nil {
		log.Printf("Failed to save %s state: %v", status, err)
	}
	cleanupNetFn(ctx, state)
}

// failStage marks a stage failed, terminates its instances and applies the
// pipeline's on_failure policy. "stop" (the default) skips every stage that has
// not finished and terminates the ones in flight; "continue" lets independent
// stages carry on, and the stages that depend on this one are skipped on the
// next pass by blockingDependency.
func failStage(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline, stageState *pipeline.StageState, reason string) {
	now := time.Now()
	stageState.Status = pipeline.StageStatusFailed
	stageState.ErrorMessage = reason
	stageState.CompletedAt = timePtr(now)
	state.FailedStages++
	calculateStageCost(stageState)
	state.CurrentCostUSD += stageState.StageCostUSD

	terminateStageInstances(ctx, stageState, stageRegion(pipelineDef.GetStage(stageState.StageID)))

	if state.OnFailure == "continue" {
		return
	}
	log.Printf("Stage %s failed and on_failure=%q, stopping the pipeline", stageState.StageID, state.OnFailure)
	for i := range state.Stages {
		st := &state.Stages[i]
		switch st.Status {
		case pipeline.StageStatusCompleted, pipeline.StageStatusFailed, pipeline.StageStatusSkipped:
			continue
		}
		if st.Status == pipeline.StageStatusLaunching || st.Status == pipeline.StageStatusRunning {
			terminateStageInstances(ctx, st, stageRegion(pipelineDef.GetStage(st.StageID)))
			st.CompletedAt = timePtr(now)
			calculateStageCost(st)
			state.CurrentCostUSD += st.StageCostUSD
		}
		st.Status = pipeline.StageStatusSkipped
		st.ErrorMessage = fmt.Sprintf("skipped: stage %s failed and on_failure is stop", stageState.StageID)
	}
}

// blockingDependency returns the first dependency of stageDef that failed or
// was skipped, so stageDef can never run; "" if there is none.
func blockingDependency(state *pipeline.PipelineState, stageDef *pipeline.Stage) string {
	for _, depID := range stageDef.DependsOn {
		depState := getStageState(state, depID)
		if depState != nil && (depState.Status == pipeline.StageStatusFailed || depState.Status == pipeline.StageStatusSkipped) {
			return depID
		}
	}
	return ""
}

func getStageState(state *pipeline.PipelineState, stageID string) *pipeline.StageState {
	for i := range state.Stages {
		if state.Stages[i].StageID == stageID {
			return &state.Stages[i]
		}
	}
	return nil
}

func dependenciesMet(state *pipeline.PipelineState, stageDef *pipeline.Stage) bool {
	for _, depID := range stageDef.DependsOn {
		depState := getStageState(state, depID)
		if depState == nil || depState.Status != pipeline.StageStatusCompleted {
			return false
		}
	}
	return true
}

func launchStage(ctx context.Context, state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState) error {
	// Create spawn AWS client (for AMI lookup)
	awsClient, err := spawnaws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create AWS client: %w", err)
	}

	// Get EC2 client for stage's region
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(stageRegion(stageDef)))
	if err != nil {
		return fmt.Errorf("load config for region %s: %w", stageRegion(stageDef), err)
	}
	ec2Client := ec2.NewFromConfig(cfg)

	// Launch instances for this stage
	for i := 0; i < stageDef.InstanceCount; i++ {
		instanceInfo, err := launchStageInstance(ctx, awsClient, ec2Client, state, stageDef, stageState, i)
		if err != nil {
			return fmt.Errorf("launch instance %d: %w", i, err)
		}
		stageState.Instances = append(stageState.Instances, *instanceInfo)
		log.Printf("Launched instance %s for stage %s (index %d)", instanceInfo.InstanceID, stageDef.StageID, i)
	}

	return nil
}

func launchStageInstance(ctx context.Context, awsClient *spawnaws.Client, ec2Client *ec2.Client, state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState, index int) (*pipeline.InstanceInfo, error) {
	region := stageRegion(stageDef)

	// Determine AMI to use
	amiID := stageDef.AMI
	if amiID == "" {
		// No AMI specified, get recommended AMI for region/instance type
		log.Printf("No AMI specified for stage %s, looking up recommended AMI...", stageDef.StageID)
		var err error
		amiID, err = awsClient.GetRecommendedAMI(ctx, region, stageDef.InstanceType)
		if err != nil {
			return nil, fmt.Errorf("lookup recommended AMI: %w", err)
		}
		log.Printf("Using recommended AMI %s for stage %s", amiID, stageDef.StageID)
	}

	// Generate user data with pipeline context
	userData, err := generatePipelineUserData(state, stageDef, stageState, index, region)
	if err != nil {
		return nil, fmt.Errorf("generate user data: %w", err)
	}

	input := buildStageRunInstancesInput(state, stageDef, stageState, index, amiID, userData, time.Now())

	// Launch
	result, err := ec2Client.RunInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("run instances failed: %w", err)
	}

	instance := result.Instances[0]
	instanceInfo := &pipeline.InstanceInfo{
		InstanceID: *instance.InstanceId,
		State:      string(instance.State.Name),
		LaunchedAt: time.Now(),
	}

	if instance.PrivateIpAddress != nil {
		instanceInfo.PrivateIP = *instance.PrivateIpAddress
	}
	if instance.PublicIpAddress != nil {
		instanceInfo.PublicIP = *instance.PublicIpAddress
	}

	// Generate DNS name
	instanceInfo.DNSName = fmt.Sprintf("%s-%d.%s.spore.host", stageDef.StageID, index, state.PipelineID)

	return instanceInfo, nil
}

// buildStageRunInstancesInput is the RunInstances request for one stage
// instance.
func buildStageRunInstancesInput(state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState, index int, amiID, userData string, now time.Time) *ec2.RunInstancesInput {
	input := &ec2.RunInstancesInput{
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		InstanceType: ec2types.InstanceType(stageDef.InstanceType),
		ImageId:      aws.String(amiID),
		UserData:     aws.String(userData),
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Name: aws.String(spawnaws.SporedInstanceProfileName),
		},
		// The user data halts the instance on every exit as its last resort
		// (#912). With this, a halt is a terminate, which needs no IAM
		// permission and no AWS CLI. The EC2 default is stop, which would leave
		// the stage's EBS volumes billing.
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate,
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags:         stageInstanceTags(state, stageDef, stageState, index, now),
			},
		},
	}

	// Add spot if specified
	if stageDef.Spot {
		input.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{
			MarketType: ec2types.MarketTypeSpot,
		}
	}

	// Add security group if streaming
	if state.SecurityGroupID != "" {
		input.SecurityGroupIds = []string{state.SecurityGroupID}
	}

	// Add placement group if EFA
	if stageDef.EFAEnabled && state.PlacementGroupID != "" {
		input.Placement = &ec2types.Placement{
			GroupName: aws.String(state.PlacementGroupID),
		}
	}
	return input
}

// stageInstanceTags are the tags on a stage instance.
//
// They used to be the pipeline/stage ids and Name only (#912). Without
// spawn:managed=true the TTL reaper, which lists only managed instances and
// whose terminate grant is conditioned on that tag, could not see a stage
// instance, and the instance's own role could not terminate itself either (the
// spored role's ec2:TerminateInstances carries the same condition). So a stage
// whose command failed ran until someone cancelled the pipeline by hand.
//
// The lifecycle tags match what `spawn launch` writes (pkg/aws buildTags):
// spawn:launch-time, plus spawn:ttl and spawn:ttl-deadline from pkg/ttl. The TTL
// is the stage's timeout plus stageBootstrapAllowance, so the reaper reclaims
// an instance only once its stage could no longer be legitimately running.
func stageInstanceTags(state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState, index int, now time.Time) []ec2types.Tag {
	name := fmt.Sprintf("%s-%s-%d", state.PipelineName, stageDef.StageID, index)
	tags := spawnaws.LifecycleTags(name, "pipeline", now)

	launch := now.UTC()
	ttlTags := ttl.Tags(launch, launch.Add(stageTTL(stageDef)))
	tags = append(tags,
		ec2types.Tag{Key: aws.String("spawn:launch-time"), Value: aws.String(launch.Format(time.RFC3339))},
		ec2types.Tag{Key: aws.String("spawn:ttl"), Value: aws.String(ttlTags["spawn:ttl"])},
		ec2types.Tag{Key: aws.String("spawn:ttl-deadline"), Value: aws.String(ttlTags["spawn:ttl-deadline"])},
		ec2types.Tag{Key: aws.String("spawn:pipeline-id"), Value: aws.String(state.PipelineID)},
		ec2types.Tag{Key: aws.String("spawn:stage-id"), Value: aws.String(stageDef.StageID)},
		ec2types.Tag{Key: aws.String("spawn:stage-index"), Value: aws.String(fmt.Sprintf("%d", stageState.StageIndex))},
		ec2types.Tag{Key: aws.String("spawn:instance-index"), Value: aws.String(fmt.Sprintf("%d", index))},
	)
	return tags
}

// stageRegion is the region a stage runs in. nil-safe, for cleanup paths that
// look a stage up by id.
func stageRegion(stageDef *pipeline.Stage) string {
	if stageDef == nil || stageDef.Region == "" {
		return defaultStageRegion
	}
	return stageDef.Region
}

// stageTimeout is the bound on a stage's command: its declared timeout, or
// defaultStageTimeout. Validate rejects an unparseable timeout, so the fallback
// covers only a definition that skipped it.
func stageTimeout(stageDef *pipeline.Stage) time.Duration {
	if stageDef != nil && stageDef.Timeout != "" {
		if d, err := time.ParseDuration(stageDef.Timeout); err == nil && d > 0 {
			return d
		}
	}
	return defaultStageTimeout
}

// stageTTL is how long a stage instance may live: the command's timeout plus
// the bootstrap and teardown around it. It is written as the instance's
// spawn:ttl-deadline and is also the point at which the orchestrator gives up
// on a stage that has reported nothing.
func stageTTL(stageDef *pipeline.Stage) time.Duration {
	return stageTimeout(stageDef) + stageBootstrapAllowance
}

// pipelineDeadline is when the orchestrator stops waiting for the pipeline:
// the sum of every stage's TTL, which bounds even a fully serial pipeline, plus
// pipelineDeadlineSlack, from the pipeline's creation.
func pipelineDeadline(state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline) time.Time {
	var total time.Duration
	for i := range pipelineDef.Stages {
		total += stageTTL(&pipelineDef.Stages[i])
	}
	return state.CreatedAt.Add(total + pipelineDeadlineSlack)
}

func generatePipelineUserData(state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState, instanceIndex int, region string) (string, error) {
	// Escape shell variables
	shellEscape := func(s string) string {
		return strings.ReplaceAll(s, "'", "'\\''")
	}

	// The command's timeout, in seconds. Shared with the instance's TTL tag so
	// the two cannot disagree.
	timeout := fmt.Sprintf("%d", int(stageTimeout(stageDef).Seconds()))

	// Build environment variables
	envVars := ""
	for k, v := range stageDef.Env {
		envVars += fmt.Sprintf("export %s='%s'\n", k, shellEscape(v))
	}

	// Generate user data script.
	//
	// The instance terminates itself on EVERY exit (#912). It used to do so
	// only at the end of the success path: the failure branch, the stage
	// timeout and any bootstrap failure under `set -e` all exited first and
	// left the instance running. spawn_finish is installed as an EXIT trap
	// before anything can fail, writes the FAILED marker unless COMPLETE was
	// written, and then terminates the instance two ways: the API (the
	// instance id from IMDSv2 via curl, because ec2-metadata exists only on
	// Amazon Linux), and a halt, which RunInstances' InstanceInitiatedShutdown
	// Behavior=terminate turns into a terminate with no IAM or AWS CLI needed.
	script := fmt.Sprintf(`#!/bin/bash
set -e

# Pipeline configuration
export SPAWN_PIPELINE_ID='%s'
export SPAWN_PIPELINE_NAME='%s'
export SPAWN_STAGE_ID='%s'
export SPAWN_STAGE_INDEX='%d'
export SPAWN_INSTANCE_INDEX='%d'
export SPAWN_S3_BUCKET='%s'
export SPAWN_S3_PREFIX='%s'
export SPAWN_RESULT_S3_BUCKET='%s'
export SPAWN_RESULT_S3_PREFIX='%s'
export SPAWN_REGION='%s'

# Terminate this instance however the script exits (#912).
SPAWN_STAGE_RESULT=""
spawn_finish() {
    rc=$?
    set +e
    trap - EXIT
    if [ "$SPAWN_STAGE_RESULT" != "complete" ]; then
        echo "Stage did not complete (exit $rc); writing FAILED marker"
        echo "failed exit=$rc" | aws s3 cp - "s3://${SPAWN_S3_BUCKET}/${SPAWN_S3_PREFIX}/stages/${SPAWN_STAGE_ID}/FAILED" --region "$SPAWN_REGION"
    fi
    echo "Terminating instance in 60s..."
    sleep 60
    IMDS_TOKEN=$(curl -sf -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
    INSTANCE_ID=$(curl -sf -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" "http://169.254.169.254/latest/meta-data/instance-id")
    if [ -n "$INSTANCE_ID" ]; then
        aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" --region "$SPAWN_REGION"
    fi
    # Backstop: the instance was launched with shutdown behaviour terminate.
    shutdown -h now || poweroff
    exit $rc
}
trap spawn_finish EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Detect architecture
ARCH=$(uname -m)
echo "Installing spored for architecture: $ARCH"

# Detect region
TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" 2>/dev/null || true)
if [ -n "$TOKEN" ]; then
    REGION=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region 2>/dev/null)
else
    REGION=$(curl -s http://169.254.169.254/latest/meta-data/placement/region 2>/dev/null || echo "us-east-1")
fi

echo "Region: $REGION"

# Determine binary name
case "$ARCH" in
    x86_64)
        BINARY="spored-linux-amd64"
        ;;
    aarch64)
        BINARY="spored-linux-arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

# Download from S3
S3_BASE_URL="https://spawn-binaries-${REGION}.s3.amazonaws.com"
FALLBACK_URL="https://spawn-binaries-us-east-1.s3.amazonaws.com"
PROJECT="spawn"

echo "Downloading spored binary..."

# Try regional bucket with project prefix first, then without prefix (legacy), then fallback to us-east-1
if curl -f -o /usr/local/bin/spored "${S3_BASE_URL}/${PROJECT}/${BINARY}" 2>/dev/null; then
    CHECKSUM_URL="${S3_BASE_URL}/${PROJECT}/${BINARY}.sha256"
    echo "Downloaded from ${REGION}"
elif curl -f -o /usr/local/bin/spored "${S3_BASE_URL}/${BINARY}" 2>/dev/null; then
    CHECKSUM_URL="${S3_BASE_URL}/${BINARY}.sha256"
    echo "Downloaded from ${REGION} (legacy path)"
else
    echo "Regional bucket unavailable, trying us-east-1..."
    if curl -f -o /usr/local/bin/spored "${FALLBACK_URL}/${PROJECT}/${BINARY}" 2>/dev/null; then
        CHECKSUM_URL="${FALLBACK_URL}/${PROJECT}/${BINARY}.sha256"
    else
        curl -f -o /usr/local/bin/spored "${FALLBACK_URL}/${BINARY}" || {
            echo "Failed to download spored binary"
            exit 1
        }
        CHECKSUM_URL="${FALLBACK_URL}/${BINARY}.sha256"
    fi
    echo "Downloaded from us-east-1"
fi

# Download and verify SHA256 checksum
echo "Verifying checksum..."
curl -f -o /tmp/spored.sha256 "${CHECKSUM_URL}" || {
    echo "Failed to download checksum"
    exit 1
}

cd /usr/local/bin
EXPECTED_CHECKSUM=$(cat /tmp/spored.sha256)
ACTUAL_CHECKSUM=$(sha256sum spored | awk '{print $1}')

if [ "$EXPECTED_CHECKSUM" != "$ACTUAL_CHECKSUM" ]; then
    echo "❌ Checksum verification failed!"
    exit 1
fi

echo "✅ Checksum verified"
chmod +x /usr/local/bin/spored

# Install AWS CLI if not present (needed for S3 operations)
if ! command -v aws &> /dev/null; then
    echo "Installing AWS CLI..."
    case "$ARCH" in
        x86_64)
            curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "/tmp/awscliv2.zip"
            ;;
        aarch64)
            curl "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip" -o "/tmp/awscliv2.zip"
            ;;
    esac
    cd /tmp && unzip -q awscliv2.zip && ./aws/install
    rm -rf /tmp/aws /tmp/awscliv2.zip
fi

# User environment variables
%s

# Create working directory
WORK_DIR="/opt/spawn/pipeline/${SPAWN_PIPELINE_ID}/${SPAWN_STAGE_ID}"
mkdir -p "$WORK_DIR"
cd "$WORK_DIR"

# Download stage inputs from S3 if configured
# TODO: Implement S3 input download based on data_input config

# Run stage command with timeout
STAGE_CMD='%s'

echo "========================================"
echo "Running stage command: $STAGE_CMD"
echo "========================================"
echo ""

# Run command with timeout. -k: a command that ignores SIGTERM is killed.
set +e
timeout -k 60 %s bash -c "$STAGE_CMD"
EXIT_CODE=$?
set -e
if [ "$EXIT_CODE" -ne 0 ]; then
    echo "Stage command failed with exit code: $EXIT_CODE"
    # spawn_finish writes the FAILED marker and terminates the instance.
    exit "$EXIT_CODE"
fi

echo ""
echo "========================================"
echo "Stage command completed successfully"
echo "========================================"

# Upload stage outputs to S3 if configured
# TODO: Implement S3 output upload based on data_output config

# Write completion marker to S3
echo "success" | aws s3 cp - "s3://${SPAWN_S3_BUCKET}/${SPAWN_S3_PREFIX}/stages/${SPAWN_STAGE_ID}/COMPLETE" --region "$SPAWN_REGION"
SPAWN_STAGE_RESULT="complete"

echo "✅ Stage complete"
exit 0
`,
		shellEscape(state.PipelineID),
		shellEscape(state.PipelineName),
		shellEscape(stageDef.StageID),
		stageState.StageIndex,
		instanceIndex,
		shellEscape(state.S3Bucket),
		shellEscape(state.S3Prefix),
		shellEscape(state.ResultS3Bucket),
		shellEscape(state.ResultS3Prefix),
		shellEscape(region),
		envVars,
		// stageDef.Command appears ONCE, escaped. It used to be interpolated twice:
		// escaped into STAGE_CMD='%s', and RAW into echo "Running stage command: %s".
		// Inside those double quotes a command containing $(...) or backticks
		// EXECUTED — and executed a second time when the stage actually ran, so a
		// substitution with side effects happened twice. The echo now prints the
		// already-quoted shell variable instead (#680).
		shellEscape(stageDef.Command),
		timeout,
	)

	// Base64 encode for EC2 user data
	return base64.StdEncoding.EncodeToString([]byte(script)), nil
}

func allInstancesRunning(ctx context.Context, stageState *pipeline.StageState) bool {
	// TODO: Query EC2 to check instance states
	// For now, simple heuristic: if launched > 1 minute ago, assume running
	if stageState.LaunchedAt == nil {
		return false
	}
	return time.Since(*stageState.LaunchedAt) > 1*time.Minute
}

type stageOutcome int

const (
	stageStillRunning stageOutcome = iota
	stageCompleted
	stageFailed
)

// checkStageOutcome decides whether a running stage has completed, failed or is
// still running.
//
// It used to look for the COMPLETE marker only, and fall back to "all instances
// terminated" read from InstanceInfo.State, which is written once at launch and
// was never refreshed (#912). A failed stage therefore stayed running forever.
// Now, in order:
//   - a FAILED marker fails the stage (it wins over COMPLETE, since with several
//     instances one may fail while another completes);
//   - a COMPLETE marker completes it;
//   - if every instance has gone (refreshed from EC2) without writing COMPLETE,
//     it failed: a spot interruption, a crash, or a script that could not
//     write its marker;
//   - if the stage has outlived its TTL, it failed.
func checkStageOutcome(ctx context.Context, state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState, now time.Time) (stageOutcome, string, error) {
	failed, err := markerExists(ctx, state, stageState.StageID, "FAILED")
	if err != nil {
		return stageStillRunning, "", err
	}
	if failed {
		return stageFailed, "stage command failed (FAILED marker written)", nil
	}

	completed, err := markerExists(ctx, state, stageState.StageID, "COMPLETE")
	if err != nil {
		return stageStillRunning, "", err
	}
	if completed {
		return stageCompleted, "", nil
	}

	alive, err := refreshInstanceStates(ctx, stageState, stageRegion(stageDef))
	if err != nil {
		// Not a decision: the TTL check below still bounds the stage.
		log.Printf("Warning: could not refresh instance states for stage %s: %v", stageState.StageID, err)
	} else if !alive {
		return stageFailed, "every stage instance exited without writing the COMPLETE marker", nil
	}

	if stageState.LaunchedAt != nil && now.Sub(*stageState.LaunchedAt) > stageTTL(stageDef) {
		return stageFailed, fmt.Sprintf("stage exceeded its timeout plus bootstrap allowance (%s)", stageTTL(stageDef)), nil
	}
	return stageStillRunning, "", nil
}

// markerExists reports whether the stage's COMPLETE or FAILED marker is in S3.
// Only a not-found answer means absent; any other error is returned, so a
// permissions or network problem is not read as "not finished yet".
func markerExists(ctx context.Context, state *pipeline.PipelineState, stageID, marker string) (bool, error) {
	key := fmt.Sprintf("%s/stages/%s/%s", state.S3Prefix, stageID, marker)
	_, err := markers.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(state.S3Bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	var nf *s3types.NotFound
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nsk) {
		return false, nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey") {
		return false, nil
	}
	return false, fmt.Errorf("head s3://%s/%s: %w", state.S3Bucket, key, err)
}

// instanceGone reports whether an EC2 instance state means the instance is no
// longer running the stage.
func instanceGone(s string) bool {
	switch ec2types.InstanceStateName(s) {
	case ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameShuttingDown,
		ec2types.InstanceStateNameStopped, ec2types.InstanceStateNameStopping:
		return true
	}
	return false
}

// refreshInstanceStates reads the stage's instances' states from EC2 into
// stageState and reports whether any is still alive.
func refreshInstanceStates(ctx context.Context, stageState *pipeline.StageState, region string) (bool, error) {
	if len(stageState.Instances) == 0 {
		return false, nil
	}
	ids := make([]string, 0, len(stageState.Instances))
	for _, inst := range stageState.Instances {
		ids = append(ids, inst.InstanceID)
	}
	client, err := ec2ForRegion(ctx, region)
	if err != nil {
		return false, fmt.Errorf("ec2 client for %s: %w", region, err)
	}
	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		return false, fmt.Errorf("describe instances: %w", err)
	}
	observed := map[string]string{}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			if inst.InstanceId != nil && inst.State != nil {
				observed[*inst.InstanceId] = string(inst.State.Name)
			}
		}
	}
	alive := false
	for i := range stageState.Instances {
		inst := &stageState.Instances[i]
		if st, ok := observed[inst.InstanceID]; ok {
			inst.State = st
		}
		if !instanceGone(inst.State) {
			alive = true
		}
	}
	return alive, nil
}

// terminateStageInstances terminates every instance of the stage not already
// gone, in the stage's own region.
func terminateStageInstances(ctx context.Context, stageState *pipeline.StageState, region string) {
	var ids []string
	for _, inst := range stageState.Instances {
		if !instanceGone(inst.State) {
			ids = append(ids, inst.InstanceID)
		}
	}
	if len(ids) == 0 {
		return
	}
	client, err := ec2ForRegion(ctx, region)
	if err != nil {
		log.Printf("Warning: Failed to load EC2 client for region %s: %v (instances %v are left to "+
			"their own self-termination and the TTL reaper)", region, err, ids)
		return
	}
	log.Printf("Terminating %d instances for stage %s in %s", len(ids), stageState.StageID, region)
	if _, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
		log.Printf("Warning: Failed to terminate instances for stage %s: %v (left to their own "+
			"self-termination and the TTL reaper)", stageState.StageID, err)
		return
	}
	now := time.Now()
	for i := range stageState.Instances {
		inst := &stageState.Instances[i]
		if !instanceGone(inst.State) {
			inst.State = string(ec2types.InstanceStateNameShuttingDown)
			inst.TerminatedAt = timePtr(now)
		}
	}
	log.Printf("Terminated instances for stage %s: %v", stageState.StageID, ids)
}

// terminateAllInstances terminates every stage's live instances, each in its
// stage's region. Cleanup used to send every TerminateInstances to us-east-1,
// so a stage in any other region was never reclaimed by it.
func terminateAllInstances(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline) {
	for i := range state.Stages {
		st := &state.Stages[i]
		terminateStageInstances(ctx, st, stageRegion(pipelineDef.GetStage(st.StageID)))
	}
}

func calculateStageCost(stageState *pipeline.StageState) {
	if stageState.LaunchedAt == nil || stageState.CompletedAt == nil {
		return
	}

	hours := stageState.CompletedAt.Sub(*stageState.LaunchedAt).Hours()
	instanceHours := hours * float64(len(stageState.Instances))
	stageState.InstanceHours = instanceHours

	// Simplified cost calculation (use $0.10/hour as default)
	stageState.StageCostUSD = instanceHours * 0.10
}

func allStagesComplete(state *pipeline.PipelineState) bool {
	for _, stage := range state.Stages {
		if stage.Status != pipeline.StageStatusCompleted &&
			stage.Status != pipeline.StageStatusFailed &&
			stage.Status != pipeline.StageStatusSkipped {
			return false
		}
	}
	return true
}

// cleanupNetworkResources deletes the pipeline's security and placement
// groups. Instances are terminated first, by terminateAllInstances.
func cleanupNetworkResources(ctx context.Context, state *pipeline.PipelineState) {
	// Cleanup security group. SYNCHRONOUS, with a retry budget — see #752: this
	// was a `go func(){ time.Sleep(30s); … }()` and the Lambda runtime freezes the
	// execution environment when the handler returns, so the delete never ran.
	if state.SecurityGroupID != "" {
		ec2Client := ec2.NewFromConfig(awsCfg)
		err := spawnaws.RetryPlacementGroupDelete(ctx, state.SecurityGroupID, func() error {
			_, derr := ec2Client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
				GroupId: aws.String(state.SecurityGroupID),
			})
			return derr
		})
		if err != nil {
			log.Printf("Warning: Failed to delete security group %s: %v (the TTL reaper will "+
				"reclaim it after its grace period)", state.SecurityGroupID, err)
		} else {
			log.Printf("Deleted security group: %s", state.SecurityGroupID)
		}
	}

	// Cleanup placement group. SYNCHRONOUS, same reason (#752).
	if state.PlacementGroupID != "" {
		{
			ec2Client := ec2.NewFromConfig(awsCfg)
			err := spawnaws.RetryPlacementGroupDelete(ctx, state.PlacementGroupID, func() error {
				_, derr := ec2Client.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{
					GroupName: aws.String(state.PlacementGroupID),
				})
				return derr
			})
			if err != nil {
				log.Printf("Warning: Failed to delete placement group %s: %v (the TTL reaper will "+
					"reclaim it after its grace period)", state.PlacementGroupID, err)
			} else {
				log.Printf("Deleted placement group: %s", state.PlacementGroupID)
			}
		}
	}
}

func reinvokeSelf(ctx context.Context, pipelineID string) error {
	functionName := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	if functionName == "" {
		return fmt.Errorf("AWS_LAMBDA_FUNCTION_NAME not set")
	}

	payload, _ := json.Marshal(PipelineEvent{
		PipelineID: pipelineID,
	})

	_, err := lambdaClient.Invoke(ctx, &lambdasvc.InvokeInput{
		FunctionName:   aws.String(functionName),
		InvocationType: "Event", // Async invocation
		Payload:        payload,
	})

	return err
}

// sendNotification sends an SNS notification if NotificationEmail is configured
func sendNotification(ctx context.Context, state *pipeline.PipelineState, subject, message string) error {
	if state.NotificationEmail == "" {
		return nil // No notification configured
	}

	// Get or create SNS topic
	topicARN, err := getOrCreateNotificationTopic(ctx, state.PipelineID)
	if err != nil {
		return fmt.Errorf("get notification topic: %w", err)
	}

	// Subscribe email if not already subscribed
	if err := ensureEmailSubscription(ctx, topicARN, state.NotificationEmail); err != nil {
		log.Printf("Warning: Failed to ensure email subscription: %v", err)
	}

	// Publish message
	_, err = snsClient.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(topicARN),
		Subject:  aws.String(subject),
		Message:  aws.String(message),
	})

	return err
}

func getOrCreateNotificationTopic(ctx context.Context, pipelineID string) (string, error) {
	topicName := fmt.Sprintf("spawn-pipeline-%s", pipelineID)

	// Try to create topic (idempotent)
	result, err := snsClient.CreateTopic(ctx, &sns.CreateTopicInput{
		Name: aws.String(topicName),
		Tags: []snstypes.Tag{
			{Key: aws.String("spawn:pipeline-id"), Value: aws.String(pipelineID)},
		},
	})

	if err != nil {
		return "", err
	}

	return *result.TopicArn, nil
}

func ensureEmailSubscription(ctx context.Context, topicARN, email string) error {
	// List existing subscriptions
	output, err := snsClient.ListSubscriptionsByTopic(ctx, &sns.ListSubscriptionsByTopicInput{
		TopicArn: aws.String(topicARN),
	})
	if err != nil {
		return err
	}

	// Check if email already subscribed
	for _, sub := range output.Subscriptions {
		if sub.Endpoint != nil && *sub.Endpoint == email {
			return nil // Already subscribed
		}
	}

	// Subscribe email
	_, err = snsClient.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: aws.String(topicARN),
		Protocol: aws.String("email"),
		Endpoint: aws.String(email),
	})

	return err
}

func timePtr(t time.Time) *time.Time {
	return &t
}
