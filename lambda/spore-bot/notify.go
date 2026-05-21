package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// NotifyRequest is sent by spored when a lifecycle event fires.
// Authentication: PKCS#7 (preferred) or legacy document+signature.
type NotifyRequest struct {
	PKCS7                     string `json:"pkcs7,omitempty"`
	InstanceIdentityDocument  string `json:"instance_identity_document,omitempty"`
	InstanceIdentitySignature string `json:"instance_identity_signature,omitempty"`
	Command                   string `json:"command,omitempty"` // e.g. "/spore" — routes to correct workspace config
	Platform                  string `json:"platform"`          // "slack"
	WorkspaceID               string `json:"workspace_id"`      // e.g. "T03NE3GTY"
	EventType                 string `json:"event_type"`        // ttl_warning, completion, etc.
	InstanceName              string `json:"instance_name"`
	InstanceID                string `json:"instance_id"`
	Region                    string `json:"region"`
	DNSName                   string `json:"dns_name,omitempty"`
	Detail                    string `json:"detail,omitempty"`
}

// handleNotify receives lifecycle events from spored and routes them to Slack.
// POST /notify — authentication via EC2 instance identity (no user credentials needed).
func handleNotify(ctx context.Context, cfg aws.Config, reg *Registry, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	var nr NotifyRequest
	if err := json.Unmarshal([]byte(request.Body), &nr); err != nil {
		return errorResp(400, "Invalid request body"), nil
	}

	// Auth: verify the instance_id is registered in this workspace.
	// Cryptographic PKCS#7 verification was unreliable across AWS regions and
	// cert rotations. Registry membership is the effective gate — an unregistered
	// instance can't trigger DMs regardless of what it sends.
	if nr.WorkspaceID == "" || nr.InstanceID == "" {
		return errorResp(400, "workspace_id and instance_id are required"), nil
	}

	msg := formatNotification(nr)

	// Look up workspace configs. When the command is known (e.g. "/spore"),
	// use the command-scoped key directly to avoid cross-app DM duplication.
	// Fall back to scanning all configs if no command is specified.
	var workspaces []WorkspaceConfig
	if nr.Command != "" {
		if ws, err := reg.getWorkspaceByKey(ctx, workspaceKey(nr.Platform, nr.WorkspaceID, nr.Command)); err == nil {
			workspaces = []WorkspaceConfig{*ws}
		}
	}
	if len(workspaces) == 0 {
		workspaces = reg.GetWorkspacesForPlatform(ctx, nr.Platform, nr.WorkspaceID)
	}
	if len(workspaces) == 0 {
		logf("notify: no workspaces found for %s#%s", nr.Platform, nr.WorkspaceID)
		return jsonOK(), nil
	}

	// Use a background context for outbound Slack calls — the request context
	// is cancelled as soon as handleNotify returns, killing in-flight HTTP calls.
	slackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := range workspaces {
		ws := &workspaces[i]
		// Pattern A: post to channel via incoming webhook — synchronous so Lambda stays alive
		if ws.IncomingWebhookURL != "" {
			postIncomingWebhook(ws.IncomingWebhookURL, msg)
		}
		// Pattern B: DM each registered Slack user
		sendUserDMs(slackCtx, cfg, reg, ws, nr.InstanceID, msg)
	}

	// Teams DMs via Bot Framework proactive messaging
	if nr.Platform == "slack" || nr.Platform == "" {
		sendTeamsDMs(slackCtx, reg, nil, nr.InstanceID, msg)
	}

	// SMS notifications for users who have registered a phone number
	go sendUserSMS(context.Background(), reg, nr)

	return jsonOK(), nil
}

