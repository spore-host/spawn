package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/pipeline"
)

const (
	defaultTableName = "spawn-pipeline-orchestration"
	maxExecutionDur  = 13 * time.Minute
	pollInterval     = 10 * time.Second
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
				Tags: []ec2types.Tag{
					{Key: aws.String("spawn:pipeline-id"), Value: aws.String(pipelineID)},
					{Key: aws.String("Name"), Value: aws.String(sgName)},
				},
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
				Tags: []ec2types.Tag{
					{Key: aws.String("spawn:pipeline-id"), Value: aws.String(pipelineID)},
					{Key: aws.String("Name"), Value: aws.String(pgName)},
				},
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

	deadline := time.Now().Add(maxExecutionDur)

	// Get topological order
	order, err := pipelineDef.GetTopologicalOrder()
	if err != nil {
		return fmt.Errorf("get topological order: %w", err)
	}

	for {
		// Reload state to check for cancellation
		currentState, err := loadPipelineState(ctx, state.PipelineID)
		if err != nil {
			log.Printf("Failed to reload pipeline state: %v", err)
		} else if currentState.CancelRequested {
			log.Println("Cancellation requested, stopping orchestration")
			state.Status = pipeline.StatusCancelled
			state.CompletedAt = timePtr(time.Now())
			if err := savePipelineState(ctx, state); err != nil {
				log.Printf("Failed to save cancelled state: %v", err)
			}
			cleanupPipelineResources(ctx, state)
			return nil
		}

		// Check timeout
		if time.Now().After(deadline) {
			if state.Status == pipeline.StatusCompleted || state.Status == pipeline.StatusFailed || state.Status == pipeline.StatusCancelled {
				log.Printf("Pipeline in terminal state %s, not reinvoking", state.Status)
				return nil
			}

			log.Println("Approaching Lambda timeout, re-invoking...")
			if err := savePipelineState(ctx, state); err != nil {
				log.Printf("Failed to save state before re-invocation: %v", err)
			}
			return reinvokeSelf(ctx, pipelineID)
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
				// Check if dependencies are met
				if dependenciesMet(state, stageDef) {
					log.Printf("Stage %s dependencies met, marking as ready", stageID)
					stageState.Status = pipeline.StageStatusReady
					madeProgress = true
				}

			case pipeline.StageStatusReady:
				// Launch stage
				log.Printf("Launching stage %s", stageID)
				if err := launchStage(ctx, state, stageDef, stageState); err != nil {
					log.Printf("Failed to launch stage %s: %v", stageID, err)
					stageState.Status = pipeline.StageStatusFailed
					stageState.ErrorMessage = err.Error()
					stageState.CompletedAt = timePtr(time.Now())
					state.FailedStages++

					// Check on_failure policy
					if state.OnFailure == "stop" {
						log.Printf("Stage %s failed and on_failure=stop, marking remaining stages as skipped", stageID)
						skipRemainingStages(state, order, stageID)
					}
					madeProgress = true
				} else {
					stageState.Status = pipeline.StageStatusLaunching
					stageState.LaunchedAt = timePtr(time.Now())
					madeProgress = true
				}

			case pipeline.StageStatusLaunching:
				// Check if instances are running
				if allInstancesRunning(ctx, stageState) {
					log.Printf("Stage %s instances all running", stageID)
					stageState.Status = pipeline.StageStatusRunning
					madeProgress = true
				}

			case pipeline.StageStatusRunning:
				// Check if stage completed
				completed, err := checkStageCompletion(ctx, state, stageState)
				if err != nil {
					log.Printf("Error checking stage %s completion: %v", stageID, err)
				} else if completed {
					log.Printf("Stage %s completed", stageID)
					stageState.Status = pipeline.StageStatusCompleted
					stageState.CompletedAt = timePtr(time.Now())
					state.CompletedStages++

					// Calculate cost
					calculateStageCost(stageState)
					state.CurrentCostUSD += stageState.StageCostUSD

					// Check budget
					if state.MaxCostUSD != nil && state.CurrentCostUSD > *state.MaxCostUSD {
						log.Printf("Budget exceeded: $%.2f > $%.2f", state.CurrentCostUSD, *state.MaxCostUSD)
						state.Status = pipeline.StatusCancelled
						state.CompletedAt = timePtr(time.Now())
						skipRemainingStages(state, order, stageID)

						// Send budget exceeded notification
						msg := fmt.Sprintf("Pipeline %s (%s) cancelled due to budget exceeded.\n\nCurrent cost: $%.2f\nBudget limit: $%.2f\nStages completed: %d/%d",
							state.PipelineID, state.PipelineName, state.CurrentCostUSD, *state.MaxCostUSD,
							state.CompletedStages, state.TotalStages)
						if err := sendNotification(ctx, state, "Spawn Pipeline: Budget Exceeded", msg); err != nil {
							log.Printf("Warning: Failed to send budget notification: %v", err)
						}

						break
					}

					madeProgress = true
				}
			}
		}

		// Save state if progress made
		if madeProgress {
			if err := savePipelineState(ctx, state); err != nil {
				log.Printf("Failed to save state: %v", err)
			}
		}

		// Check for completion
		if allStagesComplete(state) {
			log.Println("All stages completed")
			if state.FailedStages > 0 {
				state.Status = pipeline.StatusFailed
			} else {
				state.Status = pipeline.StatusCompleted
			}
			state.CompletedAt = timePtr(time.Now())
			if err := savePipelineState(ctx, state); err != nil {
				return fmt.Errorf("failed to save completion state: %w", err)
			}

			// Send completion notification
			duration := state.CompletedAt.Sub(state.CreatedAt)
			subject := fmt.Sprintf("Spawn Pipeline: %s", state.Status)
			msg := fmt.Sprintf("Pipeline %s (%s) %s.\n\nStages completed: %d\nStages failed: %d\nTotal cost: $%.2f\nDuration: %v\n\nPipeline ID: %s",
				state.PipelineName, state.PipelineID, strings.ToLower(string(state.Status)),
				state.CompletedStages, state.FailedStages, state.CurrentCostUSD, duration.Round(time.Second),
				state.PipelineID)
			if err := sendNotification(ctx, state, subject, msg); err != nil {
				log.Printf("Warning: Failed to send completion notification: %v", err)
			}

			cleanupPipelineResources(ctx, state)
			return nil
		}

		// Wait for next poll
		<-ticker.C
	}
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
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(stageDef.Region))
	if err != nil {
		return fmt.Errorf("load config for region %s: %w", stageDef.Region, err)
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
	// Determine region
	region := stageDef.Region
	if region == "" {
		region = "us-east-1" // Default region
	}

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

	// Use existing spored instance profile
	instanceProfile := "spored-instance-profile"

	// Generate user data with pipeline context
	userData, err := generatePipelineUserData(state, stageDef, stageState, index, region)
	if err != nil {
		return nil, fmt.Errorf("generate user data: %w", err)
	}

	// Build RunInstances input
	input := &ec2.RunInstancesInput{
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		InstanceType: ec2types.InstanceType(stageDef.InstanceType),
		ImageId:      aws.String(amiID),
		UserData:     aws.String(userData),
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Name: aws.String(instanceProfile),
		},
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags: []ec2types.Tag{
					{Key: aws.String("spawn:pipeline-id"), Value: aws.String(state.PipelineID)},
					{Key: aws.String("spawn:stage-id"), Value: aws.String(stageDef.StageID)},
					{Key: aws.String("spawn:stage-index"), Value: aws.String(fmt.Sprintf("%d", stageState.StageIndex))},
					{Key: aws.String("spawn:instance-index"), Value: aws.String(fmt.Sprintf("%d", index))},
					{Key: aws.String("Name"), Value: aws.String(fmt.Sprintf("%s-%s-%d", state.PipelineName, stageDef.StageID, index))},
				},
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

func generatePipelineUserData(state *pipeline.PipelineState, stageDef *pipeline.Stage, stageState *pipeline.StageState, instanceIndex int, region string) (string, error) {
	// Escape shell variables
	shellEscape := func(s string) string {
		return strings.ReplaceAll(s, "'", "'\\''")
	}

	// Determine timeout (convert to seconds)
	timeout := "3600" // Default 1 hour
	if stageDef.Timeout != "" {
		duration, err := time.ParseDuration(stageDef.Timeout)
		if err == nil {
			timeout = fmt.Sprintf("%d", int(duration.Seconds()))
		}
	}

	// Build environment variables
	envVars := ""
	for k, v := range stageDef.Env {
		envVars += fmt.Sprintf("export %s='%s'\n", k, shellEscape(v))
	}

	// Generate user data script
	script := fmt.Sprintf(`#!/bin/bash
set -e

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

echo "Downloading spored binary..."

if curl -f -o /usr/local/bin/spored "${S3_BASE_URL}/${BINARY}" 2>/dev/null; then
    CHECKSUM_URL="${S3_BASE_URL}/${BINARY}.sha256"
    echo "Downloaded from ${REGION}"
else
    echo "Regional bucket unavailable, using us-east-1"
    curl -f -o /usr/local/bin/spored "${FALLBACK_URL}/${BINARY}" || {
        echo "Failed to download spored binary"
        exit 1
    }
    CHECKSUM_URL="${FALLBACK_URL}/${BINARY}.sha256"
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

# User environment variables
%s

# Create working directory
WORK_DIR="/opt/spawn/pipeline/${SPAWN_PIPELINE_ID}/${SPAWN_STAGE_ID}"
mkdir -p "$WORK_DIR"
cd "$WORK_DIR"

# Download stage inputs from S3 if configured
# TODO: Implement S3 input download based on data_input config

# Run stage command with timeout
echo "========================================"
echo "Running stage command: %s"
echo "========================================"
echo ""

STAGE_CMD='%s'

# Run command with timeout
timeout %s bash -c "$STAGE_CMD" || {
    EXIT_CODE=$?
    echo "Stage command failed with exit code: $EXIT_CODE"

    # Write failure marker to S3
    echo "failed" | aws s3 cp - "s3://${SPAWN_S3_BUCKET}/${SPAWN_S3_PREFIX}/stages/${SPAWN_STAGE_ID}/FAILED" --region "$SPAWN_REGION"

    exit $EXIT_CODE
}

echo ""
echo "========================================"
echo "Stage command completed successfully"
echo "========================================"

# Upload stage outputs to S3 if configured
# TODO: Implement S3 output upload based on data_output config

# Write completion marker to S3
echo "success" | aws s3 cp - "s3://${SPAWN_S3_BUCKET}/${SPAWN_S3_PREFIX}/stages/${SPAWN_STAGE_ID}/COMPLETE" --region "$SPAWN_REGION"

echo "✅ Stage complete, terminating instance in 60s..."
sleep 60

# Terminate self
INSTANCE_ID=$(ec2-metadata --instance-id | cut -d " " -f 2)
aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" --region "$SPAWN_REGION"
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
		region,
		envVars,
		stageDef.Command,
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

func checkStageCompletion(ctx context.Context, state *pipeline.PipelineState, stageState *pipeline.StageState) (bool, error) {
	// Check for S3 completion marker
	markerKey := fmt.Sprintf("%s/stages/%s/COMPLETE", state.S3Prefix, stageState.StageID)

	_, err := s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(state.S3Bucket),
		Key:    aws.String(markerKey),
	})
	if err == nil {
		return true, nil // Marker exists
	}

	// Check if all instances terminated (alternative completion signal)
	allTerminated := true
	for _, inst := range stageState.Instances {
		if inst.State != "terminated" && inst.State != "stopped" {
			allTerminated = false
			break
		}
	}

	return allTerminated, nil
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

func skipRemainingStages(state *pipeline.PipelineState, order []string, failedStageID string) {
	found := false
	for _, stageID := range order {
		if stageID == failedStageID {
			found = true
			continue
		}
		if found {
			stageState := getStageState(state, stageID)
			if stageState != nil && stageState.Status == pipeline.StageStatusPending {
				stageState.Status = pipeline.StageStatusSkipped
			}
		}
	}
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

func cleanupPipelineResources(ctx context.Context, state *pipeline.PipelineState) {
	// Terminate all instances
	for _, stage := range state.Stages {
		if len(stage.Instances) == 0 {
			continue
		}

		// Collect instance IDs
		instanceIDs := []string{}
		for _, inst := range stage.Instances {
			if inst.State != "terminated" && inst.State != "stopped" {
				instanceIDs = append(instanceIDs, inst.InstanceID)
			}
		}

		if len(instanceIDs) > 0 {
			// Determine region for this stage (stages can be in different regions)
			region := "us-east-1" // Default
			for _, stageSpec := range state.Stages {
				if stageSpec.StageID == stage.StageID && stageSpec.StageIndex == stage.StageIndex {
					// This doesn't have region, need to get from original pipeline def
					// For now, use default region
					break
				}
			}

			cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
			if err != nil {
				log.Printf("Warning: Failed to load config for region %s: %v", region, err)
				continue
			}
			ec2Client := ec2.NewFromConfig(cfg)

			log.Printf("Terminating %d instances for stage %s", len(instanceIDs), stage.StageID)
			_, err = ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
				InstanceIds: instanceIDs,
			})
			if err != nil {
				log.Printf("Warning: Failed to terminate instances for stage %s: %v", stage.StageID, err)
			} else {
				log.Printf("Terminated instances for stage %s: %v", stage.StageID, instanceIDs)
			}
		}
	}

	// Cleanup security group
	if state.SecurityGroupID != "" {
		go func() {
			time.Sleep(30 * time.Second) // Wait for instances to terminate
			ec2Client := ec2.NewFromConfig(awsCfg)
			_, err := ec2Client.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
				GroupId: aws.String(state.SecurityGroupID),
			})
			if err != nil {
				log.Printf("Warning: Failed to delete security group %s: %v", state.SecurityGroupID, err)
			} else {
				log.Printf("Deleted security group: %s", state.SecurityGroupID)
			}
		}()
	}

	// Cleanup placement group
	if state.PlacementGroupID != "" {
		go func() {
			time.Sleep(30 * time.Second)
			ec2Client := ec2.NewFromConfig(awsCfg)
			_, err := ec2Client.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{
				GroupName: aws.String(state.PlacementGroupID),
			})
			if err != nil {
				log.Printf("Warning: Failed to delete placement group %s: %v", state.PlacementGroupID, err)
			} else {
				log.Printf("Deleted placement group: %s", state.PlacementGroupID)
			}
		}()
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
