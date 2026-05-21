package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// awsAccountIDRe matches a valid 12-digit AWS account ID.
var awsAccountIDRe = regexp.MustCompile(`^\d{12}$`)

const (
	dynamoSweepTable = "spawn-sweep-orchestration"
)

// handleListSweeps handles GET /api/sweeps
// When teamID is non-empty, team sweeps are merged with personal sweeps.
func handleListSweeps(ctx context.Context, cfg aws.Config, cliIamArn, teamID string) (events.APIGatewayProxyResponse, error) {
	startTime := time.Now()

	dynamodbClient := dynamodb.NewFromConfig(cfg)

	// Personal sweeps via scan with user_id filter
	scanInput := &dynamodb.ScanInput{
		TableName:        aws.String(dynamoSweepTable),
		FilterExpression: aws.String("user_id = :user_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":user_id": &types.AttributeValueMemberS{Value: cliIamArn},
		},
		Limit: aws.Int32(200),
	}
	result, err := dynamodbClient.Scan(ctx, scanInput)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to query sweeps: %v", err)), nil
	}

	seen := make(map[string]struct{})
	var sweeps []SweepInfo

	appendSweepRecords := func(items []map[string]types.AttributeValue) {
		for _, item := range items {
			var sweep SweepRecord
			if err := attributevalue.UnmarshalMap(item, &sweep); err != nil {
				continue
			}
			if _, dup := seen[sweep.SweepID]; dup {
				continue
			}
			seen[sweep.SweepID] = struct{}{}

			createdAt, _ := time.Parse(time.RFC3339, sweep.CreatedAt)
			updatedAt, _ := time.Parse(time.RFC3339, sweep.UpdatedAt)
			completedAt, _ := time.Parse(time.RFC3339, sweep.CompletedAt)

			sweepInfo := SweepInfo{
				SweepID:       sweep.SweepID,
				SweepName:     sweep.SweepName,
				Status:        sweep.Status,
				TotalParams:   sweep.TotalParams,
				Launched:      sweep.Launched,
				Failed:        sweep.Failed,
				Region:        sweep.Region,
				CreatedAt:     createdAt,
				UpdatedAt:     updatedAt,
				EstimatedCost: sweep.EstimatedCost,
			}
			if !completedAt.IsZero() {
				sweepInfo.CompletedAt = &completedAt
				sweepInfo.DurationSeconds = int(completedAt.Sub(createdAt).Seconds())
			}
			sweeps = append(sweeps, sweepInfo)
		}
	}

	appendSweepRecords(result.Items)

	// Team sweeps via team_id-index GSI
	if teamID != "" {
		if _, err := resolveTeamContext(ctx, cfg, teamID, cliIamArn); err != nil {
			return errorResponse(403, "access denied"), nil
		}
		teamResult, err := dynamodbClient.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(dynamoSweepTable),
			IndexName:              aws.String("team_id-index"),
			KeyConditionExpression: aws.String("team_id = :tid"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":tid": &types.AttributeValueMemberS{Value: teamID},
			},
		})
		if err != nil {
			log.Printf("warning: failed to query team sweeps: %v", err)
		} else {
			appendSweepRecords(teamResult.Items)
		}
	}

	elapsed := time.Since(startTime)
	log.Printf("listed %d sweeps in %v", len(sweeps), elapsed)

	if sweeps == nil {
		sweeps = []SweepInfo{}
	}

	response := SweepAPIResponse{
		Success:     true,
		TotalSweeps: len(sweeps),
		Sweeps:      sweeps,
	}

	return successResponse(response)
}

