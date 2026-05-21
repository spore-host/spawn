package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbTypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	dynamoCostHistoryTable = "spawn-cost-history"
)

// CostHistoryRecord is a single hourly cost snapshot stored in DynamoDB
type CostHistoryRecord struct {
	UserID          string         `dynamodbav:"user_id"`
	Timestamp       string         `dynamodbav:"timestamp"`
	HourlyCost      float64        `dynamodbav:"hourly_cost"`
	MonthlyEstimate float64        `dynamodbav:"monthly_estimate"`
	InstanceCount   int            `dynamodbav:"instance_count"`
	Breakdown       CostComponents `dynamodbav:"breakdown"`
	TTL             int64          `dynamodbav:"ttl,omitempty"`
}

// CostComponents breaks down cost by type
type CostComponents struct {
	Compute float64 `dynamodbav:"compute" json:"compute"`
	Storage float64 `dynamodbav:"storage" json:"storage"`
	Network float64 `dynamodbav:"network" json:"network"`
}

// CostHistoryAPIResponse is the response for GET /api/cost-history
type CostHistoryAPIResponse struct {
	Success bool               `json:"success"`
	Days    int                `json:"days"`
	History []CostHistoryPoint `json:"history"`
}

// CostHistoryPoint is a single point in the cost history chart
type CostHistoryPoint struct {
	Timestamp       string         `json:"timestamp"`
	HourlyCost      float64        `json:"hourly_cost"`
	MonthlyEstimate float64        `json:"monthly_estimate"`
	InstanceCount   int            `json:"instance_count"`
	Breakdown       CostComponents `json:"breakdown"`
}

// handleGetCostHistory handles GET /api/cost-history?days=30
func handleGetCostHistory(ctx context.Context, cfg aws.Config, days int, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	since := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)

	result, err := dynamodbClient.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(dynamoCostHistoryTable),
		KeyConditionExpression: aws.String("user_id = :uid AND #ts >= :since"),
		ExpressionAttributeNames: map[string]string{
			"#ts": "timestamp",
		},
		ExpressionAttributeValues: map[string]ddbTypes.AttributeValue{
			":uid":   &ddbTypes.AttributeValueMemberS{Value: cliIamArn},
			":since": &ddbTypes.AttributeValueMemberS{Value: since},
		},
		ScanIndexForward: aws.Bool(true), // oldest first
	})
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to query cost history: %v", err)), nil
	}

	history := make([]CostHistoryPoint, 0, len(result.Items))
	for _, item := range result.Items {
		var record CostHistoryRecord
		if err := attributevalue.UnmarshalMap(item, &record); err != nil {
			continue
		}
		history = append(history, CostHistoryPoint{
			Timestamp:       record.Timestamp,
			HourlyCost:      record.HourlyCost,
			MonthlyEstimate: record.MonthlyEstimate,
			InstanceCount:   record.InstanceCount,
			Breakdown:       record.Breakdown,
		})
	}

	return successResponse(CostHistoryAPIResponse{
		Success: true,
		Days:    days,
		History: history,
	})
}
