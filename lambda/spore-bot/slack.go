package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SlashCommand represents a parsed Slack slash command payload.
type SlashCommand struct {
	Command     string
	Text        string
	UserID      string
	WorkspaceID string
	ChannelID   string // used for channel restriction checks
	ResponseURL string
	TriggerID   string
}

// extractURLVerificationChallenge detects Slack's one-time endpoint verification
// request and returns the challenge string if present, otherwise empty string.
// Slack sends {"type":"url_verification","challenge":"..."} as JSON when you
// configure a new slash command endpoint — no HMAC required for this event.
func extractURLVerificationChallenge(body string) string {
	if !strings.HasPrefix(strings.TrimSpace(body), "{") {
		return ""
	}
	var v struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return ""
	}
	if v.Type == "url_verification" {
		return v.Challenge
	}
	return ""
}

// parseSlackCommand parses a URL-encoded Slack slash command body.
func parseSlackCommand(body string) (*SlashCommand, error) {
	vals, err := url.ParseQuery(body)
	if err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	return &SlashCommand{
		Command:     vals.Get("command"),
		Text:        strings.TrimSpace(vals.Get("text")),
		UserID:      vals.Get("user_id"),
		WorkspaceID: vals.Get("team_id"),
		ChannelID:   vals.Get("channel_id"),
		ResponseURL: vals.Get("response_url"),
		TriggerID:   vals.Get("trigger_id"),
	}, nil
}

// verifySlackSignature validates the X-Slack-Signature header.
// Uses HMAC-SHA256 with the workspace signing secret.
// Rejects requests older than 5 minutes to prevent replay attacks.
func verifySlackSignature(signingSecret, timestamp, body, sig string) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}
	if age := time.Now().Unix() - ts; age > 300 || age < -60 {
		return fmt.Errorf("request timestamp too old or in future (%ds)", age)
	}

	baseStr := "v0:" + timestamp + ":" + body
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(baseStr))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// SlackMessage is the payload sent back to Slack via response_url.
type SlackMessage struct {
	ResponseType string `json:"response_type"` // "in_channel" or "ephemeral"
	Text         string `json:"text"`
}

