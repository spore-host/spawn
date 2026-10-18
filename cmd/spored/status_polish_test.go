package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/userdata"
)

// The idle line names how long the instance has been IDLE, then when the
// action fires: "idle for 7m — stops in 23m", not "stops for 7m" (#1114).
func TestStatus_IdleLineSaysIdleFor(t *testing.T) {
	stubDaemonIdleState(t, agent.IdleState{
		Taken: time.Now().Add(-10 * time.Second), Idle: true, Reason: "System is idle",
		CPUMeasured: true, NetworkMeasured: true, DiskMeasured: true,
		LastActivity: time.Now().Add(-7 * time.Minute),
	}, true)
	out := captureStdout(t, func() { _ = renderStatusTable(newTestReport(t)) })
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "Idle timeout:") {
			line = l
		}
	}
	if !strings.Contains(line, "(idle for ") || strings.Count(line, " for ") != 1 {
		t.Errorf("idle line = %q, want \"(idle for <idle time> — <action> in <remaining>)\"", line)
	}
}

// Before spored's first sample this boot, status says so in one line instead
// of three "unknown (single sample; assuming active)" readings (#1114). The
// JSON keeps its fields, with source "pending".
func TestStatus_FirstMinuteSaysTheReadingIsPending(t *testing.T) {
	stubDaemonIdleState(t, agent.IdleState{}, false)
	old := daemonIdleNotYetPublished
	daemonIdleNotYetPublished = func() bool { return true }
	t.Cleanup(func() { daemonIdleNotYetPublished = old })

	report := newTestReport(t)
	if report.IdleReading.Source != "pending" {
		t.Fatalf("IdleReading = %+v, want pending", report.IdleReading)
	}
	out := captureStdout(t, func() { _ = renderStatusTable(report) })
	if strings.Contains(out, "single sample") || strings.Contains(out, "CPU:") {
		t.Errorf("pending status still prints the one-shot unknowns:\n%s", out)
	}
	if !strings.Contains(out, "Idle check:       first reading pending") {
		t.Errorf("pending status lacks the pending line:\n%s", out)
	}
}

// mpirun's ssh to each peer must not print "Permanently added ... to the list
// of known hosts" into the job's output: with UserKnownHostsFile=/dev/null
// every connection is a first one (#1114).
func TestMPISSHConfigSilencesKnownHostsWarnings(t *testing.T) {
	script, err := userdata.GenerateMPIUserData(userdata.MPIConfig{JobArraySize: 2, JobArrayIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "  LogLevel ERROR\n") {
		t.Errorf("MPI ssh config lacks LogLevel ERROR")
	}
}

// The compute-cost duration is billed time (compute plus boot) and must say
// so, or it reads as contradicting "Elapsed: … (N compute · M boot)" (#1114).
func TestStatus_ComputeCostSaysBilled(t *testing.T) {
	out := captureStdout(t, func() { _ = renderStatusTable(newTestReport(t)) })
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "Compute cost:") && strings.Contains(l, "$") && !strings.Contains(l, "—") && !strings.Contains(l, " billed × ") {
			t.Errorf("compute cost line doesn't say the duration is billed time: %q", l)
		}
	}
}
