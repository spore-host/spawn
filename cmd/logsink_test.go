package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The account ID from #895's report: it was in every audit line on the terminal.
const testAccountID = "942542972736"

// withLogDir points spawn's log files at a temp dir and restores everything the
// sinks touch — the open-file cache, the standard logger, --verbose and
// os.Stderr — when the test ends.
func withLogDir(t *testing.T, verbose bool) string {
	t.Helper()
	dir := t.TempDir()
	prevDir, prevVerbose, prevLog, prevStderr := spawnLogDir, spawnVerbose, log.Writer(), os.Stderr
	spawnLogDir = func() (string, error) { return dir, nil }
	spawnVerbose = verbose
	t.Cleanup(func() {
		logFilesMu.Lock()
		for p, f := range logFiles {
			_ = f.Close()
			delete(logFiles, p)
		}
		logFilesMu.Unlock()
		spawnLogDir, spawnVerbose = prevDir, prevVerbose
		log.SetOutput(prevLog)
		os.Stderr = prevStderr
	})
	return dir
}

// captureOSStderr swaps os.Stderr for a pipe while fn runs.
func captureOSStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = prev
	b, _ := io.ReadAll(r)
	return string(b)
}

// goLogTimestamp is the prefix Go's standard logger writes: "2026/10/15 09:59:58 ".
var goLogTimestamp = regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// assertNoMachineLines fails if out carries anything #895 says belongs off the
// terminal: a JSON audit object, a Go log timestamp, or the account ID.
func assertNoMachineLines(t *testing.T, what, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			t.Errorf("%s carries a JSON log line (#895): %q", what, line)
		}
	}
	if goLogTimestamp.MatchString(out) {
		t.Errorf("%s carries a Go log-timestamped line (#895):\n%s", what, out)
	}
	if strings.Contains(out, testAccountID) {
		t.Errorf("%s carries the AWS account ID (#895):\n%s", what, out)
	}
}

// TestAuditTrailIsKeptOffTheTerminalButNotLost: by default the audit events a
// launch writes go to ~/.spawn/audit.log and nowhere near the terminal; they
// must still be complete JSON records there, account ID included — the audit
// trail is the record, so moving it must not lose it.
func TestAuditTrailIsKeptOffTheTerminalButNotLost(t *testing.T) {
	dir := withLogDir(t, false)

	stderr := captureOSStderr(t, func() {
		a := newAuditLogger(testAccountID, "corr-1")
		a.LogOperationWithRegion("launch_instance", "single", "us-west-2", "initiated", nil)
		a.LogOperationWithRegion("launch_instance", "i-0abc", "us-west-2", "success", nil)
	})
	assertNoMachineLines(t, "stderr", stderr)

	data, err := os.ReadFile(filepath.Join(dir, auditLogName))
	if err != nil {
		t.Fatalf("audit trail was not written to %s: %v", auditLogName, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit.log has %d records, want 2:\n%s", len(lines), data)
	}
	for _, l := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("audit.log record is not JSON: %v: %q", err, l)
		}
		if ev["user_id"] != testAccountID || ev["operation"] != "launch_instance" {
			t.Errorf("audit record lost its content: %v", ev)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, auditLogName)); err == nil && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("audit.log is readable by others (mode %v); it names the account", fi.Mode().Perm())
	}
}

// TestAuditTrailOnStderrUnderVerbose: --verbose is the documented way to see the
// audit trail live; it must still be appended to the file too.
func TestAuditTrailOnStderrUnderVerbose(t *testing.T) {
	dir := withLogDir(t, true)
	stderr := captureOSStderr(t, func() {
		newAuditLogger(testAccountID, "corr-2").LogOperation("cancel_sweep", "sw-1", "initiated", nil)
	})
	if !strings.Contains(stderr, `"operation":"cancel_sweep"`) {
		t.Errorf("--verbose did not copy the audit event to stderr: %q", stderr)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, auditLogName)); !strings.Contains(string(data), "cancel_sweep") {
		t.Errorf("--verbose stopped the audit event reaching audit.log: %q", data)
	}
}

// TestAuditTrailFallsBackToStderrRatherThanBeingDropped: if ~/.spawn can't be
// written, an untidy terminal is the lesser harm; a silently lost audit record
// is the greater.
func TestAuditTrailFallsBackToStderrRatherThanBeingDropped(t *testing.T) {
	withLogDir(t, false)
	spawnLogDir = func() (string, error) { return "", errors.New("no home") }
	for _, verbose := range []bool{false, true} {
		var buf bytes.Buffer
		_, _ = io.WriteString(auditSink(verbose, &buf), "{\"operation\":\"x\"}\n")
		if got := strings.Count(buf.String(), "operation"); got != 1 {
			t.Errorf("verbose=%v, no usable log dir: audit event reached stderr %d times, want exactly 1: %q",
				verbose, got, buf.String())
		}
	}
}

// TestQuietRunLeavesNoLogFile: the files are opened on first write, so a
// command that logs nothing does not litter ~/.spawn with empty files.
func TestQuietRunLeavesNoLogFile(t *testing.T) {
	dir := withLogDir(t, false)
	_ = auditSink(false, io.Discard)
	_ = diagnosticSink(false, io.Discard)
	for _, name := range []string{auditLogName, diagnosticLogName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was created by a run that wrote nothing to it", name)
		}
	}
}

