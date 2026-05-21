package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// adminRequest is the unified request body for all /admin/* endpoints.
// Fields used depend on the endpoint; unused fields are ignored.
type adminRequest struct {
	// shared
	Platform    string `json:"platform"`
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	Nickname    string `json:"nickname"`

	// workspace-add
	WorkspaceName       string   `json:"workspace_name"`
	BotToken            string   `json:"bot_token"`
	SigningSecret       string   `json:"signing_secret"`
	AllowedChannels     []string `json:"allowed_channels,omitempty"`
	ConnectCodeTTLHours int      `json:"connect_code_ttl_hours,omitempty"`

	// register
	UserEmail      string   `json:"user_email,omitempty"` // alternative to user_id; resolved server-side
	InstanceID     string   `json:"instance_id"`
	RoleARN        string   `json:"role_arn"`
	DNSName        string   `json:"dns_name,omitempty"`
	TagPrefix      string   `json:"tag_prefix,omitempty"`
	AllowedActions []string `json:"allowed_actions"`

	// set-enabled
	Enabled bool `json:"enabled"`
}

// handleAdminV1 handles admin requests from REST API (v1) proxy events.
// Caller ARN comes from requestContext.identity.userArn (IAM auth).
func handleAdminV1(ctx context.Context, reg *Registry, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	return handleAdmin(ctx, reg,
		req.Path,
		req.HTTPMethod,
		req.Body,
		req.Headers,
		req.QueryStringParameters,
		req.RequestContext.Identity.UserArn,
	)
}

// handleAdminV2 handles admin requests from HTTP API (v2) proxy events.
// Caller ARN comes from requestContext.authorizer.iam.userArn (IAM auth).
func handleAdminV2(ctx context.Context, reg *Registry, req events.APIGatewayV2HTTPRequest) (events.APIGatewayProxyResponse, error) {
	return handleAdmin(ctx, reg,
		req.RawPath,
		req.RequestContext.HTTP.Method,
		req.Body,
		req.Headers,
		req.QueryStringParameters,
		req.RequestContext.Authorizer.IAM.UserARN,
	)
}

// handleAdmin routes /admin/* requests. callerARN is populated by API Gateway
// from the verified IAM identity — REST API v1 puts it in identity.userArn,
// HTTP API v2 puts it in authorizer.iam.userArn.
func handleAdmin(ctx context.Context, reg *Registry, path, method, body string, headers, queryParams map[string]string, callerARN string) (events.APIGatewayProxyResponse, error) {
	if callerARN == "" {
		return adminError(403, "IAM identity required"), nil
	}

	path = strings.TrimRight(path, "/")

	var r adminRequest
	if body != "" {
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			return adminError(400, "invalid request body"), nil
		}
	}

	// Populate tag prefix from header if not in body (prism sends X-Prism-Tag-Prefix)
	if r.TagPrefix == "" {
		for k, v := range headers {
			if strings.ToLower(k) == "x-prism-tag-prefix" {
				r.TagPrefix = v
				break
			}
		}
	}
	if r.TagPrefix == "" {
		r.TagPrefix = "spawn"
	}

	switch {
	case path == "/admin/workspace-add" && method == "POST":
		return adminWorkspaceAdd(ctx, reg, r, callerARN)
	case path == "/admin/workspace-list" && method == "GET":
		return adminWorkspaceList(ctx, reg, queryParams, callerARN)
	case path == "/admin/register" && method == "POST":
		return adminRegister(ctx, reg, r, callerARN)
	case path == "/admin/set-enabled" && method == "POST":
		return adminSetEnabled(ctx, reg, r, callerARN)
	case path == "/admin/deregister" && method == "POST":
		return adminDeregister(ctx, reg, r, callerARN)
	case path == "/admin/list" && method == "GET":
		return adminList(ctx, reg, queryParams, callerARN)
	default:
		return adminError(404, fmt.Sprintf("unknown admin route: %s %s", method, path)), nil
	}
}

