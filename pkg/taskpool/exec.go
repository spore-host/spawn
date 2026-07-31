package taskpool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// ScriptExecer is the production TaskExecer: it parses a staged TaskSpec, renders
// the pooled per-job script (taskproto.GeneratePooledJobScript — the stage/run/
// complete wrapper MINUS the self-terminate signal, so the worker survives to run
// the next task), and runs it as a bash subprocess in the task's clean workspace.
//
// The job script writes the durable completion record to S3 itself (via the
// task's IAM role, same as the one-instance path), so this executer's return
// value is just the process exit code — the authoritative signal the submitter
// polls is completion.json, not this int.
type ScriptExecer struct {
	// ResultsBucket is the per-account results bucket the job script writes
	// completion.json/.exitcode into (s3://<bucket>/tasks/<task_id>/…). The worker
	// resolves it once at startup (spawn-results-<account>-<region>) and reuses it
	// for every job.
	ResultsBucket string
	// Region is used by the job script for any private-ECR login.
	Region string
	// Bash is the shell to invoke (default "bash"); overridable for tests.
	Bash string
	// Stdout/Stderr receive the job's merged output; nil discards. The job script
	// also tees its own log to /var/log/spawn-command.log on a real instance.
	Stdout, Stderr *os.File
}

// Exec renders and runs the pooled job script for one task in workspaceDir.
// It returns the script's exit code. A non-nil error means the script could not
// be launched at all (e.g. bad spec, bash missing) — distinct from the script
// running and exiting non-zero (a task failure, exitCode!=0, err==nil).
func (e *ScriptExecer) Exec(ctx context.Context, specJSON []byte, workspaceDir string) (int, error) {
	spec, err := taskproto.ParseSpec(specJSON)
	if err != nil {
		return -1, fmt.Errorf("parse task spec: %w", err)
	}

	script := taskproto.GeneratePooledJobScript(spec, e.ResultsBucket, e.Region)

	// Write the script into the workspace and run it there, so its relative paths
	// and any scratch files land in the isolated dir (which the worker resets after).
	scriptPath := filepath.Join(workspaceDir, ".spawn-job.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return -1, fmt.Errorf("write job script: %w", err)
	}

	bash := e.Bash
	if bash == "" {
		bash = "bash"
	}
	// nosemgrep: dangerous-exec-command -- bash is a constant; scriptPath is a file this method just wrote under a workspace dir we control (not user input). The task command inside the script is the workflow author's own, same trust model as `spawn task run`.
	cmd := exec.CommandContext(ctx, bash, scriptPath)
	cmd.Dir = workspaceDir
	cmd.Stdout = e.Stdout
	cmd.Stderr = e.Stderr

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start job script: %w", err)
	}
	werr := cmd.Wait()
	if werr == nil {
		return 0, nil
	}
	// A non-zero exit is a task failure, NOT an executer error: the script ran and
	// wrote its completion record, so report the code with a nil error and let the
	// worker ack it. Only a non-ExitError (couldn't run) is an executer error.
	var exitErr *exec.ExitError
	if errors.As(werr, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, fmt.Errorf("run job script: %w", werr)
}

// DirWorkspace is the production WorkspaceProvider: each task gets a fresh
// subdirectory under Root, removed after the task. This is the workspace-isolation
// answer to the #70 gotcha — a reused worker never sees the prior task's files.
type DirWorkspace struct {
	// Root is the parent dir workspaces are created under (e.g. /var/lib/nf-work
	// on a real worker). Created on first Acquire if absent.
	Root string
}

// Acquire creates and returns a clean per-task workspace dir. The task id is
// sanitized into the dir name so a hostile/odd id can't escape Root.
func (d *DirWorkspace) Acquire(taskID string) (string, error) {
	if err := os.MkdirAll(d.Root, 0o755); err != nil {
		return "", fmt.Errorf("create workspace root %s: %w", d.Root, err)
	}
	dir := filepath.Join(d.Root, "task-"+sanitize(taskID))
	// Remove any stale dir from a prior (redelivered) attempt of the same task, so
	// Acquire always yields a genuinely clean dir even on retry.
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clear stale workspace %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", fmt.Errorf("create workspace %s: %w", dir, err)
	}
	return dir, nil
}

// Reset removes the task's workspace and all its contents.
func (d *DirWorkspace) Reset(dir string) error {
	// Guard: only remove paths under Root, never an empty or unrelated dir.
	if dir == "" || d.Root == "" {
		return nil
	}
	rel, err := filepath.Rel(d.Root, dir)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return fmt.Errorf("refusing to reset %q: not under workspace root %q", dir, d.Root)
	}
	return os.RemoveAll(dir)
}

// sanitize maps a task id to a safe path segment: keep [A-Za-z0-9._-], replace the
// rest with '_'. Prevents '/' or '..' in an id from escaping the workspace root.
func sanitize(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return "task"
	}
	return string(b)
}
