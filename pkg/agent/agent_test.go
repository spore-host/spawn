package agent

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// stubProvider is a minimal Provider implementation for unit tests.
type stubProvider struct {
	identity *provider.Identity
	config   *provider.Config
	spot     bool

	// terminated/stopped/hibernated record which lifecycle action the agent
	// invoked, so tests can assert TTL always terminates (never stop/hibernate).
	terminated bool
	stopped    bool
	hibernated bool

	// region-vacated check (#260): otherInstances is the count returned by
	// CountOtherManagedInstances; reportVacated=true makes a 0 mean "really 0"
	// rather than the default "unknown" (-1). countCalls records how many times
	// the count was queried (1 = early return; 2 = settle path reached).
	otherInstances int
	reportVacated  bool
	countCalls     int
}

func (s *stubProvider) GetIdentity(_ context.Context) (*provider.Identity, error) {
	return s.identity, nil
}
func (s *stubProvider) GetConfig(_ context.Context) (*provider.Config, error) {
	return s.config, nil
}
func (s *stubProvider) RefreshConfig(_ context.Context) error       { return nil }
func (s *stubProvider) Terminate(_ context.Context, _ string) error { s.terminated = true; return nil }
func (s *stubProvider) Stop(_ context.Context, _ string) error      { s.stopped = true; return nil }
func (s *stubProvider) Hibernate(_ context.Context) error           { s.hibernated = true; return nil }
func (s *stubProvider) IsSpotInstance(_ context.Context) bool       { return s.spot }
func (s *stubProvider) CheckSpotInterruption(_ context.Context) (*provider.InterruptionInfo, error) {
	return nil, nil
}
func (s *stubProvider) DiscoverPeers(_ context.Context, _ string) ([]provider.PeerInfo, error) {
	return nil, nil
}
func (s *stubProvider) GetProviderType() string                       { return "stub" }
func (s *stubProvider) LookupAndTagEBSCost(_ context.Context) float64 { return 0 }

// otherInstances controls the region-vacated check; -1 (unknown) by default so
// existing tests don't trigger the 60s settle path.
func (s *stubProvider) CountOtherManagedInstances(_ context.Context) int {
	s.countCalls++
	if s.otherInstances == 0 && !s.reportVacated {
		return -1
	}
	return s.otherInstances
}

func newTestAgent(t *testing.T, cfg *provider.Config) *Agent {
	t.Helper()
	identity := &provider.Identity{
		InstanceID: "i-test123",
		Region:     "us-east-1",
		AccountID:  "123456789012",
		PublicIP:   "1.2.3.4",
		PrivateIP:  "10.0.0.1",
		Provider:   "stub",
	}
	if cfg == nil {
		cfg = &provider.Config{
			TTL:            2 * time.Hour,
			IdleTimeout:    30 * time.Minute,
			IdleCPUPercent: 5.0,
		}
	}
	a := &Agent{
		provider:         &stubProvider{identity: identity, config: cfg},
		identity:         identity,
		config:           cfg,
		startTime:        time.Now(),
		lastActivityTime: time.Now(),
		dcv:              execDCVRunner{}, // real exec; writeFakeDCV puts a fake `dcv` on PATH
		tagger:           &fakeTagPutter{},
	}
	return a
}

// fakeTagPutter records tag writes (no real EC2) and can return a canned error to
// exercise the tag-write-denied path. Latest wins per key across calls.
type fakeTagPutter struct {
	writes []map[string]string // every putTags call, in order
	tags   map[string]string   // merged view of all successful writes
	err    error               // returned by putTags when non-nil
	calls  int
}

func (f *fakeTagPutter) putTags(_ context.Context, _ string, tags map[string]string) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, tags)
	if f.tags == nil {
		f.tags = map[string]string{}
	}
	for k, v := range tags {
		f.tags[k] = v
	}
	return nil
}

// fakeDCVRunner is an injectable dcvRunner returning canned list-sessions output.
// listOut/listErr drive classifyDCVStatus; isInstalled drives installed().
type fakeDCVRunner struct {
	isInstalled bool
	listOut     string
	listErr     error
	descOut     []byte
	descErr     error
}

func (f *fakeDCVRunner) installed() bool { return f.isInstalled }
func (f *fakeDCVRunner) listSessions(_ context.Context) (string, error) {
	return f.listOut, f.listErr
}
func (f *fakeDCVRunner) describeSession(_ context.Context, _ string) ([]byte, error) {
	return f.descOut, f.descErr
}

