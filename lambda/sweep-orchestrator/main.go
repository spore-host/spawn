package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
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
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/spore-host/spawn/pkg/availability"
)

// Default configuration for shared infrastructure (with environment variable overrides)
const (
	defaultTableName        = "spawn-sweep-orchestration"
	defaultCrossAccountRole = "arn:aws:iam::%s:role/SpawnSweepCrossAccountRole"
	maxExecutionDur         = 13 * time.Minute
	pollInterval            = 10 * time.Second
)

// Capacity error codes from EC2 API
var capacityErrorCodes = []string{
	"InsufficientInstanceCapacity",
	"MaxSpotInstanceCountExceeded",
	"SpotMaxPriceTooLow",
	"InsufficientFreeAddressesInSubnet",
	"InstanceLimitExceeded",
}

// isCapacityError checks if an error is a capacity-related error
func isCapacityError(err error) (bool, string) {
	if err == nil {
		return false, ""
	}

	errStr := err.Error()
	for _, code := range capacityErrorCodes {
		if strings.Contains(errStr, code) {
			return true, code
		}
	}

	return false, ""
}

// SweepEvent is the Lambda input event
type SweepEvent struct {
	SweepID       string `json:"sweep_id"`
	ForceDownload bool   `json:"force_download"`
}

// SweepRecord is the DynamoDB record structure
type SweepRecord struct {
	SweepID                string          `dynamodbav:"sweep_id"`
	SweepName              string          `dynamodbav:"sweep_name"`
	UserID                 string          `dynamodbav:"user_id"`
	CreatedAt              string          `dynamodbav:"created_at"`
	UpdatedAt              string          `dynamodbav:"updated_at"`
	CompletedAt            string          `dynamodbav:"completed_at,omitempty"`
	S3ParamsKey            string          `dynamodbav:"s3_params_key"`
	MaxConcurrent          int             `dynamodbav:"max_concurrent"`
	MaxConcurrentPerRegion int             `dynamodbav:"max_concurrent_per_region,omitempty"`
	LaunchDelay            string          `dynamodbav:"launch_delay"`
	TotalParams            int             `dynamodbav:"total_params"`
	Region                 string          `dynamodbav:"region"`
	AWSAccountID           string          `dynamodbav:"aws_account_id"`
	Status                 string          `dynamodbav:"status"`
	CancelRequested        bool            `dynamodbav:"cancel_requested"`
	EstimatedCost          float64         `dynamodbav:"estimated_cost,omitempty"`
	Budget                 float64         `dynamodbav:"budget,omitempty"`
	NextToLaunch           int             `dynamodbav:"next_to_launch"`
	Launched               int             `dynamodbav:"launched"`
	Failed                 int             `dynamodbav:"failed"`
	ErrorMessage           string          `dynamodbav:"error_message,omitempty"`
	Instances              []SweepInstance `dynamodbav:"instances"`

	// Multi-region support
	MultiRegion      bool                       `dynamodbav:"multi_region"`
	RegionStatus     map[string]*RegionProgress `dynamodbav:"region_status,omitempty"`
	DistributionMode string                     `dynamodbav:"distribution_mode,omitempty"` // "balanced" or "opportunistic"

	// MPI support
	PlacementGroup string `dynamodbav:"placement_group,omitempty"`
	EFAEnabled     bool   `dynamodbav:"efa_enabled,omitempty"`

	// Region constraints
	RegionConstraints *RegionConstraint `dynamodbav:"region_constraints,omitempty"`
	FilteredRegions   []string          `dynamodbav:"filtered_regions,omitempty"`

	// Schedule integration
	Source     string `dynamodbav:"source,omitempty"`      // "cli" | "scheduled"
	ScheduleID string `dynamodbav:"schedule_id,omitempty"` // For traceability
}

// RegionConstraint defines constraints for region selection
type RegionConstraint struct {
	Include       []string `dynamodbav:"include,omitempty"`
	Exclude       []string `dynamodbav:"exclude,omitempty"`
	Geographic    []string `dynamodbav:"geographic,omitempty"`
	ProximityFrom string   `dynamodbav:"proximity_from,omitempty"`
	CostTier      string   `dynamodbav:"cost_tier,omitempty"`
}

// RegionProgress tracks per-region sweep progress
type RegionProgress struct {
	Launched           int     `dynamodbav:"launched"`
	Failed             int     `dynamodbav:"failed"`
	ActiveCount        int     `dynamodbav:"active_count"`
	NextToLaunch       []int   `dynamodbav:"next_to_launch"`
	TotalInstanceHours float64 `dynamodbav:"total_instance_hours,omitempty"`
	EstimatedCost      float64 `dynamodbav:"estimated_cost,omitempty"`
}

// StateTransition records when an instance changed state
type StateTransition struct {
	Timestamp string `dynamodbav:"timestamp"`
	State     string `dynamodbav:"state"` // "pending", "running", "stopped", "terminated"
}

// EBSVolume represents an attached EBS volume
type EBSVolume struct {
	VolumeID   string `dynamodbav:"volume_id"`
	VolumeType string `dynamodbav:"volume_type"` // gp3, gp2, io2...
	SizeGB     int    `dynamodbav:"size_gb"`
	IOPS       int    `dynamodbav:"iops,omitempty"`
	IsRoot     bool   `dynamodbav:"is_root"`
}

// InstanceResources tracks resources attached to an instance
type InstanceResources struct {
	EBSVolumes []EBSVolume `dynamodbav:"ebs_volumes"`
	IPv4Count  int         `dynamodbav:"ipv4_count"`
}

// SweepInstance tracks individual instance state
type SweepInstance struct {
	Index              int                `dynamodbav:"index"`
	Region             string             `dynamodbav:"region"`
	InstanceID         string             `dynamodbav:"instance_id"`
	RequestedType      string             `dynamodbav:"requested_type,omitempty"` // Pattern specified (e.g., "c5.*")
	ActualType         string             `dynamodbav:"actual_type,omitempty"`    // Type actually launched
	State              string             `dynamodbav:"state"`
	LaunchedAt         string             `dynamodbav:"launched_at"`
	TerminatedAt       string             `dynamodbav:"terminated_at,omitempty"`
	ErrorMessage       string             `dynamodbav:"error_message,omitempty"`
	StateHistory       []StateTransition  `dynamodbav:"state_history,omitempty"`
	Resources          *InstanceResources `dynamodbav:"resources,omitempty"`
	HibernationEnabled bool               `dynamodbav:"hibernation_enabled,omitempty"`
}