func adminWorkspaceAdd(ctx context.Context, reg *Registry, r adminRequest, callerARN string) (events.APIGatewayProxyResponse, error) {
	if r.Platform == "" || r.WorkspaceID == "" || r.BotToken == "" || r.SigningSecret == "" {
		return adminError(400, "platform, workspace_id, bot_token, and signing_secret are required"), nil
	}

	// Resolve App ID from Slack so workspaces are scoped per-app, allowing
	// multiple Slack apps (e.g. spore-bot + prism-bot) in the same workspace.
	appID := resolveSlackAppID(ctx, r.BotToken)

	ws := &WorkspaceConfig{
		WorkspaceKey:        workspaceKey(r.Platform, r.WorkspaceID, appID),
		AppID:               appID,
		Platform:            r.Platform,
		BotToken:            r.BotToken,
		SigningSecret:       r.SigningSecret,
		WorkspaceName:       r.WorkspaceName,
		AllowedChannels:     r.AllowedChannels,
		ConnectCodeTTLHours: r.ConnectCodeTTLHours,
		InstalledBy:         callerARN,
		InstalledAt:         time.Now().UTC().Format(time.RFC3339),
	}
	if err := reg.PutWorkspace(ctx, ws); err != nil {
		return adminError(500, fmt.Sprintf("store workspace: %v", err)), nil
	}

	return adminOK(map[string]string{
		"workspace_key":  ws.WorkspaceKey,
		"workspace_name": ws.WorkspaceName,
		"installed_by":   callerARN,
	})
}

func adminWorkspaceList(ctx context.Context, reg *Registry, params map[string]string, callerARN string) (events.APIGatewayProxyResponse, error) {
	platform := params["platform"]
	workspaceID := params["workspace_id"]
	if platform == "" || workspaceID == "" {
		return adminError(400, "platform and workspace_id query params are required"), nil
	}

	ws, err := reg.GetWorkspace(ctx, platform, workspaceID)
	if err != nil {
		return adminError(404, "workspace not found"), nil
	}

	// Return workspace metadata only — never return bot_token or signing_secret
	return adminOK(map[string]interface{}{
		"workspace_key":          ws.WorkspaceKey,
		"platform":               ws.Platform,
		"workspace_name":         ws.WorkspaceName,
		"installed_by":           ws.InstalledBy,
		"installed_at":           ws.InstalledAt,
		"allowed_channels":       ws.AllowedChannels,
		"connect_code_ttl_hours": ws.ConnectCodeTTLHours,
		"has_incoming_webhook":   ws.IncomingWebhookURL != "",
		"token_rotation":         ws.TokenRotation,
	})
}

func adminRegister(ctx context.Context, reg *Registry, r adminRequest, callerARN string) (events.APIGatewayProxyResponse, error) {
	// Resolve user_email → user_id server-side when user_id is absent.
	// The prism CLI can't resolve emails client-side because it doesn't hold the bot token.
	if r.UserID == "" && r.UserEmail != "" {
		resolved, err := resolveSlackEmail(ctx, reg, r.Platform, r.WorkspaceID, r.UserEmail)
		if err != nil {
			return adminError(400, fmt.Sprintf("email resolution: %v", err)), nil
		}
		r.UserID = resolved
	}

	if r.Platform == "" || r.WorkspaceID == "" || r.UserID == "" ||
		r.InstanceID == "" || r.Nickname == "" || r.RoleARN == "" {
		return adminError(400, "platform, workspace_id, user_id (or user_email), instance_id, nickname, and role_arn are required"), nil
	}
	if len(r.AllowedActions) == 0 {
		r.AllowedActions = []string{"status"}
	}

	registration := &BotRegistration{
		UserKey:        userKey(r.Platform, r.WorkspaceID, r.UserID),
		Nickname:       r.Nickname,
		InstanceID:     r.InstanceID,
		RoleARN:        r.RoleARN,
		DNSName:        r.DNSName,
		TagPrefix:      r.TagPrefix,
		AllowedActions: r.AllowedActions,
		RegisteredBy:   callerARN,
		Platform:       r.Platform,
		Enabled:        false, // must be explicitly enabled
	}
	if err := reg.PutRegistration(ctx, registration); err != nil {
		return adminError(500, fmt.Sprintf("store registration: %v", err)), nil
	}

	return adminOK(map[string]interface{}{
		"user_key":        registration.UserKey,
		"nickname":        registration.Nickname,
		"instance_id":     registration.InstanceID,
		"allowed_actions": registration.AllowedActions,
		"enabled":         registration.Enabled,
		"registered_by":   callerARN,
		"note":            "Registration created. Run set-enabled with enabled:true to grant access.",
	})
}

