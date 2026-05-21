package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ── Slack signature verification ──────────────────────────────────────────────

func makeSlackSig(secret, timestamp, body string) string {
	base := "v0:" + timestamp + ":" + body
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(base))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySlackSignature_Valid(t *testing.T) {
	secret := "test-signing-secret"
	body := "command=/prism&text=status&user_id=U123&team_id=T456"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := makeSlackSig(secret, ts, body)

	if err := verifySlackSignature(secret, ts, body, sig); err != nil {
		t.Errorf("expected valid signature to pass, got: %v", err)
	}
}

func TestVerifySlackSignature_WrongSecret(t *testing.T) {
	body := "command=/prism&text=stop"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := makeSlackSig("real-secret", ts, body)

	err := verifySlackSignature("wrong-secret", ts, body, sig)
	if err == nil {
		t.Error("expected wrong secret to fail")
	}
}

func TestVerifySlackSignature_TamperedBody(t *testing.T) {
	secret := "test-secret"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := makeSlackSig(secret, ts, "original body")

	err := verifySlackSignature(secret, ts, "tampered body", sig)
	if err == nil {
		t.Error("expected tampered body to fail")
	}
}

func TestVerifySlackSignature_ReplayAttack(t *testing.T) {
	secret := "test-secret"
	body := "command=/prism&text=stop"
	oldTS := strconv.FormatInt(time.Now().Unix()-400, 10) // 400s ago, > 5min window
	sig := makeSlackSig(secret, oldTS, body)

	err := verifySlackSignature(secret, oldTS, body, sig)
	if err == nil {
		t.Error("expected old timestamp to be rejected as replay attack")
	}
	if !strings.Contains(err.Error(), "timestamp") {
		t.Errorf("expected timestamp error, got: %v", err)
	}
}

func TestVerifySlackSignature_FutureTimestamp(t *testing.T) {
	secret := "test-secret"
	body := "command=/prism&text=stop"
	futureTS := strconv.FormatInt(time.Now().Unix()+120, 10) // 2min in future
	sig := makeSlackSig(secret, futureTS, body)

	err := verifySlackSignature(secret, futureTS, body, sig)
	if err == nil {
		t.Error("expected far-future timestamp to be rejected")
	}
}

func TestVerifySlackSignature_InvalidTimestamp(t *testing.T) {
	err := verifySlackSignature("secret", "not-a-number", "body", "v0=abc")
	if err == nil {
		t.Error("expected invalid timestamp to fail")
	}
}

func TestVerifySlackSignature_EmptySignature(t *testing.T) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	err := verifySlackSignature("secret", ts, "body", "")
	if err == nil {
		t.Error("expected empty signature to fail")
	}
}

// ── Teams signature verification ──────────────────────────────────────────────

