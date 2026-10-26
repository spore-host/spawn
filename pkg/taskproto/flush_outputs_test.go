package taskproto

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The terminal-flush hook's stage-out of declared outputs (spawn#1297).

// outputsRun is one run of a hook with outputs.
type outputsRun struct {
	flushRun
	timeouts []string // argv of every stub timeout call, when stubbed
	rec      *CompletionRecord
}

// runOutputsHook runs script with args ($1 reason, then spored's grant, if
// any) under a stand-in aws (awsBody, as in runFlushHookWith) and a recording
// sleep. With stubTimeout the timeout(1) on PATH only records its arguments
// and runs the command; otherwise the real one is used, and the test skips
// when there is none. It returns the parsed terminal record.
func runOutputsHook(t *testing.T, script string, args []string, awsBody string, stubTimeout bool) outputsRun {
	t.Helper()
	skipOnWindows(t, "runs the Linux flush hook under bash with stubs on a POSIX PATH")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if !stubTimeout {
		if _, err := exec.LookPath("timeout"); err != nil {
			t.Skip("timeout(1) not available")
		}
	}
	_ = os.Remove(CompletionRecordPath())
	_ = os.Remove(localPath(terminalFileName))
	t.Cleanup(func() { _ = os.Remove(localPath(terminalFileName)) })

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "task-flush.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture, needs +x
		t.Fatal(err)
	}
	binDir := t.TempDir()
	awsLog, sleepLog, toLog := filepath.Join(dir, "aws.log"), filepath.Join(dir, "sleep.log"), filepath.Join(dir, "timeout.log")
	writeFakeExe(t, filepath.Join(binDir, "aws"), "#!/bin/bash\nCALLS="+awsLog+"\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\n"+awsBody)
	writeFakeExe(t, filepath.Join(binDir, "sleep"), "#!/bin/bash\nprintf '%s\\n' \"$*\" >> "+sleepLog+"\n")
	if stubTimeout {
		// timeout -k 2 <seconds> cmd...: record, then run cmd.
		writeFakeExe(t, filepath.Join(binDir, "timeout"), "#!/bin/bash\nprintf '%s\\n' \"$*\" >> "+toLog+"\nshift 3\nexec \"$@\"\n")
	}

	cmd := exec.Command("bash", append([]string{scriptPath}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	b, err := cmd.CombinedOutput()
	r := outputsRun{flushRun: flushRun{out: string(b)}}
	if ee, ok := err.(*exec.ExitError); ok {
		r.exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run hook: %v", err)
	}
	r.aws, r.sleeps, r.timeouts = readLines(awsLog), readLines(sleepLog), readLines(toLog)
	data, err := os.ReadFile(localPath(terminalFileName))
	if err != nil {
		t.Fatalf("no terminal record written: %v\n%s", err, r.out)
	}
	if r.rec, err = ParseCompletionRecord(data); err != nil {
		t.Fatalf("%v\nraw: %s\n%s", err, data, r.out)
	}
	return r
}

// outputsSpec declares a file output and a directory output, both existing,
// plus extra, and a command log for the hook to upload.
func outputsSpec(t *testing.T, budget *int, extra ...Manifest) *TaskSpec {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "tool.log")
	outDir := filepath.Join(dir, "counters")
	if err := os.WriteFile(file, []byte("progress 41%\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "spawn-command.log")
	if err := os.WriteFile(logFile, []byte("spawn_phase command start\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := flushCommandLogPath
	flushCommandLogPath = logFile
	t.Cleanup(func() { flushCommandLogPath = prev })

	s := flushSpec()
	s.Lifecycle.FlushOutputsSeconds = budget
	s.Outputs = append([]Manifest{
		{Source: file, Destination: "s3://out-bucket/run/tool.log"},
		{Source: outDir + "/", Destination: "s3://out-bucket/run/counters/"},
	}, extra...)
	return s
}

func intp(n int) *int { return &n }

// indexOfCall returns the index of the first aws call containing sub, or -1.
func indexOfCall(calls []string, sub string) int {
	for i, c := range calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// TestFlushHookStagesDeclaredOutputsOnALifecycleKill is #1297's repro: a
// cost_limit kill uploaded command.log and the record but none of the spec's
// outputs. Each must now be copied as the wrapper copies it (--recursive for a
// directory), after the log and before the record, which says complete.
func TestFlushHookStagesDeclaredOutputsOnALifecycleKill(t *testing.T) {
	for _, why := range []ExitReason{ExitCostLimitExceeded, ExitTTLExpired} {
		t.Run(string(why), func(t *testing.T) {
			spec := outputsSpec(t, nil)
			r := runOutputsHook(t, genFlush(spec, "results-bucket", "us-east-1", "run-1"),
				[]string{string(why), "60"}, "exit 0\n", false)

			file, dir := spec.Outputs[0], spec.Outputs[1]
			wantFile := "s3 cp --no-progress " + file.Source + " " + file.Destination
			wantDir := "s3 cp --no-progress --recursive " + dir.Source + " " + dir.Destination
			for _, want := range []string{wantFile, wantDir} {
				if countContaining(r.aws, want) != 1 {
					t.Errorf("want one %q; aws calls:\n%s\n%s", want, strings.Join(r.aws, "\n"), r.out)
				}
			}
			logAt, outAt, recAt := indexOfCall(r.aws, "/command.log"), indexOfCall(r.aws, wantFile), indexOfCall(r.aws, "/completion.json")
			if logAt < 0 || recAt < 0 || logAt >= outAt || outAt >= recAt {
				t.Errorf("order: command.log@%d, output@%d, completion.json@%d; want log, outputs, record", logAt, outAt, recAt)
			}
			if r.rec.OutputsStaged != OutputsComplete || len(r.rec.OutputsNotStaged) != 0 {
				t.Errorf("outputs_staged=%q not_staged=%v, want complete and none", r.rec.OutputsStaged, r.rec.OutputsNotStaged)
			}
			if r.rec.TerminalReason != why {
				t.Errorf("terminal_reason = %q, want %q", r.rec.TerminalReason, why)
			}
			if r.exitCode != 0 {
				t.Errorf("exit %d, want 0\n%s", r.exitCode, r.out)
			}
		})
	}
}

// TestFlushHookCutsASlowOutputOffAtTheBudget: an output still uploading at the
// deadline is killed by timeout(1), recorded as timed out, and the record and
// the log still land. Uses the real timeout(1) with a two-second grant; the
// slow upload is a sleep the kill ends (exec'd, so nothing outlives it). Two
// seconds, not one: $SECONDS counts whole seconds, so with one the second
// output could find the budget already spent before it starts.
func TestFlushHookCutsASlowOutputOffAtTheBudget(t *testing.T) {
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	slow := filepath.Join(t.TempDir(), "huge.bin")
	if err := os.WriteFile(slow, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := outputsSpec(t, nil, Manifest{Source: slow, Destination: "s3://out-bucket/run/huge.bin"})
	r := runOutputsHook(t, genFlush(spec, "results-bucket", "us-east-1", "run-1"),
		[]string{string(ExitTTLExpired), "2"},
		"case \"$*\" in *huge.bin*) exec "+realSleep+" 30 ;; esac\nexit 0\n", false)

	if r.rec.OutputsStaged != OutputsPartial {
		t.Errorf("outputs_staged = %q, want partial\n%s", r.rec.OutputsStaged, r.out)
	}
	want := []OutputNotStaged{{Source: slow, Reason: OutputTimedOut}}
	if len(r.rec.OutputsNotStaged) != 1 || r.rec.OutputsNotStaged[0] != want[0] {
		t.Errorf("outputs_not_staged = %+v, want %+v", r.rec.OutputsNotStaged, want)
	}
	if n := countContaining(r.aws, "huge.bin"); n != 1 {
		t.Errorf("the cut-off output was attempted %d times, want 1: a timeout is not retried", n)
	}
	for _, key := range []string{"/command.log", "/completion.json", "/.exitcode"} {
		if countContaining(r.aws, key) != 1 {
			t.Errorf("%s not uploaded after the output timed out:\n%s", key, strings.Join(r.aws, "\n"))
		}
	}
	if r.exitCode != 0 {
		t.Errorf("exit %d: a timed-out output must not fail the flush, which is about the record\n%s", r.exitCode, r.out)
	}
}

// TestFlushHookOutputsDisabled: flush_outputs_seconds: 0 stages nothing and
// says so, and declares no budget for spored to grant.
func TestFlushHookOutputsDisabled(t *testing.T) {
	spec := outputsSpec(t, intp(0))
	script := genFlush(spec, "results-bucket", "us-east-1", "run-1")
	if got := FlushHookOutputSeconds([]byte(script)); got != 0 {
		t.Errorf("disabled hook declares %d s, want 0", got)
	}
	r := runOutputsHook(t, script, []string{string(ExitCostLimitExceeded), "60"}, "exit 0\n", true)
	if n := countContaining(r.aws, "out-bucket"); n != 0 {
		t.Errorf("%d output uploads with flush_outputs_seconds 0, want none:\n%s", n, strings.Join(r.aws, "\n"))
	}
	if r.rec.OutputsStaged != OutputsDisabled {
		t.Errorf("outputs_staged = %q, want disabled", r.rec.OutputsStaged)
	}
}

// TestFlushHookStagesNothingWithoutAGrant: run over SSM (spawn terminate, the
// reaper) or by a spored older than #1297, the hook gets no $2 and runs under
// a 45 s bound with no room for outputs. It must not spend it on them.
func TestFlushHookStagesNothingWithoutAGrant(t *testing.T) {
	spec := outputsSpec(t, nil)
	r := runOutputsHook(t, genFlush(spec, "results-bucket", "us-east-1", "run-1"),
		[]string{string(ExitUserTerminated)}, "exit 0\n", true)
	if n := countContaining(r.aws, "out-bucket"); n != 0 || len(r.timeouts) != 0 {
		t.Errorf("outputs attempted with no grant (%d aws, %d timeout calls)", n, len(r.timeouts))
	}
	if r.rec.OutputsStaged != OutputsSkipped || len(r.rec.OutputsNotStaged) != 2 {
		t.Errorf("outputs_staged=%q not_staged=%+v, want skipped with both listed", r.rec.OutputsStaged, r.rec.OutputsNotStaged)
	}
	if countContaining(r.aws, "/completion.json") != 1 {
		t.Errorf("record not uploaded:\n%s", strings.Join(r.aws, "\n"))
	}
}

// TestFlushHookNeverTakesMoreThanTheSpecAllows: the grant is spored's, the
// ceiling is the spec's. Counted through the timeout(1) seam: the first copy
// is given at most the spec's 30 s however much spored passes.
func TestFlushHookNeverTakesMoreThanTheSpecAllows(t *testing.T) {
	spec := outputsSpec(t, intp(30))
	r := runOutputsHook(t, genFlush(spec, "results-bucket", "us-east-1", "run-1"),
		[]string{string(ExitTTLExpired), "999"}, "exit 0\n", true)
	if len(r.timeouts) != 2 {
		t.Fatalf("timeout calls = %v, want one per output", r.timeouts)
	}
	for _, c := range r.timeouts {
		f := strings.Fields(c)
		if len(f) < 3 || f[0] != "-k" {
			t.Errorf("timeout %q: want -k 2 <seconds> ...", c)
			continue
		}
		if n, err := strconv.Atoi(f[2]); err != nil || n < 1 || n > 30 {
			t.Errorf("timeout %q: want 1..30 s, the spec's budget", c)
		}
	}
	if r.rec.OutputsStaged != OutputsComplete {
		t.Errorf("outputs_staged = %q, want complete", r.rec.OutputsStaged)
	}
}

// TestFlushHookReportsMissingAndFailedOutputs: an output the task had not
// written yet is "missing" and not attempted; one whose upload keeps failing
// is retried like the record's uploads, then "failed", without failing the
// flush.
func TestFlushHookReportsMissingAndFailedOutputs(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-yet.txt")
	spec := outputsSpec(t, nil, Manifest{Source: missing, Destination: "s3://out-bucket/run/not-yet.txt"})
	r := runOutputsHook(t, genFlush(spec, "results-bucket", "us-east-1", "run-1"),
		[]string{string(ExitIdleTimeout), "60"},
		"case \"$*\" in *tool.log*) echo 'AccessDenied' >&2; exit 1 ;; esac\nexit 0\n", true)

	if n := countContaining(r.aws, "tool.log"); n != flushUploadAttempts {
		t.Errorf("failing output attempted %d times, want %d", n, flushUploadAttempts)
	}
	if countContaining(r.aws, "not-yet.txt") != 0 {
		t.Error("a missing output was uploaded")
	}
	got := map[string]string{}
	for _, o := range r.rec.OutputsNotStaged {
		got[filepath.Base(o.Source)] = o.Reason
	}
	if got["tool.log"] != OutputFailed || got["not-yet.txt"] != OutputMissing || len(got) != 2 {
		t.Errorf("outputs_not_staged = %+v", r.rec.OutputsNotStaged)
	}
	if r.rec.OutputsStaged != OutputsPartial || r.exitCode != 0 {
		t.Errorf("outputs_staged=%q exit=%d, want partial and 0\n%s", r.rec.OutputsStaged, r.exitCode, r.out)
	}
}

// TestFlushHookDeclaresItsOutputBudget: spored sizes its timeout from the
// header, so it must carry the spec's effective budget, and none for a task
// with no outputs.
func TestFlushHookDeclaresItsOutputBudget(t *testing.T) {
	for _, tc := range []struct {
		budget  *int
		outputs bool
		want    int
	}{
		{nil, true, DefaultFlushOutputsSeconds},
		{intp(120), true, 120},
		{intp(0), true, 0},
		{nil, false, 0},
	} {
		s := flushSpec()
		s.Lifecycle.FlushOutputsSeconds = tc.budget
		if tc.outputs {
			s.Outputs = []Manifest{{Source: "/tmp/o", Destination: "s3://b/o"}}
		}
		if got := FlushHookOutputSeconds([]byte(genFlush(s, "b", "r", "run"))); got != tc.want {
			t.Errorf("budget %v outputs %v: declared %d, want %d", tc.budget, tc.outputs, got, tc.want)
		}
	}
	if got := FlushHookOutputSeconds([]byte("#!/bin/bash\n" + flushOutputsHeader + "100000\n")); got != MaxFlushOutputsSeconds {
		t.Errorf("an out-of-range declaration reads as %d, want the max %d", got, MaxFlushOutputsSeconds)
	}
}

// TestFlushOutputsSecondsIsValidated: a negative or over-cap budget is a spec
// error, not a silent clamp.
func TestFlushOutputsSecondsIsValidated(t *testing.T) {
	for _, n := range []int{-1, MaxFlushOutputsSeconds + 1} {
		s := flushSpec()
		s.Lifecycle.FlushOutputsSeconds = intp(n)
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "flush_outputs_seconds") {
			t.Errorf("flush_outputs_seconds %d: err = %v, want a flush_outputs_seconds error", n, err)
		}
	}
	for _, n := range []int{0, MaxFlushOutputsSeconds} {
		s := flushSpec()
		s.Lifecycle.FlushOutputsSeconds = intp(n)
		if err := s.Validate(); err != nil {
			t.Errorf("flush_outputs_seconds %d: %v", n, err)
		}
	}
}

// TestOlderRecordsParseWithoutOutputsStaged: a record from before #1297 has
// no outputs_staged, and must read as "not reported".
func TestOlderRecordsParseWithoutOutputsStaged(t *testing.T) {
	rec, err := ParseCompletionRecord([]byte(`{"task_id":"t","exit_code":-1,"state":"failed","terminal_reason":"ttl_expired"}`))
	if err != nil {
		t.Fatal(err)
	}
	if rec.OutputsStaged != "" || rec.OutputsNotStaged != nil {
		t.Errorf("got %q %v, want both empty", rec.OutputsStaged, rec.OutputsNotStaged)
	}
	b, _ := json.Marshal(rec)
	if strings.Contains(string(b), "outputs_") {
		t.Errorf("an unset outcome is written out: %s", b)
	}
}
