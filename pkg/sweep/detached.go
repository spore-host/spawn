package sweep

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	// Default resource names for backward compatibility
	defaultDynamoTableName = "spawn-sweep-orchestration"
	defaultS3BucketPattern = "spawn-sweeps-%s"
	defaultLambdaFuncName  = "spawn-sweep-orchestrator"
)

// ResourceNames holds configurable resource names for sweep operations
type ResourceNames struct {
	DynamoTableName string
	S3BucketPattern string // Pattern with %s for region
	LambdaFuncName  string
}

// DefaultResourceNames returns the default resource names
func DefaultResourceNames() *ResourceNames {
	return &ResourceNames{
		DynamoTableName: defaultDynamoTableName,
		S3BucketPattern: defaultS3BucketPattern,
		LambdaFuncName:  defaultLambdaFuncName,
	}
}

// Helper functions to get resource names (for backward compatibility)
func getDynamoTableName(names *ResourceNames) string {
	if names != nil && names.DynamoTableName != "" {
		return names.DynamoTableName
	}
	return defaultDynamoTableName
}

func getS3BucketName(names *ResourceNames, region string) string {
	pattern := defaultS3BucketPattern
	if names != nil && names.S3BucketPattern != "" {
		pattern = names.S3BucketPattern
	}
	return fmt.Sprintf(pattern, region)
}

func getLambdaFuncName(names *ResourceNames) string {
	if names != nil && names.LambdaFuncName != "" {
		return names.LambdaFuncName
	}
	return defaultLambdaFuncName
}

// SweepRecord represents the DynamoDB record structure
type SweepRecord struct {
	SweepID                string          `dynamodbav:"sweep_id" json:"sweep_id"`
	SweepName              string          `dynamodbav:"sweep_name" json:"sweep_name"`
	UserID                 string          `dynamodbav:"user_id" json:"user_id"`
	CreatedAt              string          `dynamodbav:"created_at" json:"created_at"`
	UpdatedAt              string          `dynamodbav:"updated_at" json:"updated_at"`
	CompletedAt            string          `dynamodbav:"completed_at,omitempty" json:"completed_at,omitempty"`
	S3ParamsKey            string          `dynamodbav:"s3_params_key" json:"s3_params_key"`
	MaxConcurrent          int             `dynamodbav:"max_concurrent" json:"max_concurrent"`
	MaxConcurrentPerRegion int             `dynamodbav:"max_concurrent_per_region,omitempty" json:"max_concurrent_per_region,omitempty"`
	LaunchDelay            string          `dynamodbav:"launch_delay" json:"launch_delay"`
	TotalParams            int             `dynamodbav:"total_params" json:"total_params"`
	Region                 string          `dynamodbav:"region" json:"region"`
	AWSAccountID           string          `dynamodbav:"aws_account_id" json:"aws_account_id"`
	Status                 string          `dynamodbav:"status" json:"status"`
	CancelRequested        bool            `dynamodbav:"cancel_requested" json:"cancel_requested"`
	EstimatedCost          float64         `dynamodbav:"estimated_cost,omitempty" json:"estimated_cost,omitempty"`
	Budget                 float64         `dynamodbav:"budget,omitempty" json:"budget,omitempty"` // Budget limit in dollars
	NextToLaunch           int             `dynamodbav:"next_to_launch" json:"next_to_launch"`
	Launched               int             `dynamodbav:"launched" json:"launched"`
	Failed                 int             `dynamodbav:"failed" json:"failed"`
	ErrorMessage           string          `dynamodbav:"error_message,omitempty" json:"error_message,omitempty"`
	Instances              []SweepInstance `dynamodbav:"instances" json:"instances"`

	// Multi-region support
	MultiRegion      bool                       `dynamodbav:"multi_region" json:"multi_region"`
	RegionStatus     map[string]*RegionProgress `dynamodbav:"region_status,omitempty" json:"region_status,omitempty"`
	DistributionMode string                     `dynamodbav:"distribution_mode,omitempty" json:"distribution_mode,omitempty"` // "balanced" or "opportunistic"

	// MPI support
	PlacementGroup string `dynamodbav:"placement_group,omitempty" json:"placement_group,omitempty"`
	EFAEnabled     bool   `dynamodbav:"efa_enabled,omitempty" json:"efa_enabled,omitempty"`

	// Region constraints
	RegionConstraints *RegionConstraint `dynamodbav:"region_constraints,omitempty" json:"region_constraints,omitempty"`
	FilteredRegions   []string          `dynamodbav:"filtered_regions,omitempty" json:"filtered_regions,omitempty"`

	// Scheduled execution tracking
	Source     string `dynamodbav:"source,omitempty" json:"source,omitempty"`           // "cli" or "scheduled"
	ScheduleID string `dynamodbav:"schedule_id,omitempty" json:"schedule_id,omitempty"` // For scheduled sweeps
}

