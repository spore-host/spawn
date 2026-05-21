package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	defaultWatchesTable = "lagotto-watches"
	defaultHistoryTable = "lagotto-match-history"
)

// WatchInfo represents a lagotto watch for the dashboard API.
type WatchInfo struct {
	WatchID             string     `json:"watch_id" dynamodbav:"watch_id"`
	Status              string     `json:"status" dynamodbav:"status"`
	InstanceTypePattern string     `json:"instance_type_pattern" dynamodbav:"instance_type_pattern"`
	Regions             []string   `json:"regions" dynamodbav:"regions"`
	Spot                bool       `json:"spot" dynamodbav:"spot"`
	MaxPrice            float64    `json:"max_price,omitempty" dynamodbav:"max_price,omitempty"`
	Action              string     `json:"action" dynamodbav:"action"`
	CreatedAt           string     `json:"created_at" dynamodbav:"created_at"`
	ExpiresAt           string     `json:"expires_at" dynamodbav:"expires_at"`
	LastPolledAt        string     `json:"last_polled_at,omitempty" dynamodbav:"last_polled_at,omitempty"`
	MatchCount          int        `json:"match_count" dynamodbav:"match_count"`
	LastMatch           *MatchInfo `json:"last_match,omitempty" dynamodbav:"last_match,omitempty"`
}

// MatchInfo represents a capacity match for the dashboard API.
type MatchInfo struct {
	WatchID          string  `json:"watch_id" dynamodbav:"watch_id"`
	Region           string  `json:"region" dynamodbav:"region"`
	AvailabilityZone string  `json:"availability_zone" dynamodbav:"availability_zone"`
	InstanceType     string  `json:"instance_type" dynamodbav:"instance_type"`
	Price            float64 `json:"price" dynamodbav:"price"`
	IsSpot           bool    `json:"is_spot" dynamodbav:"is_spot"`
	MatchedAt        string  `json:"matched_at" dynamodbav:"matched_at"`
	ActionTaken      string  `json:"action_taken" dynamodbav:"action_taken"`
	InstanceID       string  `json:"instance_id,omitempty" dynamodbav:"instance_id,omitempty"`
}

// WatchesResponse is the API response for watch endpoints.
type WatchesResponse struct {
	Success bool        `json:"success"`
	Watches []WatchInfo `json:"watches,omitempty"`
	Watch   *WatchInfo  `json:"watch,omitempty"`
	Matches []MatchInfo `json:"matches,omitempty"`
	Error   string      `json:"error,omitempty"`
}

func handleListWatches(ctx context.Context, cfg aws.Config, userARN string) (events.APIGatewayProxyResponse, error) {
	client := dynamodb.NewFromConfig(cfg)
	tableName := getEnvOrDefault("LAGOTTO_WATCHES_TABLE", defaultWatchesTable)

	result, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		IndexName:              aws.String("user_id-index"),
		KeyConditionExpression: aws.String("user_id = :uid"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":uid": &dynamodbtypes.AttributeValueMemberS{Value: userARN},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("query watches: %v", err)), nil
	}

	watches := make([]WatchInfo, 0, len(result.Items))
	for _, item := range result.Items {
		var w WatchInfo
		if err := attributevalue.UnmarshalMap(item, &w); err != nil {
			continue
		}
		watches = append(watches, w)
	}

	return successResponse(WatchesResponse{
		Success: true,
		Watches: watches,
	})
}

func handleGetWatch(ctx context.Context, cfg aws.Config, watchID, userARN string) (events.APIGatewayProxyResponse, error) {
	client := dynamodb.NewFromConfig(cfg)
	tableName := getEnvOrDefault("LAGOTTO_WATCHES_TABLE", defaultWatchesTable)

	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"watch_id": &dynamodbtypes.AttributeValueMemberS{Value: watchID},
		},
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("get watch: %v", err)), nil
	}
	if result.Item == nil {
		return errorResponse(404, "Watch not found"), nil
	}

	var w WatchInfo
	if err := attributevalue.UnmarshalMap(result.Item, &w); err != nil {
		return errorResponse(500, fmt.Sprintf("unmarshal watch: %v", err)), nil
	}

	// Verify ownership
	if v, ok := result.Item["user_id"]; ok {
		if s, ok := v.(*dynamodbtypes.AttributeValueMemberS); ok && s.Value != userARN {
			return errorResponse(403, "Access denied"), nil
		}
	}

	return successResponse(WatchesResponse{
		Success: true,
		Watch:   &w,
	})
}

func handleWatchHistory(ctx context.Context, cfg aws.Config, userARN string) (events.APIGatewayProxyResponse, error) {
	client := dynamodb.NewFromConfig(cfg)
	tableName := getEnvOrDefault("LAGOTTO_HISTORY_TABLE", defaultHistoryTable)

	result, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		IndexName:              aws.String("user_id-index"),
		KeyConditionExpression: aws.String("user_id = :uid"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":uid": &dynamodbtypes.AttributeValueMemberS{Value: userARN},
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(100),
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("query history: %v", err)), nil
	}

	matches := make([]MatchInfo, 0, len(result.Items))
	for _, item := range result.Items {
		var m MatchInfo
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			continue
		}
		matches = append(matches, m)
	}

	return successResponse(WatchesResponse{
		Success: true,
		Matches: matches,
	})
}
