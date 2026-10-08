package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/provider"
)

// stubStatusProvider is a minimal provider.Provider for exercising
// buildStatusReport/renderStatusJSON/renderStatusTable without EC2 or a local
// config file. Mirrors pkg/agent's own unexported stubProvider, which cmd/spored
// can't reach directly (different package).
type stubStatusProvider struct {
	identity *provider.Identity
	config   *provider.Config
}

func (s *stubStatusProvider) GetIdentity(_ context.Context) (*provider.Identity, error) {
	return s.identity, nil
}
func (s *stubStatusProvider) GetConfig(_ context.Context) (*provider.Config, error) {
	return s.config, nil
}
func (s *stubStatusProvider) RefreshConfig(_ context.Context) error       { return nil }
func (s *stubStatusProvider) Terminate(_ context.Context, _ string) error { return nil }
func (s *stubStatusProvider) Stop(_ context.Context, _ string) error      { return nil }
func (s *stubStatusProvider) Hibernate(_ context.Context) error           { return nil }
func (s *stubStatusProvider) IsSpotInstance(_ context.Context) bool       { return false }
func (s *stubStatusProvider) CheckSpotInterruption(_ context.Context) (*provider.InterruptionInfo, error) {
	return nil, nil
}
func (s *stubStatusProvider) DiscoverPeers(_ context.Context, _ string) ([]provider.PeerInfo, error) {
	return nil, nil
}
func (s *stubStatusProvider) GetProviderType() string                               { return "stub" }
func (s *stubStatusProvider) LookupAndTagEBSCost(_ context.Context) (float64, bool) { return 0, false }
func (s *stubStatusProvider) CountOtherManagedInstances(_ context.Context) int      { return -1 }

func newTestReport(t *testing.T) *statusReport {
	t.Helper()
	identity := &provider.Identity{
		InstanceID: "i-0123456789abcdef0",
		Name:       "test-instance",
		Region:     "us-west-2",
		AccountID:  "123456789012",
		Provider:   "stub",
	}
	cfg := &provider.Config{
		TTL:            4 * time.Hour,
		TTLDeadline:    time.Now().Add(3 * time.Hour),
		LaunchTime:     time.Now().Add(-1 * time.Hour),
		IdleTimeout:    30 * time.Minute,
		IdleCPUPercent: 5.0,
		PricePerHour:   0.10,
		CostLimit:      5.0,
		OnComplete:     "terminate",
		CompletionFile: "/tmp/SPAWN_COMPLETE",
	}
	ag, err := agent.NewAgent(context.Background(), &stubStatusProvider{identity: identity, config: cfg})
	if err != nil {
		t.Fatalf("agent.NewAgent: %v", err)
	}
	return buildStatusReport(ag, cfg, identity)
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. Used to test renderStatusJSON/renderStatusTable,
// which — like the real `spored status` CLI path — write directly to
// os.Stdout rather than taking an io.Writer (matching every other render
// function in this file, e.g. handleConfigList).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	out := <-done
	return out
}

// TestRenderStatusJSON_StdoutIsPureJSON is the direct regression test for
// spawn#540: the entire stdout capture of the JSON renderer must parse as
// JSON, with nothing before or after it (the bug was leading log.Printf lines
// breaking json.Unmarshal with "invalid character... looking for beginning of
// value" / "Extra data").
func TestRenderStatusJSON_StdoutIsPureJSON(t *testing.T) {
	report := newTestReport(t)

	out := captureStdout(t, func() {
		if err := renderStatusJSON(report); err != nil {
			t.Fatalf("renderStatusJSON: %v", err)
		}
	})

	var decoded statusReport
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("stdout did not parse as JSON: %v\ncaptured stdout:\n%q", err, out)
	}

	if decoded.InstanceID != report.InstanceID {
		t.Errorf("instance_id = %q, want %q", decoded.InstanceID, report.InstanceID)
	}

	// The fields the issue explicitly calls out: sentinel present, TTL
	// deadline, cost-limit consumed, effective rate.
	if decoded.OnComplete.CompletionFile == "" {
		t.Error("expected on_complete.completion_file to be set (sentinel path)")
	}
	if decoded.TTL.TerminateAt == nil {
		t.Error("expected ttl.terminate_at (deadline) to be set")
	}
	if decoded.Cost == nil {
		t.Fatal("expected cost to be populated (PricePerHour > 0)")
	}
	if decoded.Cost.CostLimit == 0 {
		t.Error("expected cost.cost_limit to be populated (cost-limit consumed)")
	}
}