// ParamFileFormat matches CLI parameter file structure
type ParamFileFormat struct {
	Defaults map[string]interface{}   `json:"defaults"`
	Params   []map[string]interface{} `json:"params"`
}

// LaunchConfig represents EC2 launch configuration
type LaunchConfig struct {
	InstanceType string
	Region       string
	AMI          string
	KeyName      string
	IAMRole      string
	Tags         map[string]string
	UserData     string
	Spot         bool
	// Additional fields as needed
}

// RegionalOrchestrator manages EC2 clients for multi-region sweeps
type RegionalOrchestrator struct {
	ec2Clients  map[string]*ec2.Client
	accountID   string
	credentials *ststypes.Credentials
}

var (
	awsCfg           aws.Config
	dynamodbClient   *dynamodb.Client
	s3Client         *s3.Client
	lambdaClient     *lambdasvc.Client
	stsClient        *sts.Client
	tableName        string
	crossAccountRole string
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
	stsClient = sts.NewFromConfig(awsCfg)

	// Load configuration from environment variables with fallbacks
	tableName = getEnv("SPAWN_SWEEP_ORCHESTRATION_TABLE", defaultTableName)
	crossAccountRole = getEnv("SPAWN_CROSS_ACCOUNT_ROLE_TEMPLATE", defaultCrossAccountRole)

	log.Printf("Configuration: table=%s, cross_account_role_template=%s", tableName, crossAccountRole)
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

func handler(ctx context.Context, event SweepEvent) error {
	log.Printf("Starting sweep orchestration for sweep_id=%s", event.SweepID)

	// Load state from DynamoDB
	state, err := loadSweepState(ctx, event.SweepID)
	if err != nil {
		return fmt.Errorf("failed to load sweep state: %w", err)
	}

	// Download parameters from S3
	params, err := downloadParams(ctx, state.S3ParamsKey, event.ForceDownload)
	if err != nil {
		return fmt.Errorf("failed to download parameters: %w", err)
	}

	// Check if sweep is already in terminal state (prevents recursive loop)
	if state.Status == "COMPLETED" || state.Status == "FAILED" || state.Status == "CANCELLED" {
		log.Printf("Sweep already in terminal state: %s. Exiting without reinvocation.", state.Status)
		return nil
	}

	// Setup shared resources if INITIALIZING
	if state.Status == "INITIALIZING" {
		log.Println("Setting up shared resources...")
		// Note: Shared resources (AMI, SSH key, IAM role) are assumed to be pre-configured
		// This is handled by the CLI's existing setup logic
		state.Status = "RUNNING"
		state.UpdatedAt = time.Now().Format(time.RFC3339)
		if err := saveSweepState(ctx, state); err != nil {
			return fmt.Errorf("failed to update status to RUNNING: %w", err)
		}
	}

	// Route to appropriate polling loop based on multi-region flag
	if state.MultiRegion && len(state.RegionStatus) > 0 {
		log.Printf("Starting multi-region sweep across %d regions", len(state.RegionStatus))
		orchestrator, err := initializeRegionalOrchestrator(ctx, state)
		if err != nil {
			return fmt.Errorf("failed to initialize regional orchestrator: %w", err)
		}
		return runMultiRegionPollingLoop(ctx, state, params, orchestrator, event.SweepID)
	}

	// Legacy single-region path
	log.Printf("Starting single-region sweep in %s", state.Region)
	ec2Client, err := createCrossAccountEC2Client(ctx, state.Region, state.AWSAccountID)
	if err != nil {
		return fmt.Errorf("failed to create cross-account EC2 client: %w", err)
	}
	return runPollingLoop(ctx, state, params, ec2Client, event.SweepID)
}

func loadSweepState(ctx context.Context, sweepID string) (*SweepRecord, error) {
	result, err := dynamodbClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dynamodb get failed: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("sweep %s not found", sweepID)
	}

	var state SweepRecord
	if err := attributevalue.UnmarshalMap(result.Item, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	return &state, nil
}

func saveSweepState(ctx context.Context, state *SweepRecord) error {
	state.UpdatedAt = time.Now().Format(time.RFC3339)

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

func downloadParams(ctx context.Context, s3Key string, forceDownload bool) (*ParamFileFormat, error) {
	// Parse S3 key (format: s3://bucket/key)
	parts := strings.SplitN(strings.TrimPrefix(s3Key, "s3://"), "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid S3 key format: %s", s3Key)
	}
	bucket, key := parts[0], parts[1]

	// Check cache in /tmp
	cacheFile := fmt.Sprintf("/tmp/%s", strings.ReplaceAll(key, "/", "_"))
	if !forceDownload {
		if data, err := os.ReadFile(cacheFile); err == nil {
			log.Printf("Using cached params from %s", cacheFile)
			var params ParamFileFormat
			if err := json.Unmarshal(data, &params); err == nil {
				return &params, nil
			}
		}
	}

	// Download from S3
	log.Printf("Downloading params from s3://%s/%s", bucket, key)
	result, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get failed: %w", err)
	}
	defer result.Body.Close()

	var params ParamFileFormat
	if err := json.NewDecoder(result.Body).Decode(&params); err != nil {
		return nil, fmt.Errorf("failed to decode params: %w", err)
	}

	// Cache to /tmp
	if data, err := json.Marshal(params); err == nil {
		os.WriteFile(cacheFile, data, 0644)
	}

	return &params, nil
}

func createCrossAccountEC2Client(ctx context.Context, region, accountID string) (*ec2.Client, error) {
	roleARN := fmt.Sprintf(crossAccountRole, accountID)
	log.Printf("Assuming role: %s", roleARN)

	result, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("spawn-sweep-orchestrator"),
		DurationSeconds: aws.Int32(3600),
	})
	if err != nil {
		return nil, fmt.Errorf("assume role failed: %w", err)
	}

	creds := result.Credentials
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     *creds.AccessKeyId,
				SecretAccessKey: *creds.SecretAccessKey,
				SessionToken:    *creds.SessionToken,
				Source:          "AssumeRole",
			}, nil
		})),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create config: %w", err)
	}

	return ec2.NewFromConfig(cfg), nil
}

