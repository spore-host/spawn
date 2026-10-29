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

	// watchHistoryLimit bounds GET /api/watches/history to the newest matches.
	watchHistoryLimit int32 = 100
)

// WatchInfo represents a lagotto watch for the dashboard API.
type WatchInfo struct {
	// UserID is the owning principal's CLI IAM ARN. json:"-" because it is an
	// authorization input, not dashboard data — adding it to the struct is what
	// lets the ownership check be a plain comparison instead of a lookup into the
	// raw item map, which is how it came to be fail-open (#821).
	UserID              string     `json:"-" dynamodbav:"user_id"`
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
	// UserID is the owning principal, an authorization input like
	// WatchInfo.UserID, so it is never serialized.
	UserID           string  `json:"-" dynamodbav:"user_id"`
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
	// Truncated is set when Matches is the newest watchHistoryLimit of a longer
	// history (#1421).
	Truncated bool `json:"truncated,omitempty"`
}

func handleListWatches(ctx context.Context, cfg aws.Config, userARN string) (events.APIGatewayProxyResponse, error) {
	client := dynamodb.NewFromConfig(cfg)
	tableName := getEnvOrDefault("LAGOTTO_WATCHES_TABLE", defaultWatchesTable)

	// Every page, or an error: one page used to be all the dashboard read, so a
	// user with more than a page of watches saw a partial list as complete (#1421).
	items, err := queryAll(ctx, client, &dynamodb.QueryInput{
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

	watches := make([]WatchInfo, 0, len(items))
	for _, item := range items {
		var w WatchInfo
		if err := attributevalue.UnmarshalMap(item, &w); err != nil {
			continue
		}
		// The key condition already selects the caller's watches; this is the
		// same fail-closed comparison handleGetWatch makes, on every page, so a
		// record without the caller's user_id is never returned (#821).
		if w.UserID != userARN {
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

	// Verify ownership.
	//
	// This was the only fail-OPEN ownership check in the file set. It read the raw
	// item map and skipped the comparison entirely when `user_id` was absent — or
	// present but not a string — so a watch record missing that attribute was
	// returned to any authenticated caller who could name its ID (#821,
	// GHSA-qc2h-6qqf-wcm4). Comparing the unmarshalled field instead makes both of
	// those a mismatch against "", i.e. a 403, which is the idiom every sibling
	// handler already uses (sweeps.go, instances.go).
	if w.UserID != userARN {
		return errorResponse(403, "Access denied"), nil
	}

	return successResponse(WatchesResponse{
		Success: true,
		Watch:   &w,
	})
}

func handleWatchHistory(ctx context.Context, cfg aws.Config, userARN string) (events.APIGatewayProxyResponse, error) {
	client := dynamodb.NewFromConfig(cfg)
	tableName := getEnvOrDefault("LAGOTTO_HISTORY_TABLE", defaultHistoryTable)

	// The newest watchHistoryLimit matches, deliberately bounded: history grows
	// with every poll match and the dashboard shows recent activity. The bound is
	// reached across pages, not assumed of one, and Truncated says when older
	// matches exist, so the list never passes for the whole history (#1421).
	items, truncated, err := queryUpTo(ctx, client, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		IndexName:              aws.String("user_id-index"),
		KeyConditionExpression: aws.String("user_id = :uid"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":uid": &dynamodbtypes.AttributeValueMemberS{Value: userARN},
		},
		ScanIndexForward: aws.Bool(false),
	}, watchHistoryLimit)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("query history: %v", err)), nil
	}

	matches := make([]MatchInfo, 0, len(items))
	for _, item := range items {
		var m MatchInfo
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			continue
		}
		// Fail closed, as in handleListWatches: only the caller's records.
		if m.UserID != userARN {
			continue
		}
		matches = append(matches, m)
	}

	return successResponse(WatchesResponse{
		Success:   true,
		Matches:   matches,
		Truncated: truncated,
	})
}
