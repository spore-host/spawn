package infrastructure

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ValidationResult represents the result of infrastructure validation
type ValidationResult struct {
	Valid     bool
	Errors    []string
	Warnings  []string
	Resources map[string]ResourceStatus
}

// ResourceStatus represents the status of a single resource
type ResourceStatus struct {
	Name       string
	Type       string
	Exists     bool
	Accessible bool
	Error      string
}

// Validator validates infrastructure resources
type Validator struct {
	resolver *Resolver
	awsCfg   aws.Config
}

// NewValidator creates a new infrastructure validator
func NewValidator(resolver *Resolver, awsCfg aws.Config) *Validator {
	return &Validator{
		resolver: resolver,
		awsCfg:   awsCfg,
	}
}

// ValidateAll validates all infrastructure resources
func (v *Validator) ValidateAll(ctx context.Context) (*ValidationResult, error) {
	result := &ValidationResult{
		Valid:     true,
		Errors:    []string{},
		Warnings:  []string{},
		Resources: make(map[string]ResourceStatus),
	}

	// Validate DynamoDB tables
	v.validateDynamoDBTables(ctx, result)

	// Validate S3 buckets
	v.validateS3Buckets(ctx, result)

	// Validate Lambda functions
	v.validateLambdaFunctions(ctx, result)

	// Set overall validity
	result.Valid = len(result.Errors) == 0

	return result, nil
}

func (v *Validator) validateDynamoDBTables(ctx context.Context, result *ValidationResult) {
	client := dynamodb.NewFromConfig(v.awsCfg)

	tables := map[string]string{
		"schedules":           v.resolver.GetSchedulesTable(),
		"sweep_orchestration": v.resolver.GetSweepOrchestrationTable(),
		"alerts":              v.resolver.GetAlertsTable(),
		"alert_history":       v.resolver.GetAlertHistoryTable(),
	}

	for name, tableName := range tables {
		status := ResourceStatus{
			Name:       tableName,
			Type:       "DynamoDB Table",
			Exists:     false,
			Accessible: false,
		}

		// Try to describe the table
		_, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
			TableName: aws.String(tableName),
		})

		if err != nil {
			status.Error = err.Error()
			if contains(err.Error(), "ResourceNotFoundException") || contains(err.Error(), "not found") {
				result.Errors = append(result.Errors, fmt.Sprintf("DynamoDB table %q does not exist", tableName))
			} else if contains(err.Error(), "AccessDenied") {
				result.Errors = append(result.Errors, fmt.Sprintf("Access denied to DynamoDB table %q", tableName))
			} else {
				result.Errors = append(result.Errors, fmt.Sprintf("Error accessing DynamoDB table %q: %v", tableName, err))
			}
		} else {
			status.Exists = true
			status.Accessible = true
		}

		result.Resources[fmt.Sprintf("dynamodb_%s", name)] = status
	}
}

func (v *Validator) validateS3Buckets(ctx context.Context, result *ValidationResult) {
	client := s3.NewFromConfig(v.awsCfg)

	buckets := map[string]string{
		"binaries":  v.resolver.GetBinariesBucket(),
		"schedules": v.resolver.GetSchedulesBucket(),
	}

	for name, bucketName := range buckets {
		status := ResourceStatus{
			Name:       bucketName,
			Type:       "S3 Bucket",
			Exists:     false,
			Accessible: false,
		}

		// Try to head the bucket
		_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{
			Bucket: aws.String(bucketName),
		})

		if err != nil {
			status.Error = err.Error()
			if contains(err.Error(), "NotFound") || contains(err.Error(), "NoSuchBucket") {
				result.Errors = append(result.Errors, fmt.Sprintf("S3 bucket %q does not exist", bucketName))
			} else if contains(err.Error(), "Forbidden") || contains(err.Error(), "AccessDenied") {
				result.Errors = append(result.Errors, fmt.Sprintf("Access denied to S3 bucket %q", bucketName))
			} else {
				result.Errors = append(result.Errors, fmt.Sprintf("Error accessing S3 bucket %q: %v", bucketName, err))
			}
		} else {
			status.Exists = true
			status.Accessible = true
		}

		result.Resources[fmt.Sprintf("s3_%s", name)] = status
	}
}