func runPollingLoop(ctx context.Context, state *SweepRecord, params *ParamFileFormat, ec2Client *ec2.Client, sweepID string) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	deadline := time.Now().Add(maxExecutionDur)
	launchDelay := parseDuration(state.LaunchDelay)

	for {
		// Reload state from DynamoDB to check for cancellation
		currentState, err := loadSweepState(ctx, state.SweepID)
		if err != nil {
			log.Printf("Failed to reload sweep state: %v", err)
		} else if currentState.CancelRequested {
			log.Println("Cancellation requested, stopping orchestration")
			state.Status = "CANCELLED"
			state.CompletedAt = time.Now().Format(time.RFC3339)
			if err := saveSweepState(ctx, state); err != nil {
				log.Printf("Failed to save cancelled state: %v", err)
			}

			// Clean up placement group if auto-created
			if state.PlacementGroup != "" && strings.HasPrefix(state.PlacementGroup, "spawn-mpi-") {
				go cleanupPlacementGroup(ctx, ec2Client, state.PlacementGroup)
			}

			return nil
		}

		// Check timeout
		if time.Now().After(deadline) {
			// Double-check status before reinvoking (safety guard)
			if state.Status == "COMPLETED" || state.Status == "FAILED" || state.Status == "CANCELLED" {
				log.Printf("Sweep in terminal state %s, not reinvoking", state.Status)
				return nil
			}

			log.Println("Approaching Lambda timeout, re-invoking...")
			if err := saveSweepState(ctx, state); err != nil {
				log.Printf("Failed to save state before re-invocation: %v", err)
			}
			return reinvokeSelf(ctx, sweepID)
		}

		// Query active instances
		activeCount, err := countActiveInstances(ctx, ec2Client, state)
		if err != nil {
			log.Printf("Failed to query instances: %v", err)
		} else {
			log.Printf("Active instances: %d/%d", activeCount, state.MaxConcurrent)
		}

		// Launch next batch if slots available
		available := state.MaxConcurrent - activeCount
		if available > 0 && state.NextToLaunch < state.TotalParams {
			toLaunch := min(available, state.TotalParams-state.NextToLaunch)
			log.Printf("Launching %d instances (slots available: %d)", toLaunch, available)

			for i := 0; i < toLaunch; i++ {
				paramIndex := state.NextToLaunch
				paramSet := params.Params[paramIndex]

				// Merge defaults with param set
				config := mergeParams(params.Defaults, paramSet)

				// Launch instance
				if err := launchInstance(ctx, ec2Client, state, config, paramIndex); err != nil {
					log.Printf("Failed to launch instance %d: %v", paramIndex, err)
					state.Failed++
					state.Instances = append(state.Instances, SweepInstance{
						Index:        paramIndex,
						State:        "failed",
						ErrorMessage: err.Error(),
						LaunchedAt:   time.Now().Format(time.RFC3339),
					})
				} else {
					state.Launched++
				}

				state.NextToLaunch++

				// Save state after each launch
				if err := saveSweepState(ctx, state); err != nil {
					log.Printf("Failed to save state: %v", err)
				}

				// Delay between launches
				if launchDelay > 0 && i < toLaunch-1 {
					time.Sleep(launchDelay)
				}
			}
		}

		// Check for completion
		if state.NextToLaunch >= state.TotalParams && activeCount == 0 {
			log.Println("All instances launched and completed")
			state.Status = "COMPLETED"
			state.CompletedAt = time.Now().Format(time.RFC3339)
			if err := saveSweepState(ctx, state); err != nil {
				return fmt.Errorf("failed to save completion state: %w", err)
			}

			// Clean up placement group if auto-created
			if state.PlacementGroup != "" && strings.HasPrefix(state.PlacementGroup, "spawn-mpi-") {
				go cleanupPlacementGroup(ctx, ec2Client, state.PlacementGroup)
			}

			return nil
		}

		// Wait for next poll
		<-ticker.C
	}
}

func countActiveInstances(ctx context.Context, ec2Client *ec2.Client, state *SweepRecord) (int, error) {
	// Build instance IDs list
	var instanceIDs []string
	for _, inst := range state.Instances {
		if inst.InstanceID != "" && (inst.State == "pending" || inst.State == "running") {
			instanceIDs = append(instanceIDs, inst.InstanceID)
		}
	}

	if len(instanceIDs) == 0 {
		return 0, nil
	}

	// Query instance states
	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return 0, fmt.Errorf("describe instances failed: %w", err)
	}

	// Build a map of current AWS states
	awsStates := make(map[string]string)
	activeCount := 0
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			instanceState := string(instance.State.Name)
			awsStates[aws.ToString(instance.InstanceId)] = instanceState
			if instance.State.Name == ec2types.InstanceStateNamePending || instance.State.Name == ec2types.InstanceStateNameRunning {
				activeCount++
			}
		}
	}

	// Update state history for changed instances
	now := time.Now().Format(time.RFC3339)
	for i := range state.Instances {
		inst := &state.Instances[i]
		if inst.InstanceID == "" {
			continue
		}
		awsState, ok := awsStates[inst.InstanceID]
		if !ok {
			continue
		}
		lastState := lastStateFromHistory(inst.StateHistory)
		if awsState != lastState {
			inst.StateHistory = append(inst.StateHistory, StateTransition{
				Timestamp: now,
				State:     awsState,
			})
			inst.State = awsState
		}
	}

	return activeCount, nil
}

// lastStateFromHistory returns the most recent state from history, or empty string if none
func lastStateFromHistory(history []StateTransition) string {
	if len(history) == 0 {
		return ""
	}
	return history[len(history)-1].State
}

