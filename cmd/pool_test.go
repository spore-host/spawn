package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestPoolWorkerPolicy_Valid checks the scoped worker IAM policy is well-formed
// and correctly scoped: valid JSON, SQS limited to THIS run's queue ARN (never
// "*"), and the spec/results bucket + spawn-binaries granted. The real-AWS smoke
// caught that workers had NO SQS access at all (bare spored role) — this locks in
// the fix.
func TestPoolWorkerPolicy_Valid(t *testing.T) {
	pol := poolWorkerPolicy("123456789012", "us-east-1", "spawn-results-123456789012-us-east-1", "nf-run-abc", nil, nil)

	// Well-formed JSON.
	var doc struct {
		Version   string
		Statement []struct {
			Effect   string
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(pol), &doc); err != nil {
		t.Fatalf("worker policy is not valid JSON: %v\n%s", err, pol)
	}
	if doc.Version != "2012-10-17" {
		t.Errorf("policy Version = %q", doc.Version)
	}

	// SQS actions must be present and scoped to THIS run's queue ARN, not "*".
	var sqsStmt *struct {
		Effect   string
		Action   []string
		Resource []string
	}
	for i := range doc.Statement {
		for _, a := range doc.Statement[i].Action {
			if strings.HasPrefix(a, "sqs:") {
				sqsStmt = &doc.Statement[i]
				break
			}
		}
	}
	if sqsStmt == nil {
		t.Fatal("worker policy grants NO sqs actions — a worker can't pull from the queue (the bug the smoke found)")
	}
	wantSQS := map[string]bool{"sqs:GetQueueUrl": true, "sqs:ReceiveMessage": true, "sqs:DeleteMessage": true, "sqs:GetQueueAttributes": true}
	for _, a := range sqsStmt.Action {
		delete(wantSQS, a)
	}
	if len(wantSQS) > 0 {
		t.Errorf("worker policy missing SQS actions: %v", wantSQS)
	}
	for _, r := range sqsStmt.Resource {
		if r == "*" || !strings.Contains(r, "spawn-pool-nf-run-abc") {
			t.Errorf("SQS resource must be scoped to the run queue, got %q", r)
		}
	}

	// The spec/results bucket must be reachable (GetObject + PutObject somewhere).
	if !strings.Contains(pol, "spawn-results-123456789012-us-east-1") {
		t.Error("worker policy doesn't grant the spec/results bucket")
	}
	// The bootstrap's spored binary download must be allowed.
	if !strings.Contains(pol, "spawn-binaries-*") {
		t.Error("worker policy doesn't allow the spored binary download")
	}
}

// TestBuildPoolWorkerCommand_RestartContract checks the on-instance worker loop
// (#465): it exports the pool env, restarts on a non-zero exit, stops on exit 0
// (clean drain), and is bounded. Verified by generating the script and asserting
// its shape + bash-syntax validity.
func TestBuildPoolWorkerCommand_RestartContract(t *testing.T) {
	cmd := buildPoolWorkerCommand("nf-run-1", "spawn-results-x", "staging", "5m", taskCompletionFile)

	for _, want := range []string{
		`export SPAWN_POOL_RUN_ID="nf-run-1"`,
		`export SPAWN_POOL_SPEC_BUCKET="spawn-results-x"`,
		`spored pool-worker --idle-timeout 5m`,
		`while :; do`, // loop
		`_rc=$?`,      // capture exit
		`-eq 0 ]`,     // exit-0 branch
		`break`,       // stop on clean drain
		`sleep 10`,    // backoff before restart
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("worker command missing %q\n---\n%s", want, cmd)
		}
	}

	// Bounded: references the max-restarts guard.
	if !strings.Contains(cmd, "-ge 20") {
		t.Errorf("worker command should bound restarts at %d\n%s", poolWorkerMaxRestarts, cmd)
	}

	// Valid bash (the generated command runs as-is on the instance).
	dir := t.TempDir()
	f := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(f, []byte("#!/bin/bash\n"+cmd), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("generated worker command is not valid bash: %v\n%s", err, out)
	}
}