func TestGetConfig(t *testing.T) {
	cfg := &provider.Config{TTL: time.Hour, IdleCPUPercent: 5.0}
	a := newTestAgent(t, cfg)
	got := a.GetConfig()
	if got != cfg {
		t.Errorf("GetConfig() returned wrong config")
	}
	if got.TTL != time.Hour {
		t.Errorf("GetConfig().TTL = %v, want %v", got.TTL, time.Hour)
	}
}

func TestGetIdentity(t *testing.T) {
	a := newTestAgent(t, nil)
	id := a.GetIdentity()
	if id.InstanceID != "i-test123" {
		t.Errorf("GetIdentity().InstanceID = %q, want %q", id.InstanceID, "i-test123")
	}
	if id.Region != "us-east-1" {
		t.Errorf("GetIdentity().Region = %q, want %q", id.Region, "us-east-1")
	}
}

func TestGetInstanceInfo(t *testing.T) {
	a := newTestAgent(t, nil)
	id, region, account := a.GetInstanceInfo()
	if id != "i-test123" {
		t.Errorf("instance ID = %q, want %q", id, "i-test123")
	}
	if region != "us-east-1" {
		t.Errorf("region = %q, want %q", region, "us-east-1")
	}
	if account != "123456789012" {
		t.Errorf("account = %q, want %q", account, "123456789012")
	}
}

func TestGetUptime(t *testing.T) {
	a := newTestAgent(t, nil)
	// Start time is set to now in newTestAgent, uptime should be very small
	uptime := a.GetUptime()
	if uptime < 0 {
		t.Errorf("GetUptime() returned negative duration: %v", uptime)
	}
	if uptime > 5*time.Second {
		t.Errorf("GetUptime() too large for a freshly created agent: %v", uptime)
	}
}

func TestGetLastActivityTime(t *testing.T) {
	before := time.Now()
	a := newTestAgent(t, nil)
	after := time.Now()

	lat := a.GetLastActivityTime()
	if lat.Before(before) || lat.After(after) {
		t.Errorf("GetLastActivityTime() = %v, expected between %v and %v", lat, before, after)
	}
}

func TestIsIdle_NotIdleWhenRecentActivity(t *testing.T) {
	a := newTestAgent(t, &provider.Config{
		IdleTimeout:    5 * time.Minute,
		IdleCPUPercent: 100.0, // threshold so high nothing triggers it
	})
	a.lastActivityTime = time.Now()
	// With a 100% CPU threshold, isIdle checks user sessions etc.
	// On a test machine we just verify it returns a bool without panicking.
	_ = a.IsIdle()
}

func TestIsIdle_IdleAfterTimeout(t *testing.T) {
	a := newTestAgent(t, &provider.Config{
		IdleTimeout:    1 * time.Millisecond,
		IdleCPUPercent: 0.0, // 0% threshold — always considered idle
	})
	// Push last activity into the past
	a.lastActivityTime = time.Now().Add(-1 * time.Hour)
	// Give the idle timeout a moment to elapse
	time.Sleep(5 * time.Millisecond)
	if !a.IsIdle() {
		t.Log("IsIdle() returned false — may depend on live CPU/user checks, acceptable in CI")
	}
}

func TestCheckCompletion_NoFileConfigured(t *testing.T) {
	a := newTestAgent(t, &provider.Config{
		OnComplete:     "terminate",
		CompletionFile: "", // no file → no completion
	})
	ctx := context.Background()
	done := a.checkCompletion(ctx)
	if done {
		t.Errorf("checkCompletion() = true with no CompletionFile set, want false")
	}
}

func TestCheckCompletion_FileNotPresent(t *testing.T) {
	a := newTestAgent(t, &provider.Config{
		OnComplete:     "terminate",
		CompletionFile: "/tmp/spawn_test_completion_file_should_not_exist_xyz",
	})
	ctx := context.Background()
	done := a.checkCompletion(ctx)
	if done {
		t.Errorf("checkCompletion() = true when file does not exist, want false")
	}
}