// handleGetSweep handles GET /api/sweeps/{id}
func handleGetSweep(ctx context.Context, cfg aws.Config, sweepID, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	// Query DynamoDB for sweep
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	getInput := &dynamodb.GetItemInput{
		TableName: aws.String(dynamoSweepTable),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
	}

	result, err := dynamodbClient.GetItem(ctx, getInput)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to get sweep: %v", err)), nil
	}

	if result.Item == nil {
		return errorResponse(404, "Sweep not found"), nil
	}

	var sweep SweepRecord
	if err := attributevalue.UnmarshalMap(result.Item, &sweep); err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to unmarshal sweep: %v", err)), nil
	}

	// Verify sweep belongs to this user
	if sweep.UserID != cliIamArn {
		return errorResponse(403, "Access denied"), nil
	}

	// Build detailed sweep info
	createdAt, _ := time.Parse(time.RFC3339, sweep.CreatedAt)
	updatedAt, _ := time.Parse(time.RFC3339, sweep.UpdatedAt)
	completedAt, _ := time.Parse(time.RFC3339, sweep.CompletedAt)

	sweepInfo := SweepDetailInfo{
		SweepID:         sweep.SweepID,
		SweepName:       sweep.SweepName,
		Status:          sweep.Status,
		TotalParams:     sweep.TotalParams,
		Launched:        sweep.Launched,
		Failed:          sweep.Failed,
		Region:          sweep.Region,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
		EstimatedCost:   sweep.EstimatedCost,
		MaxConcurrent:   sweep.MaxConcurrent,
		LaunchDelay:     sweep.LaunchDelay,
		NextToLaunch:    sweep.NextToLaunch,
		CancelRequested: sweep.CancelRequested,
	}

	if !completedAt.IsZero() {
		sweepInfo.CompletedAt = &completedAt
		sweepInfo.DurationSeconds = int(completedAt.Sub(createdAt).Seconds())
	}

	// Convert instances
	for _, inst := range sweep.Instances {
		launchedAt, _ := time.Parse(time.RFC3339, inst.LaunchedAt)
		terminatedAt, _ := time.Parse(time.RFC3339, inst.TerminatedAt)

		instanceInfo := SweepInstanceInfo{
			Index:        inst.Index,
			InstanceID:   inst.InstanceID,
			State:        inst.State,
			LaunchedAt:   launchedAt,
			ErrorMessage: inst.ErrorMessage,
		}

		if !terminatedAt.IsZero() {
			instanceInfo.TerminatedAt = &terminatedAt
		}

		sweepInfo.Instances = append(sweepInfo.Instances, instanceInfo)
	}

	// Build response
	response := SweepDetailAPIResponse{
		Success: true,
		Sweep:   sweepInfo,
	}

	return successResponse(response)
}

// handleCancelSweep handles POST /api/sweeps/{id}/cancel
func handleCancelSweep(ctx context.Context, cfg aws.Config, sweepID, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	// Get sweep details
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	getInput := &dynamodb.GetItemInput{
		TableName: aws.String(dynamoSweepTable),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
	}

	result, err := dynamodbClient.GetItem(ctx, getInput)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to get sweep: %v", err)), nil
	}

	if result.Item == nil {
		return errorResponse(404, "Sweep not found"), nil
	}

	var sweep SweepRecord
	if err := attributevalue.UnmarshalMap(result.Item, &sweep); err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to unmarshal sweep: %v", err)), nil
	}

	// Verify sweep belongs to this user
	if sweep.UserID != cliIamArn {
		return errorResponse(403, "Access denied"), nil
	}

	// Check if already in terminal state
	if sweep.Status == "COMPLETED" || sweep.Status == "CANCELLED" || sweep.Status == "FAILED" {
		return errorResponse(400, fmt.Sprintf("Sweep is already %s", sweep.Status)), nil
	}

	// Update sweep status to cancelled
	now := time.Now().Format(time.RFC3339)
	updateInput := &dynamodb.UpdateItemInput{
		TableName: aws.String(dynamoSweepTable),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
		UpdateExpression: aws.String("SET #status = :status, cancel_requested = :cancel_requested, completed_at = :completed_at, updated_at = :updated_at"),
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":status":           &types.AttributeValueMemberS{Value: "CANCELLED"},
			":cancel_requested": &types.AttributeValueMemberBOOL{Value: true},
			":completed_at":     &types.AttributeValueMemberS{Value: now},
			":updated_at":       &types.AttributeValueMemberS{Value: now},
		},
	}

	_, err = dynamodbClient.UpdateItem(ctx, updateInput)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to update sweep: %v", err)), nil
	}

	// Terminate running instances using cross-account role
	var instanceIDs []string
	for _, inst := range sweep.Instances {
		if inst.State == "running" && inst.InstanceID != "" {
			instanceIDs = append(instanceIDs, inst.InstanceID)
		}
	}

	if len(instanceIDs) > 0 {
		err = terminateSweepInstancesCrossAccount(ctx, cfg, sweep.AWSAccountID, sweep.Region, instanceIDs)
		if err != nil {
			log.Printf("warning: failed to terminate instances: %v", err)
			// Don't fail the request - sweep is still marked as cancelled
		} else {
			log.Printf("terminated %d instances", len(instanceIDs))
		}
	}

	// Build response
	response := CancelSweepResponse{
		Success:             true,
		Message:             "Sweep cancelled successfully",
		InstancesTerminated: len(instanceIDs),
	}

	return successResponse(response)
}

