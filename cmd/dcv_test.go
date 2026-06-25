package cmd

import (
	"strings"
	"testing"
)

func TestExtractReadyFromTags(t *testing.T) {
	t.Run("ready with token + FQDN host", func(t *testing.T) {
		tags := map[string]string{
			"spawn:ready-status": "ready",
			"spawn:ready-url":    "https://box.5k0zfnmq.spore.host:8443/?authToken=deadbeef#console",
			"spawn:ready-token":  "deadbeef",
		}
		url, token, host, status := extractReadyFromTags(tags)
		if status != "ready" {
			t.Errorf("status = %q", status)
		}
		if token != "deadbeef" {
			t.Errorf("token = %q, want deadbeef", token)
		}
		if host != "box.5k0zfnmq.spore.host" {
			t.Errorf("host = %q, want the FQDN", host)
		}
		if url == "" {
			t.Error("url should be set")
		}
	})

	t.Run("terminal failure: status only, no url/token", func(t *testing.T) {
		tags := map[string]string{"spawn:ready-status": "session-never-created"}
		url, token, host, status := extractReadyFromTags(tags)
		if status != "session-never-created" {
			t.Errorf("status = %q", status)
		}
		if url != "" || token != "" || host != "" {
			t.Errorf("expected empty url/token/host, got url=%q token=%q host=%q", url, token, host)
		}
	})

	t.Run("empty tags", func(t *testing.T) {
		_, token, _, status := extractReadyFromTags(map[string]string{})
		if token != "" || status != "" {
			t.Errorf("empty: token=%q status=%q", token, status)
		}
	})
}

func TestDCVStatusTerminal(t *testing.T) {
	for _, s := range []string{dcvStatusNotInstalled, dcvStatusServerNotRunning, dcvStatusSessionNotCreated, dcvStatusTagWriteDenied} {
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
		dcvStatusNotInstalled:      "no NICE DCV server",
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