func (v *Validator) validateLambdaFunctions(ctx context.Context, result *ValidationResult) {
	client := lambda.NewFromConfig(v.awsCfg)

	functions := map[string]string{
		"scheduler_handler":  v.resolver.GetSchedulerHandlerARN(),
		"sweep_orchestrator": v.resolver.GetSweepOrchestratorARN(),
		"alert_handler":      v.resolver.GetAlertHandlerARN(),
		"dashboard_api":      v.resolver.GetDashboardAPIARN(),
	}

	for name, functionARN := range functions {
		status := ResourceStatus{
			Name:       functionARN,
			Type:       "Lambda Function",
			Exists:     false,
			Accessible: false,
		}

		// Extract function name from ARN (format: arn:aws:lambda:region:account:function:name)
		functionName := extractFunctionName(functionARN)

		// Try to get function configuration
		_, err := client.GetFunction(ctx, &lambda.GetFunctionInput{
			FunctionName: aws.String(functionName),
		})

		if err != nil {
			status.Error = err.Error()
			if contains(err.Error(), "ResourceNotFoundException") || contains(err.Error(), "not found") {
				result.Errors = append(result.Errors, fmt.Sprintf("Lambda function %q does not exist", functionName))
			} else if contains(err.Error(), "AccessDenied") {
				result.Errors = append(result.Errors, fmt.Sprintf("Access denied to Lambda function %q", functionName))
			} else {
				result.Errors = append(result.Errors, fmt.Sprintf("Error accessing Lambda function %q: %v", functionName, err))
			}
		} else {
			status.Exists = true
			status.Accessible = true
		}

		result.Resources[fmt.Sprintf("lambda_%s", name)] = status
	}
}

// extractFunctionName extracts the function name from a Lambda ARN
// Format: arn:aws:lambda:region:account:function:name
func extractFunctionName(arn string) string {
	// Simple extraction: split by ':' and get the last part
	parts := splitString(arn, ":")
	if len(parts) >= 7 {
		return parts[6]
	}
	return arn // Return the full ARN if parsing fails
}

// splitString is a simple string splitter
func splitString(s, sep string) []string {
	if s == "" {
		return []string{}
	}

	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if i+len(sep) <= len(s) && s[i:i+len(sep)] == sep {
			result = append(result, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	result = append(result, s[start:])
	return result
}

// contains checks if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// GetRecommendations returns recommendations based on validation errors
func (v *Validator) GetRecommendations(result *ValidationResult) []string {
	if result.Valid {
		return []string{"All infrastructure resources are accessible."}
	}

	recommendations := []string{}

	// Check for missing DynamoDB tables
	hasDynamoDBErrors := false
	for _, err := range result.Errors {
		if contains(err, "DynamoDB") {
			hasDynamoDBErrors = true
			break
		}
	}
	if hasDynamoDBErrors {
		recommendations = append(recommendations, "Deploy DynamoDB tables using CloudFormation: spawn config deploy-infrastructure")
	}

	// Check for missing S3 buckets
	hasS3Errors := false
	for _, err := range result.Errors {
		if contains(err, "S3") {
			hasS3Errors = true
			break
		}
	}
	if hasS3Errors {
		recommendations = append(recommendations, "Create S3 buckets or update configuration with correct bucket names")
	}

	// Check for missing Lambda functions
	hasLambdaErrors := false
	for _, err := range result.Errors {
		if contains(err, "Lambda") {
			hasLambdaErrors = true
			break
		}
	}
	if hasLambdaErrors {
		recommendations = append(recommendations, "Deploy Lambda functions using CloudFormation: spawn config deploy-infrastructure")
	}

	// Check if in self-hosted mode but resources not found
	if v.resolver.IsSelfHosted() {
		recommendations = append(recommendations, "Run: spawn config init --self-hosted to reconfigure infrastructure")
	}

	return recommendations
}
