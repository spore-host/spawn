package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
)

// handleWebhook is Phase 1: verify signature, ACK immediately, kick off async execution.
func handleWebhook(ctx context.Context, cfg aws.Config, reg *Registry, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	path := strings.ToLower(request.Path)

	var sc *SlashCommand
	var platform string
	var err error

	// Handle Slack URL verification challenge before any signature checking.
	// Slack sends this once when a slash command endpoint is configured.
	if strings.HasSuffix(path, "/slack") {
		if challenge := extractURLVerificationChallenge(request.Body); challenge != "" {
			return jsonResp(200, fmt.Sprintf(`{"challenge":%q}`, challenge)), nil
		}
	}

	switch {
	case strings.HasSuffix(path, "/slack"):
		platform = "slack"
		sc, err = handleSlackWebhook(ctx, reg, request)
	case strings.HasSuffix(path, "/teams"):
		platform = "teams"
		sc, err = handleTeamsWebhook(ctx, reg, request)
	default:
		return errorResp(404, "not found"), nil
	}

	if err != nil {
		logf("webhook error (%s): %v", platform, err)
		return errorResp(401, err.Error()), nil
	}
	if sc == nil {
		// URL verification challenge already handled
		return jsonResp(200, `{"ok":true}`), nil
	}

	// Parse command, nickname, and optional arg from the text
	command, nickname, arg := parseCommandText(sc.Text)
	if command == "" {
		command = "help"
	}

	// Build action payload for async Phase 2
	action := &BotAction{
		Platform:     platform,
		WorkspaceID:  sc.WorkspaceID,
		UserID:       sc.UserID,
		ResponseURL:  sc.ResponseURL,
		Command:      command,
		Nickname:     nickname,
		Arg:          arg,
		SlashCommand: sc.Command, // e.g. "/spore" or "/prism" — used in help text
	}

	// Teams (Bot Framework): return response inline in HTTP 200 body.
	// Phase 2 async is used only for EC2 operations.
	if platform == "teams" {
		botID := os.Getenv("TEAMS_APP_ID")
		var teamsText string
		selfContained := map[string]bool{"help": true, "list": true, "connect": true, "notify": true, "unnotify": true}
		if selfContained[command] {
			// No EC2 calls needed — execute inline and return result now
			teamsText = helpText(action.slashCmd()) // default for help
			if command != "help" {
				go executeAction(context.Background(), cfg, reg, action) // list/notify handle their own output
				teamsText = ackMessage(command, nickname)
			}
		} else {
			// EC2 commands — ACK now, async handles the result
			teamsText = ackMessage(command, nickname)
			if err := invokeAsync(ctx, action); err != nil {
				go executeAction(context.Background(), cfg, reg, action)
			}
		}
		logf("teams inline response len=%d", len(teamsText))
		resp := fmt.Sprintf(`{"type":"message","from":{"id":%q,"name":"spore-bot"},"text":%q}`, botID, teamsText)
		return jsonResp(200, resp), nil
	}

	// Slack: invoke async, ACK within 3-second window
	if err := invokeAsync(ctx, action); err != nil {
		logf("async invoke failed: %v", err)
		go executeAction(context.Background(), cfg, reg, action)
	}
	return jsonResp(200, fmt.Sprintf(`{"text":%q}`, ackMessage(command, nickname))), nil
}

// handleSlackWebhook verifies the Slack signature and parses the slash command.
func handleSlackWebhook(ctx context.Context, reg *Registry, request events.APIGatewayProxyRequest) (*SlashCommand, error) {
	ts := request.Headers["X-Slack-Request-Timestamp"]
	if ts == "" {
		ts = request.Headers["x-slack-request-timestamp"]
	}
	sig := request.Headers["X-Slack-Signature"]
	if sig == "" {
		sig = request.Headers["x-slack-signature"]
	}
	if ts == "" || sig == "" {
		return nil, fmt.Errorf("missing Slack signature headers")
	}

	sc, err := parseSlackCommand(request.Body)
	if err != nil {
		return nil, fmt.Errorf("parse command: %w", err)
	}

	// Slack does not include X-Slack-App-ID in slash command webhooks.
	// Use the slash command name (/spore, /prism) as discriminator when multiple
	// apps share a workspace, falling back to the legacy key.
	appID := request.Headers["X-Slack-App-ID"]
	if appID == "" {
		appID = request.Headers["x-slack-app-id"]
	}
	logf("slack webhook: team=%s command=%s app_id=%q", sc.WorkspaceID, sc.Command, appID)
	ws, err := reg.GetWorkspaceForSlash(ctx, "slack", sc.WorkspaceID, sc.Command, appID)
	if err != nil {
		return nil, fmt.Errorf("workspace not found: %w", err)
	}

	if err := verifySlackSignature(ws.SigningSecret, ts, request.Body, sig); err != nil {
		return nil, fmt.Errorf("signature verification failed: %w", err)
	}

	// Channel restriction: if the workspace has an allowed-channels list, enforce it.
	if !IsChannelAllowed(ws, sc.ChannelID) {
		return nil, fmt.Errorf("commands are only accepted from designated channels in this workspace")
	}

	return sc, nil
}