func adminSetEnabled(ctx context.Context, reg *Registry, r adminRequest, callerARN string) (events.APIGatewayProxyResponse, error) {
	if r.UserID == "" && r.UserEmail != "" {
		resolved, err := resolveSlackEmail(ctx, reg, r.Platform, r.WorkspaceID, r.UserEmail)
		if err != nil {
			return adminError(400, fmt.Sprintf("email resolution: %v", err)), nil
		}
		r.UserID = resolved
	}
	if r.Platform == "" || r.WorkspaceID == "" || r.UserID == "" || r.Nickname == "" {
		return adminError(400, "platform, workspace_id, user_id (or user_email), and nickname are required"), nil
	}

	if err := reg.SetEnabled(ctx, r.Platform, r.WorkspaceID, r.UserID, r.Nickname, r.Enabled); err != nil {
		return adminError(500, fmt.Sprintf("set enabled: %v", err)), nil
	}

	state := "disabled"
	if r.Enabled {
		state = "enabled"
	}
	return adminOK(map[string]interface{}{
		"user_key": userKey(r.Platform, r.WorkspaceID, r.UserID),
		"nickname": r.Nickname,
		"enabled":  r.Enabled,
		"state":    state,
	})
}

func adminDeregister(ctx context.Context, reg *Registry, r adminRequest, callerARN string) (events.APIGatewayProxyResponse, error) {
	if r.UserID == "" && r.UserEmail != "" {
		resolved, err := resolveSlackEmail(ctx, reg, r.Platform, r.WorkspaceID, r.UserEmail)
		if err != nil {
			return adminError(400, fmt.Sprintf("email resolution: %v", err)), nil
		}
		r.UserID = resolved
	}
	if r.Platform == "" || r.WorkspaceID == "" || r.UserID == "" || r.Nickname == "" {
		return adminError(400, "platform, workspace_id, user_id (or user_email), and nickname are required"), nil
	}

	if err := reg.DeleteRegistration(ctx, r.Platform, r.WorkspaceID, r.UserID, r.Nickname); err != nil {
		return adminError(500, fmt.Sprintf("delete registration: %v", err)), nil
	}

	return adminOK(map[string]string{"deleted": userKey(r.Platform, r.WorkspaceID, r.UserID) + "#" + r.Nickname})
}

func adminList(ctx context.Context, reg *Registry, params map[string]string, callerARN string) (events.APIGatewayProxyResponse, error) {
	platform := params["platform"]
	workspaceID := params["workspace_id"]
	userID := params["user_id"]

	if platform == "" || workspaceID == "" {
		return adminError(400, "platform and workspace_id query params are required"), nil
	}

	var regs []BotRegistration
	var err error
	if userID != "" {
		regs, err = reg.ListUserInstances(ctx, platform, workspaceID, userID)
	} else {
		regs, err = reg.ListWorkspaceRegistrations(ctx, platform, workspaceID)
	}
	if err != nil {
		return adminError(500, fmt.Sprintf("list registrations: %v", err)), nil
	}

	return adminOK(map[string]interface{}{
		"registrations": regs,
		"count":         len(regs),
	})
}

// resolveSlackAppID calls Slack's auth.test API to get the App ID for a bot token.
// Returns "" on failure so callers fall back to the legacy workspace key format.
func resolveSlackAppID(ctx context.Context, botToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com/api/auth.test", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		OK    bool   `json:"ok"`
		AppID string `json:"app_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil || !result.OK {
		return ""
	}
	return result.AppID
}

// resolveSlackEmail resolves an email address to a Slack user ID using the
// workspace's stored bot token and Slack's users.lookupByEmail API.
func resolveSlackEmail(ctx context.Context, reg *Registry, platform, workspaceID, email string) (string, error) {
	ws, err := reg.GetWorkspace(ctx, platform, workspaceID)
	if err != nil {
		return "", fmt.Errorf("workspace %s/%s not registered", platform, workspaceID)
	}
	botToken := ws.BotToken
	if botToken == "" {
		return "", fmt.Errorf("no bot token for workspace — re-run workspace-add with --bot-token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://slack.com/api/users.lookupByEmail?email="+url.QueryEscape(email), nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Slack API: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var slackResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		User  struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &slackResp); err != nil {
		return "", fmt.Errorf("parse Slack response: %w", err)
	}
	if !slackResp.OK {
		if slackResp.Error == "users_not_found" {
			return "", fmt.Errorf("no Slack user found with email %q in workspace %s", email, workspaceID)
		}
		return "", fmt.Errorf("Slack API error: %s", slackResp.Error)
	}
	return slackResp.User.ID, nil
}

func adminOK(payload interface{}) (events.APIGatewayProxyResponse, error) {
	body, _ := json.Marshal(payload)
	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}, nil
}

func adminError(code int, msg string) events.APIGatewayProxyResponse {
	body, _ := json.Marshal(map[string]string{"error": msg})
	return events.APIGatewayProxyResponse{
		StatusCode: code,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}
}