// runningHours calculates total hours the instance was in "running" state
// Falls back to total elapsed time if no state history available
func runningHoursFromHistory(history []StateTransition, launchedAt string, end time.Time) float64 {
	if len(history) == 0 {
		// Fallback: no history, use total time
		launched, err := time.Parse(time.RFC3339, launchedAt)
		if err != nil {
			return 0
		}
		hours := end.Sub(launched).Hours()
		if hours < 0 {
			return 0
		}
		return hours
	}

	totalRunning := 0.0
	for i, transition := range history {
		if transition.State != "running" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, transition.Timestamp)
		if err != nil {
			continue
		}
		// Find when this running period ended
		var endTs time.Time
		if i+1 < len(history) {
			nextTs, err := time.Parse(time.RFC3339, history[i+1].Timestamp)
			if err == nil {
				endTs = nextTs
			} else {
				endTs = end
			}
		} else {
			endTs = end
		}
		hours := endTs.Sub(ts).Hours()
		if hours > 0 {
			totalRunning += hours
		}
	}
	return totalRunning
}

func launchInstance(ctx context.Context, ec2Client *ec2.Client, state *SweepRecord, config map[string]interface{}, paramIndex int) error {
	instanceTypePattern := getStringParam(config, "instance_type", "t3.micro")

	// Parse instance type pattern (supports "c5.large|c5.xlarge" or "c5.*")
	instanceTypes, err := parseInstanceTypePattern(instanceTypePattern)
	if err != nil {
		return fmt.Errorf("invalid instance type pattern: %w", err)
	}

	var lastErr error
	for _, instanceType := range instanceTypes {
		log.Printf("Trying instance type %s for param %d", instanceType, paramIndex)

		// Try launch with this type
		err := tryLaunchInstanceSingleRegion(ctx, ec2Client, state, config, paramIndex, instanceType, instanceTypePattern)
		if err == nil {
			return nil // Success!
		}

		// Check if capacity error
		isCapacity, errorCode := isCapacityError(err)
		if isCapacity {
			log.Printf("Capacity unavailable for %s: %s, trying next type", instanceType, errorCode)
			lastErr = err
			continue
		}

		// Non-capacity error (auth, AMI, config) - fail immediately
		return err
	}

	return fmt.Errorf("all instance types exhausted: %w", lastErr)
}

// tryLaunchInstanceSingleRegion attempts to launch with specific instance type (single-region sweeps)
func tryLaunchInstanceSingleRegion(ctx context.Context, ec2Client *ec2.Client, state *SweepRecord, config map[string]interface{}, paramIndex int, instanceType string, instanceTypePattern string) error {
	// Extract launch configuration
	ami := getStringParam(config, "ami", "")
	keyName := getStringParam(config, "key_name", "")
	iamRole := getStringParam(config, "iam_role", "spawnd-role")
	spot := getBoolParam(config, "spot", false)

	// Build RunInstances input
	input := &ec2.RunInstancesInput{
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		InstanceType: ec2types.InstanceType(instanceType),
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Name: aws.String(iamRole),
		},
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags: []ec2types.Tag{
					{Key: aws.String("spawn:sweep-id"), Value: aws.String(state.SweepID)},
					{Key: aws.String("spawn:sweep-index"), Value: aws.String(fmt.Sprintf("%d", paramIndex))},
					{Key: aws.String("Name"), Value: aws.String(fmt.Sprintf("%s-%d", state.SweepName, paramIndex))},
				},
			},
		},
	}

	if ami != "" {
		input.ImageId = aws.String(ami)
	}

	if keyName != "" {
		input.KeyName = aws.String(keyName)
	}

	if spot {
		input.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{
			MarketType: ec2types.MarketTypeSpot,
		}
	}

	// Launch
	result, err := ec2Client.RunInstances(ctx, input)
	if err != nil {
		// Track availability stats for failure (async)
		isCapacity, errorCode := isCapacityError(err)
		go func() {
			trackCtx := context.Background()
			if trackErr := availability.RecordFailure(trackCtx, dynamodbClient, state.Region, instanceType, isCapacity, errorCode); trackErr != nil {
				log.Printf("Failed to record availability failure for %s/%s: %v", state.Region, instanceType, trackErr)
			}
		}()

		return fmt.Errorf("run instances failed: %w", err)
	}

	instanceID := *result.Instances[0].InstanceId
	log.Printf("Launched instance %s for param %d", instanceID, paramIndex)

	// Track availability stats for success (async)
	go func() {
		trackCtx := context.Background()
		if trackErr := availability.RecordSuccess(trackCtx, dynamodbClient, state.Region, instanceType); trackErr != nil {
			log.Printf("Failed to record availability success for %s/%s: %v", state.Region, instanceType, trackErr)
		}
	}()

	// Record instance with initial state history
	launchTime := time.Now().Format(time.RFC3339)
	state.Instances = append(state.Instances, SweepInstance{
		Index:         paramIndex,
		Region:        state.Region,
		InstanceID:    instanceID,
		RequestedType: instanceTypePattern, // e.g., "c5.*" or "c5.large|m5.large"
		ActualType:    instanceType,        // e.g., "c5.large"
		State:         "pending",
		LaunchedAt:    launchTime,
		StateHistory: []StateTransition{
			{Timestamp: launchTime, State: "pending"},
		},
	})

	return nil
}

func reinvokeSelf(ctx context.Context, sweepID string) error {
	functionName := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	if functionName == "" {
		return fmt.Errorf("AWS_LAMBDA_FUNCTION_NAME not set")
	}

	payload, _ := json.Marshal(SweepEvent{
		SweepID:       sweepID,
		ForceDownload: false,
	})

	_, err := lambdaClient.Invoke(ctx, &lambdasvc.InvokeInput{
		FunctionName:   aws.String(functionName),
		InvocationType: "Event", // Async invocation
		Payload:        payload,
	})

	return err
}

// Helper functions

func mergeParams(defaults, params map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range defaults {
		result[k] = v
	}
	for k, v := range params {
		result[k] = v
	}
	return result
}

