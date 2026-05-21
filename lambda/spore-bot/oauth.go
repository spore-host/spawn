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
)

const (
	slackAuthorizeURL = "https://slack.com/oauth/v2/authorize"
	slackAccessURL    = "https://slack.com/api/oauth.v2.access"
	oauthScopes       = "commands,chat:write,users:read,users:read.email,incoming-webhook"
	oauthStateMaxAge  = 10 * time.Minute
)

// handleOAuthRedirect starts the Slack OAuth flow for a given platform.
// Platform credentials are read from {PLATFORM}_SLACK_CLIENT_ID env vars.
// GET /{platform}/oauth
func handleOAuthRedirect(platform string, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	clientID := oauthEnv(platform, "SLACK_CLIENT_ID")
	if clientID == "" {
		return oauthError(500, platform+" Slack OAuth not configured"), nil
	}

	verifier, err := generateOAuthVerifier()
	if err != nil {
		return oauthError(500, "Failed to generate PKCE verifier"), nil
	}
	challenge := oauthSHA256B64URL(verifier)
	state := oauthSignedState(platform, verifier)

	redirectURI := oauthRedirectURI(platform, request)
	authURL := fmt.Sprintf("%s?client_id=%s&scope=%s&redirect_uri=%s&code_challenge=%s&code_challenge_method=S256&state=%s",
		slackAuthorizeURL,
		url.QueryEscape(clientID),
		url.QueryEscape(oauthScopes),
		url.QueryEscape(redirectURI),
		url.QueryEscape(challenge),
		url.QueryEscape(state),
	)

	return events.APIGatewayProxyResponse{
		StatusCode: 302,
		Headers:    map[string]string{"Location": authURL, "Cache-Control": "no-store"},
	}, nil
}

// handleOAuthCallback exchanges the Slack code for tokens, stores the workspace,
// and redirects to the platform's success URL.
// GET /{platform}/oauth/callback?code=...&state=...
func handleOAuthCallback(ctx context.Context, reg *Registry, platform string, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	clientID := oauthEnv(platform, "SLACK_CLIENT_ID")
	clientSecret := oauthEnv(platform, "SLACK_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return oauthError(500, platform+" Slack OAuth not configured"), nil
	}
	successURL := oauthEnv(platform, "OAUTH_SUCCESS_URL")

	if errParam := request.QueryStringParameters["error"]; errParam != "" {
		return oauthRedirectResult(successURL, "error="+url.QueryEscape(errParam)), nil
	}

	code := request.QueryStringParameters["code"]
	state := request.QueryStringParameters["state"]
	if code == "" {
		return oauthError(400, "Missing OAuth code"), nil
	}

	verifier, err := oauthExtractVerifier(platform, state)
	if err != nil {
		return oauthError(400, fmt.Sprintf("Invalid state: %v", err)), nil
	}

	token, err := oauthExchangeCode(clientID, clientSecret, code, verifier, oauthRedirectURI(platform, request))
	if err != nil {
		return oauthError(500, fmt.Sprintf("OAuth exchange failed: %v", err)), nil
	}

	// Store workspace credentials — keyed by app ID when available so multiple
	// Slack apps can coexist in the same workspace.
	ws := &WorkspaceConfig{
		WorkspaceKey:  workspaceKey("slack", token.Team.ID, token.AppID),
		AppID:         token.AppID,
		Platform:      "slack",
		BotToken:      token.AccessToken,
		WorkspaceName: token.Team.Name,
		InstalledBy:   token.AuthedUser.ID,
		InstalledAt:   time.Now().UTC().Format(time.RFC3339),
	}
	// Slack signals rotation by returning refresh_token + expires_in.
	if token.RefreshToken != "" {
		ws.RefreshToken = token.RefreshToken
		ws.TokenRotation = true
		if token.ExpiresIn > 0 {
			ws.TokenExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Unix()
		}
	} else if token.ExpiresIn > 0 {
		// Token has an expiry but no explicit refresh token in this response.
		ws.TokenRotation = true
		ws.TokenExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Unix()
	}
	if token.IncomingWebhook.URL != "" {
		ws.IncomingWebhookURL = token.IncomingWebhook.URL
		ws.IncomingWebhookChannel = token.IncomingWebhook.Channel
	}

	if err := reg.PutWorkspace(ctx, ws); err != nil {
		logf("oauth: store workspace %s: %v", token.Team.ID, err)
		return oauthError(500, "Failed to store workspace credentials"), nil
	}

	// Also write a command-scoped copy (e.g. slack#T#/prism) so the slash command
	// handler can find the signing secret and bot token without knowing the app ID.
	// The platform prefix in the OAuth path determines the command name.
	cmdKey := workspaceKey("slack", token.Team.ID, "/"+platform)
	cmdWS := *ws
	cmdWS.WorkspaceKey = cmdKey
	if err := reg.PutWorkspace(ctx, &cmdWS); err != nil {
		logf("oauth: store command-scoped workspace %s: %v", cmdKey, err)
		// Non-fatal — app-scoped record is the primary one
	}

	return oauthRedirectResult(successURL, fmt.Sprintf("bot=connected&workspace=%s&workspace_name=%s",
		url.QueryEscape(token.Team.ID),
		url.QueryEscape(token.Team.Name),
	)), nil
}