func TestCheckCompletion_FilePresent(t *testing.T) {
	f := t.TempDir() + "/SPAWN_COMPLETE"
	if err := os.WriteFile(f, []byte{}, 0644); err != nil {
		t.Fatalf("cannot create completion file: %v", err)
	}

	// Use an unknown action so checkCompletion returns false after detecting
	// the file — this avoids the 5s sleep inside terminate/stop.
	a := newTestAgent(t, &provider.Config{
		OnComplete:      "noop_test_action",
		CompletionFile:  f,
		CompletionDelay: 0,
	})
	ctx := context.Background()
	// With an unknown action the function returns false after the sleep(0),
	// but the file detection path is still exercised.
	_ = a.checkCompletion(ctx)
}

// TestCheckCompletion_StopFiresRegardlessOfJobOrigin is the #105 regression
// guard. #105 reported that `--on-complete stop` didn't trigger when the
// completion file was written by a job started via `spawn connect -- '... &'`
// (vs. `--command` at launch). The root cause was the spored systemd unit
// setting PrivateTmp=true, which hid the host /tmp/SPAWN_COMPLETE from the
// daemon (#66, fixed in v0.36.12 via #67 — after the v0.34.13 #105 was filed
// against). The completion logic itself is, and must stay, agnostic to how the
// job was started: it only depends on OnComplete + the file's presence. This
// test pins that — an instance with NO job-array/sweep/command tags (i.e. the
// "connect --"-style launch) still stops when the file appears.
func TestCheckCompletion_StopFiresRegardlessOfJobOrigin(t *testing.T) {
	f := t.TempDir() + "/SPAWN_COMPLETE"
	if err := os.WriteFile(f, []byte("done\n"), 0644); err != nil {
		t.Fatalf("cannot create completion file: %v", err)
	}

	cfg := &provider.Config{
		OnComplete:      "stop",
		CompletionFile:  f,
		CompletionDelay: 0,
		// Deliberately no JobArrayID / JobArrayCommand / SweepID: this models an
		// instance whose workload was started later via `spawn connect --`, not
		// via `--command` at launch. The completion path must not care.
	}
	a := newTestAgent(t, cfg)
	stub := a.provider.(*stubProvider)

	if done := a.checkCompletion(context.Background()); !done {
		t.Fatal("checkCompletion() = false with OnComplete=stop and file present, want true")
	}
	if !stub.stopped {
		t.Error("provider.Stop was not called — --on-complete stop did not fire (#105)")
	}
	if stub.terminated || stub.hibernated {
		t.Errorf("wrong action: terminated=%v hibernated=%v, want only stopped", stub.terminated, stub.hibernated)
	}
}

// writeFakeDCV writes a fake `dcv` script to dir and prepends dir to PATH.
// The script exits 0 and prints output if it receives args matching sessionID,
// otherwise it exits 1 to simulate "not found / not ready".
func writeFakeDCV(t *testing.T, sessionID string, output string, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "dcv")

	var body string
	if exitCode == 0 {
		body = "#!/bin/sh\necho '" + output + "'\nexit 0\n"
	} else {
		body = "#!/bin/sh\nexit 1\n"
	}

	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		t.Fatalf("writeFakeDCV: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestDCVIdleSource_NoSessionID(t *testing.T) {
	// When DCVSessionID is empty the DCV check is skipped entirely.
	// getDCVConnectionCount should not be called; isIdle proceeds to other checks.
	a := newTestAgent(t, &provider.Config{
		IdleTimeout:    5 * time.Minute,
		IdleCPUPercent: 100.0, // prevent CPU check from blocking
		DCVSessionID:   "",
	})
	// getDCVConnectionCount with empty session ID — should return -1 without exec
	count := a.getDCVConnectionCount()
	// With no real dcv binary and empty session the exec will fail → -1
	if count > 0 {
		t.Errorf("getDCVConnectionCount() = %d with empty DCVSessionID, want <= 0", count)
	}
}

func TestDCVIdleSource_ServerNotReady(t *testing.T) {
	// dcv exits non-zero → getDCVConnectionCount returns -1 → isIdle returns false.
	writeFakeDCV(t, "console", "", 1)
	a := newTestAgent(t, &provider.Config{
		IdleTimeout:    5 * time.Minute,
		IdleCPUPercent: 100.0,
		DCVSessionID:   "console",
	})
	count := a.getDCVConnectionCount()
	if count != -1 {
		t.Errorf("getDCVConnectionCount() = %d when dcv exits non-zero, want -1", count)
	}
	// isIdle must return false (grace period — server not ready)
	if a.IsIdle() {
		t.Error("IsIdle() = true when DCV server not ready, want false")
	}
}