func getStringParam(params map[string]interface{}, key, defaultValue string) string {
	if v, ok := params[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return defaultValue
}

func getBoolParam(params map[string]interface{}, key string, defaultValue bool) bool {
	if v, ok := params[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return defaultValue
}

// parseInstanceTypePattern parses instance type pattern into list
// Supports:
// - Single: "c5.large"
// - Pipe list: "c5.large|c5.xlarge|m5.large"
// - Wildcard: "c5.*" (expands to all c5 types, sorted small to large)
func parseInstanceTypePattern(pattern string) ([]string, error) {
	if pattern == "" {
		return []string{"t3.micro"}, nil // Default
	}

	// If no wildcard or pipe, return as-is
	if !strings.Contains(pattern, "|") && !strings.Contains(pattern, "*") {
		return []string{pattern}, nil
	}

	// Pipe-separated list
	if strings.Contains(pattern, "|") {
		types := strings.Split(pattern, "|")
		result := make([]string, 0, len(types))
		for _, t := range types {
			trimmed := strings.TrimSpace(t)
			if trimmed != "" {
				result = append(result, trimmed)
			}
		}
		if len(result) == 0 {
			return nil, fmt.Errorf("pipe-separated pattern resulted in empty list")
		}
		return result, nil
	}

	// Wildcard expansion (c5.*, m5.*)
	if strings.Contains(pattern, "*") {
		return expandWildcard(pattern)
	}

	return []string{pattern}, nil
}

// expandWildcard expands wildcard patterns to known instance types
func expandWildcard(pattern string) ([]string, error) {
	if !strings.HasSuffix(pattern, ".*") {
		return nil, fmt.Errorf("wildcard must be in format 'family.*' (e.g., 'c5.*')")
	}

	// Extract family (e.g., "c5" from "c5.*")
	family := strings.TrimSuffix(pattern, ".*")
	if family == "" {
		return nil, fmt.Errorf("empty instance family in wildcard pattern")
	}

	// Common instance sizes (small to large) for spot fallback
	sizes := []string{
		"nano", "micro", "small", "medium", "large", "xlarge",
		"2xlarge", "4xlarge", "8xlarge", "12xlarge", "16xlarge",
		"18xlarge", "24xlarge", "32xlarge", "48xlarge", "56xlarge",
		"96xlarge", "112xlarge", "metal",
	}

	result := make([]string, 0, len(sizes))
	for _, size := range sizes {
		result = append(result, fmt.Sprintf("%s.%s", family, size))
	}

	return result, nil
}

func parseDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// updateRegionalCosts calculates and updates instance hours and costs per region
func updateRegionalCosts(state *SweepRecord) {
	if !state.MultiRegion || len(state.RegionStatus) == 0 {
		return
	}

	// Reset costs (we'll recalculate from scratch each time)
	for _, regionStatus := range state.RegionStatus {
		regionStatus.TotalInstanceHours = 0
		regionStatus.EstimatedCost = 0
	}

	now := time.Now()

	// Calculate costs for each instance
	for _, inst := range state.Instances {
		if inst.LaunchedAt == "" || inst.Region == "" {
			continue
		}

		var endTime time.Time
		var err error
		if inst.TerminatedAt != "" {
			endTime, err = time.Parse(time.RFC3339, inst.TerminatedAt)
			if err != nil {
				log.Printf("Failed to parse termination time for instance %s: %v", inst.InstanceID, err)
				endTime = now // Use now as fallback
			}
		} else {
			endTime = now
		}

		// Use state-aware running hours (only charge for time in "running" state)
		hours := runningHoursFromHistory(inst.StateHistory, inst.LaunchedAt, endTime)
		if hours < 0 {
			hours = 0
		}

		// Get pricing for actual instance type used (fallback to requested type if not set)
		instanceType := inst.ActualType
		if instanceType == "" {
			instanceType = inst.RequestedType
		}
		if instanceType == "" {
			instanceType = "t3.micro" // Fallback default
		}

		// Get hourly rate from pricing package
		hourlyRate := getPricingRate(inst.Region, instanceType)
		cost := hours * hourlyRate

		// Accumulate to region
		if regionStatus, ok := state.RegionStatus[inst.Region]; ok {
			regionStatus.TotalInstanceHours += hours
			regionStatus.EstimatedCost += cost
		}
	}

	// Log updated costs
	for region, status := range state.RegionStatus {
		if status.EstimatedCost > 0 {
			log.Printf("Region %s costs: $%.2f (%.1f instance-hours)", region, status.EstimatedCost, status.TotalInstanceHours)
		}
	}
}

// getPricingRate returns hourly rate for instance type in region
// This is a simplified version - full pricing logic is in pkg/pricing
func getPricingRate(region, instanceType string) float64 {
	// Simplified pricing lookup
	// Real implementation would use pkg/pricing.GetEC2HourlyRate()
	// For now, use rough estimates
	baseRates := map[string]float64{
		"t3.micro":   0.0104,
		"t3.small":   0.0208,
		"t3.medium":  0.0416,
		"t3.large":   0.0832,
		"t3.xlarge":  0.1664,
		"t3.2xlarge": 0.3328,
		"m5.large":   0.096,
		"m5.xlarge":  0.192,
		"m5.2xlarge": 0.384,
		"c5.large":   0.085,
		"c5.xlarge":  0.17,
		"c5.2xlarge": 0.34,
		"c6i.large":  0.085,
		"c6i.xlarge": 0.17,
	}

	rate, ok := baseRates[strings.ToLower(instanceType)]
	if !ok {
		// Default estimate for unknown types
		return 0.10
	}

	return rate
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// initializeRegionalOrchestrator creates EC2 clients for all regions in the sweep
func initializeRegionalOrchestrator(ctx context.Context, state *SweepRecord) (*RegionalOrchestrator, error) {
	// Assume cross-account role once
	roleARN := fmt.Sprintf(crossAccountRole, state.AWSAccountID)
	log.Printf("Assuming role: %s", roleARN)

	result, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("spawn-sweep-orchestrator"),
		DurationSeconds: aws.Int32(3600),
	})
	if err != nil {
		return nil, fmt.Errorf("assume role failed: %w", err)
	}

	orchestrator := &RegionalOrchestrator{
		ec2Clients:  make(map[string]*ec2.Client),
		accountID:   state.AWSAccountID,
		credentials: result.Credentials,
	}

	// Create EC2 client for each region
	for region := range state.RegionStatus {
		client, err := createEC2ClientWithCredentials(ctx, region, result.Credentials)
		if err != nil {
			log.Printf("WARNING: Failed to create EC2 client for region %s: %v", region, err)
			log.Printf("Region %s may be restricted or unavailable. Params in this region will be marked as failed.", region)
			// Mark all params in this region as failed
			if err := markRegionParamsFailed(ctx, state, region, fmt.Sprintf("Region unavailable: %v", err)); err != nil {
				log.Printf("Failed to mark region %s params as failed: %v", region, err)
			}
			continue
		}

		// Validate region access with a simple API call
		_, err = client.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
			Filters: []ec2types.Filter{
				{
					Name:   aws.String("region-name"),
					Values: []string{region},
				},
			},
		})
		if err != nil {
			log.Printf("WARNING: Region %s is not accessible: %v", region, err)
			log.Printf("This may be due to account restrictions or SCP policies. Params in region %s will be marked as failed.", region)
			if err := markRegionParamsFailed(ctx, state, region, fmt.Sprintf("Region access denied: %v", err)); err != nil {
				log.Printf("Failed to mark region %s params as failed: %v", region, err)
			}
			continue
		}

		orchestrator.ec2Clients[region] = client
		log.Printf("Initialized EC2 client for region %s", region)
	}

	if len(orchestrator.ec2Clients) == 0 {
		return nil, fmt.Errorf("no accessible regions found")
	}

	log.Printf("Successfully initialized %d regional EC2 clients", len(orchestrator.ec2Clients))
	return orchestrator, nil
}

