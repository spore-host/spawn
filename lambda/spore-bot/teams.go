package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ── Incoming Activity ─────────────────────────────────────────────────────────

// TeamsActivity represents an incoming Bot Framework activity from Teams.
type TeamsActivity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Text string `json:"text"`
	From struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"from"`
	Recipient struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"recipient"`
	Conversation struct {
		ID               string `json:"id"`
		IsGroup          bool   `json:"isGroup"`
		ConversationType string `json:"conversationType"`
	} `json:"conversation"`
	ChannelID   string `json:"channelId"`
	ServiceURL  string `json:"serviceUrl"`
	ChannelData struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
	} `json:"channelData"`
}

// TeamsConversationRef stores the information needed to send proactive messages.
type TeamsConversationRef struct {
	ServiceURL     string `dynamodbav:"service_url"`
	ConversationID string `dynamodbav:"conversation_id"`
	TenantID       string `dynamodbav:"tenant_id"`
	BotID          string `dynamodbav:"bot_id"`
	UserID         string `dynamodbav:"user_id"`
}

// ── Signature Verification ────────────────────────────────────────────────────

// verifyTeamsSignature validates Teams outgoing webhook HMAC-SHA256 signature.
func verifyTeamsSignature(sharedSecret, body, authHeader string) error {
	if !strings.HasPrefix(authHeader, "HMAC ") {
		return fmt.Errorf("missing HMAC authorization")
	}
	sigB64 := strings.TrimPrefix(authHeader, "HMAC ")
	sigBytes, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(sharedSecret))
	mac.Write([]byte(body))
	if !hmac.Equal(mac.Sum(nil), sigBytes) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// ── Activity Parsing ──────────────────────────────────────────────────────────

func parseTeamsActivity(body string) (*SlashCommand, string, error) {
	var activity TeamsActivity
	if err := json.Unmarshal([]byte(body), &activity); err != nil {
		return nil, "", fmt.Errorf("parse Teams activity: %w", err)
	}

	text := strings.TrimSpace(activity.Text)
	// Strip @mention prefix: "<at>BotName</at> /prism stop" → "/prism stop"
	if idx := strings.Index(text, ">"); idx >= 0 {
		text = strings.TrimSpace(text[idx+1:])
	}
	parts := strings.Fields(text)
	cmd, args := "", ""
	if len(parts) > 0 {
		cmd = parts[0]
		if len(parts) > 1 {
			args = strings.Join(parts[1:], " ")
		}
	}

	logf("teams activity: channel=%s tenant=%s conv=%s serviceURL=%s text=%q",
		activity.ChannelID, activity.ChannelData.Tenant.ID,
		activity.Conversation.ID, activity.ServiceURL, text)

	// Encode serviceURL|conversationID so Phase 2 can send proactive responses
	responseURL := activity.ServiceURL
	if activity.Conversation.ID != "" {
		responseURL = activity.ServiceURL + "|" + activity.Conversation.ID
	}

	sc := &SlashCommand{
		Command:     cmd,
		Text:        args,
		UserID:      activity.From.ID,
		WorkspaceID: activity.ChannelData.Tenant.ID,
		ResponseURL: responseURL,
		ChannelID:   activity.Conversation.ID,
	}
	return sc, activity.ServiceURL, nil
}

// storeTeamsConversationRef saves the conversation reference for proactive messaging.
// Keyed by user_key so we can look up a user's Teams conversation for DM notifications.
func storeTeamsConversationRef(ctx context.Context, reg *Registry, userKey string, activity TeamsActivity) {
	ref := map[string]dynamodbtypes.AttributeValue{
		"user_key":        &dynamodbtypes.AttributeValueMemberS{Value: userKey},
		"nickname":        &dynamodbtypes.AttributeValueMemberS{Value: "teams_ref"},
		"service_url":     &dynamodbtypes.AttributeValueMemberS{Value: activity.ServiceURL},
		"conversation_id": &dynamodbtypes.AttributeValueMemberS{Value: activity.Conversation.ID},
		"tenant_id":       &dynamodbtypes.AttributeValueMemberS{Value: activity.ChannelData.Tenant.ID},
		"bot_id":          &dynamodbtypes.AttributeValueMemberS{Value: activity.Recipient.ID},
		"platform":        &dynamodbtypes.AttributeValueMemberS{Value: "teams"},
		"updated_at":      &dynamodbtypes.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
	}
	_, _ = reg.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(reg.registryTable),
		Item:      ref,
	})
}

// ── Bot Framework Token ───────────────────────────────────────────────────────

var (
	teamsBFToken    string
	teamsBFTokenExp time.Time
	teamsBFTokenMu  sync.Mutex
)

