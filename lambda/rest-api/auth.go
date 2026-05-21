package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Principal represents a validated API key caller.
type Principal struct {
	KeyID     string
	Project   string // "spore", "prism", etc.
	AccountID string // AWS account the key belongs to
	CreatedAt time.Time
}

func validateAPIKey(ctx context.Context, key string) (*Principal, error) {
	table := os.Getenv("API_KEYS_TABLE")
	if table == "" {
		table = "spore-api-keys"
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return nil, err
	}
	client := dynamodb.NewFromConfig(cfg)
	out, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]dynamodbtypes.AttributeValue{
			"api_key": &dynamodbtypes.AttributeValueMemberS{Value: key},
		},
	})
	if err != nil || out.Item == nil {
		return nil, fmt.Errorf("key not found")
	}

	get := func(k string) string {
		if v, ok := out.Item[k].(*dynamodbtypes.AttributeValueMemberS); ok {
			return v.Value
		}
		return ""
	}

	// Check revoked
	if get("revoked") == "true" {
		return nil, fmt.Errorf("key revoked")
	}

	return &Principal{
		KeyID:     key[:8] + "...",
		Project:   get("project"),
		AccountID: get("account_id"),
	}, nil
}

// GenerateAPIKey creates a new random API key (used by spawn api-key create).
func GenerateAPIKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk_" + hex.EncodeToString(b), nil
}
