package taskpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spore-host/spawn/pkg/taskproto"
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
	// Results writes the failed completion record for a task whose spec does not
	// parse (#1149). The job script writes every other record, but it is rendered
	// from the spec, so for an unparseable one nothing else ever will. AccountID
	// and Region locate the default results prefix (spawn-results-<account>-
	// <region>/tasks), the one `spawn task status` reads. Nil Results means no
	// such record can be written: the worker logs that and still acks.
	Results   ResultsAPI
	AccountID string
	Region    string
	// now overrides time.Now for deterministic idle-timeout tests. Nil → time.Now.
	now func() time.Time
	// sleep overrides the claim-error backoff for tests, so they count retries
	// instead of waiting them out. Nil → sleepCtx.
	sleep func(ctx context.Context, d time.Duration) bool
}

// claimErrorBackoff is the pause between Claim attempts after a claim error.
const claimErrorBackoff = time.Second

// IsQueueGone reports whether err (from any Queue call, wrapped or not) is SQS's
// NonExistentQueue: the run's queue has been deleted, i.e. `spawn pool drain`
// ran. A worker that sees it has nothing left to do and drains (#1146).
//
// SQS also reports a caller with no access to the queue as NonExistentQueue, so
// this is true for a worker whose policy lacks the queue. Draining is the right
// response there too: retrying cannot grant the permission, and a worker that
// cannot reach its queue only bills.
func IsQueueGone(err error) bool {
	return err != nil && isNonExistentQueue(err)
}

// Run drives the pull loop until the worker has nothing left to do, then returns
// nil so the caller exits cleanly and the instance's on_complete (terminate)
// fires. The worker drains when:
//
//   - the queue has been empty for IdleTimeout (scale to zero);
//   - the queue no longer exists (`spawn pool drain` deleted it, #1146) — at
//     once, without retrying, since a deleted queue never comes back; or
//   - every Claim has failed for IdleTimeout. A transient claim error (throttle,
//     network) is retried, but a worker that has been unable to claim anything
//     for a whole idle window is idle, and retrying it until TTL only bills.
//
// A single task's failure does NOT stop the loop — it acks the task and
// continues, because a worker pool's job is to drain the queue, not to adjudicate
// task success (Nextflow's errorStrategy / the completion record own that). The
// only error Run returns is ctx's. Returns the count of tasks the worker executed
// (for logs/tests).
func (w *Worker) Run(ctx context.Context) (executed int, err error) {
	now := w.now
	if now == nil {
		now = time.Now
	}
	sleep := w.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	idleDeadline := now().Add(w.Config.IdleTimeout)

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return executed, ctxErr
		}

		claimed, cerr := w.Queue.Claim(ctx, w.Config.PollWaitSeconds)
		if cerr != nil {
			if IsQueueGone(cerr) {
				w.logf("queue is gone (pool drained); draining: %v", cerr)
				return executed, nil
			}
			if now().After(idleDeadline) {
				w.logf("no task claimed for %s, only claim errors; draining: %v", w.Config.IdleTimeout, cerr)
				return executed, nil
			}
			// A transient claim error shouldn't kill the worker: log and retry after
			// a short beat. A malformed message was already dropped by Claim.
			w.logf("claim error (continuing): %v", cerr)
			if !sleep(ctx, claimErrorBackoff) {
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

// runOne fetches, executes, acks, and resets a single claimed task. Once the spec
// is fetched, the task is ALWAYS acked (so it isn't redelivered forever on a
// permanent failure) and the workspace is ALWAYS reset (so nothing leaks into the
// next task). Who writes the completion record the submitter polls depends on
// how far the task got:
//
//   - the job script ran: the script wrote it, success or failure, and the pool
//     doesn't re-decide that;
//   - the spec does not parse: no script can be rendered, so the worker writes a
//     failed spec_error record itself before acking (#1149);
//   - the execer failed before the script started (could not render, write or
//     start it): the worker writes a failed record with the class the execer
//     names (spec_error or instance_health) before acking (#1237);
//   - the execer failed after the script started: NO record is written, since
//     the script may already have written its own, and the task is acked.
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

	// ValidatePooled as well as the parse (#1231): a spec staged by an older
	// CLI, or written to the spec prefix directly, never went through
	// Pool.Enqueue's check, and a refused spec can no more run here than an
	// unparseable one.
	if perr := parsePooledSpec(specJSON); perr != nil {
		// The spec can never run, on this worker or any other, so redelivery would
		// only spin. Write the failed record first so `task status` has an answer,
		// then ack. A record write that fails is logged and the task acked anyway:
		// retrying it would keep this worker busy (and billing) on a task that
		// cannot run, and leaving the message would redeliver it every visibility
		// timeout, resetting the idle clock so the pool never drains.
		w.logf("task %s: spec does not parse or is refused on a pool, cannot run: %v", taskID, perr)
		w.recordFailedBeforeRun(ctx, taskID, specJSON, taskproto.RetrySpecError)
		w.ack(ctx, c)
		return
	}

	exit, execErr := w.Execer.Exec(ctx, specJSON, dir)
	var notRun *notStartedError
	if errors.As(execErr, &notRun) {
		// The job script never started, so nothing wrote a record for this
		// attempt. Write one before the ack, as for an unparseable spec (#1237).
		w.logf("task %s: job script did not start: %v", taskID, execErr)
		w.recordFailedBeforeRun(ctx, taskID, specJSON, notRun.class)
	} else if execErr != nil {
		// The script started, then failed to finish in a way that is not an exit
		// code. It may have written its record, so the worker must not overwrite
		// it (see runOne's doc). Ack anyway so we don't spin on it.
		w.logf("task %s: exec error (exit %d): %v", taskID, exit, execErr)
	} else if exit != 0 {
		w.logf("task %s: completed with non-zero exit %d", taskID, exit)
	} else {
		w.logf("task %s: completed", taskID)
	}

	// Ack: delete the task so it isn't redelivered. A worker that dies BEFORE
	// reaching here leaves the task on the queue → redelivered → retried elsewhere.
	w.ack(ctx, c)
}

func (w *Worker) ack(ctx context.Context, c *Claimed) {
	if ackErr := w.Queue.Ack(ctx, c); ackErr != nil {
		w.logf("task %s: ack failed (may redeliver): %v", c.Ref.TaskID, ackErr)
	}
}

// parsePooledSpec parses specJSON and applies the pooled-task restrictions.
func parsePooledSpec(specJSON []byte) error {
	spec, err := taskproto.ParseSpec(specJSON)
	if err != nil {
		return err
	}
	return spec.ValidatePooled()
}

// recordFailedBeforeRun writes the failed completion record for a task whose
// job script never ran, logging rather than returning any failure (see runOne).
func (w *Worker) recordFailedBeforeRun(ctx context.Context, taskID string, specJSON []byte, class taskproto.RetryClass) {
	now := w.now
	if now == nil {
		now = time.Now
	}
	if w.Results == nil {
		w.logf("task %s: no results writer configured; no completion record written", taskID)
		return
	}
	uri, err := recordFailedBeforeRun(ctx, w.Results, w.AccountID, w.Region, taskID, specJSON, class, now())
	if err != nil {
		w.logf("task %s: could not write the failed completion record: %v", taskID, err)
		return
	}
	w.logf("task %s: wrote failed completion record %s (retry_class %s)", taskID, uri, class)
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
