package taskproto

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func flushSpec() *TaskSpec {
	return &TaskSpec{
		TaskID:    "ttl-no-record-demo",
		Command:   []string{"bash", "-c", "sleep 3600"},
		Lifecycle: Lifecycle{TTL: "5m", CostLimit: 0.05},
	}
}

// runFlushHook writes the generated hook to a temp file, runs it with a
// stand-in `aws` that records its invocations, and returns the hook's stdout+
// stderr plus the recorded `aws` argv lines.
func runFlushHook(t *testing.T, script, reason string) (output string, awsCalls []string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "task-flush.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatalf("write flush script: %v", err)
	}

	binDir := t.TempDir()
	callLog := filepath.Join(dir, "aws-calls.log")
	writeFakeExe(t, filepath.Join(binDir, "aws"),
		"#!/bin/bash\nprintf '%s\\n' \"$*\" >> "+callLog+"\nexit 0\n")

	cmd := exec.Command("bash", scriptPath, reason)
	// The stub must survive the hook's own PATH export. It does because that export
	// APPENDS the system directories to the inherited PATH rather than replacing
	// it — an earlier version substituted an absolute PATH, which silently made
	// the stub (and, in production, any aws CLI outside those six directories, e.g.
	// Ubuntu's /snap/bin) unreachable. TestFlushHookFindsAwsOutsideTheSystemDirs
	// pins that behaviour directly.
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, _ := cmd.CombinedOutput()

	calls := ""
	if b, err := os.ReadFile(callLog); err == nil {
		calls = string(b)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(calls), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return string(out), lines
}

