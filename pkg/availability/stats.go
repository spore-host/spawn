package availability

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	availabilityTableName = "spawn-availability-stats"
	backoffInitial        = 5 * time.Minute
	backoffMultiplier     = 2
	backoffMax            = 1 * time.Hour
	defaultTTLDays        = 7
)

// AvailabilityStats represents availability statistics for a region+instance_type combination
type AvailabilityStats struct {
	StatID           string `dynamodbav:"stat_id"` // "{region}#{instance_type}"
	Region           string `dynamodbav:"region"`
	InstanceType     string `dynamodbav:"instance_type"`
	SuccessCount     int    `dynamodbav:"success_count"`
	FailureCount     int    `dynamodbav:"failure_count"`
	ConsecutiveFails int    `dynamodbav:"consecutive_fails"`       // For exponential backoff
	LastSuccess      string `dynamodbav:"last_success"`            // RFC3339 timestamp
	LastFailure      string `dynamodbav:"last_failure"`            // RFC3339 timestamp
	LastErrorCode    string `dynamodbav:"last_error_code"`         // e.g., "InsufficientInstanceCapacity"
	BackoffUntil     string `dynamodbav:"backoff_until,omitempty"` // RFC3339 timestamp
	UpdatedAt        string `dynamodbav:"updated_at"`              // RFC3339 timestamp
	TTLTimestamp     int64  `dynamodbav:"ttl_timestamp"`           // Unix epoch for DynamoDB TTL
}

// makeStatID creates a composite key for region+instance_type
func makeStatID(region, instanceType string) string {
	return fmt.Sprintf("%s#%s", region, instanceType)
}

// GetStats retrieves availability stats for a region+instance_type, returns empty stats if not found
func GetStats(ctx context.Context, client *dynamodb.Client, region, instanceType string) (*AvailabilityStats, error) {
	statID := makeStatID(region, instanceType)

	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(availabilityTableName),
		Key: map[string]types.AttributeValue{
			"stat_id": &types.AttributeValueMemberS{Value: statID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get stats: %w", err)
	}

	// Return empty stats if not found
	if result.Item == nil {
		return &AvailabilityStats{
			StatID:       statID,
			Region:       region,
			InstanceType: instanceType,
		}, nil
	}

	var stats AvailabilityStats
	if err := attributevalue.UnmarshalMap(result.Item, &stats); err != nil {
		return nil, fmt.Errorf("failed to unmarshal stats: %w", err)
	}

	return &stats, nil
}

// RecordSuccess records a successful instance launch
func RecordSuccess(ctx context.Context, client *dynamodb.Client, region, instanceType string) error {
	statID := makeStatID(region, instanceType)
	now := time.Now()
	ttl := now.AddDate(0, 0, defaultTTLDays).Unix()

	// Atomic update: increment success count, reset backoff, clear consecutive fails
	_, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(availabilityTableName),
		Key: map[string]types.AttributeValue{
			"stat_id": &types.AttributeValueMemberS{Value: statID},
		},
		UpdateExpression: aws.String("SET success_count = if_not_exists(success_count, :zero) + :one, " +
			"last_success = :now, " +
			"updated_at = :now, " +
			"ttl_timestamp = :ttl, " +
			"#region = :region, " +
			"instance_type = :instance_type, " +
			"consecutive_fails = :zero " +
			"REMOVE backoff_until"),
		ExpressionAttributeNames: map[string]string{
			"#region": "region",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":zero":          &types.AttributeValueMemberN{Value: "0"},
			":one":           &types.AttributeValueMemberN{Value: "1"},
			":now":           &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
			":ttl":           &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", ttl)},
			":region":        &types.AttributeValueMemberS{Value: region},
			":instance_type": &types.AttributeValueMemberS{Value: instanceType},
		},
	})

	return err
}