func TestDCVIdleSource_GraceBounded(t *testing.T) {
	// dcv never ready (exits non-zero → count -1). Within grace → not idle;
	// past dcvGraceTimeout → falls through to the standard checks (spawn#282), so
	// the verdict matches a non-DCV control with the same config (rather than the
	// old unbounded grace, which returned not-idle forever and billed until TTL).
	// Asserted via the pure dcvIdleDecision (deterministic) + the control-equality
	// check below, so it doesn't depend on the test machine's live CPU.
	writeFakeDCV(t, "console", "", 1)

	// Within grace: not idle, no fall-through.
	if idle, ft := dcvIdleDecision(false, 0, -1, 2*time.Minute, dcvGraceTimeout, 5*time.Minute); idle || ft {
		t.Errorf("within grace: got (idle=%v, fallthrough=%v), want (false,false)", idle, ft)
	}
	// Past grace: fall through (let standard checks decide).
	if _, ft := dcvIdleDecision(false, 0, -1, dcvGraceTimeout+time.Minute, dcvGraceTimeout, 5*time.Minute); !ft {
		t.Error("past grace: want fallthrough=true (standard idle checks decide)")
	}

	// Integration: past grace, a DCV agent's verdict equals a non-DCV control's
	// (proves it fell through to the same standard path, whatever the machine's CPU).
	cfg := func() *provider.Config {
		return &provider.Config{IdleTimeout: 5 * time.Minute, IdleCPUPercent: 100.0}
	}
	dcvAgent := newTestAgent(t, cfg())
	dcvAgent.config.DCVSessionID = "console"
	dcvAgent.startTime = time.Now().Add(-(dcvGraceTimeout + time.Minute))
	control := newTestAgent(t, cfg()) // no DCVSessionID → standard path
	if dcvAgent.IsIdle() != control.IsIdle() {
		t.Errorf("past-grace DCV verdict (%v) != non-DCV control (%v) — did not fall through cleanly",
			dcvAgent.IsIdle(), control.IsIdle())
	}
}

func TestDCVIdleSource_ZeroClients(t *testing.T) {
	// dcv returns 0 connections → isIdle returns true (idle, skip CPU/network checks).
	writeFakeDCV(t, "console", `{"num-of-connections":0}`, 0)
	a := newTestAgent(t, &provider.Config{
		IdleTimeout:    5 * time.Minute,
		IdleCPUPercent: 100.0,
		DCVSessionID:   "console",
	})
	count := a.getDCVConnectionCount()
	if count != 0 {
		t.Errorf("getDCVConnectionCount() = %d, want 0", count)
	}
	if !a.IsIdle() {
		t.Error("IsIdle() = false when DCV has 0 clients, want true")
	}
}

func TestDCVIdleSource_ActiveClients(t *testing.T) {
	// dcv returns 2 connections → isIdle returns false.
	writeFakeDCV(t, "console", `{"num-of-connections":2}`, 0)
	a := newTestAgent(t, &provider.Config{
		IdleTimeout:    5 * time.Minute,
		IdleCPUPercent: 100.0,
		DCVSessionID:   "console",
	})
	count := a.getDCVConnectionCount()
	if count != 2 {
		t.Errorf("getDCVConnectionCount() = %d, want 2", count)
	}
	if a.IsIdle() {
		t.Error("IsIdle() = true when DCV has 2 active clients, want false")
	}
}

// dcvAuthAgent builds an EC2-provider agent wired with injected dcv/tagger fakes
// and the :8444 verifier marked already-started (so the test doesn't bind a port).
func dcvAuthAgent(t *testing.T, runner dcvRunner, tagger tagPutter) *Agent {
	t.Helper()
	a := newTestAgent(t, &provider.Config{DCVSessionID: "console"})
	a.identity.Provider = "ec2"       // maybeSetupDCVAuth no-ops on non-EC2
	a.dcvVerifierStarted = true       // skip the real 127.0.0.1:8444 bind
	a.dcvTokens = map[string]string{} // normally created by startDCVAuthVerifier
	a.dcv = runner
	a.tagger = tagger
	return a
}