// RegionConstraint defines constraints for region selection (embedded for DynamoDB compatibility)
type RegionConstraint struct {
	Include       []string `dynamodbav:"include,omitempty" json:"include,omitempty"`
	Exclude       []string `dynamodbav:"exclude,omitempty" json:"exclude,omitempty"`
	Geographic    []string `dynamodbav:"geographic,omitempty" json:"geographic,omitempty"`
	ProximityFrom string   `dynamodbav:"proximity_from,omitempty" json:"proximity_from,omitempty"`
	CostTier      string   `dynamodbav:"cost_tier,omitempty" json:"cost_tier,omitempty"`
}

// RegionProgress tracks per-region sweep progress
type RegionProgress struct {
	Launched           int     `dynamodbav:"launched" json:"launched"`
	Failed             int     `dynamodbav:"failed" json:"failed"`
	ActiveCount        int     `dynamodbav:"active_count" json:"active_count"`
	NextToLaunch       []int   `dynamodbav:"next_to_launch" json:"next_to_launch"`
	TotalInstanceHours float64 `dynamodbav:"total_instance_hours,omitempty" json:"total_instance_hours,omitempty"`
	EstimatedCost      float64 `dynamodbav:"estimated_cost,omitempty" json:"estimated_cost,omitempty"`
}

// SweepInstance tracks individual instance state
type SweepInstance struct {
	Index         int    `dynamodbav:"index" json:"index"`
	Region        string `dynamodbav:"region" json:"region"`
	InstanceID    string `dynamodbav:"instance_id" json:"instance_id"`
	RequestedType string `dynamodbav:"requested_type,omitempty" json:"requested_type,omitempty"` // Pattern specified
	ActualType    string `dynamodbav:"actual_type,omitempty" json:"actual_type,omitempty"`       // Type actually launched
	State         string `dynamodbav:"state" json:"state"`
	LaunchedAt    string `dynamodbav:"launched_at" json:"launched_at"`
	TerminatedAt  string `dynamodbav:"terminated_at,omitempty" json:"terminated_at,omitempty"`
	ErrorMessage  string `dynamodbav:"error_message,omitempty" json:"error_message,omitempty"`
}

// ParamFileFormat matches the CLI parameter file structure
type ParamFileFormat struct {
	Defaults map[string]interface{}   `json:"defaults"`
	Params   []map[string]interface{} `json:"params"`
}

// GroupParamsByRegion groups parameter sets by their target region
func GroupParamsByRegion(params []map[string]interface{}, defaults map[string]interface{}) map[string][]int {
	groups := make(map[string][]int)
	defaultRegion := "us-east-1" // Fallback default

	// Extract default region if specified
	if r, ok := defaults["region"]; ok {
		if regionStr, ok := r.(string); ok && regionStr != "" {
			defaultRegion = regionStr
		}
	}

	// Group each param by its region
	for i, param := range params {
		region := defaultRegion

		// Check if this param specifies a region override
		if r, ok := param["region"]; ok {
			if regionStr, ok := r.(string); ok && regionStr != "" {
				region = regionStr
			}
		}

		groups[region] = append(groups[region], i)
	}

	return groups
}

