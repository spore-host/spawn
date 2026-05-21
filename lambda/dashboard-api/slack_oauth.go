package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	slackAuthorizeURL = "https://slack.com/oauth/v2/authorize"
	slackAccessURL    = "https://slack.com/api/oauth.v2.access"
	slackExchangeURL  = "https://slack.com/api/oauth.v2.exchange"
	slackOAuthScopes  = "commands,chat:write,users:read,users:read.email,incoming-webhook"
	pkceStateMaxAge   = 10 * time.Minute
)

// handleSlackOAuthRedirect redirects the user to Slack's OAuth authorization page.
// Includes PKCE code_challenge and a signed state carrying the code_verifier.
// GET /api/slack/oauth
func handleSlackOAuthRedirect(request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	clientID := os.Getenv("SLACK_CLIENT_ID")
	if clientID == "" {
		return errorResponse(500, "Slack OAuth not configured"), nil
	}

	// PKCE: generate code_verifier, derive code_challenge
	verifier, err := generateCodeVerifier()
	if err != nil {
		return errorResponse(500, "Failed to generate PKCE verifier"), nil
	}
	challenge := sha256B64URL(verifier)
	state := signedState(verifier)

	redirectURI := slackRedirectURI(request)
	authURL := fmt.Sprintf("%s?client_id=%s&scope=%s&redirect_uri=%s&code_challenge=%s&code_challenge_method=S256&state=%s",
		slackAuthorizeURL,
		url.QueryEscape(clientID),
		url.QueryEscape(slackOAuthScopes),
		url.QueryEscape(redirectURI),
		url.QueryEscape(challenge),
		url.QueryEscape(state),
	)

	return events.APIGatewayProxyResponse{
		StatusCode: 302,
		Headers: map[string]string{
			"Location":                     authURL,
			"Access-Control-Allow-Origin":  "*",
			"Access-Control-Allow-Headers": "Content-Type",
		},
	}, nil
}

// handleSlackOAuthCallback exchanges the Slack OAuth code for a bot token,
// stores the workspace credentials in spore-bot-workspaces DynamoDB, and
// redirects the user to the dashboard with a success indicator.
// GET /api/slack/oauth/callback?code=...&state=...
func handleSlackOAuthCallback(ctx context.Context, cfg aws.Config, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	clientID := os.Getenv("SLACK_CLIENT_ID")
	clientSecret := os.Getenv("SLACK_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return errorResponse(500, "Slack OAuth not configured"), nil
	}

	// Check for error from Slack (user denied)
	if errParam := request.QueryStringParameters["error"]; errParam != "" {
		return redirectToDashboard("error=" + url.QueryEscape(errParam)), nil
	}

	code := request.QueryStringParameters["code"]
	if code == "" {
		return errorResponse(400, "Missing OAuth code"), nil
	}

	// PKCE: extract and verify the code_verifier from state
	state := request.QueryStringParameters["state"]
	verifier, err := extractVerifier(state)
	if err != nil {
		return errorResponse(400, fmt.Sprintf("Invalid state: %v", err)), nil
	}

	// Exchange code + code_verifier for bot token
	token, err := exchangeSlackCode(ctx, clientID, clientSecret, code, verifier, slackRedirectURI(request))
	if err != nil {
		return errorResponse(500, fmt.Sprintf("OAuth exchange failed: %v", err)), nil
	}

	// Store workspace in spore-bot-workspaces DynamoDB
	workspacesTable := getEnvOrDefault("SPORE_BOT_WORKSPACES_TABLE", "spore-bot-workspaces")
	if err := storeSlackWorkspace(ctx, cfg, workspacesTable, token); err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to store workspace: %v", err)), nil
	}

	// Redirect to dashboard with success
	return redirectToDashboard(fmt.Sprintf("bot=connected&workspace=%s&workspace_name=%s",
		url.QueryEscape(token.Team.ID),
		url.QueryEscape(token.Team.Name),
	)), nil
}