// RecordFailure records a failed instance launch
func RecordFailure(ctx context.Context, client *dynamodb.Client, region, instanceType string, isCapacityError bool, errorCode string) error {
	statID := makeStatID(region, instanceType)
	now := time.Now()
	ttl := now.AddDate(0, 0, defaultTTLDays).Unix()

	// Get current stats to calculate backoff
	stats, err := GetStats(ctx, client, region, instanceType)
	if err != nil {
		return fmt.Errorf("failed to get stats for backoff calculation: %w", err)
	}

	// Calculate backoff only for capacity errors
	var backoffUntil string
	if isCapacityError {
		consecutiveFails := stats.ConsecutiveFails + 1
		backoffDuration := calculateBackoff(consecutiveFails)
		backoffUntil = now.Add(backoffDuration).Format(time.RFC3339)
	}

	// Build update expression
	updateExpr := "SET failure_count = if_not_exists(failure_count, :zero) + :one, " +
		"last_failure = :now, " +
		"last_error_code = :error_code, " +
		"updated_at = :now, " +
		"ttl_timestamp = :ttl, " +
		"#region = :region, " +
		"instance_type = :instance_type"

	exprNames := map[string]string{
		"#region": "region",
	}

	exprValues := map[string]types.AttributeValue{
		":zero":          &types.AttributeValueMemberN{Value: "0"},
		":one":           &types.AttributeValueMemberN{Value: "1"},
		":now":           &types.AttributeValueMemberS{Value: now.Format(time.RFC3339)},
		":error_code":    &types.AttributeValueMemberS{Value: errorCode},
		":ttl":           &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", ttl)},
		":region":        &types.AttributeValueMemberS{Value: region},
		":instance_type": &types.AttributeValueMemberS{Value: instanceType},
	}

	if isCapacityError {
		updateExpr += ", consecutive_fails = if_not_exists(consecutive_fails, :zero) + :one, backoff_until = :backoff"
		exprValues[":backoff"] = &types.AttributeValueMemberS{Value: backoffUntil}
	}

	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(availabilityTableName),
		Key: map[string]types.AttributeValue{
			"stat_id": &types.AttributeValueMemberS{Value: statID},
		},
		UpdateExpression:          aws.String(updateExpr),
		ExpressionAttributeNames:  exprNames,
		ExpressionAttributeValues: exprValues,
	})

	return err
}

// calculateBackoff calculates exponential backoff duration based on consecutive failures
func calculateBackoff(consecutiveFailures int) time.Duration {
	if consecutiveFailures <= 0 {
		return 0
	}

	backoff := time.Duration(float64(backoffInitial) * math.Pow(float64(backoffMultiplier), float64(consecutiveFailures-1)))
	if backoff > backoffMax {
		backoff = backoffMax
	}

	return backoff
}

// IsInBackoff checks if a region+instance_type is currently in backoff period
func IsInBackoff(ctx context.Context, client *dynamodb.Client, region, instanceType string) (bool, error) {
	stats, err := GetStats(ctx, client, region, instanceType)
	if err != nil {
		return false, err
	}

	if stats.BackoffUntil == "" {
		return false, nil
	}

	backoffUntil, err := time.Parse(time.RFC3339, stats.BackoffUntil)
	if err != nil {
		return false, nil // Invalid timestamp, treat as not in backoff
	}

	return time.Now().Before(backoffUntil), nil
}

// CalculateScore calculates an availability score for a region+instance_type
// Returns 0.5 for regions with no data (neutral), 0.0-1.0 based on success rate + recency
func CalculateScore(stats *AvailabilityStats) float64 {
	total := stats.SuccessCount + stats.FailureCount
	if total == 0 {
		return 0.5 // Neutral score for unknown regions
	}

	successRate := float64(stats.SuccessCount) / float64(total)

	// Recency weight: prefer recently successful regions
	recency := 1.0
	if stats.LastSuccess != "" {
		lastSuccess, err := time.Parse(time.RFC3339, stats.LastSuccess)
		if err == nil {
			hoursSince := time.Since(lastSuccess).Hours()
			recency = math.Exp(-hoursSince / 24.0) // Exponential decay over 24 hours
		}
	}

	// Weighted score: 80% success rate, 20% recency
	score := successRate * (0.8 + 0.2*recency)
	return score
}

// ListStatsByRegions retrieves availability stats for multiple regions and instance type
func ListStatsByRegions(ctx context.Context, client *dynamodb.Client, regions []string, instanceType string) (map[string]*AvailabilityStats, error) {
	result := make(map[string]*AvailabilityStats)

	for _, region := range regions {
		stats, err := GetStats(ctx, client, region, instanceType)
		if err != nil {
			return nil, fmt.Errorf("failed to get stats for %s: %w", region, err)
		}
		result[region] = stats
	}

	return result, nil
}