// UploadParamsToS3 uploads parameter file to S3 and returns the S3 key
func UploadParamsToS3(ctx context.Context, cfg aws.Config, paramFormat *ParamFileFormat, sweepID, region string) (string, error) {
	s3Client := s3.NewFromConfig(cfg)

	bucket := getS3BucketName(nil, region)
	key := fmt.Sprintf("sweeps/%s/params.json", sweepID)

	// Marshal params to JSON
	data, err := json.Marshal(paramFormat)
	if err != nil {
		return "", fmt.Errorf("failed to marshal params: %w", err)
	}

	// Upload to S3
	_, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload to S3: %w", err)
	}

	s3Key := fmt.Sprintf("s3://%s/%s", bucket, key)
	return s3Key, nil
}

// CreateSweepRecord creates a new DynamoDB record for the sweep
func CreateSweepRecord(ctx context.Context, cfg aws.Config, record *SweepRecord) error {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	// Get current user identity for UserID
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	record.UserID = *identity.Arn

	// Set timestamps and initial state
	now := time.Now().Format(time.RFC3339)
	record.CreatedAt = now
	record.UpdatedAt = now
	record.Status = "INITIALIZING"
	record.NextToLaunch = 0
	record.Launched = 0
	record.Failed = 0
	record.Instances = []SweepInstance{}

	// Marshal record
	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal record: %w", err)
	}

	// Put item to DynamoDB
	_, err = dynamodbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(getDynamoTableName(nil)),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to create DynamoDB record: %w", err)
	}

	return nil
}

// InvokeSweepOrchestrator invokes the Lambda function for sweep orchestration
func InvokeSweepOrchestrator(ctx context.Context, cfg aws.Config, sweepID string) error {
	lambdaClient := lambda.NewFromConfig(cfg)

	payload, err := json.Marshal(map[string]interface{}{
		"sweep_id":       sweepID,
		"force_download": false,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	// Invoke Lambda asynchronously
	_, err = lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(getLambdaFuncName(nil)),
		InvocationType: "Event", // Async invocation
		Payload:        payload,
	})
	if err != nil {
		return fmt.Errorf("failed to invoke Lambda: %w", err)
	}

	return nil
}

// QuerySweepStatus queries DynamoDB for the current sweep status
func QuerySweepStatus(ctx context.Context, cfg aws.Config, sweepID string) (*SweepRecord, error) {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	result, err := dynamodbClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(getDynamoTableName(nil)),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query DynamoDB: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("sweep %s not found", sweepID)
	}

	var record SweepRecord
	if err := attributevalue.UnmarshalMap(result.Item, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal record: %w", err)
	}

	return &record, nil
}

// LoadSweepStateFromDynamoDB loads the sweep state from DynamoDB
func LoadSweepStateFromDynamoDB(ctx context.Context, cfg aws.Config, sweepID string) (*SweepRecord, error) {
	return QuerySweepStatus(ctx, cfg, sweepID)
}

// SaveSweepState saves the sweep state to DynamoDB
func SaveSweepState(ctx context.Context, cfg aws.Config, record *SweepRecord) error {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	record.UpdatedAt = time.Now().Format(time.RFC3339)

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal record: %w", err)
	}

	_, err = dynamodbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(getDynamoTableName(nil)),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to save state: %w", err)
	}

	return nil
}

