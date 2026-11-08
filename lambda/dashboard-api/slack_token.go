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
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/spore-host/spawn/pkg/botsecret"
	"github.com/spore-host/spawn/pkg/security"
)

// getFreshBotToken returns a valid bot token for a workspace, refreshing it
// automatically if token rotation is enabled and the token is expired.
// workspaceKey is in the form "slack#T03NE3GTY". The tokens are stored
// KMS-encrypted (#868): they are opened here, and a refresh is written back
// sealed.
func getFreshBotToken(ctx context.Context, cfg aws.Config, tableName, workspaceKey string) (string, error) {
	clientID := os.Getenv("SLACK_CLIENT_ID")
	return freshBotTokenWith(ctx, dynamodb.NewFromConfig(cfg), kms.NewFromConfig(cfg), tableName, workspaceKey, time.Now(),
		func(refresh string) (string, string, int, error) {
			return exchangeRefreshToken(clientID, slackClientSecret(), refresh)
		})
}

// freshBotTokenWith is getFreshBotToken over explicit clients, clock and
// exchange (a fake in tests).
func freshBotTokenWith(ctx context.Context, client *dynamodb.Client, kmsClient security.KMSAPI, tableName, workspaceKey string, now time.Time,
	exchange func(refreshToken string) (string, string, int, error)) (string, error) {
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

	botToken, err := botsecret.BotToken.Open(ctx, kmsClient, workspaceKey, botsecret.BotToken.Stored(result.Item))
	if err != nil {
		return "", err
	}
	if botToken == "" {
		return "", fmt.Errorf("no bot token for workspace %s", workspaceKey)
	}

	// Check if token rotation is enabled and token is expired/near-expiry
	tokenRotation := boolAttr(result.Item, "token_rotation")
	if !tokenRotation {
		return botToken, nil
	}

	expiresAt := int64Attr(result.Item, "token_expires_at")
	if expiresAt == 0 || now.Add(5*time.Minute).Unix() < expiresAt {
		// Not expired (or no expiry set) — return current token
		// Refresh 5 minutes before actual expiry to avoid race conditions
		return botToken, nil
	}

	// Token is expired — refresh it
	refreshToken, err := botsecret.RefreshToken.Open(ctx, kmsClient, workspaceKey, botsecret.RefreshToken.Stored(result.Item))
	if err != nil {
		return "", err
	}
	if refreshToken == "" {
		return "", fmt.Errorf("token expired but no refresh token stored for %s", workspaceKey)
	}

	newToken, newRefresh, expiresIn, err := exchange(refreshToken)
	if err != nil {
		return "", fmt.Errorf("refresh token for %s: %w", workspaceKey, err)
	}

	newExpiresAt := now.Add(time.Duration(expiresIn) * time.Second).Unix()
	if err := writeRotatedTokens(ctx, client, kmsClient, tableName, workspaceKey, newToken, newRefresh, newExpiresAt); err != nil {
		return "", err
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
