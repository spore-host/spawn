package taskpool

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// The default fakeFetcher spec's task id is "x", so this is where `task status`
// reads its record.
const wantDefaultSpecRecord = "s3://spawn-results-123456789012-us-west-2/tasks/x/completion.json"

// runOneWith drains one task (the default, valid fakeFetcher spec) through a
// worker using ex and ws, and returns the queue and results bucket.
func runOneWith(t *testing.T, ex TaskExecer, ws WorkspaceProvider) (*fakeSQS, *fakeResults) {
	t.Helper()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSQS(clk.now)
	results := &fakeResults{objects: map[string]string{}, liveAtPut: map[string]int{}, queue: f}
	w := newWorker(t, f, ex, ws, clk, 5*time.Second)
	w.Results = results
	w.AccountID = "123456789012"
	w.Region = "us-west-2"
	if err := w.Queue.Submit(context.Background(), TaskRef{TaskID: "x", SpecURI: "s3://b/specs/x.json"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := make(chan struct{})
	go func() { _, _ = w.Run(context.Background()); close(done) }()
	if !drainClock(t, clk, done) {
		t.Fatal("worker did not drain")
	}
	return f, results
}

// TestWorker_ExecerFailureBeforeScriptWritesFailedRecordBeforeAck is the #1237
// gate. ScriptExecer can fail before the job script runs: the script cannot be
// written (a bad workspace, a full disk) or started (bash missing). Nothing
// writes a record then, and the worker used to ack anyway, so `spawn task
// status` reported the task unknown forever. Driven through the real
// ScriptExecer so the gate covers Exec's classification, not just the worker's.
func TestWorker_ExecerFailureBeforeScriptWritesFailedRecordBeforeAck(t *testing.T) {
	cases := map[string]func(t *testing.T) (TaskExecer, WorkspaceProvider){
		"script cannot be written": func(t *testing.T) (TaskExecer, WorkspaceProvider) {
			// countingWorkspace hands out /ws/... dirs that do not exist.
			return &ScriptExecer{AccountID: "123456789012", Region: "us-west-2"}, &countingWorkspace{}
		},
		"script cannot be started": func(t *testing.T) (TaskExecer, WorkspaceProvider) {
			return &ScriptExecer{
				AccountID: "123456789012", Region: "us-west-2",
				Bash: filepath.Join(t.TempDir(), "no-such-bash"),
			}, &DirWorkspace{Root: t.TempDir()}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			ex, ws := setup(t)
			f, results := runOneWith(t, ex, ws)

			body, ok := results.objects[wantDefaultSpecRecord]
			if !ok {
				t.Fatalf("no completion record at %s; wrote %v", wantDefaultSpecRecord, keys(results.objects))
			}
			rec, err := taskproto.ParseCompletionRecord([]byte(body))
			if err != nil {
				t.Fatalf("record does not parse: %v", err)
			}
			if rec.TaskID != "x" || rec.State != taskproto.StateFailed || rec.ExitCode != -1 {
				t.Errorf("record = %+v, want task x, state failed, exit -1", rec)
			}
			// The issue's classification: a fresh worker usually succeeds.
			if rec.RetryClass != taskproto.RetryInstanceHealth || !rec.RetryClass.Retryable() {
				t.Errorf("retry_class = %q (retryable=%v), want instance_health (retryable)", rec.RetryClass, rec.RetryClass.Retryable())
			}
			if live := results.liveAtPut[wantDefaultSpecRecord]; live != 1 {
				t.Errorf("record written with %d message(s) on the queue, want 1 (written before the ack)", live)
			}
			if f.liveCount() != 0 {
				t.Errorf("task not acked: %d remain", f.liveCount())
			}
		})
	}
}

// TestScriptExecer_RenderFailureIsSpecError: a spec that parses but cannot be
// rendered into a pooled job script is a spec problem, not the worker's
// (#1237), so it must be marked not-started with class spec_error. The worker
// refuses this spec before Exec today (#1231), which is why Exec is called
// directly here: the classification has to hold for any caller.
func TestScriptExecer_RenderFailureIsSpecError(t *testing.T) {
	ws := &DirWorkspace{Root: t.TempDir()}
	dir, err := ws.Acquire("nf-bad")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Reset(dir) }()
	if _, err := taskproto.ParseSpec([]byte(dockerSocketSpecJSON)); err != nil {
		t.Fatalf("fixture must parse (it must fail at render, not parse): %v", err)
	}
	ex := &ScriptExecer{AccountID: "123456789012", Region: "us-west-2"}
	_, err = ex.Exec(context.Background(), []byte(dockerSocketSpecJSON), dir)
	var ns *notStartedError
	if !errors.As(err, &ns) {
		t.Fatalf("Exec error = %v, want a not-started error", err)
	}
	if ns.class != taskproto.RetrySpecError {
		t.Errorf("class = %q, want spec_error", ns.class)
	}
}

// TestWorker_NotStartedClassReachesTheRecord: the worker writes whatever class
// the execer names, rather than a fixed one.
func TestWorker_NotStartedClassReachesTheRecord(t *testing.T) {
	ex := &recordingExecer{exitFor: func(int) (int, error) {
		return -1, notStarted(taskproto.RetrySpecError, "build job script: %w", errors.New("bad"))
	}}
	_, results := runOneWith(t, ex, &countingWorkspace{})
	rec, err := taskproto.ParseCompletionRecord([]byte(results.objects[wantDefaultSpecRecord]))
	if err != nil {
		t.Fatalf("no parseable record: %v (wrote %v)", err, keys(results.objects))
	}
	if rec.RetryClass != taskproto.RetrySpecError {
		t.Errorf("retry_class = %q, want spec_error", rec.RetryClass)
	}
}

// TestWorker_ErrorAfterScriptStartedWritesNoRecord: a failure once the script
// is running (a non-ExitError from Wait) may come after the script wrote its
// own record, and the worker must not overwrite it (#1237). Still acked.
func TestWorker_ErrorAfterScriptStartedWritesNoRecord(t *testing.T) {
	ex := &recordingExecer{exitFor: func(int) (int, error) {
		return -1, errors.New("run job script: wait: i/o error")
	}}
	f, results := runOneWith(t, ex, &countingWorkspace{})
	if len(results.objects) != 0 {
		t.Errorf("worker wrote %v after the script started; it may overwrite the script's record", keys(results.objects))
	}
	if f.liveCount() != 0 {
		t.Errorf("task not acked: %d remain", f.liveCount())
	}
}