// TestDiagnosticLogLinesStayOffTheTerminal drives a real spawn command through
// rootCmd, so the routing has to be WIRED (cobra.OnInitialize), not merely
// available: a command's library code calls log.Printf — as pkg/aws's pricing
// lookup does mid-launch — and the line must land in spawn.log, not stderr.
func TestDiagnosticLogLinesStayOffTheTerminal(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		dir := withLogDir(t, false)
		const line = "pricing: m8a.4xlarge in us-west-2 = $0.9738/hr (truffle, live AWS Price List)"

		probe := &cobra.Command{
			Use:    "zz-log-probe-895",
			Hidden: true,
			RunE: func(*cobra.Command, []string) error {
				log.Printf("%s", line)
				return nil
			},
		}
		rootCmd.AddCommand(probe)
		args := []string{"zz-log-probe-895"}
		if verbose {
			args = append(args, "--verbose")
		}
		rootCmd.SetArgs(args)
		var execErr error
		stderr := captureOSStderr(t, func() { execErr = rootCmd.Execute() })
		rootCmd.RemoveCommand(probe)
		rootCmd.SetArgs(nil)
		if execErr != nil {
			t.Fatalf("execute probe: %v", execErr)
		}

		if verbose {
			if !strings.Contains(stderr, line) {
				t.Errorf("--verbose: diagnostic line not copied to stderr: %q", stderr)
			}
		} else {
			assertNoMachineLines(t, "stderr", stderr)
			if strings.Contains(stderr, "pricing:") {
				t.Errorf("diagnostic line reached the terminal without --verbose: %q", stderr)
			}
		}
		if data, _ := os.ReadFile(filepath.Join(dir, diagnosticLogName)); !strings.Contains(string(data), line) {
			t.Errorf("verbose=%v: diagnostic line not kept in %s: %q", verbose, diagnosticLogName, data)
		}
	}
}

// TestCommandsBuildAuditLoggersThroughTheSink discovers every audit.NewLogger
// invocation in the command package. Each one that is not newAuditLogger's own
// is a logger writing wherever its caller pointed it — which, five times over,
// was os.Stderr (#895).
func TestCommandsBuildAuditLoggersThroughTheSink(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("discovered no Go files in cmd (err=%v); the scan proves nothing", err)
	}
	direct := regexp.MustCompile(`\baudit\.NewLogger\(`)
	viaSink := regexp.MustCompile(`\bnewAuditLogger\(`)
	sinkUsers := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "logsink.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "//") {
				continue
			}
			if direct.MatchString(l) {
				t.Errorf("%s:%d builds an audit logger directly; use newAuditLogger so the "+
					"trail goes to ~/.spawn/audit.log, not the terminal (#895): %s", f, i+1, strings.TrimSpace(l))
			}
			if viaSink.MatchString(l) {
				sinkUsers++
			}
		}
	}
	if sinkUsers == 0 {
		t.Fatal("found no newAuditLogger call sites; the scan is not seeing the commands")
	}
}

// TestRelaySporedStderr: an older spored (before #895) still prints its agent
// diagnostics during `spored status`; spawn drops those Go-log lines unless
// --verbose, and always relays anything else (an error is not a diagnostic).
func TestRelaySporedStderr(t *testing.T) {
	const sporedErr = "2026/10/15 17:00:45 Not idle: CPU usage could not be measured (no sample interval yet); assuming active (100.00%)\n" +
		"2026/10/15 17:00:45 Agent initialized for instance i-0abc in us-west-2 (account: " + testAccountID + ", provider: ec2)\n" +
		"Error: initialize agent: no IMDS\n"

	var quiet bytes.Buffer
	relaySporedStderr(&quiet, sporedErr, false)
	assertNoMachineLines(t, "relayed stderr", quiet.String())
	if !strings.Contains(quiet.String(), "Error: initialize agent: no IMDS") {
		t.Errorf("a non-log error line from spored was dropped: %q", quiet.String())
	}

	var loud bytes.Buffer
	relaySporedStderr(&loud, sporedErr, true)
	if loud.String() != sporedErr {
		t.Errorf("--verbose must relay spored's stderr verbatim; got %q", loud.String())
	}
}

// TestEmitStatusTableModeHasNoAgentLogLines: the `spawn status demo` output from
// #895, through the relay, with spored's agent lines on its stderr.
func TestEmitStatusTableModeHasNoAgentLogLines(t *testing.T) {
	const table = "  demo  (i-028c23f816d2eb68e)\n"
	const sporedErr = "2026/10/15 17:00:45 Not idle: CPU usage could not be measured (no sample interval yet); assuming active (100.00%)\n"
	var stdout, stderr bytes.Buffer
	emitStatus(&stdout, &stderr, table, sporedErr, nil, false, false)
	assertNoMachineLines(t, "stdout", stdout.String())
	assertNoMachineLines(t, "stderr", stderr.String())
	if stdout.String() != table {
		t.Errorf("status table altered: %q", stdout.String())
	}
}