// TestBuildPoolWorkerCommand_ExitZeroStops functionally verifies the contract by
// running a stubbed loop: a fake `spored` that exits 0 must run exactly once (no
// restart), proving a clean drain terminates the instance rather than looping.
func TestBuildPoolWorkerCommand_ExitZeroStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: runs the generated pool-worker loop, which runs on a Linux instance, under bash with a stub on a :-separated PATH (#1002)")
	}
	dir := t.TempDir()
	// Stub `spored` on PATH that records each call and exits 0.
	stub := filepath.Join(dir, "spored")
	countFile := filepath.Join(dir, "count")
	script := "#!/bin/bash\necho x >> " + countFile + "\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatalf("stub: %v", err)
	}
	body := buildPoolWorkerCommand("r", "b", "staging", "1s", filepath.Join(dir, "SPAWN_COMPLETE"))
	runner := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(runner, []byte("#!/bin/bash\nexport PATH="+dir+":$PATH\n"+body), 0o700); err != nil {
		t.Fatalf("runner: %v", err)
	}
	if out, err := exec.Command("bash", runner).CombinedOutput(); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(countFile)
	if got := strings.Count(string(data), "x"); got != 1 {
		t.Errorf("exit-0 worker ran %d times, want exactly 1 (no restart on clean drain)", got)
	}
}

// TestPoolWorkerPolicy_ExtraBuckets: --s3-read/--s3-write widen the worker policy
// to a task's own input/output buckets (the documented per-task-bucket limitation
// fix), while keeping least-privilege by default. Read buckets get GetObject;
// write buckets get Get/Put/Delete.
func TestPoolWorkerPolicy_ExtraBuckets(t *testing.T) {
	pol := poolWorkerPolicy("123456789012", "us-east-1", "spawn-results-123456789012-us-east-1", "nf-run-abc",
		[]string{"my-inputs", "s3://ref-data/genomes"}, []string{"my-outputs"})

	var doc struct {
		Statement []struct {
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(pol), &doc); err != nil {
		t.Fatalf("policy not valid JSON with extra buckets: %v\n%s", err, pol)
	}

	// Read bucket → GetObject on my-inputs/* and ref-data/* (prefix stripped to bucket).
	if !strings.Contains(pol, `"arn:aws:s3:::my-inputs/*"`) {
		t.Error("read bucket my-inputs not granted GetObject")
	}
	if !strings.Contains(pol, `"arn:aws:s3:::ref-data/*"`) {
		t.Error("read bucket ref-data (from s3://ref-data/genomes) not granted; prefix should be stripped to bucket")
	}
	// Write bucket → Delete present for my-outputs, absent for a read-only bucket.
	var sawWriteDelete bool
	for _, s := range doc.Statement {
		hasDelete := false
		for _, a := range s.Action {
			if a == "s3:DeleteObject" {
				hasDelete = true
			}
		}
		if hasDelete {
			for _, r := range s.Resource {
				if strings.Contains(r, "my-outputs") {
					sawWriteDelete = true
				}
				if strings.Contains(r, "my-inputs") {
					t.Error("read-only bucket my-inputs must NOT get DeleteObject")
				}
			}
		}
	}
	if !sawWriteDelete {
		t.Error("write bucket my-outputs should get DeleteObject (read/write access)")
	}
}

// runPoolWorkerScript runs the generated worker loop under bash with a stub
// `spored` that exits rc, and a no-op `sleep` so the restart backoff costs
// nothing. Returns the completion file's contents ("" if it was never written)
// and how many times the stub ran.
func runPoolWorkerScript(t *testing.T, rc int) (record string, runs int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only: runs the generated pool-worker loop, which runs on a Linux instance, under bash with stubs on a :-separated PATH (#1002)")
	}
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	stubs := map[string]string{
		"spored": "#!/bin/bash\necho x >> " + countFile + "\nexit " + strconv.Itoa(rc) + "\n",
		"sleep":  "#!/bin/bash\nexit 0\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			t.Fatalf("stub %s: %v", name, err)
		}
	}
	completion := filepath.Join(dir, "SPAWN_COMPLETE")
	runner := filepath.Join(dir, "run.sh")
	script := "#!/bin/bash\nexport PATH=" + dir + ":$PATH\n" + buildPoolWorkerCommand("r", "b", "staging", "1s", completion)
	if err := os.WriteFile(runner, []byte(script), 0o700); err != nil {
		t.Fatalf("runner: %v", err)
	}
	if out, err := exec.Command("bash", runner).CombinedOutput(); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(countFile)
	rec, _ := os.ReadFile(completion)
	return string(rec), strings.Count(string(data), "x")
}

