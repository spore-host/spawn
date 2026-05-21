package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// getFreshBotToken returns a valid bot token for a workspace, refreshing it
// automatically if token rotation is enabled and the token is expired.
// workspaceKey is in the form "slack#T03NE3GTY".
func getFreshBotToken(ctx context.Context, cfg aws.Config, tableName, workspaceKey string) (string, error) {
	client := dynamodb.NewFromConfig(cfg)

	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: workspaceKey},
		},
	})
	if err != nil {
		return "", fmt.Errorf("get workspace: %w", err)
	}
	if result.Item == nil {
		return "", fmt.Errorf("workspace %s not found", workspaceKey)
	}

	botToken := stringAttr(result.Item, "bot_token")
	if botToken == "" {
		return "", fmt.Errorf("no bot_token for workspace %s", workspaceKey)
	}

	// Check if token rotation is enabled and token is expired/near-expiry
	tokenRotation := boolAttr(result.Item, "token_rotation")
	if !tokenRotation {
		return botToken, nil
	}

	expiresAt := int64Attr(result.Item, "token_expires_at")
	if expiresAt == 0 || time.Now().Add(5*time.Minute).Unix() < expiresAt {
		// Not expired (or no expiry set) — return current token
		// Refresh 5 minutes before actual expiry to avoid race conditions
		return botToken, nil
	}

	// Token is expired — refresh it
	refreshToken := stringAttr(result.Item, "refresh_token")
	if refreshToken == "" {
		return "", fmt.Errorf("token expired but no refresh_token stored for %s", workspaceKey)
	}

	clientID := os.Getenv("SLACK_CLIENT_ID")
	clientSecret := os.Getenv("SLACK_CLIENT_SECRET")
	newToken, newRefresh, expiresIn, err := exchangeRefreshToken(clientID, clientSecret, refreshToken)
	if err != nil {
		return "", fmt.Errorf("refresh token for %s: %w", workspaceKey, err)
	}

	// Update DynamoDB with new tokens
	newExpiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: workspaceKey},
		},
		UpdateExpression: aws.String("SET bot_token = :t, refresh_token = :r, token_expires_at = :e"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":t": &dynamodbtypes.AttributeValueMemberS{Value: newToken},
			":r": &dynamodbtypes.AttributeValueMemberS{Value: newRefresh},
			":e": &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(newExpiresAt, 10)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("update tokens: %w", err)
	}
	return newToken, nil
}

// exchangeRefreshToken calls Slack's oauth.v2.exchange to get new access + refresh tokens.
func exchangeRefreshToken(clientID, clientSecret, refreshToken string) (accessToken, newRefreshToken string, expiresIn int, err error) {
	resp, err := http.PostForm("https://slack.com/api/oauth.v2.exchange", url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return "", "", 0, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error,omitempty"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", 0, fmt.Errorf("parse response: %w", err)
	}
	if !result.OK {
		return "", "", 0, fmt.Errorf("Slack API error: %s", result.Error)
	}
	return result.AccessToken, result.RefreshToken, result.ExpiresIn, nil
}

// ── DynamoDB attribute helpers ────────────────────────────────────────────────

func stringAttr(item map[string]dynamodbtypes.AttributeValue, key string) string {
	if v, ok := item[key].(*dynamodbtypes.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func boolAttr(item map[string]dynamodbtypes.AttributeValue, key string) bool {
	if v, ok := item[key].(*dynamodbtypes.AttributeValueMemberBOOL); ok {
		return v.Value
	}
	return false
}

func int64Attr(item map[string]dynamodbtypes.AttributeValue, key string) int64 {
	if v, ok := item[key].(*dynamodbtypes.AttributeValueMemberN); ok {
		n, _ := strconv.ParseInt(v.Value, 10, 64)
		return n
	}
	return 0
}