// handleSlackTokenRotate handles Slack's proactive token rotation webhook.
// Slack calls this when a token needs to be rotated (if configured in app settings).
// POST /api/slack/token/rotate
func handleSlackTokenRotate(ctx context.Context, cfg aws.Config, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	clientID := os.Getenv("SLACK_CLIENT_ID")
	clientSecret := os.Getenv("SLACK_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return errorResponse(500, "Slack OAuth not configured"), nil
	}

	// Slack sends the event as JSON
	var event struct {
		Token string `json:"token"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal([]byte(request.Body), &event); err != nil {
		return errorResponse(400, "Invalid request body"), nil
	}

	workspacesTable := getEnvOrDefault("SPORE_BOT_WORKSPACES_TABLE", "spore-bot-workspaces")

	// Find workspace by current token, refresh, and update
	newToken, err := rotateTokenByCurrentToken(ctx, cfg, workspacesTable, clientID, clientSecret, event.Token)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Token rotation failed: %v", err)), nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       fmt.Sprintf(`{"ok":true,"token":%q}`, newToken),
	}, nil
}

// slackOAuthTokenResponse is the response from Slack's oauth.v2.access endpoint.
type slackOAuthTokenResponse struct {
	OK                   bool   `json:"ok"`
	Error                string `json:"error,omitempty"`
	AppID                string `json:"app_id"`
	AccessToken          string `json:"access_token"`
	RefreshToken         string `json:"refresh_token,omitempty"`
	TokenRotationEnabled bool   `json:"token_rotation_enabled,omitempty"`
	ExpiresIn            int    `json:"expires_in,omitempty"` // seconds
	BotToken             string `json:"-"`                    // set after parsing
	Team                 struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	AuthedUser struct {
		ID string `json:"id"`
	} `json:"authed_user"`
	// IncomingWebhook is only present when the user selects a channel during OAuth.
	// It provides a ready-to-use webhook URL for posting to that channel.
	IncomingWebhook struct {
		URL              string `json:"url"`
		Channel          string `json:"channel"`
		ChannelID        string `json:"channel_id"`
		ConfigurationURL string `json:"configuration_url"`
	} `json:"incoming_webhook,omitempty"`
}

func exchangeSlackCode(ctx context.Context, clientID, clientSecret, code, codeVerifier, redirectURI string) (*slackOAuthTokenResponse, error) {
	vals := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	if codeVerifier != "" {
		vals.Set("code_verifier", codeVerifier)
	}
	resp, err := http.PostForm(slackAccessURL, vals)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var token slackOAuthTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if !token.OK {
		return nil, fmt.Errorf("Slack API error: %s", token.Error)
	}
	token.BotToken = token.AccessToken
	return &token, nil
}

// storeSlackWorkspace writes the workspace credentials to DynamoDB.
// Uses an app-scoped key (slack#{workspace}#{app}) when the app_id is present,
// so multiple Slack apps can coexist in the same workspace.
func storeSlackWorkspace(ctx context.Context, cfg aws.Config, tableName string, token *slackOAuthTokenResponse) error {
	client := dynamodb.NewFromConfig(cfg)

	// App-scoped key when app_id is available; legacy key for backwards compat.
	workspaceKey := "slack#" + token.Team.ID
	if token.AppID != "" {
		workspaceKey = "slack#" + token.Team.ID + "#" + token.AppID
	}

	// Preserve existing signing_secret — look up both key formats.
	legacyKey := "slack#" + token.Team.ID
	existing, _ := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: legacyKey},
		},
	})

	// Signing secret is app-level — stored in Lambda env, auto-populated for all workspaces
	signingSecret := os.Getenv("SLACK_SIGNING_SECRET")
	if signingSecret == "" && existing != nil && existing.Item != nil {
		if v, ok := existing.Item["signing_secret"].(*dynamodbtypes.AttributeValueMemberS); ok {
			signingSecret = v.Value
		}
	}

	ws := map[string]interface{}{
		"workspace_key":  workspaceKey,
		"app_id":         token.AppID,
		"bot_token":      token.BotToken,
		"signing_secret": signingSecret,
		"platform":       "slack",
		"workspace_name": token.Team.Name,
		"installed_by":   "oauth:" + token.AuthedUser.ID,
		"installed_at":   time.Now().UTC().Format(time.RFC3339),
		"token_rotation": token.TokenRotationEnabled,
	}
	// Store incoming webhook URL if the user selected a channel during OAuth.
	// This enables zero-config channel notifications without bot registration.
	if token.IncomingWebhook.URL != "" {
		ws["incoming_webhook_url"] = token.IncomingWebhook.URL
		ws["incoming_webhook_channel"] = token.IncomingWebhook.Channel
	}
	// Slack signals rotation by returning refresh_token + expires_in, not token_rotation_enabled.
	if token.RefreshToken != "" {
		ws["refresh_token"] = token.RefreshToken
		ws["token_rotation"] = true
		if token.ExpiresIn > 0 {
			ws["token_expires_at"] = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Unix()
		}
	}

	item, err := attributevalue.MarshalMap(ws)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	})
	return err
}

// rotateTokenByCurrentToken scans for a workspace with the given token,
// exchanges its refresh_token for new credentials, and updates DynamoDB.
func rotateTokenByCurrentToken(ctx context.Context, cfg aws.Config, tableName, clientID, clientSecret, currentToken string) (string, error) {
	client := dynamodb.NewFromConfig(cfg)

	// Scan for workspace matching current token
	result, err := client.Scan(ctx, &dynamodb.ScanInput{
		TableName:        aws.String(tableName),
		FilterExpression: aws.String("bot_token = :t"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":t": &dynamodbtypes.AttributeValueMemberS{Value: currentToken},
		},
	})
	if err != nil || len(result.Items) == 0 {
		return "", fmt.Errorf("workspace not found for token")
	}

	item := result.Items[0]
	refreshTokenAttr, ok := item["refresh_token"].(*dynamodbtypes.AttributeValueMemberS)
	if !ok || refreshTokenAttr.Value == "" {
		return "", fmt.Errorf("no refresh_token stored for workspace")
	}
	workspaceKey := item["workspace_key"].(*dynamodbtypes.AttributeValueMemberS).Value

	newToken, newRefresh, expiresIn, err := exchangeRefreshToken(clientID, clientSecret, refreshTokenAttr.Value)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}

	// Update DynamoDB with new tokens
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"workspace_key": &dynamodbtypes.AttributeValueMemberS{Value: workspaceKey},
		},
		UpdateExpression: aws.String("SET bot_token = :t, refresh_token = :r, token_expires_at = :e"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":t": &dynamodbtypes.AttributeValueMemberS{Value: newToken},
			":r": &dynamodbtypes.AttributeValueMemberS{Value: newRefresh},
			":e": &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt, 10)},
		},
	})
	return newToken, err
}

// ── PKCE helpers ──────────────────────────────────────────────────────────────

// generateCodeVerifier creates a 32-byte random code_verifier (base64url, no padding).
func generateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sha256B64URL computes BASE64URL(SHA256(s)) without padding — the PKCE code_challenge.
func sha256B64URL(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// signedState embeds the code_verifier in an HMAC-signed state parameter.
// Format: base64url(verifier:timestamp) + "." + HMAC-SHA256(payload, SLACK_CLIENT_SECRET)
func signedState(verifier string) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := base64.RawURLEncoding.EncodeToString([]byte(verifier + ":" + ts))
	sig := hmacB64URL(payload)
	return payload + "." + sig
}

// extractVerifier validates the HMAC-signed state and returns the code_verifier.
func extractVerifier(state string) (string, error) {
	if state == "" {
		return "", fmt.Errorf("missing state")
	}
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("malformed state")
	}
	payload, sig := parts[0], parts[1]

	// Verify HMAC
	if expectedSig := hmacB64URL(payload); !hmac.Equal([]byte(expectedSig), []byte(sig)) {
		return "", fmt.Errorf("state signature mismatch")
	}

	// Decode payload
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("decode state: %w", err)
	}
	idx := strings.LastIndex(string(raw), ":")
	if idx < 0 {
		return "", fmt.Errorf("malformed state payload")
	}
	verifier := string(raw[:idx])
	tsStr := string(raw[idx+1:])

	// Check freshness
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid timestamp in state")
	}
	if time.Since(time.Unix(ts, 0)) > pkceStateMaxAge {
		return "", fmt.Errorf("state expired (>%v)", pkceStateMaxAge)
	}

	return verifier, nil
}

func hmacB64URL(data string) string {
	secret := os.Getenv("SLACK_CLIENT_SECRET")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ── utilities ─────────────────────────────────────────────────────────────────

func slackRedirectURI(request events.APIGatewayProxyRequest) string {
	if override := os.Getenv("SLACK_REDIRECT_URI"); override != "" {
		return override
	}
	host := request.Headers["Host"]
	if host == "" {
		host = request.Headers["host"]
	}
	if host == "" {
		host = "api.spore.host"
	}
	return "https://" + host + "/api/slack/oauth/callback"
}

func redirectToDashboard(params string) events.APIGatewayProxyResponse {
	dashboardURL := os.Getenv("DASHBOARD_URL")
	if dashboardURL == "" {
		dashboardURL = "https://spore.host/dashboard.html"
	}
	if params != "" {
		if strings.Contains(dashboardURL, "?") {
			dashboardURL += "&" + params
		} else {
			dashboardURL += "?" + params
		}
	}
	return events.APIGatewayProxyResponse{
		StatusCode: 302,
		Headers: map[string]string{
			"Location":                    dashboardURL,
			"Access-Control-Allow-Origin": "*",
		},
	}
}
