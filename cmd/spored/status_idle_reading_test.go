package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
)

func stubDaemonIdleState(t *testing.T, st agent.IdleState, ok bool) {
	t.Helper()
	old := daemonIdleState
	daemonIdleState = func(time.Time) (agent.IdleState, bool) { return st, ok }
	t.Cleanup(func() { daemonIdleState = old })
}

// With the daemon running, status shows its readings and its verdict (#1090).
// In the v0.131.1 idle test the daemon logged "Not idle: Disk I/O 1204224
// bytes" every minute while every status screen said CPU, network and disk
// were "unknown (single sample)".
func TestStatus_ShowsTheDaemonsIdleReadings(t *testing.T) {
	stubDaemonIdleState(t, agent.IdleState{
		Taken: time.Now().Add(-20 * time.Second), Reason: "Not idle: Disk I/O 1204224 bytes",
		CPUPercent: 0.4, CPUMeasured: true,
		NetworkBytes: 41000, NetworkMeasured: true,
		DiskBytes: 1204224, DiskMeasured: true,
		LastActivity: time.Now(),
	}, true)
	report := newTestReport(t)

	if report.IdleReading.Source != "daemon" || !report.CPUMeasured || !report.NetworkMeasured || !report.DiskMeasured {
		t.Fatalf("report did not take the daemon's readings: %+v / %+v", report.IdleReading, report)
	}
	out := captureStdout(t, func() { _ = renderStatusTable(report) })
	if strings.Contains(out, "unknown") {
		t.Errorf("status still prints an unknown reading with the daemon's measurements available:\n%s", out)
	}
	for _, want := range []string{"Disk I/O:         1.1 MB/min", "Idle check:       Not idle: Disk I/O 1204224 bytes", "spored's last check, 20s ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}

	var js map[string]any
	if err := json.Unmarshal([]byte(captureStdout(t, func() { _ = renderStatusJSON(report) })), &js); err != nil {
		t.Fatal(err)
	}
	r, _ := js["idle_reading"].(map[string]any)
	if r["source"] != "daemon" || r["reason"] != "Not idle: Disk I/O 1204224 bytes" {
		t.Errorf("JSON idle_reading = %v, want the daemon's source and reason", r)
	}
}

// The daemon's idle verdict drives the idle line too: idle for as long as its
// last activity says, not this process's own guess.
func TestStatus_DaemonIdleDecisionDrivesTheIdleLine(t *testing.T) {
	stubDaemonIdleState(t, agent.IdleState{
		Taken: time.Now().Add(-10 * time.Second), Idle: true, Reason: "System is idle (CPU: 0.10%, Network: 2000 bytes, Disk: 0 bytes, GPU: 0.00%)",
		CPUMeasured: true, NetworkMeasured: true, DiskMeasured: true,
		LastActivity: time.Now().Add(-7 * time.Minute),
	}, true)
	report := newTestReport(t)
	if !report.IdleTimeout.IsIdle || report.IdleTimeout.IdleSeconds < 6*60 || report.IdleTimeout.IdleSeconds > 8*60 {
		t.Errorf("idle = %+v, want idle for ~7m from the daemon's last activity", report.IdleTimeout)
	}
}

// With no fresh daemon reading, status samples once and says that is what it
// did, rather than presenting a one-shot "unknown" as the instance's state.
func TestStatus_LabelsTheOneShotFallback(t *testing.T) {
	stubDaemonIdleState(t, agent.IdleState{}, false)
	report := newTestReport(t)
	if report.IdleReading.Source != "one-shot" {
		t.Fatalf("IdleReading = %+v, want one-shot", report.IdleReading)
	}
	out := captureStdout(t, func() { _ = renderStatusTable(report) })
	if !strings.Contains(out, "Idle check:       daemon reading unavailable; one-shot sample") {
		t.Errorf("one-shot reading is not labelled:\n%s", out)
	}
}