// postIncomingWebhook POSTs a message to a Slack incoming webhook URL.
func postIncomingWebhook(webhookURL, text string) {
	payload, _ := json.Marshal(map[string]string{"text": text})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", webhookURL, bytes.NewReader(payload))
	if err != nil {
		logf("incoming webhook request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		logf("incoming webhook call error: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		logf("incoming webhook returned %d", resp.StatusCode)
	}
}

// sendUserDMs sends DMs to each Slack user registered for this instance.
func sendUserDMs(ctx context.Context, cfg aws.Config, reg *Registry, ws *WorkspaceConfig, instanceID, text string) {
	if ws.BotToken == "" {
		return
	}

	// Query instance_id-index GSI for all registrations for this instance
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

	// Get a fresh bot token (handles rotation if enabled)
	botToken := ws.BotToken

	for _, item := range result.Items {
		userKey, ok := item["user_key"].(*dynamodbtypes.AttributeValueMemberS)
		if !ok {
			continue
		}
		// user_key = "{platform}#{workspace}#{user_id}"
		parts := strings.SplitN(userKey.Value, "#", 3)
		if len(parts) != 3 {
			continue
		}
		userID := parts[2]

		// Send DM via chat.postMessage
		go postSlackDM(ctx, botToken, userID, text)
	}
}

// postSlackDM sends a direct message to a Slack user via chat.postMessage.
// Uses its own 8-second timeout context so caller context cancellation doesn't abort the call.
func postSlackDM(ctx context.Context, botToken, userID, text string) {
	dmCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ctx = dmCtx
	payload, _ := json.Marshal(map[string]string{
		"channel": userID, // Using user ID as channel opens a DM
		"text":    text,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://slack.com/api/chat.postMessage",
		bytes.NewReader(payload))
	if err != nil {
		logf("DM request error for %s: %v", userID, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		logf("DM call error for %s: %v", userID, err)
		return
	}
	defer resp.Body.Close()
	var slackResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	if body, err := io.ReadAll(resp.Body); err == nil {
		if jsonErr := json.Unmarshal(body, &slackResp); jsonErr == nil && !slackResp.OK {
			logf("DM to %s failed: %s", userID, slackResp.Error)
		}
	}
}

// formatNotification builds the Slack message text for a lifecycle event.
func formatNotification(nr NotifyRequest) string {
	name := nr.InstanceName
	if name == "" {
		name = nr.InstanceID
	}

	var icon, verb string
	switch nr.EventType {
	case "ttl_warning":
		icon, verb = "⏱️", fmt.Sprintf("*%s* terminates in %s", name, nr.Detail)
	case "ttl_expired":
		icon, verb = "🔴", fmt.Sprintf("*%s* has terminated — scheduled end time reached", name)
	case "idle_warning":
		icon, verb = "💤", fmt.Sprintf("*%s* will stop in %s — no activity detected", name, nr.Detail)
	case "idle_stopped":
		icon, verb = "⏹️", fmt.Sprintf("*%s* has stopped — idle timeout reached", name)
	case "idle_hibernated":
		icon, verb = "💤", fmt.Sprintf("*%s* has hibernated — idle timeout reached", name)
	case "idle_terminated": // legacy — kept for older spored versions
		icon, verb = "🔴", fmt.Sprintf("*%s* has terminated — idle timeout reached", name)
	case "completion":
		icon, verb = "✅", fmt.Sprintf("*%s* has completed", name)
	case "spot_interrupt":
		icon, verb = "⚠️", fmt.Sprintf("*%s* received a Spot interruption notice — %s", name, nr.Detail)
	case "pre_stop_start":
		icon, verb = "🔄", fmt.Sprintf("*%s* is running its shutdown task before terminating", name)
	default:
		icon, verb = "ℹ️", fmt.Sprintf("*%s*: %s", name, nr.EventType)
	}

	msg := fmt.Sprintf("%s %s", icon, verb)
	if nr.DNSName != "" {
		msg += fmt.Sprintf("\n  URL: https://%s", nr.DNSName)
	}
	if nr.InstanceID != "" {
		msg += fmt.Sprintf("\n  AWS Instance ID: `%s`", nr.InstanceID)
	}
	if nr.Region != "" {
		msg += fmt.Sprintf("  Region: %s", nr.Region)
	}
	return msg
}

// sendUserSMS sends SMS notifications to users who have registered a phone number
// for the instance. It looks up registrations via the instance_id-index GSI, then
// for each user checks if a _phone record exists in the registry.
func sendUserSMS(ctx context.Context, reg *Registry, nr NotifyRequest) {
	project := strings.TrimPrefix(nr.Command, "/")
	if project == "" {
		project = "spore"
	}
	if twilioProjectNumber(project) == "" {
		return // Twilio not configured — skip silently
	}
	fromNumber := twilioProjectNumber(project)
	if fromNumber == "" {
		return
	}

	// Query instance registrations
	result, err := reg.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(reg.registryTable),
		IndexName:              aws.String("instance_id-index"),
		KeyConditionExpression: aws.String("instance_id = :iid"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":iid": &dynamodbtypes.AttributeValueMemberS{Value: nr.InstanceID},
		},
	})
	if err != nil || len(result.Items) == 0 {
		return
	}

	// Build the SMS message once
	extra := map[string]string{
		"remaining":     "5 minutes",
		"idle_duration": "25m",
		"cost":          "$0.00",
	}
	if nr.Detail != "" {
		extra["remaining"] = nr.Detail
		extra["idle_duration"] = nr.Detail
	}
	msgText, options := twilioMessage(nr.InstanceName, nr.EventType, extra)

	smsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	seen := map[string]bool{}
	for _, item := range result.Items {
		ukAttr, ok := item["user_key"].(*dynamodbtypes.AttributeValueMemberS)
		if !ok {
			continue
		}
		userKey := ukAttr.Value
		if seen[userKey] {
			continue
		}
		seen[userKey] = true

		// Look up _phone record for this user
		phoneResult, err := reg.client.GetItem(smsCtx, &dynamodb.GetItemInput{
			TableName: aws.String(reg.registryTable),
			Key: map[string]dynamodbtypes.AttributeValue{
				"user_key": &dynamodbtypes.AttributeValueMemberS{Value: userKey},
				"nickname": &dynamodbtypes.AttributeValueMemberS{Value: "_phone"},
			},
		})
		if err != nil || phoneResult.Item == nil {
			continue
		}
		phoneAttr, ok := phoneResult.Item["phone"].(*dynamodbtypes.AttributeValueMemberS)
		if !ok || phoneAttr.Value == "" {
			continue
		}
		toPhone := phoneAttr.Value

		// Store pending notification so the reply can be matched
		if len(options) > 0 {
			_ = twilioStorePending(smsCtx, fromNumber, toPhone, project, nr.InstanceID, nr.Region, nr.EventType, options)
		}

		// Send the SMS
		go func(phone string) {
			if err := twilioSend(smsCtx, project, phone, msgText); err != nil {
				logf("SMS to %s failed: %v", phone, err)
			}
		}(toPhone)
	}
}

func jsonOK() events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       `{"ok":true}`,
	}
}
