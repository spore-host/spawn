package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	defaultDynamoDBTableName = "spawn-user-accounts"
)

var (
	dynamoDBTableName string
)

func init() {
	// Load table name from environment variable with fallback
	dynamoDBTableName = getEnvOrDefault("SPAWN_USER_ACCOUNTS_TABLE", defaultDynamoDBTableName)
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getUserAccount retrieves user account info from DynamoDB
func getUserAccount(ctx context.Context, cfg aws.Config, userID string) (*UserAccountRecord, error) {
	client := dynamodb.NewFromConfig(cfg)

	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(dynamoDBTableName),
		Key: map[string]types.AttributeValue{
			"user_id": &types.AttributeValueMemberS{Value: userID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get item from DynamoDB: %w", err)
	}

	// If item doesn't exist, return nil (not an error)
	if result.Item == nil {
		return nil, nil
	}

	// Unmarshal the item
	var record UserAccountRecord
	err = attributevalue.UnmarshalMap(result.Item, &record)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal DynamoDB item: %w", err)
	}

	return &record, nil
}

// updateLastAccess updates the last access timestamp for a user
func updateLastAccess(ctx context.Context, cfg aws.Config, userID string) error {
	client := dynamodb.NewFromConfig(cfg)

	now := time.Now().UTC().Format(time.RFC3339)

	_, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(dynamoDBTableName),
		Key: map[string]types.AttributeValue{
			"user_id": &types.AttributeValueMemberS{Value: userID},
		},
		UpdateExpression: aws.String("SET last_access = :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":now": &types.AttributeValueMemberS{Value: now},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update last access: %w", err)
	}

	return nil
}