// TestFlushHookNeverOverwritesTheWrapperRecord is the single most important
// assertion in this file, and it guards against a regression far worse than the
// bug being fixed.
//
// The success path is: command returns -> wrapper writes completion.json ->
// wrapper writes SPAWN_COMPLETE -> spored acts on on_complete -> terminate() ->
// runPreStop() -> this hook. So the hook runs on EVERY normally-completed task
// too. Without the interlock it would relabel every one of them
// failed/exit_code -1, i.e. turn a working fleet's records into garbage.
func TestFlushHookNeverOverwritesTheWrapperRecord(t *testing.T) {
	// Stand in for the wrapper having already written its record.
	const sentinel = `{"task_id":"ttl-no-record-demo","state":"completed","exit_code":0}`
	if err := os.WriteFile(CompletionRecordPath(), []byte(sentinel), 0o600); err != nil {
		t.Fatalf("seed wrapper record: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(CompletionRecordPath()) })

	script := GenerateFlushScript(flushSpec(), "results-bucket", "us-east-1", "run-1")
	out, awsCalls := runFlushHook(t, script, string(ExitTTLExpired))

	if len(awsCalls) != 0 {
		t.Errorf("the hook must not touch S3 when the wrapper already wrote a record; aws was called:\n%s\noutput: %s",
			strings.Join(awsCalls, "\n"), out)
	}
	after, err := os.ReadFile(CompletionRecordPath())
	if err != nil {
		t.Fatalf("the hook deleted the wrapper's record: %v", err)
	}
	if string(after) != sentinel {
		t.Errorf("the wrapper's completion record was modified:\n got: %s\nwant: %s", after, sentinel)
	}
	if !strings.Contains(out, "already wrote its completion record") {
		t.Errorf("the hook should say why it did nothing, got: %s", out)
	}
}

// TestFlushHookWritesAParsableTerminalRecord is spawn#632's actual repro: the
// command never returned, so there is no wrapper record, and the hook has to
// produce the one the reporter found missing. It must be the SAME shape the six
// workflow adapters parse — built with a shell heredoc, so a stray comma is a
// real risk, which is why this parses rather than string-matches.
func TestFlushHookWritesAParsableTerminalRecord(t *testing.T) {
	_ = os.Remove(CompletionRecordPath())
	if err := os.WriteFile(StartedAtPath(), []byte("2026-10-01T21:45:39Z"), 0o600); err != nil {
		t.Fatalf("seed started-at: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(StartedAtPath()) })

	script := GenerateFlushScript(flushSpec(), "results-bucket", "us-west-2", "run-7")
	out, awsCalls := runFlushHook(t, script, string(ExitTTLExpired))

	data, err := os.ReadFile(localPath(terminalFileName))
	if err != nil {
		t.Fatalf("no terminal record written: %v\noutput: %s", err, out)
	}
	if !json.Valid(data) {
		t.Fatalf("terminal record is not valid JSON:\n%s", data)
	}
	rec, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatalf("parse terminal record: %v\nraw: %s", err, data)
	}

	if rec.TaskID != "ttl-no-record-demo" {
		t.Errorf("task_id = %q", rec.TaskID)
	}
	if rec.RunID != "run-7" {
		t.Errorf("run_id = %q, want run-7 — --wait rejects a record whose run_id is not its own", rec.RunID)
	}
	// "failed", not a new "terminated": State is documented `completed | failed`
	// and six adapters parse this object. The WHY rides in the two additive
	// fields below.
	if rec.State != StateFailed {
		t.Errorf("state = %q, want %q", rec.State, StateFailed)
	}
	if rec.RetryClass != RetryTTLExpired {
		t.Errorf("retry_class = %q, want %q", rec.RetryClass, RetryTTLExpired)
	}
	if rec.TerminalReason != ExitTTLExpired {
		t.Errorf("terminal_reason = %q, want %q", rec.TerminalReason, ExitTTLExpired)
	}
	// -1 distinguishes "never returned a status" from a real 0 or non-zero exit.
	if rec.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1 (the process never returned)", rec.ExitCode)
	}
	if rec.StartedAt != "2026-10-01T21:45:39Z" {
		t.Errorf("started_at = %q — the wrapper's persisted value should carry through", rec.StartedAt)
	}
	if rec.EndedAt == "" {
		t.Error("ended_at must be stamped")
	}

	// It must actually be uploaded to the task's prefix, under the key adapters poll.
	joined := strings.Join(awsCalls, "\n")
	if !strings.Contains(joined, "s3://results-bucket/tasks/ttl-no-record-demo/completion.json") {
		t.Errorf("the terminal record was never uploaded to the results prefix; aws calls:\n%s", joined)
	}
}

// TestFlushHookUploadsTheLogBeforeTheRecord. The log is what the reporter
// actually needed — spawn_phase() has already written stage-in/pull/command
// timings into it — so it must go first: a hook that runs out of time should
// still have salvaged the more useful half.
func TestFlushHookUploadsTheLogBeforeTheRecord(t *testing.T) {
	_ = os.Remove(CompletionRecordPath())
	script := GenerateFlushScript(flushSpec(), "results-bucket", "us-east-1", "run-2")

	logIdx := strings.Index(script, "/var/log/spawn-command.log")
	recIdx := strings.Index(script, localPath(terminalFileName))
	if logIdx < 0 || recIdx < 0 {
		t.Fatalf("expected both the log upload and the record write (log@%d rec@%d)", logIdx, recIdx)
	}
	if logIdx > recIdx {
		t.Error("the command log must be uploaded before the completion record is written")
	}
}

// TestFlushHookRecordsLogsOnlyWhenTheUploadSucceeded: logs[] is a list of s3://
// URIs an adapter will try to fetch. Listing a key that was never uploaded sends
// the reader to a 404, which is a worse failure than an empty list.
func TestFlushHookRecordsLogsOnlyWhenTheUploadSucceeded(t *testing.T) {
	_ = os.Remove(CompletionRecordPath())
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	script := GenerateFlushScript(flushSpec(), "results-bucket", "us-east-1", "run-3")
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "task-flush.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write script: %v", err)
	}
	// An `aws` that fails ONLY the command.log copy; the record upload still works.
	binDir := t.TempDir()
	writeFakeExe(t, filepath.Join(binDir, "aws"), `#!/bin/bash
for a in "$@"; do
  case "$a" in */command.log) exit 1 ;; esac
done
exit 0
`)
	cmd := exec.Command("bash", scriptPath, string(ExitTTLExpired))
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, _ := cmd.CombinedOutput()

	data, err := os.ReadFile(localPath(terminalFileName))
	if err != nil {
		t.Fatalf("a failed log upload must still produce a record: %v\noutput: %s", err, out)
	}
	rec, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatalf("parse: %v\nraw: %s", err, data)
	}
	if len(rec.Logs) != 0 {
		t.Errorf("logs must stay empty when the upload failed, got %v", rec.Logs)
	}
	// The record itself is still the point of the exercise.
	if rec.RetryClass != RetryTTLExpired {
		t.Errorf("retry_class = %q, want %q", rec.RetryClass, RetryTTLExpired)
	}
}