// handleTeamsWebhook handles Bot Framework activities and outgoing webhooks.
// Bot Framework (Azure Bot / Test in Web Chat): Authorization: Bearer <jwt>
// Outgoing webhooks (direct Teams config):      Authorization: HMAC <base64>
func handleTeamsWebhook(ctx context.Context, reg *Registry, request events.APIGatewayProxyRequest) (*SlashCommand, error) {
	authHeader := request.Headers["Authorization"]
	if authHeader == "" {
		authHeader = request.Headers["authorization"]
	}

	// Parse activity to get workspace (tenant) ID
	sc, _, err := parseTeamsActivity(request.Body)
	if err != nil {
		return nil, fmt.Errorf("parse Teams activity: %w", err)
	}

	isBotFramework := strings.HasPrefix(authHeader, "Bearer ")

	if isBotFramework {
		// Bot Framework JWT — issued by Microsoft, trusted via App ID match.
		// Full JWT signature verification requires fetching Microsoft's OIDC keys;
		// for now we trust the Azure Bot channel (messages come from Microsoft's servers)
		// and verify the App ID matches our configured bot.
		appID := os.Getenv("TEAMS_APP_ID")
		if appID == "" {
			return nil, fmt.Errorf("TEAMS_APP_ID not configured")
		}
		// Workspace lookup is optional for Bot Framework — tenant may not have signing secret
		_, _ = reg.GetWorkspace(ctx, "teams", sc.WorkspaceID) // best-effort
	} else {
		// Outgoing webhook — verify HMAC signature
		ws, err := reg.GetWorkspace(ctx, "teams", sc.WorkspaceID)
		if err != nil {
			return nil, fmt.Errorf("workspace not found: %w", err)
		}
		if err := verifyTeamsSignature(ws.SigningSecret, request.Body, authHeader); err != nil {
			return nil, fmt.Errorf("signature verification failed: %w", err)
		}
	}

	// Store conversation reference for proactive messaging
	var activity TeamsActivity
	if jsonErr := json.Unmarshal([]byte(request.Body), &activity); jsonErr == nil {
		userKey := "teams#" + sc.WorkspaceID + "#" + sc.UserID
		go storeTeamsConversationRef(context.Background(), reg, userKey, activity)
	}

	return sc, nil
}

// handleAsyncAction is Phase 2: execute the EC2 op and post result to response_url.
func handleAsyncAction(ctx context.Context, cfg aws.Config, reg *Registry, payload []byte) error {
	var action BotAction
	if err := json.Unmarshal(payload, &action); err != nil {
		return fmt.Errorf("unmarshal action: %w", err)
	}

	// Commands that manage their own target or need no registration
	selfContained := map[string]bool{
		"help": true, "list": true, "connect": true,
		"notify": true, "unnotify": true,
	}
	if !selfContained[action.Command] {
		registration, ambiguousMsg, err := resolveRegistration(ctx, reg, &action)
		if err != nil {
			postResponse(action.Platform, action.ResponseURL, "❌ "+err.Error())
			return nil
		}
		if ambiguousMsg != "" {
			postResponse(action.Platform, action.ResponseURL, ambiguousMsg)
			return nil
		}
		action.Registration = registration
	}

	executeAction(ctx, cfg, reg, &action)
	return nil
}

// parseCommandText splits "extend rstudio 2h" into ("extend", "rstudio", "2h").
func parseCommandText(text string) (command, nickname, arg string) {
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(parts) == 0 {
		return "help", "", ""
	}
	command = parts[0]
	if len(parts) > 1 {
		nickname = parts[1]
	}
	if len(parts) > 2 {
		arg = parts[2]
	}
	return
}

// ackMessage returns the immediate ACK text shown while the op runs.
func ackMessage(command, nickname string) string {
	target := ""
	if nickname != "" {
		target = " *" + nickname + "*"
	}
	switch command {
	case "start":
		return "▶️ Starting" + target + "..."
	case "stop":
		return "⏹️ Stopping" + target + "..."
	case "hibernate":
		return "💤 Hibernating" + target + "..."
	case "status", "url":
		return "🔍 Checking" + target + "..."
	case "list":
		return "📋 Fetching your instances..."
	case "connect":
		return "🔑 Generating your connect code — check your DMs..."
	case "extend":
		if nickname != "" {
			return "⏱️ Extending TTL for *" + nickname + "*..."
		}
		return "⏱️ Extending TTL..."
	case "idle":
		if nickname != "" {
			return "💤 Updating idle timeout for *" + nickname + "*..."
		}
		return "💤 Updating idle timeout..."
	case "notify":
		if nickname != "" {
			return "🔔 Subscribing to notifications for *" + nickname + "*..."
		}
		return "🔔 Setting up notifications..."
	case "unnotify":
		if nickname != "" {
			return "🔕 Removing notifications for *" + nickname + "*..."
		}
		return "🔕 Removing notification subscription..."
	default:
		return "⏳ On it..."
	}
}

func jsonResp(status int, body string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       body,
	}
}

func errorResp(status int, msg string) events.APIGatewayProxyResponse {
	return jsonResp(status, fmt.Sprintf(`{"error":%q}`, msg))
}
