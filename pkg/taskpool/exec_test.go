package taskpool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// specJSON builds a minimal host (no-container, no-I/O) TaskSpec whose command is
// the given argv, marshaled to JSON — what a worker fetches from S3. No inputs or
// outputs, so the job script's only S3 calls are the best-effort completion-record
// uploads (which fail without AWS but don't affect the command's exit code — the
// wrapper deliberately does not `set -e`).
func specJSON(t *testing.T, taskID string, argv ...string) []byte {
	t.Helper()
	spec := &taskproto.TaskSpec{
		TaskID:    taskID,
		Command:   argv,
		Resources: taskproto.ResourceRequest{Architecture: "x86_64"},
		Lifecycle: taskproto.Lifecycle{TTL: "1h", OnComplete: "terminate"},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	return b
}

// TestScriptExecer_RunsAndReportsExit runs a real job script on the host and
// checks the exit code flows through: 0 for a succeeding command, the command's
// own code for a failure. (The completion-record S3 uploads fail with no AWS, but
// the script doesn't `set -e`, so `exit $rc` still carries the command's status.)
func TestScriptExecer_RunsAndReportsExit(t *testing.T) {
	ws := &DirWorkspace{Root: t.TempDir()}
	ex := &ScriptExecer{ResultsBucket: "spawn-results-x", Region: "us-east-1"}

	t.Run("success", func(t *testing.T) {
		dir, err := ws.Acquire("nf-ok")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer ws.Reset(dir)
		// Write a sentinel so we can confirm the command ran in the workspace dir.
		rc, err := ex.Exec(context.Background(), specJSON(t, "nf-ok", "bash", "-c", "echo hi > ran.txt; exit 0"), dir)
		if err != nil {
			t.Fatalf("Exec error: %v", err)
		}
		if rc != 0 {
			t.Fatalf("exit = %d, want 0", rc)
		}
		if _, err := os.Stat(filepath.Join(dir, "ran.txt")); err != nil {
			t.Errorf("command did not run in the workspace dir: %v", err)
		}
	})

	t.Run("failure exit code flows through", func(t *testing.T) {
		dir, err := ws.Acquire("nf-fail")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer ws.Reset(dir)
		rc, err := ex.Exec(context.Background(), specJSON(t, "nf-fail", "bash", "-c", "exit 17"), dir)
		if err != nil {
			t.Fatalf("Exec error (a task failure is exitCode!=0, not an error): %v", err)
		}
		if rc != 17 {
			t.Fatalf("exit = %d, want 17", rc)
		}
	})
}

// TestScriptExecer_BadSpecIsError: an unparseable/invalid spec is an executer
// error (couldn't run), distinct from a task that ran and failed.
func TestScriptExecer_BadSpecIsError(t *testing.T) {
	ws := &DirWorkspace{Root: t.TempDir()}
	ex := &ScriptExecer{ResultsBucket: "b", Region: "us-east-1"}
	dir, _ := ws.Acquire("nf-bad")
	defer ws.Reset(dir)

	if _, err := ex.Exec(context.Background(), []byte("{not json"), dir); err == nil {
		t.Fatal("expected an error for malformed spec JSON")
	}
}

// TestDirWorkspace_IsolatesAndResets: each Acquire yields a distinct clean dir,
// Reset removes it, and a redelivered task id gets a fresh (empty) dir even if a
// stale one lingered — the #70 workspace-isolation-on-reuse guarantee.
func TestDirWorkspace_IsolatesAndResets(t *testing.T) {
	root := t.TempDir()
	ws := &DirWorkspace{Root: root}

	d1, err := ws.Acquire("nf-a")
	if err != nil {
		t.Fatalf("Acquire a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d1, "leftover.bam"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	d2, err := ws.Acquire("nf-b")
	if err != nil {
		t.Fatalf("Acquire b: %v", err)
	}
	if d1 == d2 {
		t.Fatalf("two tasks got the same workspace dir %q (not isolated)", d1)
	}

	// Reset d1, then re-acquire the SAME task id: must be a clean dir, no leftover.
	if err := ws.Reset(d1); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := os.Stat(d1); !os.IsNotExist(err) {
		t.Errorf("Reset did not remove %q", d1)
	}
	d1again, err := ws.Acquire("nf-a")
	if err != nil {
		t.Fatalf("re-Acquire a: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d1again, "leftover.bam")); !os.IsNotExist(err) {
		t.Errorf("re-acquired workspace still has the prior task's leftover.bam (not clean on reuse)")
	}
}

// TestDirWorkspace_ResetRefusesOutsideRoot: Reset must never remove a path that
// isn't under Root — a guard against a bug wiping an unrelated dir.
func TestDirWorkspace_ResetRefusesOutsideRoot(t *testing.T) {
	ws := &DirWorkspace{Root: t.TempDir()}
	other := t.TempDir()
	if err := ws.Reset(other); err == nil {
		t.Fatalf("Reset should refuse a path outside Root; got nil error for %q", other)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("Reset removed an out-of-root dir it should have refused: %v", err)
	}
	// A sanitized odd task id still lands under Root and resets fine.
	dir, err := ws.Acquire("../../etc/passwd")
	if err != nil {
		t.Fatalf("Acquire odd id: %v", err)
	}
	if !strings.HasPrefix(dir, ws.Root) {
		t.Fatalf("workspace %q escaped Root %q", dir, ws.Root)
	}
	if err := ws.Reset(dir); err != nil {
		t.Errorf("Reset of sanitized in-root dir failed: %v", err)
	}
}

// TestDirWorkspace_NotWorldWritable pins the workspace mode (#459). The job script
// runs as the same user that creates the dir — spored is User=root and the pooled
// script has no `su -` — so nothing needs group/other write. A world-writable
// workspace would let any local account tamper with a live task's files, including
// the .spawn-job.sh that Exec writes there and then executes.
func TestDirWorkspace_NotWorldWritable(t *testing.T) {
	ws := &DirWorkspace{Root: t.TempDir()}
	dir, err := ws.Acquire("nf-perms")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat workspace: %v", err)
	}
	perm := fi.Mode().Perm()
	if perm&0o022 != 0 {
		t.Errorf("workspace %q mode = %#o, want no group/other write bits (0o022 clear)", dir, perm)
	}
	if perm != 0o700 {
		t.Errorf("workspace %q mode = %#o, want 0700", dir, perm)
	}
	// The owner must still be able to create the job script and cd into the dir.
	if err := os.WriteFile(filepath.Join(dir, ".probe"), []byte("x"), 0o600); err != nil {
		t.Errorf("owner cannot write into its own workspace: %v", err)
	}
}

// TestDirWorkspace_ReacquireResetsMode covers the MkdirAll-is-a-no-op-on-existing-dir
// case: if a stale dir survives with loose perms, Acquire must still hand back a
// 0700 workspace rather than silently inheriting them.
func TestDirWorkspace_ReacquireResetsMode(t *testing.T) {
	ws := &DirWorkspace{Root: t.TempDir()}
	dir, err := ws.Acquire("nf-stale")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod to simulate a stale loose dir: %v", err)
	}
	again, err := ws.Acquire("nf-stale")
	if err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	fi, err := os.Stat(again)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("re-acquired workspace mode = %#o, want 0700", perm)
	}
}