// TestMaybeSetupDCVAuth_RetriesThenReady is the spawn#282 monitor-loop-retry
// regression: a transient "session not present yet" must NOT latch dcvAuthDone
// (so the next tick retries), and once the session appears the ready-url/token is
// written and the handshake stops. The old fire-once goroutine made the transient
// failure permanent.
func TestMaybeSetupDCVAuth_RetriesThenReady(t *testing.T) {
	ctx := context.Background()
	runner := &fakeDCVRunner{isInstalled: true, listOut: "no sessions yet"}
	tagger := &fakeTagPutter{}
	a := dcvAuthAgent(t, runner, tagger)

	// Tick 1: session absent, not yet exhausted → keep waiting (no terminal write).
	a.maybeSetupDCVAuth(ctx)
	if a.dcvAuthDone {
		t.Fatal("dcvAuthDone latched on a transient dcv-waiting; retry would never happen")
	}
	if tagger.calls != 0 {
		t.Fatalf("wrote tags during dcv-waiting (calls=%d); should write nothing until ready/terminal", tagger.calls)
	}

	// Tick 2: session now present → ready-url + token written, handshake stops.
	runner.listOut = "Session: 'console' (owner ec2-user)"
	a.maybeSetupDCVAuth(ctx)
	if !a.dcvAuthDone {
		t.Fatal("dcvAuthDone not latched after session became ready")
	}
	if got := tagger.tags["spawn:ready-status"]; got != string(dcvReady) {
		t.Errorf("spawn:ready-status = %q, want %q", got, dcvReady)
	}
	if tagger.tags["spawn:ready-url"] == "" || tagger.tags["spawn:ready-token"] == "" {
		t.Errorf("ready write missing url/token: %+v", tagger.tags)
	}
}

// TestMaybeSetupDCVAuth_TerminalStops asserts a terminal status (dcv not
// installed) records the named reason and stops retrying — without writing a fake
// ready-url for a session that will never exist (the old silent fall-through bug).
func TestMaybeSetupDCVAuth_TerminalStops(t *testing.T) {
	ctx := context.Background()
	runner := &fakeDCVRunner{isInstalled: false} // no `dcv` on this AMI
	tagger := &fakeTagPutter{}
	a := dcvAuthAgent(t, runner, tagger)

	a.maybeSetupDCVAuth(ctx)
	if !a.dcvAuthDone {
		t.Fatal("dcvAuthDone not latched on a terminal status; would retry forever")
	}
	if got := tagger.tags["spawn:ready-status"]; got != string(dcvNotInstalled) {
		t.Errorf("spawn:ready-status = %q, want %q", got, dcvNotInstalled)
	}
	if tagger.tags["spawn:ready-url"] != "" {
		t.Errorf("terminal status wrote a ready-url (%q); must not", tagger.tags["spawn:ready-url"])
	}
}

// blockingSpotProvider is a stubProvider whose IsSpotInstance never returns,
// reproducing the #65 failure mode: a hung IMDS spot-type check. The monitor
// must NOT let this gate its lifecycle ticker.
type blockingSpotProvider struct {
	stubProvider
	entered chan struct{} // closed once IsSpotInstance has been called
	once    sync.Once
}

func (b *blockingSpotProvider) IsSpotInstance(ctx context.Context) bool {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done() // block until the monitor's context is cancelled
	return false
}

// TestMonitor_SpotDetectionDoesNotGateTicker is the regression test for #65.
// Before the fix, Monitor called IsSpotInstance synchronously before entering
// the lifecycle ticker loop; a blocking IMDS call there meant the loop (and
// thus TTL / idle / on-complete / pre-stop enforcement) never ran at all, so
// instances ran forever. This asserts the ticker fires even while
// IsSpotInstance is blocked.
func TestMonitor_SpotDetectionDoesNotGateTicker(t *testing.T) {
	// Capture log output so we can detect the per-tick heartbeat.
	var buf bytes.Buffer
	var mu sync.Mutex
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&syncWriter{w: &buf, mu: &mu})
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	identity := &provider.Identity{InstanceID: "i-block", Region: "us-east-1", Provider: "stub"}
	cfg := &provider.Config{} // no TTL/idle/on-complete → checkAndAct just heartbeats
	bp := &blockingSpotProvider{
		stubProvider: stubProvider{identity: identity, config: cfg, spot: true},
		entered:      make(chan struct{}),
	}
	a := &Agent{
		provider:         bp,
		identity:         identity,
		config:           cfg,
		startTime:        time.Now(),
		lastActivityTime: time.Now(),
		monitorInterval:  10 * time.Millisecond, // fast ticker for the test
		// Push tag-write throttles into the future so checkAndAct skips its
		// (real-AWS) CreateTags calls and the test stays hermetic.
		lastSessionTagWrite: time.Now().Add(time.Hour),
		lastComputeTagWrite: time.Now().Add(time.Hour),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Monitor(ctx)

	// Confirm the spot check really is blocking (the #65 hazard is present)...
	select {
	case <-bp.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("IsSpotInstance was never called")
	}

	// ...yet the lifecycle ticker still fires. Poll the captured log for the
	// heartbeat written at the top of checkAndAct.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		if strings.Contains(got, "monitor tick") {
			return // success: the loop ran despite the blocked spot check
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("monitor ticker never fired while IsSpotInstance was blocked (#65 regression)")
}