// oauthTokenResponse is the response from Slack's oauth.v2.access endpoint.
type oauthTokenResponse struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	AppID        string `json:"app_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Team         struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	AuthedUser struct {
		ID string `json:"id"`
	} `json:"authed_user"`
	IncomingWebhook struct {
		URL     string `json:"url"`
		Channel string `json:"channel"`
	} `json:"incoming_webhook,omitempty"`
}

func oauthExchangeCode(clientID, clientSecret, code, codeVerifier, redirectURI string) (*oauthTokenResponse, error) {
	vals := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	if codeVerifier != "" {
		vals.Set("code_verifier", codeVerifier)
	}
	req, err := http.NewRequest(http.MethodPost, slackAccessURL, strings.NewReader(vals.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var token oauthTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if !token.OK {
		return nil, fmt.Errorf("Slack error: %s", token.Error)
	}
	return &token, nil
}

// oauthRedirectURI constructs the callback URL for this platform.
func oauthRedirectURI(platform string, request events.APIGatewayProxyRequest) string {
	host := request.Headers["host"]
	if host == "" {
		host = request.Headers["Host"]
	}
	scheme := "https"
	if h := request.Headers["x-forwarded-proto"]; h != "" {
		scheme = h
	}
	return fmt.Sprintf("%s://%s/%s/oauth/callback", scheme, host, platform)
}

// oauthEnv reads a per-platform environment variable.
// Platform "prism" + key "SLACK_CLIENT_ID" → env var "PRISM_SLACK_CLIENT_ID".
func oauthEnv(platform, key string) string {
	return os.Getenv(strings.ToUpper(platform) + "_" + key)
}

// ── PKCE helpers ──────────────────────────────────────────────────────────────

func generateOAuthVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func oauthSHA256B64URL(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// oauthSignedState embeds platform + code_verifier in an HMAC-signed state parameter.
// Signed with BOT_OAUTH_SECRET env var (shared across platforms).
func oauthSignedState(platform, verifier string) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := base64.RawURLEncoding.EncodeToString([]byte(platform + ":" + verifier + ":" + ts))
	sig := oauthHMAC(payload)
	return payload + "." + sig
}

func oauthExtractVerifier(platform, state string) (string, error) {
	if state == "" {
		return "", fmt.Errorf("missing state")
	}
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("malformed state")
	}
	payload, sig := parts[0], parts[1]
	if expected := oauthHMAC(payload); !hmac.Equal([]byte(expected), []byte(sig)) {
		return "", fmt.Errorf("state signature invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("decode state: %w", err)
	}
	// Format: platform:verifier:timestamp
	// Split from the right to avoid breaking verifiers that contain ":"
	last := strings.LastIndex(string(raw), ":")
	if last < 0 {
		return "", fmt.Errorf("malformed state payload")
	}
	tsStr := string(raw[last+1:])
	rest := string(raw[:last])

	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid timestamp in state")
	}
	if time.Since(time.Unix(ts, 0)) > oauthStateMaxAge {
		return "", fmt.Errorf("state expired")
	}

	// rest = platform:verifier
	sep := strings.Index(rest, ":")
	if sep < 0 {
		return "", fmt.Errorf("malformed state payload")
	}
	statePlatform := rest[:sep]
	verifier := rest[sep+1:]
	if statePlatform != platform {
		return "", fmt.Errorf("state platform mismatch")
	}
	return verifier, nil
}

func oauthHMAC(data string) string {
	secret := getEnv("BOT_OAUTH_SECRET", "change-me")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func oauthRedirectResult(successURL, params string) events.APIGatewayProxyResponse {
	target := successURL
	if target == "" {
		target = "https://spore.host/dashboard"
	}
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return events.APIGatewayProxyResponse{
		StatusCode: 302,
		Headers:    map[string]string{"Location": target + sep + params, "Cache-Control": "no-store"},
	}
}

func oauthError(code int, msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: code,
		Headers:    map[string]string{"Content-Type": "text/plain"},
		Body:       msg,
	}
}

// oauthPlatform extracts the platform name from paths like /prism/oauth or /prism/oauth/callback.
// Returns ("prism", true) for either form, ("", false) if the path doesn't match.
func oauthPlatform(path string) (string, bool) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) >= 2 && parts[1] == "oauth" {
		platform := parts[0]
		if platform != "" && platform != "admin" && platform != "notify" {
			return platform, true
		}
	}
	return "", false
}
