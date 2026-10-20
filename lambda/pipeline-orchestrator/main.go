package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
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
	"github.com/spore-host/spawn/pkg/sporedinstall"
	"github.com/spore-host/spawn/pkg/ttl"
)

const (
	defaultTableName = "spawn-pipeline-orchestration"
	maxExecutionDur  = 13 * time.Minute
	pollInterval     = 10 * time.Second

	// defaultStageRegion is where a stage with no region launches. The launch,
	// the user data and the cleanup all go through stageRegion so they agree;
	// cleanup used to hardcode it while launch used the Lambda's own region.
	defaultStageRegion = pipeline.DefaultStageRegion

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

// networkEC2API is what the orchestrator needs from EC2 in a stage's region to
// create and delete the pipeline's security and placement groups.
type networkEC2API interface {
	CreateSecurityGroup(ctx context.Context, params *ec2.CreateSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error)
	AuthorizeSecurityGroupIngress(ctx context.Context, params *ec2.AuthorizeSecurityGroupIngressInput, optFns ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	DeleteSecurityGroup(ctx context.Context, params *ec2.DeleteSecurityGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error)
	CreatePlacementGroup(ctx context.Context, params *ec2.CreatePlacementGroupInput, optFns ...func(*ec2.Options)) (*ec2.CreatePlacementGroupOutput, error)
	DeletePlacementGroup(ctx context.Context, params *ec2.DeletePlacementGroupInput, optFns ...func(*ec2.Options)) (*ec2.DeletePlacementGroupOutput, error)
}

// runInstancesAPI is the EC2 call that launches a stage instance.
type runInstancesAPI interface {
	RunInstances(ctx context.Context, params *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
}

// Seams over the AWS side effects, replaced in tests.
var (
	launchEC2ForRegion = func(ctx context.Context, region string) (runInstancesAPI, error) {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			return nil, err
		}
		return ec2.NewFromConfig(cfg), nil
	}
	netEC2ForRegion = func(ctx context.Context, region string) (networkEC2API, error) {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			return nil, err
		}
		return ec2.NewFromConfig(cfg), nil
	}
	// legacyNetworkRegion is where an orchestrator before #929 created the
	// pipeline's single security and placement group: its own region.
	legacyNetworkRegion = func() string { return awsCfg.Region }
	// priceFn is the on-demand rate of instanceType in region, from truffle
	// via pkg/aws (live Price List, or its static table).
	priceFn = func(ctx context.Context, instanceType, region string) (float64, string, error) {
		p, err := spawnaws.ResolveDisplayPrice(ctx, awsCfg, instanceType, region)
		if err != nil {
			return 0, "", err
		}
		return p.PricePerHour, p.SourceLabel(), nil
	}
	recommendedAMIFn = func(ctx context.Context, region, instanceType string) (string, error) {
		awsClient, err := spawnaws.NewClient(ctx)
		if err != nil {
			return "", fmt.Errorf("create AWS client: %w", err)
		}
		return awsClient.GetRecommendedAMI(ctx, region, instanceType)
	}
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
	putObjectFn  = func(ctx context.Context, in *s3.PutObjectInput) error {
		_, err := s3Client.PutObject(ctx, in)
		return err
	}
)

// PipelineEvent is the Lambda input event. RunID is the run the invocation is
// for: `spawn pipeline launch` and every re-invocation send it, and an
// invocation for a run the record no longer holds exits (#943). Empty from a
// CLI or an orchestrator from before run ids, and then not checked.
type PipelineEvent struct {
	PipelineID string `json:"pipeline_id"`
	RunID      string `json:"run_id,omitempty"`
}

// errSuperseded is returned by savePipelineState when the record now belongs
// to another run of the pipeline: this chain was replaced (#943) and must not
// write over the new run's record.
var errSuperseded = errors.New("the pipeline record belongs to another run")

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

	// A re-invocation of a run that has since been replaced (`spawn pipeline
	// launch --replace`). The new run has its own chain.
	if event.RunID != "" && event.RunID != state.RunID {
		log.Printf("Invocation is for run %q but the record is run %q; this run was replaced, exiting", event.RunID, state.RunID)
		return nil
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
			// Groups already created in other regions are recorded in state.
			cleanupNetFn(ctx, state)
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

	// Only over this run's own record (#943). This was an unconditional
	// PutItem, so a chain whose run had been replaced wrote its stale state
	// over the new run's record on every save.
	in := &dynamodb.PutItemInput{
		TableName:           aws.String(tableName),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(run_id)"),
	}
	if state.RunID != "" {
		in.ConditionExpression = aws.String("run_id = :run")
		in.ExpressionAttributeValues = map[string]types.AttributeValue{
			":run": &types.AttributeValueMemberS{Value: state.RunID},
		}
	}

	// Retry logic with exponential backoff
	for retries := 0; retries < 3; retries++ {
		_, err = dynamodbClient.PutItem(ctx, in)
		if err == nil {
			return nil
		}
		var ccf *types.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return errSuperseded
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

	return setupNetworkResources(ctx, state, pipelineDef)
}

// setupNetworkResources creates the pipeline's security groups and placement
// groups, one per stage region (#929). Both are regional, and they used to be
// created once in the Lambda's own region and then handed to RunInstances in
// each stage's region, where they do not exist. Each is recorded in state as
// soon as it exists, so a failure part way is still cleaned up.
func setupNetworkResources(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline) error {
	// A streaming pipeline needs the security group in every stage region,
	// since it goes on every stage instance there (buildStageRunInstancesInput).
	// An EFA stage needs it in its own region: EFA requires a group that admits
	// all traffic from itself, which the default group of a non-default VPC
	// need not (#956).
	efaRegions := map[string]bool{}
	for _, r := range stageRegions(pipelineDef, func(st *pipeline.Stage) bool { return st.EFAEnabled }) {
		efaRegions[r] = true
	}
	streaming := pipelineDef.HasStreamingStages()
	for _, region := range stageRegions(pipelineDef, func(st *pipeline.Stage) bool { return streaming || st.EFAEnabled }) {
		if _, ok := state.SecurityGroups[region]; ok {
			continue
		}
		log.Printf("Creating the pipeline security group in %s (streaming=%v, efa=%v)...", region, streaming, efaRegions[region])
		client, err := netEC2ForRegion(ctx, region)
		if err != nil {
			return fmt.Errorf("ec2 client for %s: %w", region, err)
		}
		sgID, err := createStreamingSecurityGroup(ctx, client, state, region, efaRegions[region])
		if err != nil {
			return fmt.Errorf("create security group in %s: %w", region, err)
		}
		log.Printf("Created security group %s in %s", sgID, region)
	}

	if pipelineDef.HasEFAStages() {
		for _, region := range stageRegions(pipelineDef, func(st *pipeline.Stage) bool { return st.EFAEnabled }) {
			if _, ok := state.PlacementGroups[region]; ok {
				continue
			}
			log.Printf("Pipeline has EFA stages, creating placement group in %s...", region)
			client, err := netEC2ForRegion(ctx, region)
			if err != nil {
				return fmt.Errorf("ec2 client for %s: %w", region, err)
			}
			pgName, err := createPlacementGroup(ctx, client, state)
			if err != nil {
				return fmt.Errorf("create placement group in %s: %w", region, err)
			}
			if state.PlacementGroups == nil {
				state.PlacementGroups = map[string]string{}
			}
			state.PlacementGroups[region] = pgName
			log.Printf("Created placement group %s in %s", pgName, region)
		}
	}

	return nil
}

// stageRegions is the sorted set of regions of the stages keep selects.
func stageRegions(pipelineDef *pipeline.Pipeline, keep func(*pipeline.Stage) bool) []string {
	seen := map[string]bool{}
	var out []string
	for i := range pipelineDef.Stages {
		st := &pipelineDef.Stages[i]
		if !keep(st) {
			continue
		}
		r := stageRegion(st)
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// createStreamingSecurityGroup creates the pipeline's security group in region
// (client's region) and records it in state.SecurityGroups. Ingress is from the
// group itself only: the streaming ports and SSH between the pipeline's own
// instances. SSH used to be open to 0.0.0.0/0 (#929).
func createStreamingSecurityGroup(ctx context.Context, ec2Client networkEC2API, state *pipeline.PipelineState, region string, efa bool) (string, error) {
	pipelineID := state.PipelineID
	sgName := networkGroupName(state)
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

	sgID := aws.ToString(createResult.GroupId)
	// Recorded before the ingress call, so a failure there still deletes it.
	if state.SecurityGroups == nil {
		state.SecurityGroups = map[string]string{}
	}
	state.SecurityGroups[region] = sgID

	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: streamingIngress(sgID, efa),
	})
	if err != nil {
		return "", fmt.Errorf("authorize ingress failed: %w", err)
	}

	return sgID, nil
}

// streamingIngress is the security group's ingress: TCP 50000-60000 and SSH,
// both from members of the group only, plus, in a region with an EFA stage,
// all traffic from members of the group, which EFA requires (#956). Nothing is
// open to an address range. A new group's default egress (all traffic) covers
// EFA's outbound requirement.
func streamingIngress(sgID string, efa bool) []ec2types.IpPermission {
	self := []ec2types.UserIdGroupPair{{GroupId: aws.String(sgID)}}
	var perms []ec2types.IpPermission
	if efa {
		perms = append(perms, ec2types.IpPermission{
			IpProtocol:       aws.String("-1"),
			UserIdGroupPairs: self,
		})
	}
	return append(perms, []ec2types.IpPermission{
		{
			IpProtocol:       aws.String("tcp"),
			FromPort:         aws.Int32(50000),
			ToPort:           aws.Int32(60000),
			UserIdGroupPairs: self,
		},
		{
			IpProtocol:       aws.String("tcp"),
			FromPort:         aws.Int32(22),
			ToPort:           aws.Int32(22),
			UserIdGroupPairs: self,
		},
	}...)
}

// networkGroupName names the pipeline's security and placement groups. Per run
// (#943): names are unique per VPC and per region, and a run replaced with
// --replace may still be deleting its groups when the new run creates its own.
func networkGroupName(state *pipeline.PipelineState) string {
	if state.RunID == "" {
		return fmt.Sprintf("spawn-pipeline-%s", state.PipelineID)
	}
	return fmt.Sprintf("spawn-pipeline-%s-%s", state.PipelineID, state.RunID)
}

func createPlacementGroup(ctx context.Context, ec2Client networkEC2API, state *pipeline.PipelineState) (string, error) {
	pipelineID := state.PipelineID
	pgName := networkGroupName(state)

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

// securityGroupFor is the pipeline's security group in region, or "" if it
// has none there. A pipeline set up before #929 has only the legacy single
// group, which exists in the Lambda's region and nowhere else.
func securityGroupFor(state *pipeline.PipelineState, region string) string {
	if id, ok := state.SecurityGroups[region]; ok {
		return id
	}
	if legacy := legacySecurityGroupID(state); len(state.SecurityGroups) == 0 && legacy != "" && region == legacyNetworkRegion() {
		return legacy
	}
	return ""
}

// legacySecurityGroupID is the single security group an orchestrator before
// #929 created, in the Lambda's region. The field is deprecated; this is its
// one reader, for pipelines in flight across the upgrade.
func legacySecurityGroupID(state *pipeline.PipelineState) string {
	//lint:ignore SA1019 the deprecated field's one intended reader (#929)
	return state.SecurityGroupID
}

// placementGroupFor is securityGroupFor for placement groups.
func placementGroupFor(state *pipeline.PipelineState, region string) string {
	if name, ok := state.PlacementGroups[region]; ok {
		return name
	}
	if len(state.PlacementGroups) == 0 && state.PlacementGroupID != "" && region == legacyNetworkRegion() {
		return state.PlacementGroupID
	}
	return ""
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
			return reinvokeFn(ctx, pipelineID, state.RunID)
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
	} else if currentState.RunID != state.RunID {
		// The pipeline id was launched again with --replace while this run
		// was in flight (#943). The record is the new run's now, so this
		// chain must not write to it. Its instances are listed only in this
		// chain's copy of the state, so it terminates them here rather than
		// leaving them to their TTL, and stops.
		log.Printf("Run %q was replaced by run %q; terminating this run's instances and stopping", state.RunID, currentState.RunID)
		terminateAllInstances(ctx, state, pipelineDef)
		cleanupNetFn(ctx, state)
		return true, nil
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
			if err := launchStage(ctx, state, pipelineDef, stageDef, stageState); err != nil {
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
				calculateStageCost(ctx, state, stageDef, stageState)
				state.CurrentCostUSD += stageState.StageCostUSD
				madeProgress = true
			}
		}
	}

	// Check the budget against the ESTIMATED spend so far: finished stages
	// plus the elapsed time of the ones still running, at each stage's
	// on-demand rate (#930). It used to be checked only when a stage
	// completed, so a long-running stage cost nothing until it finished, and
	// at a flat $0.10/instance-hour whatever the instance type. Once every
	// stage has finished there is nothing left to stop, so the pipeline is
	// reported as it ended rather than cancelled.
	if state.MaxCostUSD != nil && !allStagesComplete(state) {
		if est := estimatedCost(ctx, state, pipelineDef, now); est > *state.MaxCostUSD {
			log.Printf("Budget exceeded: estimated $%.2f > $%.2f", est, *state.MaxCostUSD)
			// This used to set CANCELLED and `break` out of the switch only:
			// the loop carried on, the status was never saved, and the next
			// invocation boundary returned without terminating anything.
			// Finish the pipeline here.
			finishPipeline(ctx, state, pipelineDef, pipeline.StatusCancelled, "budget exceeded")

			msg := fmt.Sprintf("Pipeline %s (%s) cancelled: its estimated cost exceeded the budget.\n\nEstimated cost: $%.2f %s\nBudget limit: $%.2f\nStages completed: %d/%d",
				state.PipelineID, state.PipelineName, state.CurrentCostUSD, costEstimateNote, *state.MaxCostUSD,
				state.CompletedStages, state.TotalStages)
			if err := notifyFn(ctx, state, "Spawn Pipeline: Budget Exceeded", msg); err != nil {
				log.Printf("Warning: Failed to send budget notification: %v", err)
			}
			return true, nil
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
		msg := fmt.Sprintf("Pipeline %s (%s) %s.\n\nStages completed: %d\nStages failed: %d\nEstimated cost: $%.2f %s\nDuration: %v\n\nPipeline ID: %s",
			state.PipelineName, state.PipelineID, strings.ToLower(string(state.Status)),
			state.CompletedStages, state.FailedStages, state.CurrentCostUSD, costEstimateNote, duration.Round(time.Second),
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
			calculateStageCost(ctx, state, pipelineDef.GetStage(st.StageID), st)
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
	calculateStageCost(ctx, state, pipelineDef.GetStage(stageState.StageID), stageState)
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
			calculateStageCost(ctx, state, pipelineDef.GetStage(st.StageID), st)
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

// putStageConfig writes the pipeline definition where the stage runner reads it:
// s3://<s3_bucket>/<pipeline.StageConfigKey>, named by every stage instance's
// s3-config-key tag.
func putStageConfig(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline) error {
	body, err := json.Marshal(pipelineDef)
	if err != nil {
		return fmt.Errorf("marshal stage config: %w", err)
	}
	key := pipeline.StageConfigKey(state.DataPrefix(), state.PipelineID)
	if err := putObjectFn(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(state.S3Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	}); err != nil {
		return fmt.Errorf("write stage config s3://%s/%s: %w", state.S3Bucket, key, err)
	}
	return nil
}

// stageConfigURL is the s3-config-key tag's value.
func stageConfigURL(state *pipeline.PipelineState) string {
	return fmt.Sprintf("s3://%s/%s", state.S3Bucket, pipeline.StageConfigKey(state.DataPrefix(), state.PipelineID))
}

// stageInstanceProfile is the profile stage instances launch with: the one
// `spawn pipeline launch` created for this pipeline's data, or the shared
// default for a record written before it did.
func stageInstanceProfile(state *pipeline.PipelineState) string {
	if state.InstanceProfile != "" {
		return state.InstanceProfile
	}
	return spawnaws.SporedInstanceProfileName
}

func launchStage(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline, stageDef *pipeline.Stage, stageState *pipeline.StageState) error {
	// Get EC2 client for stage's region
	ec2Client, err := launchEC2ForRegion(ctx, stageRegion(stageDef))
	if err != nil {
		return fmt.Errorf("load config for region %s: %w", stageRegion(stageDef), err)
	}

	// Priced before anything is launched, so a budget that cannot be enforced
	// launches nothing (#930).
	if err := ensureStagePrice(ctx, state, stageDef, stageState); err != nil {
		return err
	}

	// The stage runner reads the definition from the pipeline's own bucket
	// (#910). Written on every stage launch, not once at setup, so a pipeline
	// already in flight when this was deployed gets it too.
	if err := putStageConfig(ctx, state, pipelineDef); err != nil {
		return err
	}

	// Launch instances for this stage
	for i := 0; i < stageDef.InstanceCount; i++ {
		instanceInfo, err := launchStageInstance(ctx, ec2Client, state, stageDef, stageState, i)
		if err != nil {
			return fmt.Errorf("launch instance %d: %w", i, err)
		}
		stageState.Instances = append(stageState.Instances, *instanceInfo)
		log.Printf("Launched instance %s for stage %s (index %d)", instanceInfo.InstanceID, stageDef.StageID, i)
	}

	return nil
}

func launchStageInstance(ctx context.Context, ec2Client runInstancesAPI, state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState, index int) (*pipeline.InstanceInfo, error) {
	region := stageRegion(stageDef)

	// Determine AMI to use
	amiID := stageDef.AMI
	if amiID == "" {
		// No AMI specified, get recommended AMI for region/instance type
		log.Printf("No AMI specified for stage %s, looking up recommended AMI...", stageDef.StageID)
		var err error
		amiID, err = recommendedAMIFn(ctx, region, stageDef.InstanceType)
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
	// EC2 refuses user data over 16 KiB with a 400; say so plainly instead
	// (spawn#1063). The stage command is embedded in it.
	if err := spawnaws.CheckUserDataSize(userData); err != nil {
		return nil, fmt.Errorf("stage %s: %w", stageDef.StageID, err)
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
			Name: aws.String(stageInstanceProfile(state)),
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

	// The pipeline's groups in THIS stage's region (#929). RunInstances runs
	// in the stage's region, and a group from another region does not exist
	// there.
	region := stageRegion(stageDef)
	sg := securityGroupFor(state, region)
	switch {
	case stageDef.EFAEnabled:
		// An EFA interface, built as `spawn launch --efa` builds it (#956).
		// efa_enabled used to add only the placement group, so the instance
		// got an ENA interface and `fi_info -p efa` found nothing. The group
		// goes on the interface: RunInstances refuses instance-level groups
		// alongside NetworkInterfaces.
		var groups []string
		if sg != "" {
			groups = []string{sg}
		}
		input.NetworkInterfaces = []ec2types.InstanceNetworkInterfaceSpecification{spawnaws.EFANetworkInterface("", groups)}
	case sg != "":
		input.SecurityGroupIds = []string{sg}
	}
	if pg := placementGroupFor(state, region); stageDef.EFAEnabled && pg != "" {
		input.Placement = &ec2types.Placement{
			GroupName: aws.String(pg),
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
		// What spored run-pipeline-stage reads to find its data and its
		// definition (pkg/pipeline loadStageInfoFromTags). Without
		// s3-config-key the runner refuses to start (#910).
		ec2types.Tag{Key: aws.String("spawn:s3-bucket"), Value: aws.String(state.S3Bucket)},
		// The RUN's prefix (#943): the runner keys its markers, outputs and
		// upstream inputs by this tag, so a runner of any version stays inside
		// its own run.
		ec2types.Tag{Key: aws.String("spawn:s3-prefix"), Value: aws.String(state.DataPrefix())},
		ec2types.Tag{Key: aws.String("spawn:s3-config-key"), Value: aws.String(stageConfigURL(state))},
	)
	if state.RunID != "" {
		tags = append(tags, ec2types.Tag{Key: aws.String("spawn:run-id"), Value: aws.String(state.RunID)})
	}
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
		if d, err := ttl.ParseDuration(stageDef.Timeout); err == nil && d > 0 {
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

	// The stage's timeout, in seconds. Shared with the instance's TTL tag so
	// the two cannot disagree. It now bounds the whole stage runner: input
	// download, command and output upload.
	timeout := fmt.Sprintf("%d", int(stageTimeout(stageDef).Seconds()))

	// The fallback FAILED marker, for when spored never ran or died without
	// writing one. Rendered from the same pkg/pipeline function the runner and
	// the orchestrator use, so the three cannot disagree on the key.
	failedMarker := fmt.Sprintf("s3://%s/%s", state.S3Bucket,
		pipeline.StageInstanceMarkerKey(state.DataPrefix(), stageDef.StageID, instanceIndex, pipeline.MarkerFailed))

	// Generate user data script.
	//
	// The stage itself is run by `spored run-pipeline-stage` (#910), which
	// reads the stage from this instance's tags and the definition from S3,
	// downloads data_input, runs the command, uploads data_output and writes
	// the COMPLETE marker, or FAILED on any error. This script used to run the
	// command inline and leave data_input/data_output as TODOs, so stages got
	// no inputs and their outputs were lost (#911).
	//
	// The instance terminates itself on EVERY exit (#912). spawn_finish is
	// installed as an EXIT trap before anything can fail, writes the FAILED
	// marker unless the stage completed, and then terminates the instance two
	// ways: the API (the instance id from IMDSv2 via curl, because ec2-metadata
	// exists only on Amazon Linux), and a halt, which RunInstances'
	// InstanceInitiatedShutdownBehavior=terminate turns into a terminate with
	// no IAM or AWS CLI needed.
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
export SPAWN_RUN_ID='%s'
export SPAWN_REGION='%s'
SPAWN_FAILED_MARKER='%s'

# Terminate this instance however the script exits (#912).
SPAWN_STAGE_RESULT=""
spawn_finish() {
    rc=$?
    set +e
    trap - EXIT
    if [ "$SPAWN_STAGE_RESULT" != "complete" ]; then
        echo "Stage did not complete (exit $rc); writing FAILED marker"
        echo "failed exit=$rc" | aws s3 cp - "$SPAWN_FAILED_MARKER" --region "$SPAWN_REGION"
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

# Download from S3 (public buckets, regional for low latency)
S3_BASE_URL="https://spawn-binaries-${REGION}.s3.amazonaws.com"
FALLBACK_URL="https://spawn-binaries-us-east-1.s3.amazonaws.com"
PROJECT="spawn"
%s
echo "Downloading spored binary..."
# Verified install, from pkg/sporedinstall: checksum AND signature, failing
# closed, installed only after every check passes (#1173). Same candidates in
# the same order as before: regional project-prefixed, regional legacy, then
# both under us-east-1. No "|| true": a failure exits, and the EXIT trap writes
# FAILED and terminates the instance.
if ! spore_install_verified_spored "$BINARY" \
      "${S3_BASE_URL}/${PROJECT}" \
      "${S3_BASE_URL}" \
      "${FALLBACK_URL}/${PROJECT}" \
      "${FALLBACK_URL}"; then
    echo "spored install failed; refusing to run an unverified agent"
    exit 1
fi

# Install AWS CLI if not present: spawn_finish uses it for the fallback FAILED
# marker and the API terminate.
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

# spored's AWS clients take the region from here as well as from IMDS.
export AWS_REGION="$SPAWN_REGION"
export AWS_DEFAULT_REGION="$SPAWN_REGION"

# Working directory: the stage command runs in it.
WORK_DIR="/opt/spawn/pipeline/${SPAWN_PIPELINE_ID}/${SPAWN_STAGE_ID}"
mkdir -p "$WORK_DIR"
cd "$WORK_DIR"

echo "========================================"
echo "Running stage ${SPAWN_STAGE_ID} with spored run-pipeline-stage"
echo "========================================"

# The stage runner: inputs, command, outputs, then the COMPLETE marker (or
# FAILED). -k: a runner that ignores SIGTERM is killed.
set +e
timeout -k 60 %s /usr/local/bin/spored run-pipeline-stage
EXIT_CODE=$?
set -e
if [ "$EXIT_CODE" -ne 0 ]; then
    echo "Stage failed with exit code: $EXIT_CODE"
    # spawn_finish writes the fallback FAILED marker and terminates the instance.
    exit "$EXIT_CODE"
fi

# spored exits 0 only after its outputs are uploaded and COMPLETE is written.
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
		// This run's stage-data prefix (#943), where the runner reads and
		// writes; the pipeline's s3_prefix plus /runs/<run-id>.
		shellEscape(state.DataPrefix()),
		shellEscape(state.RunID),
		// No SPAWN_RESULT_S3_BUCKET/PREFIX (#1013): result_s3_bucket is
		// deprecated (#989), the stage's instance profile grants only
		// s3_bucket/<s3_prefix>/*, and nothing collects from there, so a
		// command that trusted them got AccessDenied.
		shellEscape(region),
		shellEscape(failedMarker),
		// The spored signing key and the one verified installer, shared with
		// pkg/launcher, cmd/app.go and cmd/upgrade_spored.go. This script used
		// to install spored with its own inline curl and a sha256 served from
		// the same bucket, never checking the signature (#1173, the gap
		// GHSA-3x4q-99gh-fxhx closed for the other paths). Passed as an
		// argument, not spliced into the format string, because the generated
		// shell contains % characters.
		sporedinstall.KeySetupScript()+sporedinstall.VerifiedInstallFunc(),
		// The stage command and env are no longer in the user data at all:
		// the runner reads them from the definition and runs the command with
		// exec, so there is nothing here for a command's $(...) or quotes to
		// break (#680).
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
//
// Markers are per instance (#930): every instance of a stage wrote the same
// stage-level key, so with instance_count > 1 the first instance to finish
// marked the stage complete, its dependents launched while the others were
// still running, and a sibling's later FAILED was never read. Now, in order:
//   - any instance's FAILED marker fails the stage (as does the legacy
//     stage-level FAILED, written by instances launched before #930);
//   - the stage completes when EVERY instance has written COMPLETE (the legacy
//     stage-level COMPLETE counts only for a single-instance stage, where it
//     means the same thing);
//   - an instance that has gone (refreshed from EC2) without writing COMPLETE
//     fails the stage, since it never can complete: a spot interruption, a
//     crash, or a script that could not write its marker;
//   - if the stage has outlived its TTL, it failed.
func checkStageOutcome(ctx context.Context, state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState, now time.Time) (stageOutcome, string, error) {
	want := expectedInstances(stageDef, stageState)

	legacyFailed, err := markerExists(ctx, state, pipeline.StageMarkerKey(state.DataPrefix(), stageState.StageID, pipeline.MarkerFailed))
	if err != nil {
		return stageStillRunning, "", err
	}
	if legacyFailed {
		return stageFailed, "stage command failed (FAILED marker written)", nil
	}

	complete := make([]bool, want)
	nComplete := 0
	for i := 0; i < want; i++ {
		failed, err := markerExists(ctx, state, pipeline.StageInstanceMarkerKey(state.DataPrefix(), stageState.StageID, i, pipeline.MarkerFailed))
		if err != nil {
			return stageStillRunning, "", err
		}
		if failed {
			return stageFailed, fmt.Sprintf("instance %d of %d failed (FAILED marker written)", i, want), nil
		}
		done, err := markerExists(ctx, state, pipeline.StageInstanceMarkerKey(state.DataPrefix(), stageState.StageID, i, pipeline.MarkerComplete))
		if err != nil {
			return stageStillRunning, "", err
		}
		if done {
			complete[i] = true
			nComplete++
		}
	}
	if nComplete == want {
		return stageCompleted, "", nil
	}
	if want == 1 {
		legacyDone, err := markerExists(ctx, state, pipeline.StageMarkerKey(state.DataPrefix(), stageState.StageID, pipeline.MarkerComplete))
		if err != nil {
			return stageStillRunning, "", err
		}
		if legacyDone {
			return stageCompleted, "", nil
		}
	}

	if _, err := refreshInstanceStates(ctx, stageState, stageRegion(stageDef)); err != nil {
		// Not a decision: the TTL check below still bounds the stage.
		log.Printf("Warning: could not refresh instance states for stage %s: %v", stageState.StageID, err)
	} else {
		if len(stageState.Instances) == 0 {
			return stageFailed, "the stage has no instances", nil
		}
		for i := range stageState.Instances {
			if i < want && !complete[i] && instanceGone(stageState.Instances[i].State) {
				return stageFailed, fmt.Sprintf("instance %d of %d (%s) exited without writing its COMPLETE marker",
					i, want, stageState.Instances[i].InstanceID), nil
			}
		}
	}

	if stageState.LaunchedAt != nil && now.Sub(*stageState.LaunchedAt) > stageTTL(stageDef) {
		return stageFailed, fmt.Sprintf("stage exceeded its timeout plus bootstrap allowance (%s)", stageTTL(stageDef)), nil
	}
	return stageStillRunning, "", nil
}

// expectedInstances is how many instances must report COMPLETE: the stage's
// instance_count. launchStage launches exactly that many or fails the stage.
func expectedInstances(stageDef *pipeline.Stage, stageState *pipeline.StageState) int {
	if stageDef != nil && stageDef.InstanceCount > 0 {
		return stageDef.InstanceCount
	}
	if n := len(stageState.Instances); n > 0 {
		return n
	}
	return 1
}

// markerExists reports whether the marker at key is in the pipeline's bucket.
// Only a not-found answer means absent; any other error is returned, so a
// permissions or network problem is not read as "not finished yet".
func markerExists(ctx context.Context, state *pipeline.PipelineState, key string) (bool, error) {
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

// costEstimateNote qualifies every cost the orchestrator reports: it is an
// estimate, not a bill.
const costEstimateNote = "(estimate: on-demand rates; spot is billed less; EBS and data transfer not included)"

// calculateStageCost sets the stage's instance-hours and ESTIMATED cost, from
// launch to completion, at the stage's on-demand rate (#930). It used to be a
// flat $0.10 per instance-hour, about 1/300th of a p4d.24xlarge. A stage
// launched before rates were recorded is priced now; one whose rate cannot be
// resolved is costed at 0 and logged as unpriced.
func calculateStageCost(ctx context.Context, state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState) {
	if stageState.LaunchedAt == nil || stageState.CompletedAt == nil {
		return
	}
	if err := ensureStagePrice(ctx, state, stageDef, stageState); err != nil {
		log.Printf("Warning: %v", err)
	}

	hours := stageState.CompletedAt.Sub(*stageState.LaunchedAt).Hours()
	instanceHours := hours * float64(len(stageState.Instances))
	stageState.InstanceHours = instanceHours
	stageState.StageCostUSD = instanceHours * stageState.PricePerHourUSD
	if stageState.PricePerHourUSD == 0 && instanceHours > 0 {
		log.Printf("Warning: stage %s has no price; its %.2f instance-hours are not in the cost estimate",
			stageState.StageID, instanceHours)
	}
}

// ensureStagePrice resolves the stage's on-demand rate once and records it.
// On-demand, also for spot stages: an upper bound, so a budget is never
// under-enforced because the spot price moved. A stage that cannot be priced
// is an error only when the pipeline has a budget, because a budget that
// cannot be priced cannot be enforced (the --cost-limit rule in pkg/aws).
func ensureStagePrice(ctx context.Context, state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState) error {
	if stageState.PricePerHourUSD > 0 || stageDef == nil {
		return nil
	}
	rate, source, err := priceFn(ctx, stageDef.InstanceType, stageRegion(stageDef))
	if err == nil && rate <= 0 {
		err = fmt.Errorf("no on-demand price for %s in %s", stageDef.InstanceType, stageRegion(stageDef))
	}
	if err != nil {
		if state.MaxCostUSD != nil {
			return fmt.Errorf("cannot price stage %s (%s in %s), required to enforce max_cost_usd $%.2f: %w",
				stageDef.StageID, stageDef.InstanceType, stageRegion(stageDef), *state.MaxCostUSD, err)
		}
		log.Printf("Warning: cannot price stage %s (%s in %s): %v; its cost will not be estimated",
			stageDef.StageID, stageDef.InstanceType, stageRegion(stageDef), err)
		return nil
	}
	stageState.PricePerHourUSD = rate
	stageState.PriceSource = source
	log.Printf("Stage %s: %s in %s = $%.4f/h on-demand (%s)", stageDef.StageID, stageDef.InstanceType,
		stageRegion(stageDef), rate, source)
	return nil
}

// estimatedCost is the pipeline's estimated spend at now: the finished stages'
// costs plus each in-flight stage's instance-hours so far at its rate.
func estimatedCost(ctx context.Context, state *pipeline.PipelineState, pipelineDef *pipeline.Pipeline, now time.Time) float64 {
	total := state.CurrentCostUSD
	for i := range state.Stages {
		st := &state.Stages[i]
		if st.Status != pipeline.StageStatusLaunching && st.Status != pipeline.StageStatusRunning {
			continue
		}
		if st.LaunchedAt == nil {
			continue
		}
		// A stage launched before rates were recorded is priced now.
		if err := ensureStagePrice(ctx, state, pipelineDef.GetStage(st.StageID), st); err != nil {
			log.Printf("Warning: %v", err)
		}
		total += now.Sub(*st.LaunchedAt).Hours() * float64(len(st.Instances)) * st.PricePerHourUSD
	}
	return total
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
// groups, each in the region it was created in (#929). Instances are
// terminated first, by terminateAllInstances.
func cleanupNetworkResources(ctx context.Context, state *pipeline.PipelineState) {
	sgs := map[string]string{}
	for region, id := range state.SecurityGroups {
		sgs[region] = id
	}
	if legacy := legacySecurityGroupID(state); legacy != "" && len(state.SecurityGroups) == 0 {
		sgs[legacyNetworkRegion()] = legacy
	}
	pgs := map[string]string{}
	for region, name := range state.PlacementGroups {
		pgs[region] = name
	}
	if state.PlacementGroupID != "" && len(state.PlacementGroups) == 0 {
		pgs[legacyNetworkRegion()] = state.PlacementGroupID
	}

	// SYNCHRONOUS, with a retry budget — see #752: this was a
	// `go func(){ time.Sleep(30s); … }()` and the Lambda runtime freezes the
	// execution environment when the handler returns, so the delete never ran.
	for _, region := range sortedKeys(sgs) {
		id := sgs[region]
		client, err := netEC2ForRegion(ctx, region)
		if err != nil {
			log.Printf("Warning: no EC2 client for %s to delete security group %s: %v", region, id, err)
			continue
		}
		err = spawnaws.RetryPlacementGroupDelete(ctx, id, func() error {
			_, derr := client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)})
			return derr
		})
		if err != nil {
			log.Printf("Warning: Failed to delete security group %s in %s: %v (the TTL reaper will "+
				"reclaim it after its grace period)", id, region, err)
		} else {
			log.Printf("Deleted security group %s in %s", id, region)
		}
	}

	for _, region := range sortedKeys(pgs) {
		name := pgs[region]
		client, err := netEC2ForRegion(ctx, region)
		if err != nil {
			log.Printf("Warning: no EC2 client for %s to delete placement group %s: %v", region, name, err)
			continue
		}
		err = spawnaws.RetryPlacementGroupDelete(ctx, name, func() error {
			_, derr := client.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{GroupName: aws.String(name)})
			return derr
		})
		if err != nil {
			log.Printf("Warning: Failed to delete placement group %s in %s: %v (the TTL reaper will "+
				"reclaim it after its grace period)", name, region, err)
		} else {
			log.Printf("Deleted placement group %s in %s", name, region)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func reinvokeSelf(ctx context.Context, pipelineID, runID string) error {
	functionName := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	if functionName == "" {
		return fmt.Errorf("AWS_LAMBDA_FUNCTION_NAME not set")
	}

	payload, _ := json.Marshal(PipelineEvent{
		PipelineID: pipelineID,
		RunID:      runID,
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