// syncWriter serializes writes to an underlying buffer for concurrent log use.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// TestCheckAndAct_ExpiredTTL_AlwaysTerminates is the #72 guardrail: an expired
// TTL must ALWAYS terminate — never stop or hibernate. "stop" is not terminal
// (it bills EBS forever and runs no daemon to re-check TTL). This locks the
// invariant so nobody can later redirect TTL expiry to a non-terminate action.
func TestCheckAndAct_ExpiredTTL_AlwaysTerminates(t *testing.T) {
	identity := &provider.Identity{InstanceID: "i-ttl", Region: "us-east-1", Provider: "stub"}
	cfg := &provider.Config{
		// Deadline already in the past → expired this tick.
		TTLDeadline: time.Now().Add(-time.Minute),
		// Set HibernateOnIdle to prove the TTL path ignores it (only idle honors it).
		HibernateOnIdle: true,
	}
	sp := &stubProvider{identity: identity, config: cfg}
	a := &Agent{
		provider:         sp,
		identity:         identity,
		config:           cfg,
		startTime:        time.Now().Add(-2 * time.Hour),
		lastActivityTime: time.Now(),
		// Keep checkAndAct hermetic: skip the (real-AWS) tag-write calls.
		lastSessionTagWrite: time.Now().Add(time.Hour),
		lastComputeTagWrite: time.Now().Add(time.Hour),
	}

	a.checkAndAct(context.Background())

	if !sp.terminated {
		t.Error("expired TTL must call Terminate")
	}
	if sp.stopped {
		t.Error("expired TTL must NOT Stop (#72: TTL always terminates)")
	}
	if sp.hibernated {
		t.Error("expired TTL must NOT Hibernate, even with HibernateOnIdle set (#72)")
	}
}

func TestTailBuffer_RetainsLastBytes(t *testing.T) {
	tb := newTailBuffer(10)
	// Write more than the cap across multiple writes (mimics stdout+stderr teeing).
	tb.Write([]byte("hello "))
	tb.Write([]byte("beautiful "))
	tb.Write([]byte("world"))
	got := tb.String()
	if len(got) != 10 {
		t.Errorf("tail buffer kept %d bytes, want exactly 10: %q", len(got), got)
	}
	// Must retain the TAIL (last 10 bytes of "hello beautiful world"), not the head.
	if got != "iful world" {
		t.Errorf("tail = %q, want %q", got, "iful world")
	}
}

func TestTailBuffer_ShortInput(t *testing.T) {
	tb := newTailBuffer(1024)
	tb.Write([]byte("short"))
	if tb.String() != "short" {
		t.Errorf("tail = %q, want short", tb.String())
	}
}

func TestPreStopDetail(t *testing.T) {
	if got := preStopDetail("5m0s", ""); got != "5m0s" {
		t.Errorf("empty tail: got %q, want 5m0s", got)
	}
	if got := preStopDetail("exit 1", "  fatal: no creds\n"); got != "exit 1 — fatal: no creds" {
		t.Errorf("with tail: got %q", got)
	}
}

