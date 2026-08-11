package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// interruptInfo is a fixed sample notice used across the webhook tests.
func interruptInfo() *provider.InterruptionInfo {
	return &provider.InterruptionInfo{
		Action: "terminate",
		Time:   time.Date(2026, 6, 21, 18, 42, 0, 0, time.UTC),
	}
}

// TestEmitSpotInterruptionWebhook_PayloadAndEcho verifies the happy path: with a
// configured URL spored POSTs the fixed fact-struct, echoes WebhookCorrelation
// verbatim, carries the AWS action + on-node facts, and does NOT include a
// client_token (issue #228 — the node can't see it).
func TestEmitSpotInterruptionWebhook_PayloadAndEcho(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		close(done)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAgent(t, &provider.Config{
		SpotWebhookURL:     srv.URL,
		WebhookCorrelation: "consumer-opaque-blob-42",
		WebhookTimeout:     2 * time.Second,
	})
	a.identity.AvailabilityZone = "us-east-1a"
	a.identity.Name = "mpi-node-0"

	a.emitSpotInterruptionWebhook(interruptInfo())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook endpoint never received the POST")
	}

	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	var p map[string]any
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("payload not valid JSON: %v\n%s", err, gotBody)
	}
	if p["event"] != "spot_interruption" {
		t.Errorf("event = %v, want spot_interruption", p["event"])
	}
	if p["action"] != "terminate" {
		t.Errorf("action = %v, want terminate (AWS verbatim)", p["action"])
	}
	if p["correlation"] != "consumer-opaque-blob-42" {
		t.Errorf("correlation = %v, want the verbatim echo", p["correlation"])
	}
	if p["instance_id"] != "i-test123" {
		t.Errorf("instance_id = %v", p["instance_id"])
	}
	if p["az"] != "us-east-1a" {
		t.Errorf("az = %v, want us-east-1a", p["az"])
	}
	if p["name_tag"] != "mpi-node-0" {
		t.Errorf("name_tag = %v", p["name_tag"])
	}
	if p["interruption_deadline"] != "2026-06-21T18:42:00Z" {
		t.Errorf("interruption_deadline = %v", p["interruption_deadline"])
	}
	// The node cannot see the RunInstances ClientToken (#228): it must NOT appear.
	if _, present := p["client_token"]; present {
		t.Error("payload must not contain client_token — it is not on-node knowledge (#228)")
	}
}

// TestEmitSpotInterruptionWebhook_DisabledWhenNoURL verifies opt-in: an empty
// URL means today's behavior — nothing is sent.
func TestEmitSpotInterruptionWebhook_DisabledWhenNoURL(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAgent(t, &provider.Config{}) // no SpotWebhookURL
	a.emitSpotInterruptionWebhook(interruptInfo())

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("webhook fired %d times with no URL configured; want 0", got)
	}
}

// TestEmitSpotInterruptionWebhook_BestEffortDrop verifies a dead/erroring
// endpoint never panics or blocks the caller — failure is silently dropped.
func TestEmitSpotInterruptionWebhook_BestEffortDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 5xx → dropped
	}))
	defer srv.Close()

	a := newTestAgent(t, &provider.Config{SpotWebhookURL: srv.URL, WebhookTimeout: time.Second})
	// Must simply return; a panic or hang fails the test.
	a.emitSpotInterruptionWebhook(interruptInfo())
}

// TestSpotWebhook_FiresOnce verifies the fire-once guard the handler applies: the
// spot monitor re-enters every 5s until the node dies, but the webhook must POST
// exactly once. This mirrors the guarded call in checkSpotInterruption without
// driving its heavyweight Cleanup/pre-stop side effects.
func TestSpotWebhook_FiresOnce(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAgent(t, &provider.Config{SpotWebhookURL: srv.URL, WebhookTimeout: 2 * time.Second})
	info := interruptInfo()

	// Three monitor re-entries, each applying the same guard the handler uses.
	for i := 0; i < 3; i++ {
		if !a.spotWebhookFired {
			a.spotWebhookFired = true
			a.emitSpotInterruptionWebhook(info)
		}
	}

	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("webhook POSTed %d times across 3 entries; want exactly 1 (fire-once)", got)
	}
}

// TestEmitCompletionWebhook_PayloadAndEcho is the spawn#497 counterpart to
// TestEmitSpotInterruptionWebhook_PayloadAndEcho: with a configured URL spored
// POSTs the fixed completion fact-struct, echoing WebhookCorrelation verbatim
// and carrying the configured OnComplete action.
func TestEmitCompletionWebhook_PayloadAndEcho(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		close(done)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAgent(t, &provider.Config{
		CompletionWebhookURL: srv.URL,
		WebhookCorrelation:   "calque-run-42",
		WebhookTimeout:       2 * time.Second,
		OnComplete:           "terminate",
	})
	a.identity.Name = "vllm-node-0"

	a.emitCompletionWebhook(context.Background())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("completion webhook endpoint never received the POST")
	}

	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	var p map[string]any
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("payload not valid JSON: %v\n%s", err, gotBody)
	}
	if p["event"] != "completion" {
		t.Errorf("event = %v, want completion", p["event"])
	}
	if p["on_complete"] != "terminate" {
		t.Errorf("on_complete = %v, want terminate", p["on_complete"])
	}
	if p["correlation"] != "calque-run-42" {
		t.Errorf("correlation = %v, want the verbatim echo", p["correlation"])
	}
	if p["instance_id"] != "i-test123" {
		t.Errorf("instance_id = %v", p["instance_id"])
	}
	if p["name_tag"] != "vllm-node-0" {
		t.Errorf("name_tag = %v", p["name_tag"])
	}
}

// TestEmitCompletionWebhook_DisabledWhenNoURL verifies opt-in: an empty URL
// means today's behavior — nothing is sent.
func TestEmitCompletionWebhook_DisabledWhenNoURL(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := newTestAgent(t, &provider.Config{}) // no CompletionWebhookURL
	a.emitCompletionWebhook(context.Background())

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("completion webhook fired %d times with no URL configured; want 0", got)
	}
}

// TestEmitCompletionWebhook_BestEffortDrop verifies a dead/erroring endpoint
// never panics or blocks the caller — failure is silently dropped, mirroring
// the spot webhook's best-effort discipline.
func TestEmitCompletionWebhook_BestEffortDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 5xx → dropped
	}))
	defer srv.Close()

	a := newTestAgent(t, &provider.Config{CompletionWebhookURL: srv.URL, WebhookTimeout: time.Second})
	// Must simply return; a panic or hang fails the test.
	a.emitCompletionWebhook(context.Background())
}

// TestCheckCompletion_FiresCompletionWebhook verifies checkCompletion actually
// wires emitCompletionWebhook into the completion path (not just that the
// function works in isolation) — the end-to-end guard for spawn#497.
func TestCheckCompletion_FiresCompletionWebhook(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := t.TempDir() + "/SPAWN_COMPLETE"
	if err := os.WriteFile(f, []byte{}, 0644); err != nil {
		t.Fatalf("cannot create completion file: %v", err)
	}

	a := newTestAgent(t, &provider.Config{
		OnComplete:           "noop_test_action", // avoids the terminate/stop sleep
		CompletionFile:       f,
		CompletionDelay:      0,
		CompletionWebhookURL: srv.URL,
		WebhookTimeout:       2 * time.Second,
	})
	_ = a.checkCompletion(context.Background())

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("completion webhook POSTed %d times via checkCompletion; want exactly 1", got)
	}
}