// ValidateParameterSets validates all parameter sets before launch
func ValidateParameterSets(ctx context.Context, cfg aws.Config, params *ParamFileFormat, accountID string) error {
	// Group validations by region to minimize API calls
	regionInstanceTypes := make(map[string]map[string]bool)

	// Collect all instance types per region
	for i, paramSet := range params.Params {
		// Get region (from param set or defaults)
		region := getStringValue(paramSet, "region", "")
		if region == "" {
			if defaults, ok := params.Defaults["region"].(string); ok {
				region = defaults
			} else {
				return fmt.Errorf("param set %d: no region specified", i)
			}
		}

		// Get instance type (from param set or defaults)
		instanceType := getStringValue(paramSet, "instance_type", "")
		if instanceType == "" {
			if defaults, ok := params.Defaults["instance_type"].(string); ok {
				instanceType = defaults
			} else {
				return fmt.Errorf("param set %d: no instance_type specified", i)
			}
		}

		// Add to validation map
		if regionInstanceTypes[region] == nil {
			regionInstanceTypes[region] = make(map[string]bool)
		}
		regionInstanceTypes[region][instanceType] = false // false = not yet validated
	}

	// Create STS client for cross-account access
	stsClient := sts.NewFromConfig(cfg)

	// Assume cross-account role
	roleArn := fmt.Sprintf("arn:aws:iam::%s:role/SpawnSweepCrossAccountRole", accountID)
	assumeResult, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleArn),
		RoleSessionName: aws.String("spawn-validate-" + time.Now().Format("20060102-150405")),
		DurationSeconds: aws.Int32(900), // 15 minutes
	})
	if err != nil {
		return fmt.Errorf("failed to assume role for validation: %w", err)
	}

	// Validate each region's instance types
	for region, instanceTypes := range regionInstanceTypes {
		// Create EC2 client for this region
		creds := assumeResult.Credentials
		ec2Cfg, err := config.LoadDefaultConfig(ctx,
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
			return fmt.Errorf("failed to create config for region %s: %w", region, err)
		}

		ec2Client := ec2.NewFromConfig(ec2Cfg)

		// Query available instance types in this region
		for instanceType := range instanceTypes {
			result, err := ec2Client.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{
				Filters: []ec2types.Filter{
					{
						Name:   aws.String("instance-type"),
						Values: []string{instanceType},
					},
				},
			})
			if err != nil {
				return fmt.Errorf("failed to validate instance type %s in %s: %w", instanceType, region, err)
			}

			if len(result.InstanceTypeOfferings) == 0 {
				return fmt.Errorf("instance type %s is not available in region %s", instanceType, region)
			}

			regionInstanceTypes[region][instanceType] = true
		}
	}

	return nil
}

func getStringValue(m map[string]interface{}, key, defaultValue string) string {
	if val, ok := m[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return defaultValue
}

// TerminateSweepInstances terminates EC2 instances via cross-account access
func TerminateSweepInstances(ctx context.Context, cfg aws.Config, accountID, region string, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	// Create STS client
	stsClient := sts.NewFromConfig(cfg)

	// Assume cross-account role
	roleArn := fmt.Sprintf("arn:aws:iam::%s:role/SpawnSweepCrossAccountRole", accountID)
	assumeResult, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleArn),
		RoleSessionName: aws.String("spawn-cancel-" + time.Now().Format("20060102-150405")),
		DurationSeconds: aws.Int32(900), // 15 minutes
	})
	if err != nil {
		return fmt.Errorf("failed to assume role: %w", err)
	}

	// Create EC2 client with assumed role credentials
	creds := assumeResult.Credentials
	ec2Cfg, err := config.LoadDefaultConfig(ctx,
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
		return fmt.Errorf("failed to create config: %w", err)
	}

	ec2Client := ec2.NewFromConfig(ec2Cfg)

	// Terminate instances
	_, err = ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instances: %w", err)
	}

	return nil
}

// TerminateSweepInstancesDirect terminates EC2 instances using provided credentials (no cross-account)
func TerminateSweepInstancesDirect(ctx context.Context, cfg aws.Config, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	ec2Client := ec2.NewFromConfig(cfg)

	// Terminate instances
	_, err := ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instances: %w", err)
	}

	return nil
}