// TestPoolWorkerCleanExitWritesTheCompletionRecord is #1146: a worker that
// drains cleanly must write the completion record spored watches, or
// on_complete=terminate never fires and the worker bills until its TTL. Run, not
// grepped: the record must land at the path the script was given, as JSON
// spored's completion reader accepts.
func TestPoolWorkerCleanExitWritesTheCompletionRecord(t *testing.T) {
	rec, runs := runPoolWorkerScript(t, 0)
	if runs != 1 {
		t.Fatalf("clean-exit worker ran %d times, want 1", runs)
	}
	if rec == "" {
		t.Fatal("a cleanly drained worker wrote no completion record, so on_complete=terminate never fires (#1146)")
	}
	var got struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(rec), &got); err != nil {
		t.Fatalf("completion record is not JSON: %v\n%s", err, rec)
	}
	if got.Status != "completed" || got.ExitCode != 0 {
		t.Errorf("completion record = %+v, want status completed, exit 0", got)
	}
}

// TestPoolWorkerThatGivesUpWritesAFailedRecord: a worker that exhausts its
// restarts can do no more work. It used to stop there and wait for the TTL; it
// must write a failed record so it terminates instead.
func TestPoolWorkerThatGivesUpWritesAFailedRecord(t *testing.T) {
	rec, runs := runPoolWorkerScript(t, 3)
	if runs != poolWorkerMaxRestarts {
		t.Fatalf("failing worker ran %d times, want %d (bounded restarts)", runs, poolWorkerMaxRestarts)
	}
	var got struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(rec), &got); err != nil {
		t.Fatalf("a worker that gave up wrote no valid completion record (%v): %q", err, rec)
	}
	if got.Status != "failed" || got.ExitCode != 3 {
		t.Errorf("completion record = %+v, want status failed, exit 3", got)
	}
}

// TestPoolWorkerScriptWritesTheFileSporedWatches ties the two halves together:
// the production script writes taskCompletionFile with an anchored heredoc
// redirect (not a mention in a comment or echo), and the launch config tells
// spored to watch that same file and terminate on it.
func TestPoolWorkerScriptWritesTheFileSporedWatches(t *testing.T) {
	cfg := poolWorkerBaseConfig("us-east-1", "ami-1", "prof", "")
	if cfg.OnComplete != "terminate" {
		t.Errorf("OnComplete = %q, want terminate (owner decision on #1146)", cfg.OnComplete)
	}
	if cfg.CompletionFile != taskCompletionFile {
		t.Fatalf("CompletionFile = %q, want %q", cfg.CompletionFile, taskCompletionFile)
	}
	script := buildPoolWorkerCommand("r", "b", "staging", "5m", cfg.CompletionFile)
	re := regexp.MustCompile(`(?m)^\s*cat > "` + regexp.QuoteMeta(cfg.CompletionFile) + `" <<JSONEOF$`)
	if n := len(re.FindAllString(script, -1)); n != 2 {
		t.Errorf("script writes %s %d times, want 2 (clean drain + give up)\n%s", cfg.CompletionFile, n, script)
	}
}

// TestPoolCostLimitReachesTheWorkerLaunchConfig is #1183: --cost-limit is a
// per-worker ceiling handed to spored through the launch config, as for
// `spawn launch`. Driven through the real flag so a rename or a lost binding fails.
func TestPoolCostLimitReachesTheWorkerLaunchConfig(t *testing.T) {
	old := poolCostLimit
	t.Cleanup(func() { poolCostLimit = old; _ = poolCreateCmd.Flags().Set("cost-limit", "0") })
	if err := poolCreateCmd.Flags().Set("cost-limit", "2.5"); err != nil {
		t.Fatalf("set --cost-limit: %v", err)
	}
	if got := poolWorkerBaseConfig("us-east-1", "ami-1", "prof", "").CostLimit; got != 2.5 {
		t.Errorf("worker launch config CostLimit = %v, want 2.5", got)
	}
}

// TestPoolDrainMessageAlreadyDrained: the second drain says so, and does not
// claim to have deleted anything.
func TestPoolDrainMessageAlreadyDrained(t *testing.T) {
	msg := poolDrainMessage("r1", true)
	if !strings.Contains(msg, "already drained") || strings.Contains(msg, "queue deleted") {
		t.Errorf("already-drained message = %q", msg)
	}
}