// getTeamsBotToken acquires a Bot Framework access token via client credentials.
// Tokens are cached until 5 minutes before expiry.
func getTeamsBotToken(ctx context.Context) (string, error) {
	teamsBFTokenMu.Lock()
	defer teamsBFTokenMu.Unlock()

	if teamsBFToken != "" && time.Now().Before(teamsBFTokenExp) {
		return teamsBFToken, nil
	}

	appID := os.Getenv("TEAMS_APP_ID")
	appSecret := os.Getenv("TEAMS_APP_SECRET")
	tenantID := os.Getenv("TEAMS_TENANT_ID")
	if appID == "" || appSecret == "" {
		return "", fmt.Errorf("TEAMS_APP_ID or TEAMS_APP_SECRET not configured")
	}
	// Single-tenant apps must use their own tenant ID.
	// Multi-tenant apps can use botframework.com.
	if tenantID == "" {
		tenantID = "botframework.com"
	}
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID)
	vals := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {appID},
		"client_secret": {appSecret},
		"scope":         {"https://api.botframework.com/.default"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL,
		strings.NewReader(vals.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("token error %s: %s", result.Error, result.ErrorDesc)
	}

	teamsBFToken = result.AccessToken
	teamsBFTokenExp = time.Now().Add(time.Duration(result.ExpiresIn-300) * time.Second)
	return teamsBFToken, nil
}

// ── Proactive Messaging ───────────────────────────────────────────────────────

// postTeamsProactive sends a proactive message to a Teams conversation.
// Requires a stored conversation reference.
func postTeamsProactive(ctx context.Context, serviceURL, conversationID, text string) error {
	token, err := getTeamsBotToken(ctx)
	if err != nil {
		return fmt.Errorf("get bot token: %w", err)
	}

	endpoint := strings.TrimRight(serviceURL, "/") +
		"/v3/conversations/" + conversationID + "/activities"
	logf("teams proactive: endpoint=%s token_len=%d", endpoint, len(token))

	botID := os.Getenv("TEAMS_APP_ID")
	msg := map[string]interface{}{
		"type": "message",
		"text": text,
		"from": map[string]string{
			"id":   botID,
			"name": "spore-bot",
		},
	}
	data, _ := json.Marshal(msg)

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post message: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Teams API %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// postTeamsResponse sends a Phase 2 response to a Teams conversation.
// serviceURL is the Bot Framework service URL, conversationID is from the activity.
// For the response_url we encode both as "serviceURL|conversationID".
func postTeamsResponse(responseURL, text string) error {
	// responseURL is encoded as "serviceURL|conversationID" by parseTeamsActivity
	parts := strings.SplitN(responseURL, "|", 2)
	serviceURL := parts[0]
	conversationID := ""
	if len(parts) == 2 {
		conversationID = parts[1]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if conversationID != "" {
		return postTeamsProactive(ctx, serviceURL, conversationID, text)
	}

	// Fallback: direct POST to serviceURL (outgoing webhook style)
	msg := map[string]interface{}{"type": "message", "text": text}
	data, _ := json.Marshal(msg)
	return httpPost(serviceURL, "application/json", data)
}

// sendTeamsDMs sends a DM notification to each registered Teams user for an instance.
func sendTeamsDMs(ctx context.Context, reg *Registry, ws *WorkspaceConfig, instanceID, text string) {
	client := dynamodb.NewFromConfig(cfg)
	result, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(reg.registryTable),
		IndexName:              aws.String("instance_id-index"),
		KeyConditionExpression: aws.String("instance_id = :iid"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":iid": &dynamodbtypes.AttributeValueMemberS{Value: instanceID},
		},
	})
	if err != nil || len(result.Items) == 0 {
		return
	}

	for _, item := range result.Items {
		userKeyAttr, ok := item["user_key"].(*dynamodbtypes.AttributeValueMemberS)
		if !ok {
			continue
		}
		// Only process Teams registrations
		if !strings.HasPrefix(userKeyAttr.Value, "teams#") {
			continue
		}
		parts := strings.SplitN(userKeyAttr.Value, "#", 3)
		if len(parts) != 3 {
			continue
		}
		userID := parts[2]
		tenantID := parts[1]

		// Look up stored conversation reference for this user
		refResult, err := client.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(reg.registryTable),
			Key: map[string]dynamodbtypes.AttributeValue{
				"user_key": &dynamodbtypes.AttributeValueMemberS{
					Value: "teams#" + tenantID + "#" + userID,
				},
				"nickname": &dynamodbtypes.AttributeValueMemberS{Value: "teams_ref"},
			},
		})
		if err != nil || refResult.Item == nil {
			continue
		}

		serviceURLAttr := refResult.Item["service_url"].(*dynamodbtypes.AttributeValueMemberS)
		convIDAttr := refResult.Item["conversation_id"].(*dynamodbtypes.AttributeValueMemberS)

		go func(svcURL, convID string) {
			dmCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := postTeamsProactive(dmCtx, svcURL, convID, text); err != nil {
				logf("Teams DM error for %s: %v", userID, err)
			}
		}(serviceURLAttr.Value, convIDAttr.Value)
	}
}
