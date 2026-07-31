package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	cmd := buildPoolWorkerCommand("nf-run-1", "spawn-results-x", "staging", "5m")

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
	dir := t.TempDir()
	// Stub `spored` on PATH that records each call and exits 0.
	stub := filepath.Join(dir, "spored")
	countFile := filepath.Join(dir, "count")
	script := "#!/bin/bash\necho x >> " + countFile + "\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatalf("stub: %v", err)
	}
	body := buildPoolWorkerCommand("r", "b", "staging", "1s")
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