// createEC2ClientWithCredentials creates an EC2 client for a specific region using provided credentials
func createEC2ClientWithCredentials(ctx context.Context, region string, creds *ststypes.Credentials) (*ec2.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     *creds.AccessKeyId,
				SecretAccessKey: *creds.SecretAccessKey,
				SessionToken:    *creds.SessionToken,
				Source:          "AssumeRole",
			}, nil
		})),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create config: %w", err)
	}

	return ec2.NewFromConfig(cfg), nil
}

// markRegionParamsFailed marks all params in a region as failed
func markRegionParamsFailed(ctx context.Context, state *SweepRecord, region, errorMsg string) error {
	regionStatus := state.RegionStatus[region]
	if regionStatus == nil {
		return nil
	}

	// Mark all pending params in this region as failed
	for _, paramIndex := range regionStatus.NextToLaunch {
		state.Instances = append(state.Instances, SweepInstance{
			Index:        paramIndex,
			Region:       region,
			State:        "failed",
			ErrorMessage: errorMsg,
			LaunchedAt:   time.Now().Format(time.RFC3339),
		})
		state.Failed++
		regionStatus.Failed++
	}

	// Clear the queue since all are now failed
	regionStatus.NextToLaunch = []int{}
	return saveSweepState(ctx, state)
}

// runMultiRegionPollingLoop orchestrates launches across multiple regions
func runMultiRegionPollingLoop(ctx context.Context, state *SweepRecord, params *ParamFileFormat, orchestrator *RegionalOrchestrator, sweepID string) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	deadline := time.Now().Add(maxExecutionDur)
	launchDelay := parseDuration(state.LaunchDelay)

	for {
		// Reload state from DynamoDB to check for cancellation
		currentState, err := loadSweepState(ctx, state.SweepID)
		if err != nil {
			log.Printf("Failed to reload sweep state: %v", err)
		} else {
			state = currentState // Update local state
			if currentState.CancelRequested {
				log.Println("Cancellation requested, stopping orchestration")
				state.Status = "CANCELLED"
				state.CompletedAt = time.Now().Format(time.RFC3339)
				if err := saveSweepState(ctx, state); err != nil {
					log.Printf("Failed to save cancelled state: %v", err)
				}

				// Clean up placement group if auto-created
				if state.PlacementGroup != "" && strings.HasPrefix(state.PlacementGroup, "spawn-mpi-") {
					// Use the first available EC2 client for cleanup
					for _, client := range orchestrator.ec2Clients {
						go cleanupPlacementGroup(ctx, client, state.PlacementGroup)
						break
					}
				}

				return nil
			}
		}

		// Check timeout
		if time.Now().After(deadline) {
			// Double-check status before reinvoking
			if state.Status == "COMPLETED" || state.Status == "FAILED" || state.Status == "CANCELLED" {
				log.Printf("Sweep in terminal state %s, not reinvoking", state.Status)
				return nil
			}

			log.Println("Approaching Lambda timeout, re-invoking...")
			// Update regional costs before re-invoking
			updateRegionalCosts(state)
			if err := saveSweepState(ctx, state); err != nil {
				log.Printf("Failed to save state before re-invocation: %v", err)
			}
			return reinvokeSelf(ctx, sweepID)
		}

		// Query active instances per region concurrently
		activeByRegion := queryActiveInstancesByRegion(ctx, orchestrator, state)

		// Calculate global capacity
		totalActive := 0
		for _, count := range activeByRegion {
			totalActive += count
		}
		globalAvailable := state.MaxConcurrent - totalActive

		log.Printf("Global: %d active, %d available (max: %d)", totalActive, globalAvailable, state.MaxConcurrent)

		// Launch instances if capacity available
		if globalAvailable > 0 {
			if err := launchAcrossRegions(ctx, orchestrator, state, params, activeByRegion, globalAvailable, launchDelay); err != nil {
				log.Printf("Error during launch: %v", err)
			}
		}

		// Check for completion
		allDone := true
		for _, regionStatus := range state.RegionStatus {
			if len(regionStatus.NextToLaunch) > 0 {
				allDone = false
				break
			}
		}

		if allDone && totalActive == 0 {
			log.Println("All instances launched and completed")
			state.Status = "COMPLETED"
			state.CompletedAt = time.Now().Format(time.RFC3339)
			// Final cost update before completion
			updateRegionalCosts(state)
			if err := saveSweepState(ctx, state); err != nil {
				return fmt.Errorf("failed to save completion state: %w", err)
			}

			// Clean up placement group if auto-created
			if state.PlacementGroup != "" && strings.HasPrefix(state.PlacementGroup, "spawn-mpi-") {
				// Use the first available EC2 client for cleanup (placement groups are region-specific)
				for _, client := range orchestrator.ec2Clients {
					go cleanupPlacementGroup(ctx, client, state.PlacementGroup)
					break
				}
			}

			return nil
		}

		// Wait for next poll
		<-ticker.C
	}
}

