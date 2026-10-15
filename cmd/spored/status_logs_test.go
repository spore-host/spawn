package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"
)

// runStatusCmdCapturing runs the real `spored status` cobra command with args,
// with the agent work replaced by fake: what the query agent does in
// handleStatus — log.Printf its initialization and idle check, then print the
// report. It returns the captured stdout and stderr.
func runStatusCmdCapturing(t *testing.T, args []string, fake func()) (stdout, stderr string) {
	t.Helper()
	prevRun, prevLog := runStatus, log.Writer()
	prevOut, prevErr := os.Stdout, os.Stderr
	t.Cleanup(func() {
		runStatus = prevRun
		log.SetOutput(prevLog)
		os.Stdout, os.Stderr = prevOut, prevErr
	})
	runStatus = func(bool, string) error { fake(); return nil }

	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW

	root := newRootCmd()
	root.SetArgs(append([]string{"status"}, args...))
	execErr := root.Execute()

	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = prevOut, prevErr
	o, _ := io.ReadAll(outR)
	e, _ := io.ReadAll(errR)
	if execErr != nil {
		t.Fatalf("spored status %v: %v", args, execErr)
	}
	return string(o), string(e)
}

// fakeStatusQuery logs exactly the lines #895's reporter saw (and #540's), then
// prints a status line.
func fakeStatusQuery() {
	log.Printf("Agent initialized for instance i-0abc in us-west-2 (account: 942542972736, provider: ec2)")
	log.Printf("Not idle: CPU usage could not be measured (no sample interval yet); assuming active (100.00%%)")
	fmt.Println("  demo  (i-0abc)")
}

// TestSporedStatus_AgentDiagnosticsOffByDefault is the spored half of spawn#895:
// `spored status` is a human report (or a JSON document), and the query agent's
// log lines — one of which carries the AWS account ID — must not appear on
// either stream unless asked for with --verbose.
func TestSporedStatus_AgentDiagnosticsOffByDefault(t *testing.T) {
	for _, args := range [][]string{nil, {"--output", "json"}} {
		stdout, stderr := runStatusCmdCapturing(t, args, fakeStatusQuery)
		if !strings.Contains(stdout, "demo  (i-0abc)") {
			t.Fatalf("status %v: the report itself is missing from stdout: %q", args, stdout)
		}
		both := stdout + stderr
		for _, leak := range []string{"Not idle:", "Agent initialized", "942542972736"} {
			if strings.Contains(both, leak) {
				t.Errorf("status %v: %q reached the terminal without --verbose (spawn#895)\nstdout=%q\nstderr=%q",
					args, leak, stdout, stderr)
			}
		}
	}
}

// TestSporedStatus_VerboseSendsDiagnosticsToStderrOnly: --verbose must make the
// diagnostics visible again (a diagnostic nobody can ask for is its own bug) and
// must put them on stderr, never stdout (spawn#540).
func TestSporedStatus_VerboseSendsDiagnosticsToStderrOnly(t *testing.T) {
	stdout, stderr := runStatusCmdCapturing(t, []string{"--verbose"}, fakeStatusQuery)
	if !strings.Contains(stderr, "Not idle:") {
		t.Errorf("--verbose did not surface the agent's diagnostics on stderr: %q", stderr)
	}
	if strings.Contains(stdout, "Not idle:") || strings.Contains(stdout, "Agent initialized") {
		t.Errorf("agent diagnostics reached stdout under --verbose: %q", stdout)
	}
}

// statusLogSink is the invariant itself, independent of the command wiring.
func TestStatusLogSink(t *testing.T) {
	if got := statusLogSink(false, &bytes.Buffer{}); got != io.Discard {
		t.Errorf("statusLogSink(verbose=false) = %T, want io.Discard", got)
	}
	var buf bytes.Buffer
	if got := statusLogSink(true, &buf); got != io.Writer(&buf) {
		t.Errorf("statusLogSink(verbose=true) did not return the stderr writer")
	}
}
