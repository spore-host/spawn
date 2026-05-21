package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbTypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	dynamoAlertPreferencesTable = "spawn-alert-preferences"
)

// AlertPreferences stores per-user alert configuration
type AlertPreferences struct {
	UserID                 string  `dynamodbav:"user_id"                            json:"user_id"`
	Enabled                bool    `dynamodbav:"enabled"                            json:"enabled"`
	CostThresholdHourly    float64 `dynamodbav:"cost_threshold_hourly,omitempty"    json:"cost_threshold_hourly,omitempty"`
	CostThresholdDaily     float64 `dynamodbav:"cost_threshold_daily,omitempty"     json:"cost_threshold_daily,omitempty"`
	InstanceCountThreshold int     `dynamodbav:"instance_count_threshold,omitempty" json:"instance_count_threshold,omitempty"`
	QueueDepthHigh         int     `dynamodbav:"queue_depth_high,omitempty"         json:"queue_depth_high,omitempty"`
	QueueDepthLow          int     `dynamodbav:"queue_depth_low,omitempty"          json:"queue_depth_low,omitempty"`
	NotificationEmail      string  `dynamodbav:"notification_email,omitempty"       json:"notification_email,omitempty"`
}

// AlertPreferencesAPIResponse is the response for GET /api/alert-preferences
type AlertPreferencesAPIResponse struct {
	Success     bool             `json:"success"`
	Preferences AlertPreferences `json:"preferences"`
}

// handleGetAlertPreferences handles GET /api/alert-preferences
func handleGetAlertPreferences(ctx context.Context, cfg aws.Config, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	result, err := dynamodbClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(dynamoAlertPreferencesTable),
		Key: map[string]ddbTypes.AttributeValue{
			"user_id": &ddbTypes.AttributeValueMemberS{Value: cliIamArn},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to get alert preferences: %v", err)), nil
	}

	prefs := AlertPreferences{
		UserID:  cliIamArn,
		Enabled: false,
	}

	if result.Item != nil {
		if err := attributevalue.UnmarshalMap(result.Item, &prefs); err != nil {
			return errorResponse(500, fmt.Sprintf("Failed to unmarshal preferences: %v", err)), nil
		}
	}

	return successResponse(AlertPreferencesAPIResponse{
		Success:     true,
		Preferences: prefs,
	})
}

// handleSaveAlertPreferences handles POST /api/alert-preferences
func handleSaveAlertPreferences(ctx context.Context, cfg aws.Config, body, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	var prefs AlertPreferences
	if err := json.Unmarshal([]byte(body), &prefs); err != nil {
		return errorResponse(400, fmt.Sprintf("Invalid request body: %v", err)), nil
	}

	// Always use the authenticated user's ID
	prefs.UserID = cliIamArn

	dynamodbClient := dynamodb.NewFromConfig(cfg)

	item, err := attributevalue.MarshalMap(prefs)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to marshal preferences: %v", err)), nil
	}

	_, err = dynamodbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(dynamoAlertPreferencesTable),
		Item:      item,
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to save preferences: %v", err)), nil
	}

	return successResponse(map[string]interface{}{
		"success": true,
		"message": "Alert preferences saved",
	})
}
