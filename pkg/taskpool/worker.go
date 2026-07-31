package taskpool

import (
	"context"
	"fmt"
	"io"
	"time"
)

// SpecFetcher loads a staged TaskSpec's raw JSON by its s3:// URI. Abstracted so
// the worker loop is unit-testable without S3 (the integration test wires the
// real S3 client; unit tests inject an in-memory map).
type SpecFetcher interface {
	Fetch(ctx context.Context, specURI string) ([]byte, error)
}

// TaskExecer runs one already-fetched task to completion on THIS worker: stage
// inputs, run the command (host or container), stage outputs, and write the
// durable completion record to S3. It returns the task's exit code (0 = success).
//
// This is the per-job unit the pool reuses in place of a whole instance. Its
// implementation reuses the taskproto stage/run/complete contract (the same
// sequence GenerateWrapper emits for the one-instance-per-task path) MINUS the
// self-terminate signal — a pooled worker must keep pulling, not shut down. It is
// an interface here so the loop's control flow (claim → run → ack → reset →
// idle-drain) is tested without executing real commands.
type TaskExecer interface {
	// Exec runs the task whose spec JSON is specJSON in the given clean workspace
	// dir. It must be self-contained: any failure is reported as a non-nil error
	// OR a non-zero exit code, never a panic, so the loop can ack and continue.
	Exec(ctx context.Context, specJSON []byte, workspaceDir string) (exitCode int, err error)
}

// WorkspaceProvider hands the loop a CLEAN per-task workspace and reclaims it
// afterward. Workspace isolation on reuse is the #70 gotcha: a reused worker
// carries the prior task's files (e.g. CALL_VARIANTS leaves *.bam/*.vcf.gz), so
// each task must run in a fresh dir. Reset() is called after every task,
// success or fail, so leftovers never leak into the next job.
type WorkspaceProvider interface {
	// Acquire returns a clean workspace dir for a task id.
	Acquire(taskID string) (dir string, err error)
	// Reset reclaims the workspace after a task (removes its contents).
	Reset(dir string) error
}

// WorkerConfig configures one worker's pull loop.
type WorkerConfig struct {
	// PollWaitSeconds is the SQS long-poll wait per Claim (0..20). 20 minimizes
	// empty round-trips while a worker waits for the next task.
	PollWaitSeconds int32
	// IdleTimeout is how long a worker keeps polling an empty queue before it
	// drains itself (returns from Run). This is the scale-to-zero property: once
	// the fan-out is done, workers self-exit and the instance terminates → $0.
	// The reaper is the backstop if this is ever missed.
	IdleTimeout time.Duration
	// Log receives one-line progress messages; nil discards them.
	Log io.Writer
}

// Worker drains a run-scoped queue: claim → fetch spec → run in a clean workspace
// → ack → reset, until the queue is idle for IdleTimeout. It is the fungible unit
// the cohort pool launches N of; every worker runs this identical loop.
type Worker struct {
	Queue     *Queue
	Fetcher   SpecFetcher
	Execer    TaskExecer
	Workspace WorkspaceProvider
	Config    WorkerConfig
	// now overrides time.Now for deterministic idle-timeout tests. Nil → time.Now.
	now func() time.Time
}

// Run drives the pull loop until the queue is idle for the configured
// IdleTimeout, then returns. It returns a non-nil error only for a condition that
// should stop THIS worker (e.g. the queue vanished); a single task's failure is
// NOT such a condition — the loop acks it and continues, because a worker pool's
// job is to drain the queue, not to adjudicate task success (Nextflow's
// errorStrategy / the completion record own that). Returns the count of tasks the
// worker executed (for logs/tests).
func (w *Worker) Run(ctx context.Context) (executed int, err error) {
	now := w.now
	if now == nil {
		now = time.Now
	}
	idleDeadline := now().Add(w.Config.IdleTimeout)

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return executed, ctxErr
		}

		claimed, cerr := w.Queue.Claim(ctx, w.Config.PollWaitSeconds)
		if cerr != nil {
			// A claim error (throttle, transient) shouldn't kill the worker: log and
			// retry after a short beat. A malformed message was already dropped by
			// Claim, so this is a transport issue — keep the pool alive.
			w.logf("claim error (continuing): %v", cerr)
			if !sleepCtx(ctx, time.Second) {
				return executed, ctx.Err()
			}
			continue
		}

		if claimed == nil {
			// Queue empty right now. If we've been idle past the deadline, drain.
			if now().After(idleDeadline) {
				w.logf("idle for %s with empty queue; draining", w.Config.IdleTimeout)
				return executed, nil
			}
			continue // long-poll already waited; loop and poll again
		}

		// Got a task — reset the idle clock and run it.
		idleDeadline = now().Add(w.Config.IdleTimeout)
		w.runOne(ctx, claimed)
		executed++
	}
}

// runOne fetches, executes, acks, and resets a single claimed task. Every failure
// mode is contained: the task is ALWAYS acked (so it isn't redelivered forever on
// a permanent failure) and the workspace is ALWAYS reset (so nothing leaks into
// the next task). A task that legitimately failed still wrote a completion record
// via the execer, which the submitter polls — the pool doesn't re-decide that.
func (w *Worker) runOne(ctx context.Context, c *Claimed) {
	taskID := c.Ref.TaskID

	dir, err := w.Workspace.Acquire(taskID)
	if err != nil {
		// Can't get a workspace → we cannot run this task on this worker. Do NOT ack:
		// let it redeliver to a (possibly healthier) worker after the visibility
		// timeout. Reset nothing (no dir acquired).
		w.logf("task %s: workspace acquire failed (leaving for redelivery): %v", taskID, err)
		return
	}
	// From here the workspace is ours; always reset it when done.
	defer func() {
		if rerr := w.Workspace.Reset(dir); rerr != nil {
			w.logf("task %s: workspace reset failed: %v", taskID, rerr)
		}
	}()

	specJSON, err := w.Fetcher.Fetch(ctx, c.Ref.SpecURI)
	if err != nil {
		// Spec fetch failed. This is likely transient (S3 blip) — leave the task for
		// redelivery rather than acking a task we never ran.
		w.logf("task %s: spec fetch failed (leaving for redelivery): %v", taskID, err)
		return
	}

	exit, execErr := w.Execer.Exec(ctx, specJSON, dir)
	if execErr != nil {
		// The execer itself errored (couldn't even run the protocol). It should have
		// written a failed completion record; regardless, ack so we don't spin on it.
		w.logf("task %s: exec error (exit %d): %v", taskID, exit, execErr)
	} else if exit != 0 {
		w.logf("task %s: completed with non-zero exit %d", taskID, exit)
	} else {
		w.logf("task %s: completed", taskID)
	}

	// Ack: the task ran and wrote its completion record (success or failure). We
	// delete it so it isn't redelivered. A worker that dies BEFORE reaching here
	// leaves the task on the queue → redelivered → retried elsewhere.
	if ackErr := w.Queue.Ack(ctx, c); ackErr != nil {
		w.logf("task %s: ack failed (may redeliver): %v", taskID, ackErr)
	}
}

func (w *Worker) logf(format string, a ...interface{}) {
	if w.Config.Log == nil {
		return
	}
	fmt.Fprintf(w.Config.Log, "[taskpool worker] "+format+"\n", a...)
}

// sleepCtx sleeps d or until ctx is done; returns false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