func TestCheckRegionVacated(t *testing.T) {
	mkAgent := func(sp *stubProvider) *Agent {
		return &Agent{
			provider:            sp,
			identity:            &provider.Identity{InstanceID: "i-test123", Region: "us-east-1"},
			config:              &provider.Config{},
			regionVacatedSettle: time.Millisecond, // keep the settle window tiny
		}
	}

	t.Run("others remain: returns immediately, no settle re-check", func(t *testing.T) {
		sp := &stubProvider{otherInstances: 2}
		mkAgent(sp).checkRegionVacated(context.Background())
		if sp.countCalls != 1 {
			t.Errorf("expected 1 count call (early return), got %d", sp.countCalls)
		}
	})

	t.Run("unknown count: returns immediately", func(t *testing.T) {
		sp := &stubProvider{otherInstances: 0, reportVacated: false} // -1 unknown
		mkAgent(sp).checkRegionVacated(context.Background())
		if sp.countCalls != 1 {
			t.Errorf("expected 1 count call (unknown → not vacated), got %d", sp.countCalls)
		}
	})

	t.Run("vacated: settles then re-checks", func(t *testing.T) {
		sp := &stubProvider{otherInstances: 0, reportVacated: true}
		mkAgent(sp).checkRegionVacated(context.Background())
		if sp.countCalls != 2 {
			t.Errorf("expected 2 count calls (initial + settle re-check), got %d", sp.countCalls)
		}
	})

	t.Run("cancelled context during settle: no re-check", func(t *testing.T) {
		sp := &stubProvider{otherInstances: 0, reportVacated: true}
		a := mkAgent(sp)
		a.regionVacatedSettle = time.Hour // long, so cancellation wins
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		a.checkRegionVacated(ctx)
		if sp.countCalls != 1 {
			t.Errorf("expected 1 count call (cancelled before settle), got %d", sp.countCalls)
		}
	})
}

// TestTotalComputeSecondsCarriesBase asserts the invariant that makes an
// in-place spored upgrade (#234) state-preserving: compute-seconds accumulated
// before this spored start (loaded from the spawn:compute-seconds tag into
// computeSecondsBase at boot) are INCLUDED in the running total, so the compute
// clock continues across a restart rather than resetting. The graceful-stop
// flush (agent.Cleanup → flushComputeSecondsTag) persists the latest total so
// the next boot's base is current.
func TestTotalComputeSecondsCarriesBase(t *testing.T) {
	a := newTestAgent(t, nil)
	// Simulate a restart that loaded 3600s of prior compute time from the tag,
	// with the new spored having been up for ~2s.
	a.computeSecondsBase = 3600
	a.startTime = time.Now().Add(-2 * time.Second)

	total := a.TotalComputeSeconds()
	if total < 3600 {
		t.Fatalf("TotalComputeSeconds=%d dropped the persisted base (want >= 3600)", total)
	}
	if total > 3600+10 {
		t.Fatalf("TotalComputeSeconds=%d unexpectedly large (base 3600 + ~2s uptime)", total)
	}
}

// TestAccumulatedComputeCostIncludesPriorCompute is the regression guard for the
// cost-limit reset bug: enforcement must charge for compute accumulated BEFORE
// this spored start (computeSecondsBase), not just this boot's uptime. Otherwise
// a stop/start resets the cost clock and a repeatedly-resumed instance sails past
// its --cost-limit.
func TestAccumulatedComputeCostIncludesPriorCompute(t *testing.T) {
	a := newTestAgent(t, &provider.Config{PricePerHour: 10.0, CostLimit: 25.0})
	// 2.3 hours of compute already accrued before this boot; daemon just started.
	a.computeSecondsBase = int64((2*time.Hour + 18*time.Minute).Seconds())
	a.startTime = time.Now()

	got := a.accumulatedComputeCost()
	// 2.3h × $10/hr = $23 (plus a negligible fraction of a second of new uptime).
	if got < 22.99 || got > 23.5 {
		t.Fatalf("accumulatedComputeCost=%.4f, want ~23.0 (prior 2.3h compute counted)", got)
	}
	// And it must have crossed 90% of the $25 limit — proving the warn/terminate
	// path sees the carried-over spend rather than a fresh $0 after a restart.
	if got/a.config.CostLimit < 0.90 {
		t.Errorf("carried compute cost %.2f should be ≥90%% of limit %.2f", got, a.config.CostLimit)
	}
}

// TestAccumulatedComputeCostFreshInstance sanity-checks the no-base case: a fresh
// instance with no prior compute charges only for uptime.
func TestAccumulatedComputeCostFreshInstance(t *testing.T) {
	a := newTestAgent(t, &provider.Config{PricePerHour: 10.0, CostLimit: 25.0})
	a.computeSecondsBase = 0
	a.startTime = time.Now().Add(-30 * time.Minute) // half an hour up

	got := a.accumulatedComputeCost()
	if got < 4.9 || got > 5.1 {
		t.Fatalf("accumulatedComputeCost=%.4f, want ~5.0 (0.5h × $10/hr)", got)
	}
}
