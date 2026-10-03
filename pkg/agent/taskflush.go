package agent

import (
	"context"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// taskFlushTimeout bounds the terminal-flush hook. It is a log upload plus two
// small S3 PUTs, so seconds in the normal case; the cap exists so a wedged hook
// (a hung `aws` call against a throttled endpoint) cannot delay the shutdown it
// is attached to. Deliberately well under the spot window's ~2 minutes, because
// this runs ahead of any user pre-stop hook on that path too.
const taskFlushTimeout = 45 * time.Second

// flushTaskRecord runs the task terminal-flush hook, if this instance has one.
//
// Why here (spawn#632): a task killed by a lifecycle limit mid-command left no
// completion.json and no command.log, because both of those are written by the
// wrapper AFTER the user command returns. The log is the one artifact that
// answers "how far did it get" — the wrapper's spawn_phase() markers are already
// in it — so losing it means the next attempt's TTL is another blind guess.
//
// This runs from runPreStop, which is called synchronously before
// provider.Terminate/Stop/Hibernate on every exit path spored mediates. That
// matters: the alternative, a SIGTERM trap inside the wrapper, only fires during
// OS shutdown and would race its S3 upload against network teardown. Here the
// instance is still fully healthy and the API calls are ordinary.
//
// It is best-effort by construction. Every failure is logged and swallowed: a
// missing hook is the normal case for a non-task launch, and a hook that fails
// must never block the termination that protects the user's bill.
//
// Not covered, honestly: an instance killed WITHOUT spored's involvement — a
// direct `aws ec2 terminate-instances`, an out-of-band reaper, or a hard host
// failure. spored never runs its pre-stop path there, so there is nothing to
// hang this off. Closing that case needs a signal from the terminating side
// (e.g. the reaper's graceful SSM path), which is a separate change.
func (a *Agent) flushTaskRecord(why taskproto.ExitReason) {
	if a.taskFlushDone {
		return
	}

	// A plain `spawn launch` has no task identity or results prefix, so no hook is
	// installed and there is nothing to do. Stat rather than config: the bootstrap
	// writes the file only on the task path, which keeps this from needing a flag
	// plumbed through every launch surface.
	if _, err := os.Stat(taskproto.FlushScriptPath()); err != nil {
		return
	}

	// The completed path has already written the authoritative record by the time
	// spored acts on the signal. The hook's own interlock also catches this, but
	// not invoking it at all keeps the normal success path free of a pointless
	// root exec.
	if why == taskproto.ExitCompleted {
		return
	}

	a.taskFlushDone = true

	ctx, cancel := context.WithTimeout(context.Background(), taskFlushTimeout)
	defer cancel()

	log.Printf("task flush: writing a terminal completion record (reason: %s)", why)

	// Run as root (spored's own identity) and NOT via `su` to the instance user,
	// unlike the user pre-stop hook. The hook's job is to reach S3 with the
	// instance profile's credentials, which come from IMDS and are identical for
	// either user, and the file is root-owned 0700 precisely so an unprivileged
	// user cannot influence what root executes here.
	//
	// The reason token is passed as a separate argv element, never interpolated
	// into a shell string: it originates in this package as a typed constant, and
	// keeping it out of a shell means it stays that way.
	cmd := exec.CommandContext(ctx, taskproto.FlushScriptPath(), string(why))
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		log.Printf("task flush output: %s", out)
	}
	if err != nil {
		// Worth a loud line: the user is about to lose the instance, and if this
		// failed then the diagnostics they would reach for are not in S3.
		log.Printf("task flush: hook failed (%v) — no terminal record was written for this task", err)
		return
	}
	log.Printf("task flush: terminal record written")
}