// queryActiveInstancesByRegion queries active instance counts per region concurrently
func queryActiveInstancesByRegion(ctx context.Context, orchestrator *RegionalOrchestrator, state *SweepRecord) map[string]int {
	activeByRegion := make(map[string]int)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for region, client := range orchestrator.ec2Clients {
		wg.Add(1)
		go func(r string, c *ec2.Client) {
			defer wg.Done()
			count, err := countActiveInstancesInRegion(ctx, c, state, r)
			if err != nil {
				log.Printf("Failed to query instances in %s: %v", r, err)
				return
			}
			mu.Lock()
			activeByRegion[r] = count
			// Update region status
			if state.RegionStatus[r] != nil {
				state.RegionStatus[r].ActiveCount = count
			}
			mu.Unlock()
		}(region, client)
	}
	wg.Wait()

	return activeByRegion
}

// countActiveInstancesInRegion counts active instances in a specific region
func countActiveInstancesInRegion(ctx context.Context, ec2Client *ec2.Client, state *SweepRecord, region string) (int, error) {
	// Build instance IDs list for this region
	var instanceIDs []string
	for _, inst := range state.Instances {
		if inst.Region == region && inst.InstanceID != "" && (inst.State == "pending" || inst.State == "running") {
			instanceIDs = append(instanceIDs, inst.InstanceID)
		}
	}

	if len(instanceIDs) == 0 {
		return 0, nil
	}

	// Query instance states
	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return 0, fmt.Errorf("describe instances failed: %w", err)
	}

	activeCount := 0
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			state := instance.State.Name
			if state == ec2types.InstanceStateNamePending || state == ec2types.InstanceStateNameRunning {
				activeCount++
			}
		}
	}

	return activeCount, nil
}

// sortRegionsByAvailability sorts regions based on availability stats for opportunistic mode
func sortRegionsByAvailability(ctx context.Context, regions []string, state *SweepRecord, params *ParamFileFormat) []string {
	// Balanced mode: keep existing alphabetical order
	if state.DistributionMode != "opportunistic" {
		return regions
	}

	// Extract instance type from defaults (common across sweep)
	instanceType := getStringParam(params.Defaults, "instance_type", "")
	if instanceType == "" {
		log.Printf("Warning: No instance_type in defaults, using balanced mode")
		return regions
	}

	// Load availability stats for each region
	type regionScore struct {
		region string
		score  float64
	}
	scores := make([]regionScore, 0, len(regions))

	for _, region := range regions {
		stats, err := availability.GetStats(ctx, dynamodbClient, region, instanceType)
		if err != nil {
			log.Printf("Warning: Failed to get availability stats for %s/%s: %v", region, instanceType, err)
			// Include region with neutral score on error
			scores = append(scores, regionScore{region, 0.5})
			continue
		}

		// Skip regions in backoff
		if stats.BackoffUntil != "" {
			backoffUntil, err := time.Parse(time.RFC3339, stats.BackoffUntil)
			if err == nil && time.Now().Before(backoffUntil) {
				log.Printf("Skipping region %s: in backoff until %s", region, stats.BackoffUntil)
				continue
			}
		}

		// Calculate score
		score := availability.CalculateScore(stats)
		scores = append(scores, regionScore{region, score})
		log.Printf("Region %s availability score: %.2f (success: %d, failure: %d)",
			region, score, stats.SuccessCount, stats.FailureCount)
	}

	// Sort descending by score (highest availability first)
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	// Return sorted region list
	result := make([]string, len(scores))
	for i, s := range scores {
		result[i] = s.region
	}

	log.Printf("Opportunistic mode: sorted regions by availability: %v", result)
	return result
}

// launchAcrossRegions launches instances across regions with fair distribution
func launchAcrossRegions(ctx context.Context, orchestrator *RegionalOrchestrator, state *SweepRecord, params *ParamFileFormat, activeByRegion map[string]int, globalAvailable int, launchDelay time.Duration) error {
	// Count regions with pending work
	regionsWithWork := 0
	for region, regionStatus := range state.RegionStatus {
		if _, hasClient := orchestrator.ec2Clients[region]; hasClient && len(regionStatus.NextToLaunch) > 0 {
			regionsWithWork++
		}
	}

	if regionsWithWork == 0 {
		return nil
	}

	// Calculate fair share per region
	fairShare := max(1, globalAvailable/regionsWithWork)
	log.Printf("Fair share: %d instances per region (%d regions with work)", fairShare, regionsWithWork)

	// Collect regions with pending work
	regions := make([]string, 0, len(orchestrator.ec2Clients))
	for region := range orchestrator.ec2Clients {
		if len(state.RegionStatus[region].NextToLaunch) > 0 {
			regions = append(regions, region)
		}
	}

	// Sort regions: alphabetically for balanced mode, by availability for opportunistic mode
	if state.DistributionMode == "opportunistic" {
		regions = sortRegionsByAvailability(ctx, regions, state, params)
	} else {
		sort.Strings(regions)
	}

	// Launch instances concurrently per region
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, region := range regions {
		regionStatus := state.RegionStatus[region]
		client := orchestrator.ec2Clients[region]

		if len(regionStatus.NextToLaunch) == 0 {
			continue
		}

		wg.Add(1)
		go func(r string, rs *RegionProgress, c *ec2.Client) {
			defer wg.Done()

			toLaunch := min(fairShare, len(rs.NextToLaunch))

			// Apply per-region concurrent limit if set
			if state.MaxConcurrentPerRegion > 0 {
				regionCapacity := state.MaxConcurrentPerRegion - activeByRegion[r]
				if regionCapacity < 0 {
					regionCapacity = 0
				}
				toLaunch = min(toLaunch, regionCapacity)
			}

			log.Printf("Region %s: launching %d instances", r, toLaunch)

			for i := 0; i < toLaunch; i++ {
				if len(rs.NextToLaunch) == 0 {
					break
				}

				paramIndex := rs.NextToLaunch[0]
				paramSet := params.Params[paramIndex]

				// Merge defaults with param set
				config := mergeParams(params.Defaults, paramSet)

				// Launch instance in this region
				if err := launchInstanceInRegion(ctx, c, state, config, paramIndex, r); err != nil {
					log.Printf("Failed to launch instance %d in %s: %v", paramIndex, r, err)
					mu.Lock()
					state.Failed++
					rs.Failed++
					state.Instances = append(state.Instances, SweepInstance{
						Index:        paramIndex,
						Region:       r,
						State:        "failed",
						ErrorMessage: err.Error(),
						LaunchedAt:   time.Now().Format(time.RFC3339),
					})
					mu.Unlock()
				} else {
					mu.Lock()
					state.Launched++
					rs.Launched++
					mu.Unlock()
				}

				// Remove from queue
				mu.Lock()
				rs.NextToLaunch = rs.NextToLaunch[1:]
				mu.Unlock()

				// Delay between launches
				if launchDelay > 0 && i < toLaunch-1 {
					time.Sleep(launchDelay)
				}
			}
		}(region, regionStatus, client)
	}

	wg.Wait()

	// Save state after batch
	return saveSweepState(ctx, state)
}