// handleCleanupSweeps handles POST /api/sweeps/cleanup
func handleCleanupSweeps(ctx context.Context, cfg aws.Config, body string, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	// Parse request body
	var request struct {
		SweepIDs []string `json:"sweep_ids"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		return errorResponse(400, "Invalid request body"), nil
	}

	if len(request.SweepIDs) == 0 {
		return errorResponse(400, "No sweep IDs provided"), nil
	}

	const maxSweepCleanupIDs = 100
	if len(request.SweepIDs) > maxSweepCleanupIDs {
		return errorResponse(400, fmt.Sprintf("too many sweep_ids: max %d", maxSweepCleanupIDs)), nil
	}

	dynamodbClient := dynamodb.NewFromConfig(cfg)
	deletedCount := 0

	// Delete each sweep
	for _, sweepID := range request.SweepIDs {
		// First, get the sweep to verify it belongs to this user
		getInput := &dynamodb.GetItemInput{
			TableName: aws.String(dynamoSweepTable),
			Key: map[string]types.AttributeValue{
				"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
			},
		}

		result, err := dynamodbClient.GetItem(ctx, getInput)
		if err != nil {
			log.Printf("warning: failed to get sweep %s: %v", sweepID, err)
			continue
		}

		if result.Item == nil {
			log.Printf("warning: sweep %s not found", sweepID)
			continue
		}

		var sweep SweepRecord
		if err := attributevalue.UnmarshalMap(result.Item, &sweep); err != nil {
			log.Printf("warning: failed to unmarshal sweep %s: %v", sweepID, err)
			continue
		}

		// Verify sweep belongs to this user
		if sweep.UserID != cliIamArn {
			log.Printf("warning: sweep %s not owned by caller", sweepID)
			continue
		}

		// Only delete if completed, cancelled, or failed
		if sweep.Status != "COMPLETED" && sweep.Status != "CANCELLED" && sweep.Status != "FAILED" {
			log.Printf("warning: sweep %s is %s, skipping", sweepID, sweep.Status)
			continue
		}

		// Delete the sweep
		deleteInput := &dynamodb.DeleteItemInput{
			TableName: aws.String(dynamoSweepTable),
			Key: map[string]types.AttributeValue{
				"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
			},
		}

		_, err = dynamodbClient.DeleteItem(ctx, deleteInput)
		if err != nil {
			log.Printf("warning: failed to delete sweep %s: %v", sweepID, err)
			continue
		}

		deletedCount++
	}

	// Build response
	response := struct {
		Success      bool   `json:"success"`
		Message      string `json:"message"`
		DeletedCount int    `json:"deleted_count"`
	}{
		Success:      true,
		Message:      fmt.Sprintf("Successfully deleted %d sweep(s)", deletedCount),
		DeletedCount: deletedCount,
	}

	return successResponse(response)
}

// terminateSweepInstancesCrossAccount terminates instances using cross-account role assumption
func terminateSweepInstancesCrossAccount(ctx context.Context, cfg aws.Config, accountID, region string, instanceIDs []string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	// Validate account ID format before constructing ARN.
	if !awsAccountIDRe.MatchString(accountID) {
		return fmt.Errorf("invalid AWS account ID %q", accountID)
	}

	// Assume cross-account role
	stsClient := sts.NewFromConfig(cfg)
	roleArn := fmt.Sprintf("arn:aws:iam::%s:role/SpawnSweepCrossAccountRole", accountID)

	assumeResult, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleArn),
		RoleSessionName: aws.String("dashboard-cancel-" + time.Now().Format("20060102-150405")),
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