// TestFlushHookReasonToRetryClass pins the mapping. Only reasons with genuinely
// matching semantics get a class; the rest get "" (an unclassified terminal
// state) rather than a guess, because RetryClass.Retryable() decides whether an
// adapter spends money running the task again.
func TestFlushHookReasonToRetryClass(t *testing.T) {
	cases := []struct {
		reason    ExitReason
		wantClass RetryClass
	}{
		{ExitTTLExpired, RetryTTLExpired},
		{ExitSpotInterruption, RetrySpotInterruption},
		// No RetryClass means "cost limit" and "idle" — neither has a class whose
		// meaning fits, and inventing one would mislead a retry decision. They are
		// still fully identified by terminal_reason.
		{ExitCostLimitExceeded, RetryNone},
		{ExitIdleTimeout, RetryNone},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			_ = os.Remove(CompletionRecordPath())
			_ = os.Remove(localPath(terminalFileName))
			script := GenerateFlushScript(flushSpec(), "b", "us-east-1", "r")
			out, _ := runFlushHook(t, script, string(tc.reason))

			data, err := os.ReadFile(localPath(terminalFileName))
			if err != nil {
				t.Fatalf("no record: %v\noutput: %s", err, out)
			}
			rec, err := ParseCompletionRecord(data)
			if err != nil {
				t.Fatalf("parse: %v\nraw: %s", err, data)
			}
			if rec.RetryClass != tc.wantClass {
				t.Errorf("retry_class = %q, want %q", rec.RetryClass, tc.wantClass)
			}
			// Whatever the class, the reason must always be recorded verbatim —
			// that is the field that makes a cost-limit kill distinguishable from
			// an idle stop.
			if rec.TerminalReason != tc.reason {
				t.Errorf("terminal_reason = %q, want %q", rec.TerminalReason, tc.reason)
			}
		})
	}
}

// TestFlushHookSurvivesAnUnknownReason: a future exit path that forgets to add
// its token must still get a record, not a crash or an empty file.
func TestFlushHookSurvivesAnUnknownReason(t *testing.T) {
	_ = os.Remove(CompletionRecordPath())
	_ = os.Remove(localPath(terminalFileName))
	script := GenerateFlushScript(flushSpec(), "b", "us-east-1", "r")
	out, _ := runFlushHook(t, script, "some_future_reason")

	data, err := os.ReadFile(localPath(terminalFileName))
	if err != nil {
		t.Fatalf("an unknown reason must still produce a record: %v\noutput: %s", err, out)
	}
	rec, err := ParseCompletionRecord(data)
	if err != nil {
		t.Fatalf("parse: %v\nraw: %s", err, data)
	}
	if rec.RetryClass != RetryNone {
		t.Errorf("an unrecognized reason must not be assigned a retry class, got %q", rec.RetryClass)
	}
	if rec.TerminalReason != "some_future_reason" {
		t.Errorf("terminal_reason = %q, want it recorded verbatim", rec.TerminalReason)
	}
}