// launchInstanceInRegion launches an instance with fallback support
func launchInstanceInRegion(ctx context.Context, ec2Client *ec2.Client, state *SweepRecord, config map[string]interface{}, paramIndex int, region string) error {
	instanceTypePattern := getStringParam(config, "instance_type", "t3.micro")

	// Parse instance type pattern (supports "c5.large|c5.xlarge" or "c5.*")
	instanceTypes, err := parseInstanceTypePattern(instanceTypePattern)
	if err != nil {
		return fmt.Errorf("invalid instance type pattern: %w", err)
	}

	var lastErr error
	for _, instanceType := range instanceTypes {
		log.Printf("Trying instance type %s for param %d in %s", instanceType, paramIndex, region)

		// Try launch with this type
		err := tryLaunchInstance(ctx, ec2Client, state, config, paramIndex, region, instanceType, instanceTypePattern)
		if err == nil {
			return nil // Success!
		}

		// Check if capacity error
		isCapacity, errorCode := isCapacityError(err)
		if isCapacity {
			log.Printf("Capacity unavailable for %s: %s, trying next type", instanceType, errorCode)
			lastErr = err
			continue
		}

		// Non-capacity error (auth, AMI, config) - fail immediately
		return err
	}

	return fmt.Errorf("all instance types exhausted: %w", lastErr)
}

// tryLaunchInstance attempts to launch with specific instance type
func tryLaunchInstance(ctx context.Context, ec2Client *ec2.Client, state *SweepRecord, config map[string]interface{}, paramIndex int, region string, instanceType string, instanceTypePattern string) error {
	// Extract launch configuration
	// instanceType is provided as parameter
	ami := getStringParam(config, "ami", "")
	keyName := getStringParam(config, "key_name", "")
	iamRole := getStringParam(config, "iam_role", "spawnd-role")
	spot := getBoolParam(config, "spot", false)

	// Build RunInstances input
	input := &ec2.RunInstancesInput{
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		InstanceType: ec2types.InstanceType(instanceType),
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{
			Name: aws.String(iamRole),
		},
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags: []ec2types.Tag{
					{Key: aws.String("spawn:sweep-id"), Value: aws.String(state.SweepID)},
					{Key: aws.String("spawn:sweep-index"), Value: aws.String(fmt.Sprintf("%d", paramIndex))},
					{Key: aws.String("Name"), Value: aws.String(fmt.Sprintf("%s-%d", state.SweepName, paramIndex))},
				},
			},
		},
	}

	if ami != "" {
		input.ImageId = aws.String(ami)
	}

	if keyName != "" {
		input.KeyName = aws.String(keyName)
	}

	if spot {
		input.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{
			MarketType: ec2types.MarketTypeSpot,
		}
	}

	// Launch
	result, err := ec2Client.RunInstances(ctx, input)
	if err != nil {
		// Track availability stats for failure (async)
		isCapacity, errorCode := isCapacityError(err)
		go func() {
			trackCtx := context.Background()
			if trackErr := availability.RecordFailure(trackCtx, dynamodbClient, region, instanceType, isCapacity, errorCode); trackErr != nil {
				log.Printf("Failed to record availability failure for %s/%s: %v", region, instanceType, trackErr)
			}
		}()

		return fmt.Errorf("run instances failed: %w", err)
	}

	instanceID := *result.Instances[0].InstanceId
	log.Printf("Launched instance %s in %s for param %d", instanceID, region, paramIndex)

	// Track availability stats for success (async)
	go func() {
		trackCtx := context.Background()
		if trackErr := availability.RecordSuccess(trackCtx, dynamodbClient, region, instanceType); trackErr != nil {
			log.Printf("Failed to record availability success for %s/%s: %v", region, instanceType, trackErr)
		}
	}()

	// Record instance
	state.Instances = append(state.Instances, SweepInstance{
		Index:         paramIndex,
		Region:        region,
		InstanceID:    instanceID,
		RequestedType: instanceTypePattern, // e.g., "c5.*" or "c5.large|m5.large"
		ActualType:    instanceType,        // e.g., "c5.large"
		State:         "pending",
		LaunchedAt:    time.Now().Format(time.RFC3339),
	})

	return nil
}

// cleanupPlacementGroup removes a spawn-managed placement group after sweep completion
func cleanupPlacementGroup(ctx context.Context, ec2Client *ec2.Client, placementGroupName string) error {
	// Wait for all instances to terminate before deleting placement group
	time.Sleep(30 * time.Second)

	_, err := ec2Client.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{
		GroupName: aws.String(placementGroupName),
	})

	if err != nil {
		log.Printf("Warning: Failed to delete placement group %s: %v", placementGroupName, err)
		// Non-fatal, placement groups are cheap to leave around
	} else {
		log.Printf("Deleted placement group: %s", placementGroupName)
	}

	return nil
}
