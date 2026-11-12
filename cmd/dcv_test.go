package cmd

import (
	"strings"
	"testing"
)

func TestExtractReadyFromTags(t *testing.T) {
	t.Run("current spored: ready, token-free, FQDN host", func(t *testing.T) {
		tags := map[string]string{
			"spawn:ready-status": "ready",
			"spawn:ready-url":    "https://box.5k0zfnmq.spore.host:8443/#console",
			"spawn:ready-auth":   "token",
		}
		url, legacy, host, status, auth := extractReadyFromTags(tags)
		if status != "ready" || auth != "token" {
			t.Errorf("status = %q auth = %q", status, auth)
		}
		if legacy != "" {
			t.Errorf("legacy token = %q, want none from a current spored", legacy)
		}
		if host != "box.5k0zfnmq.spore.host" {
			t.Errorf("host = %q, want the FQDN", host)
		}
		if url == "" {
			t.Error("url should be set")
		}
	})

	// A current spored never publishes a token, so a token-shaped value next
	// to spawn:ready-auth (planted, or left over) must not be picked up.
	t.Run("current spored: a stray token tag is ignored", func(t *testing.T) {
		tags := map[string]string{
			"spawn:ready-status": "ready",
			"spawn:ready-url":    "https://box/?authToken=planted#console",
			"spawn:ready-token":  "planted",
			"spawn:ready-auth":   "token",
		}
		if _, legacy, _, _, _ := extractReadyFromTags(tags); legacy != "" {
			t.Errorf("legacy token = %q, want it ignored when spawn:ready-auth is set", legacy)
		}
	})

	t.Run("older spored: token from the tag", func(t *testing.T) {
		tags := map[string]string{
			"spawn:ready-status": "ready",
			"spawn:ready-url":    "https://box.5k0zfnmq.spore.host:8443/?authToken=deadbeef#console",
			"spawn:ready-token":  "deadbeef",
		}
		_, legacy, host, _, auth := extractReadyFromTags(tags)
		if legacy != "deadbeef" || auth != "" {
			t.Errorf("legacy = %q auth = %q, want deadbeef and none", legacy, auth)
		}
		if host != "box.5k0zfnmq.spore.host" {
			t.Errorf("host = %q", host)
		}
	})

	t.Run("older spored: web token from the url", func(t *testing.T) {
		tags := map[string]string{
			"spawn:ready-status": "ready",
			"spawn:ready-url":    "https://box.5k0zfnmq.spore.host/?spore_token=cafe",
		}
		if _, legacy, _, _, _ := extractReadyFromTags(tags); legacy != "cafe" {
			t.Errorf("legacy = %q, want cafe", legacy)
		}
	})

	t.Run("terminal failure: status only, no url/token", func(t *testing.T) {
		tags := map[string]string{"spawn:ready-status": "session-never-created"}
		url, legacy, host, status, _ := extractReadyFromTags(tags)
		if status != "session-never-created" {
			t.Errorf("status = %q", status)
		}
		if url != "" || legacy != "" || host != "" {
			t.Errorf("expected empty url/token/host, got url=%q token=%q host=%q", url, legacy, host)
		}
	})

	t.Run("empty tags", func(t *testing.T) {
		_, legacy, _, status, _ := extractReadyFromTags(map[string]string{})
		if legacy != "" || status != "" {
			t.Errorf("empty: token=%q status=%q", legacy, status)
		}
	})

	t.Run("web ready-url (no token, port-less host)", func(t *testing.T) {
		tags := map[string]string{
			"spawn:ready-status": "ready",
			"spawn:ready-url":    "https://box.5k0zfnmq.spore.host/",
			"spawn:ready-auth":   "none",
		}
		_, legacy, host, status, auth := extractReadyFromTags(tags)
		if status != "ready" || auth != "none" {
			t.Errorf("status = %q auth = %q", status, auth)
		}
		if legacy != "" {
			t.Errorf("web ready-url should have no token, got %q", legacy)
		}
		if host != "box.5k0zfnmq.spore.host" {
			t.Errorf("host = %q, want the FQDN (parsed up to the trailing slash)", host)
		}
	})
}

func TestDCVStatusTerminal(t *testing.T) {
	for _, s := range []string{dcvStatusNotInstalled, dcvStatusServerNotRunning, dcvStatusSessionNotCreated, dcvStatusTagWriteDenied, webStatusNotResponding, webStatusProxyFailed} {
		if !dcvStatusTerminal(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range []string{dcvStatusWaiting, dcvStatusReady, ""} {
		if dcvStatusTerminal(s) {
			t.Errorf("%q should NOT be terminal", s)
		}
	}
}

func TestDCVFailureMessage(t *testing.T) {
	// Each terminal status produces a distinct, non-generic message naming the layer.
	cases := map[string]string{
		dcvStatusNotInstalled:      "DCV server isn't installed",
		dcvStatusServerNotRunning:  "DCV server failed to start",
		dcvStatusSessionNotCreated: "session was never created",
		dcvStatusTagWriteDenied:    "couldn't write its ready tag",
	}
	for status, want := range cases {
		msg := dcvFailureMessage(status, "i-0abc")
		if !strings.Contains(msg, want) {
			t.Errorf("status %q: message %q does not contain %q", status, msg, want)
		}
	}
	// Waiting/empty fall back to a timeout message (not a named failure).
	if msg := dcvFailureMessage(dcvStatusWaiting, "i-0abc"); !strings.Contains(msg, "timed out") {
		t.Errorf("waiting should yield a timeout message, got %q", msg)
	}
	// The instance ID is surfaced for the inspect hint.
	if msg := dcvFailureMessage(dcvStatusServerNotRunning, "i-0abc"); !strings.Contains(msg, "i-0abc") {
		t.Errorf("message should include the instance id, got %q", msg)
	}
}