// TestFlushHookFindsAwsOutsideTheSystemDirs. The hook exports a PATH because a
// systemd service inherits a minimal one, but it must APPEND the system dirs
// rather than replace PATH: an absolute export drops an aws CLI installed
// anywhere else, and Ubuntu's snap package installs it to /snap/bin. A hook that
// cannot find `aws` salvages nothing, which is the bug it exists to fix.
//
// This fails against the substituting version: the stub becomes unreachable and
// no upload is attempted.
func TestFlushHookFindsAwsOutsideTheSystemDirs(t *testing.T) {
	_ = os.Remove(CompletionRecordPath())
	script := GenerateFlushScript(flushSpec(), "results-bucket", "us-east-1", "run-9")
	_, awsCalls := runFlushHook(t, script, string(ExitTTLExpired))

	if len(awsCalls) == 0 {
		t.Fatal("the hook never invoked aws — an aws CLI outside the system dirs is unreachable")
	}
	if !strings.Contains(strings.Join(awsCalls, "\n"), "completion.json") {
		t.Errorf("expected a completion.json upload, got:\n%s", strings.Join(awsCalls, "\n"))
	}
}

// TestFlushHookIsValidBash catches a quoting error in the generator without
// needing to execute anything — the script is built by string concatenation, so
// this is the cheapest guard against an unterminated heredoc.
func TestFlushHookIsValidBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := GenerateFlushScript(flushSpec(), "b", "us-east-1", "r")
	path := filepath.Join(t.TempDir(), "flush.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("generated flush hook is not valid bash: %v\n%s\n---\n%s", err, out, script)
	}
}

// TestFlushHookQuotesAHostileTaskID. task_id reaches this script from a spec
// file, and the script is assembled by concatenation — an unquoted id with a
// quote or a `$(…)` in it would be a command-injection vector in a root-run
// script.
func TestFlushHookQuotesAHostileTaskID(t *testing.T) {
	spec := flushSpec()
	spec.TaskID = `x'; touch /tmp/spawn-pwned-632; echo '`
	script := GenerateFlushScript(spec, "b", "us-east-1", "r")

	if strings.Contains(script, "touch /tmp/spawn-pwned-632; echo") &&
		!strings.Contains(script, `'"'"'`) && !strings.Contains(script, `\'`) {
		t.Errorf("hostile task_id is not shell-quoted:\n%s", script)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	path := filepath.Join(t.TempDir(), "flush.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("hostile task_id produced invalid bash: %v\n%s", err, out)
	}
}

// TestWrapperPersistsStartedAtForTheFlushHook: the hook is a separate process
// and cannot read the wrapper's shell variables, so without this file a flushed
// record would have to omit started_at or invent one.
func TestWrapperPersistsStartedAtForTheFlushHook(t *testing.T) {
	w := GenerateWrapper(flushSpec(), "b", "us-east-1", false, "r")
	if !strings.Contains(w, shQuote(StartedAtPath())) {
		t.Errorf("the wrapper must persist STARTED_AT to %s for the flush hook\n---\n%s", StartedAtPath(), w)
	}
	// It has to be written EARLY — before the user command, which is the whole
	// window in which the flush hook might need it.
	atIdx := strings.Index(w, shQuote(StartedAtPath()))
	cmdIdx := strings.Index(w, "# ---- run user command ----")
	if atIdx < 0 || cmdIdx < 0 || atIdx > cmdIdx {
		t.Errorf("STARTED_AT must be persisted before the user command runs (at@%d cmd@%d)", atIdx, cmdIdx)
	}
}

// TestFlushHookStagingFileIsNotTheInterlockFile. The hook writes its record to a
// different filename than the one whose absence it checks. If they were the same
// file, a hook invoked twice (or re-invoked after a partial failure) would see
// its OWN output and conclude the wrapper had finished, silently skipping the
// flush it was called to perform.
func TestFlushHookStagingFileIsNotTheInterlockFile(t *testing.T) {
	if terminalFileName == completionFileName {
		t.Fatal("the flush hook's staging file must not be the file its interlock tests for")
	}
	script := GenerateFlushScript(flushSpec(), "b", "us-east-1", "r")
	// The interlock reads the wrapper's path; the write targets the staging path.
	if !strings.Contains(script, "if [ -f "+shQuote(CompletionRecordPath())+" ]") {
		t.Errorf("missing the wrapper-record interlock\n---\n%s", script)
	}
	if !strings.Contains(script, "cat > "+shQuote(localPath(terminalFileName))) {
		t.Errorf("the record must be staged to %s\n---\n%s", localPath(terminalFileName), script)
	}
}
