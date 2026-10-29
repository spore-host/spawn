package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

const (
	dynamoSweepTable = "spawn-sweep-orchestration"
	// sweepUserIndex is the table's user_id (HASH) / created_at (RANGE) GSI,
	// projection ALL. The dashboard role grants Query on exactly this index
	// (scripts/setup-dashboard-lambda-role.sh, the Sweeps statement).
	sweepUserIndex = "user_id-created_at-index"
)

// handleListSweeps handles GET /api/sweeps
// When teamID is non-empty, team sweeps are merged with personal sweeps.
func handleListSweeps(ctx context.Context, cfg aws.Config, cliIamArn, teamID string) (events.APIGatewayProxyResponse, error) {
	startTime := time.Now()

	dynamodbClient := dynamodb.NewFromConfig(cfg)

	// Personal sweeps via a Query on user_id-created_at-index (#1426), every
	// page (#1421). This was a Scan of the whole table filtered on user_id, so
	// each dashboard load read every user's sweeps. The index projects ALL
	// attributes (scripts/setup-sweep-dynamodb.sh), so its items are complete
	// records and need no read-back from the base table; and every writer sets
	// both user_id and created_at, so no sweep is missing from it. Newest first.
	personal, err := queryAll(ctx, dynamodbClient, &dynamodb.QueryInput{
		TableName:              aws.String(dynamoSweepTable),
		IndexName:              aws.String(sweepUserIndex),
		KeyConditionExpression: aws.String("user_id = :user_id"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":user_id": &types.AttributeValueMemberS{Value: cliIamArn},
		},
		ScanIndexForward: aws.Bool(false),
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to query sweeps: %v", err)), nil
	}

	seen := make(map[string]struct{})
	var sweeps []SweepInfo

	appendSweepRecords := func(items []map[string]types.AttributeValue, owned func(SweepRecord) bool) {
		for _, item := range items {
			var sweep SweepRecord
			if err := attributevalue.UnmarshalMap(item, &sweep); err != nil {
				continue
			}
			if !owned(sweep) {
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

	// Fail closed on every page: a personal sweep is the caller's only if its
	// user_id says so, whatever the filter returned.
	appendSweepRecords(personal, func(s SweepRecord) bool { return s.UserID == cliIamArn })

	// Team sweeps via team_id-index GSI
	if teamID != "" {
		if _, err := resolveTeamContext(ctx, cfg, teamID, cliIamArn); err != nil {
			return errorResponse(403, "access denied"), nil
		}
		// Every page, and a failure is an error: this used to log a warning
		// and return the personal sweeps alone, a partial list presented as
		// complete (#1421).
		teamItems, err := queryAll(ctx, dynamodbClient, &dynamodb.QueryInput{
			TableName:              aws.String(dynamoSweepTable),
			IndexName:              aws.String("team_id-index"),
			KeyConditionExpression: aws.String("team_id = :tid"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":tid": &types.AttributeValueMemberS{Value: teamID},
			},
		})
		if err != nil {
			return errorResponse(500, fmt.Sprintf("Failed to query team sweeps: %v", err)), nil
		}
		appendSweepRecords(teamItems, func(s SweepRecord) bool { return s.TeamID == teamID })
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
		SweepID:          sweep.SweepID,
		SweepName:        sweep.SweepName,
		Status:           sweep.Status,
		TotalParams:      sweep.TotalParams,
		Launched:         sweep.Launched,
		Failed:           sweep.Failed,
		Region:           sweep.Region,
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
		EstimatedCost:    sweep.EstimatedCost,
		MaxConcurrent:    sweep.MaxConcurrent,
		LaunchDelay:      sweep.LaunchDelay,
		NextToLaunch:     sweep.NextToLaunch,
		CancelRequested:  sweep.CancelRequested,
		CancelTerminated: sweep.CancelTerminated,
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

	// The members are not terminated here (#1349). Only the sweep orchestrator
	// may act in the member account: SpawnSweepCrossAccountRole trusts
	// SpawnSweepOrchestratorRole alone, and the owner decided against giving
	// this Lambda a terminate path of its own. The record now says CANCELLED;
	// invoking the orchestrator makes it terminate the sweep's live members
	// even when no invocation of it is running to notice the flag.
	pending := 0
	for _, inst := range sweep.Instances {
		if inst.InstanceID != "" && liveMemberState[inst.State] {
			pending++
		}
	}

	message := "Sweep cancelled. The sweep orchestrator is terminating its instances."
	if err := invokeSweepOrchestrator(ctx, cfg, sweepID); err != nil {
		log.Printf("warning: sweep %s cancelled, but invoking %s failed: %v", sweepID, sweepOrchestratorFunction(), err)
		message = fmt.Sprintf("Sweep cancelled. The sweep orchestrator could not be started to terminate its "+
			"instances; if they are still running in a few minutes, run: spawn cancel %s", sweepID)
	}

	response := CancelSweepResponse{
		Success:              true,
		Message:              message,
		InstancesTerminated:  0,
		InstancesToTerminate: pending,
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

// liveMemberState are the states the orchestrator terminates on a cancel.
var liveMemberState = map[string]bool{"pending": true, "running": true, "stopping": true, "stopped": true}

// defaultSweepOrchestratorFunction is the function scripts/deploy-sweep-orchestrator.sh
// creates, the name the CLI and scheduler-handler invoke.
const defaultSweepOrchestratorFunction = "spawn-sweep-orchestrator"

// sweepOrchestratorFunction is the orchestrator to invoke, overridable as in
// scheduler-handler.
func sweepOrchestratorFunction() string {
	return getEnvOrDefault("SPAWN_ORCHESTRATOR_FUNCTION_NAME", defaultSweepOrchestratorFunction)
}

// orchestratorEvent is the orchestrator's input (pkg/sweep.OrchestratorEvent).
type orchestratorEvent struct {
	SweepID string `json:"sweep_id"`
}

// invokeSweepOrchestrator starts the orchestrator for sweepID asynchronously,
// as `spawn launch --detach` does, with this Lambda's own credentials
// (lambda:InvokeFunction, setup-dashboard-lambda-role.sh).
func invokeSweepOrchestrator(ctx context.Context, cfg aws.Config, sweepID string) error {
	payload, err := json.Marshal(orchestratorEvent{SweepID: sweepID})
	if err != nil {
		return err
	}
	_, err = lambdasvc.NewFromConfig(cfg).Invoke(ctx, &lambdasvc.InvokeInput{
		FunctionName:   aws.String(sweepOrchestratorFunction()),
		InvocationType: lambdatypes.InvocationTypeEvent,
		Payload:        payload,
	})
	return err
}