func TestVerifyTeamsSignature_Valid(t *testing.T) {
	secret := "teams-bot-secret"
	body := `{"type":"message","text":"prism stop rstudio"}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig := "HMAC " + encodeBase64(mac.Sum(nil))

	if err := verifyTeamsSignature(secret, body, sig); err != nil {
		t.Errorf("expected valid Teams signature to pass, got: %v", err)
	}
}

func TestVerifyTeamsSignature_Wrong(t *testing.T) {
	err := verifyTeamsSignature("secret", "body", "HMAC AAAA")
	if err == nil {
		t.Error("expected wrong Teams signature to fail")
	}
}

func TestVerifyTeamsSignature_MissingPrefix(t *testing.T) {
	err := verifyTeamsSignature("secret", "body", "Bearer sometoken")
	if err == nil {
		t.Error("expected non-HMAC auth header to fail")
	}
}

// ── Command parsing ───────────────────────────────────────────────────────────

func TestParseCommandText(t *testing.T) {
	tests := []struct {
		input    string
		wantCmd  string
		wantNick string
	}{
		{"stop rstudio", "stop", "rstudio"},
		{"STATUS", "status", ""}, // case-insensitive
		{"hibernate jupyter", "hibernate", "jupyter"},
		{"list", "list", ""},
		{"", "help", ""},                         // empty → help
		{"  stop  rstudio  ", "stop", "rstudio"}, // trimmed
		{"stop i-0abc123", "stop", "i-0abc123"},  // instance ID as target
		{"url", "url", ""},
		{"help", "help", ""},
		{"unknown", "unknown", ""}, // unknown command passes through, handler rejects
	}
	for _, tt := range tests {
		cmd, nick, _ := parseCommandText(tt.input)
		if cmd != tt.wantCmd {
			t.Errorf("parseCommandText(%q) command = %q, want %q", tt.input, cmd, tt.wantCmd)
		}
		if nick != tt.wantNick {
			t.Errorf("parseCommandText(%q) nickname = %q, want %q", tt.input, nick, tt.wantNick)
		}
	}
}

// ── Registration resolution ───────────────────────────────────────────────────

func makeReg(nickname, instanceID, dnsName string) BotRegistration {
	return BotRegistration{
		UserKey:        "slack#T123#U456",
		Nickname:       nickname,
		InstanceID:     instanceID,
		DNSName:        dnsName,
		AllowedActions: []string{"start", "stop", "status", "hibernate", "url"},
		TagPrefix:      "spawn",
		Platform:       "slack",
	}
}

// mockRegistry implements listUserInstances without DynamoDB.
type mockRegistry struct {
	regs []BotRegistration
}

func (m *mockRegistry) list() []BotRegistration { return m.regs }

// resolveFromList is a testable extraction of the resolution logic.
func resolveFromList(regs []BotRegistration, target string) (*BotRegistration, string) {
	if len(regs) == 0 {
		return nil, "no instances registered"
	}
	if target == "" {
		if len(regs) == 1 {
			return &regs[0], ""
		}
		names := make([]string, len(regs))
		for i, r := range regs {
			names[i] = r.Nickname
		}
		return nil, fmt.Sprintf("multiple instances: %s", strings.Join(names, ", "))
	}
	for i := range regs {
		if strings.EqualFold(regs[i].Nickname, target) {
			return &regs[i], ""
		}
	}
	for i := range regs {
		if strings.EqualFold(regs[i].InstanceID, target) {
			return &regs[i], ""
		}
		if regs[i].DNSName != "" && strings.EqualFold(regs[i].DNSName, target) {
			return &regs[i], ""
		}
	}
	return nil, fmt.Sprintf("no instance named %q", target)
}

func TestResolveRegistration_ByNickname(t *testing.T) {
	regs := []BotRegistration{
		makeReg("rstudio", "i-0abc123", "rstudio.abc.prismcloud.host"),
		makeReg("jupyter", "i-0def456", ""),
	}
	reg, msg := resolveFromList(regs, "rstudio")
	if reg == nil || reg.InstanceID != "i-0abc123" {
		t.Errorf("expected rstudio, got %v (msg: %q)", reg, msg)
	}
}

func TestResolveRegistration_ByInstanceID(t *testing.T) {
	regs := []BotRegistration{makeReg("rstudio", "i-0abc123", "")}
	reg, msg := resolveFromList(regs, "i-0abc123")
	if reg == nil || reg.Nickname != "rstudio" {
		t.Errorf("expected match by instance ID, got %v (msg: %q)", reg, msg)
	}
}

func TestResolveRegistration_ByDNSName(t *testing.T) {
	regs := []BotRegistration{makeReg("rstudio", "i-0abc123", "rstudio.abc.prismcloud.host")}
	reg, msg := resolveFromList(regs, "rstudio.abc.prismcloud.host")
	if reg == nil || reg.Nickname != "rstudio" {
		t.Errorf("expected match by DNS name, got %v (msg: %q)", reg, msg)
	}
}

func TestResolveRegistration_CaseInsensitive(t *testing.T) {
	regs := []BotRegistration{makeReg("RStudio", "i-0abc123", "")}
	reg, _ := resolveFromList(regs, "rstudio")
	if reg == nil {
		t.Error("expected case-insensitive nickname match")
	}
}

func TestResolveRegistration_SingleNoNickname(t *testing.T) {
	regs := []BotRegistration{makeReg("rstudio", "i-0abc123", "")}
	reg, msg := resolveFromList(regs, "")
	if reg == nil {
		t.Errorf("expected single instance to be returned without nickname, msg: %q", msg)
	}
}

func TestResolveRegistration_MultipleNoNickname(t *testing.T) {
	regs := []BotRegistration{
		makeReg("rstudio", "i-0abc123", ""),
		makeReg("jupyter", "i-0def456", ""),
	}
	reg, msg := resolveFromList(regs, "")
	if reg != nil {
		t.Error("expected nil when multiple instances and no nickname")
	}
	if !strings.Contains(msg, "rstudio") || !strings.Contains(msg, "jupyter") {
		t.Errorf("expected disambiguation message listing both, got: %q", msg)
	}
}

func TestResolveRegistration_NotFound(t *testing.T) {
	regs := []BotRegistration{makeReg("rstudio", "i-0abc123", "")}
	reg, msg := resolveFromList(regs, "nonexistent")
	if reg != nil {
		t.Error("expected nil for nonexistent nickname")
	}
	if !strings.Contains(msg, "nonexistent") {
		t.Errorf("expected not-found message, got: %q", msg)
	}
}

func TestResolveRegistration_Empty(t *testing.T) {
	reg, msg := resolveFromList(nil, "")
	if reg != nil {
		t.Error("expected nil for empty registry")
	}
	if msg == "" {
		t.Error("expected non-empty message for empty registry")
	}
}

// ── Teams activity parsing ────────────────────────────────────────────────────

func TestParseTeamsActivity(t *testing.T) {
	body := `{
		"type": "message",
		"text": "<at>BotName</at> /prism stop rstudio",
		"from": {"id": "user-abc-123", "name": "Alice"},
		"serviceUrl": "https://smba.trafficmanager.net/apis/",
		"channelData": {"tenant": {"id": "tenant-xyz-789"}}
	}`

	sc, _, err := parseTeamsActivity(body)
	if err != nil {
		t.Fatalf("parseTeamsActivity: %v", err)
	}
	if sc.UserID != "user-abc-123" {
		t.Errorf("UserID = %q, want user-abc-123", sc.UserID)
	}
	if sc.WorkspaceID != "tenant-xyz-789" {
		t.Errorf("WorkspaceID = %q, want tenant-xyz-789", sc.WorkspaceID)
	}
	// Mention should be stripped; text should contain the command
	if !strings.Contains(sc.Text, "stop") {
		t.Errorf("Text should contain 'stop', got: %q", sc.Text)
	}
}

// ── ACK messages ─────────────────────────────────────────────────────────────

func TestAckMessage(t *testing.T) {
	tests := []struct {
		cmd  string
		nick string
		want string
	}{
		{"start", "rstudio", "▶️ Starting *rstudio*..."},
		{"stop", "", "⏹️ Stopping..."},
		{"hibernate", "jupyter", "💤 Hibernating *jupyter*..."},
		{"status", "rstudio", "🔍 Checking *rstudio*..."},
		{"url", "", "🔍 Checking..."},
		{"list", "", "📋 Fetching your instances..."},
		{"unknown", "", "⏳ On it..."},
	}
	for _, tt := range tests {
		got := ackMessage(tt.cmd, tt.nick)
		if got != tt.want {
			t.Errorf("ackMessage(%q, %q) = %q, want %q", tt.cmd, tt.nick, got, tt.want)
		}
	}
}

// ── Status formatting ─────────────────────────────────────────────────────────

func TestFormatSlackStatus(t *testing.T) {
	tests := []struct {
		state    string
		wantIcon string
	}{
		{"running", "🟢"},
		{"stopped", "🔴"},
		{"stopping", "🔴"},
		{"hibernated", "💤"}, // set by executor when StateReason.Code == Client.UserInitiatedHibernate
		{"pending", "🟡"},
	}
	for _, tt := range tests {
		result := formatSlackStatus(InstanceStatus{Nickname: "rstudio", InstanceID: "i-0abc123", State: tt.state})
		if !strings.Contains(result, tt.wantIcon) {
			t.Errorf("state %q: expected icon %q in %q", tt.state, tt.wantIcon, result)
		}
	}
}

func TestFormatSlackStatus_WithIPAndDNS(t *testing.T) {
	result := formatSlackStatus(InstanceStatus{
		Nickname:   "rstudio",
		InstanceID: "i-0abc123",
		State:      "running",
		IP:         "1.2.3.4",
		DNSName:    "rstudio.abc.prismcloud.host",
	})
	if !strings.Contains(result, "1.2.3.4") {
		t.Error("expected IP in status output")
	}
	if !strings.Contains(result, "rstudio.abc.prismcloud.host") {
		t.Error("expected DNS name in status output")
	}
}

func TestFormatSlackStatus_RichCard(t *testing.T) {
	result := formatSlackStatus(InstanceStatus{
		Nickname:     "spore-bot-test",
		InstanceID:   "i-038954d0b2e861273",
		State:        "running",
		InstanceType: "t3.small",
		AZ:           "us-east-1f",
		IP:           "98.92.241.152",
		DNSName:      "spore-bot-test.5k0zfnmq.spore.host",
		TTL:          "4h",
		IdleTimeout:  "1h",
	})
	for _, want := range []string{
		"🟢", "spore-bot-test", "Running",
		"Instance Type", "t3.small",
		"Region", "us-east-1f",
		"IP Address", "98.92.241.152",
		"URL", "https://spore-bot-test.5k0zfnmq.spore.host",
		"TTL", "4h",
		"Idle Timeout", "1h",
		"Instance ID", "i-038954d0b2e861273",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %q in status card\nGot:\n%s", want, result)
		}
	}
}

// ── URL verification challenge ────────────────────────────────────────────────

func TestExtractURLVerificationChallenge_Valid(t *testing.T) {
	body := `{"type":"url_verification","token":"abc","challenge":"3eZbrw1aCnBI3Gu"}`
	got := extractURLVerificationChallenge(body)
	if got != "3eZbrw1aCnBI3Gu" {
		t.Errorf("expected challenge, got %q", got)
	}
}

func TestExtractURLVerificationChallenge_NotVerification(t *testing.T) {
	body := `{"type":"message","text":"hello"}`
	if got := extractURLVerificationChallenge(body); got != "" {
		t.Errorf("expected empty for non-verification, got %q", got)
	}
}

func TestExtractURLVerificationChallenge_FormEncoded(t *testing.T) {
	body := "command=/prism&text=stop&user_id=U123"
	if got := extractURLVerificationChallenge(body); got != "" {
		t.Errorf("expected empty for form-encoded body, got %q", got)
	}
}

// ── Slack command parsing ─────────────────────────────────────────────────────

func TestParseSlackCommand(t *testing.T) {
	body := "command=/prism&text=stop+rstudio&user_id=U04ABC&team_id=T03XYZ&response_url=https://hooks.slack.com/commands/abc"
	sc, err := parseSlackCommand(body)
	if err != nil {
		t.Fatalf("parseSlackCommand: %v", err)
	}
	if sc.Command != "/prism" {
		t.Errorf("Command = %q, want /prism", sc.Command)
	}
	if sc.Text != "stop rstudio" {
		t.Errorf("Text = %q, want 'stop rstudio'", sc.Text)
	}
	if sc.UserID != "U04ABC" {
		t.Errorf("UserID = %q, want U04ABC", sc.UserID)
	}
	if sc.WorkspaceID != "T03XYZ" {
		t.Errorf("WorkspaceID = %q, want T03XYZ", sc.WorkspaceID)
	}
}

// ── Action allowed check ──────────────────────────────────────────────────────

func TestIsActionAllowed(t *testing.T) {
	reg := makeReg("test", "i-0abc", "")
	reg.AllowedActions = []string{"status", "stop"}

	if !isActionAllowed(&reg, "stop") {
		t.Error("stop should be allowed")
	}
	if !isActionAllowed(&reg, "status") {
		t.Error("status should be allowed")
	}
	if isActionAllowed(&reg, "start") {
		t.Error("start should not be allowed")
	}
	if isActionAllowed(&reg, "hibernate") {
		t.Error("hibernate should not be allowed")
	}
}