// postSlackResponse sends a delayed response to Slack's response_url.
// If text is a JSON blocks array (starts with "[{"), it is sent as Block Kit.
func postSlackResponse(responseURL, text string, inChannel bool) error {
	msgType := "ephemeral"
	if inChannel {
		msgType = "in_channel"
	}
	var payload interface{}
	if strings.HasPrefix(strings.TrimSpace(text), "[{") {
		var blocks []map[string]interface{} // nosemgrep: go.lang.security.deserialization.unsafe-deserialization-interface.go-unsafe-deserialization-interface
		if err := json.Unmarshal([]byte(text), &blocks); err == nil {
			payload = map[string]interface{}{
				"response_type": msgType,
				"blocks":        blocks,
			}
		}
	}
	if payload == nil {
		payload = SlackMessage{ResponseType: msgType, Text: text}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	return httpPost(responseURL, "application/json", data)
}

// InstanceStatus holds all display data for a status card.
type InstanceStatus struct {
	Nickname        string // user-facing name
	InstanceID      string // i-...
	State           string // running, stopped, stopping, hibernated, pending
	InstanceType    string // t3.small, g7e.xlarge, etc.
	AZ              string // us-east-1a
	IP              string // public IP
	DNSName         string // spore-bot-test.5k0zfnmq.spore.host
	LaunchTime      string // RFC3339
	TTL             string // "4h"
	OnComplete      string // "terminate" | "stop"
	IdleTimeout     string // "1h"
	HibernateOnIdle bool   // idle action: hibernate instead of stop
	LoggedInCount   int    // active SSH/terminal sessions (from spawn:logged-in-count tag)
}

// formatSlackStatus returns a Slack Block Kit JSON array for a status card.
// Block Kit renders fields in a two-column grid, solving variable-width font alignment.
func formatSlackStatus(s InstanceStatus) string {
	icon := "🟡"
	stateLabel := s.State
	switch s.State {
	case "running":
		icon, stateLabel = "🟢", "Running"
	case "stopped":
		icon, stateLabel = "🔴", "Stopped"
	case "hibernated":
		icon, stateLabel = "💤", "Hibernated (RAM saved)"
	case "stopping":
		icon, stateLabel = "🔴", "Stopping..."
	case "pending":
		icon, stateLabel = "🟡", "Starting..."
	case "shutting-down":
		icon, stateLabel = "🔴", "Shutting down..."
	case "terminated":
		icon, stateLabel = "⚫", "Terminated"
	}

	field := func(label, value string) map[string]interface{} {
		return map[string]interface{}{
			"type": "mrkdwn",
			"text": fmt.Sprintf("*%s*\n%s", label, value),
		}
	}

	var fields []interface{}

	if s.InstanceType != "" {
		fields = append(fields, field("Instance Type", s.InstanceType))
	}
	if s.AZ != "" {
		fields = append(fields, field("Region", s.AZ))
	}
	if s.IP != "" {
		fields = append(fields, field("IP Address", "`"+s.IP+"`"))
	}
	if s.DNSName != "" {
		fields = append(fields, field("URL", "https://"+s.DNSName))
	}
	if s.LaunchTime != "" {
		if t, err := time.Parse(time.RFC3339, s.LaunchTime); err == nil {
			fields = append(fields, field("Launched",
				fmt.Sprintf("%s (%s ago)", t.UTC().Format("2 Jan 15:04 UTC"), formatHMS(time.Since(t)))))
		}
	}
	if s.TTL != "" {
		if ttlDur, err := time.ParseDuration(s.TTL); err == nil && s.LaunchTime != "" {
			if launched, err := time.Parse(time.RFC3339, s.LaunchTime); err == nil {
				terminateAt := launched.Add(ttlDur)
				remaining := time.Until(terminateAt)
				var val string
				if remaining > 0 {
					val = fmt.Sprintf("%s (%s remaining)", terminateAt.UTC().Format("2 Jan 15:04 UTC"), formatHMS(remaining))
				} else {
					label := "terminating..."
					if s.State == "terminated" || s.State == "shutting-down" {
						label = "expired"
					}
					val = fmt.Sprintf("%s (%s)", terminateAt.UTC().Format("2 Jan 15:04 UTC"), label)
				}
				fields = append(fields, field("TTL", val))
			}
		} else {
			fields = append(fields, field("TTL", "after "+s.TTL+" from launch"))
		}
	}
	idleAction := "stop"
	if s.HibernateOnIdle {
		idleAction = "hibernate"
	}
	if s.IdleTimeout != "" {
		fields = append(fields, field("Idle Timeout", fmt.Sprintf("after %s idle → %s", s.IdleTimeout, idleAction)))
	} else {
		fields = append(fields, field("Idle Timeout", "None"))
	}
	if s.LoggedInCount > 0 {
		fields = append(fields, field("Active Sessions", fmt.Sprintf("%d user(s) logged in", s.LoggedInCount)))
	}
	fields = append(fields, field("Instance ID", "`"+s.InstanceID+"`"))

	blocks := []interface{}{
		map[string]interface{}{
			"type": "section",
			"text": map[string]interface{}{
				"type": "mrkdwn",
				"text": fmt.Sprintf("%s *%s* — %s", icon, s.Nickname, stateLabel),
			},
		},
		map[string]interface{}{
			"type":   "section",
			"fields": fields,
		},
	}

	data, err := json.Marshal(blocks)
	if err != nil {
		// Fallback to plain text on marshal failure
		return fmt.Sprintf("%s *%s* — %s\n`%s`", icon, s.Nickname, stateLabel, s.InstanceID)
	}
	return string(data)
}

// formatHMS formats a duration as hh:mm:ss countdown.
func formatHMS(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// formatDuration formats a duration as "2h 15m" or "45m" etc.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 && m > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", m)
}