// TestRenderStatusTable_Unchanged confirms the human table renderer still
// prints the same identity/lifecycle/cost sections as before the refactor
// (spawn#540 rebuilt handleStatus around a shared statusReport struct; the
// table output itself must not have regressed).
func TestRenderStatusTable_Unchanged(t *testing.T) {
	report := newTestReport(t)

	out := captureStdout(t, func() {
		if err := renderStatusTable(report); err != nil {
			t.Fatalf("renderStatusTable: %v", err)
		}
	})

	for _, want := range []string{
		"test-instance", "i-0123456789abcdef0",
		"spored:", "Started:", "Elapsed:", "TTL:", "remaining",
		"Idle timeout:", "On complete:", "terminate",
		"Compute cost:", "Cumulative cost:", "Cost limit:",
		"CPU:", "Network:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q; got:\n%s", want, out)
		}
	}

	// The table is prose, not data — it must NOT parse as JSON. (Sanity check
	// that the two renderers are actually different, not the same code path.)
	var v any
	if err := json.Unmarshal([]byte(out), &v); err == nil {
		t.Error("table output unexpectedly parsed as JSON")
	}
}

// A first-boot instance must NOT report its boot time as time spent stopped.
//
// The reported case was `Elapsed: 22m 13s (20m 0s compute · 2m 13s stopped)` on
// an instance that had never been stopped: stoppedTime was a RESIDUE — elapsed
// since launch minus compute since spored started — so it absorbed boot,
// cloud-init and package installs and asserted they were a stop (#735).
func TestStatusReport_FirstBootReportsNoStoppedTime(t *testing.T) {
	report := newTestReport(t)

	if report.StoppedSeconds != 0 {
		t.Errorf("StoppedSeconds = %.0f on an instance that was never stopped — "+
			"that is boot time mislabelled (#735)", report.StoppedSeconds)
	}
	// The boot gap is real and now reported in its own right: spored knows both
	// the instance launch time and its own start, so it is measured rather than
	// inferred. A slow boot is worth seeing.
	if report.BootSeconds <= 0 {
		t.Errorf("BootSeconds = %.0f, want > 0 — the launch-to-spored gap is the "+
			"residue that used to be called 'stopped'", report.BootSeconds)
	}
	// And it must account for the elapsed time rather than vanishing.
	if sum := report.ComputeSeconds + report.BootSeconds + report.StoppedSeconds; sum < report.ElapsedSeconds-2 {
		t.Errorf("compute+boot+stopped = %.0f but elapsed = %.0f; %.0fs is unaccounted for",
			sum, report.ElapsedSeconds, report.ElapsedSeconds-sum)
	}
}

// The rendered line must not claim a stop that did not happen.
func TestStatusTable_NeverStoppedOmitsTheStoppedTerm(t *testing.T) {
	report := newTestReport(t)
	// The fixture's agent was created just now, so TotalComputeSeconds rounds to
	// zero and the pre-existing `computeTime > 0` guard suppresses the whole
	// breakdown. Set it to the reported shape — 20 minutes of compute after a
	// 2m13s boot — so the renderer is actually exercised.
	report.ComputeSeconds = (20 * time.Minute).Seconds()
	report.BootSeconds = (2*time.Minute + 13*time.Second).Seconds()
	report.StoppedSeconds = 0
	report.ElapsedSeconds = (22*time.Minute + 13*time.Second).Seconds()

	out := captureStdout(t, func() {
		if err := renderStatusTable(report); err != nil {
			t.Fatalf("renderStatusTable: %v", err)
		}
	})
	if strings.Contains(out, "stopped") {
		t.Errorf("output claims stopped time on a never-stopped instance:\n%s", out)
	}
	if !strings.Contains(out, "boot") {
		t.Errorf("output does not report the boot gap:\n%s", out)
	}
}

// CPU must render as an honest unknown rather than a confident reading when there
// was no sample interval — which, for a one-shot `spored status`, is the common
// case rather than an edge one (#734).
func TestStatusTable_UnmeasuredCPUIsNotPrintedAsAValue(t *testing.T) {
	report := newTestReport(t)
	report.CPUMeasured = false
	report.CPUPercent = 100.0 // the assume-active sentinel

	out := captureStdout(t, func() {
		if err := renderStatusTable(report); err != nil {
			t.Fatalf("renderStatusTable: %v", err)
		}
	})
	if strings.Contains(out, "CPU:              100.0%") {
		t.Errorf("an unmeasured sentinel was printed as a reading:\n%s", out)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("output does not mark the CPU reading unknown:\n%s", out)
	}

	report.CPUMeasured = true
	report.CPUPercent = 42.5
	out = captureStdout(t, func() { _ = renderStatusTable(report) })
	if !strings.Contains(out, "42.5%") {
		t.Errorf("a real measurement was not printed:\n%s", out)
	}
}
