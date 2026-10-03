package agent

import (
	"context"
	"fmt"
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
	hook := taskproto.FlushScriptPath()
	if _, err := os.Stat(hook); err != nil {
		return
	}

	// Refuse to exec a hook that anyone but its owner could have modified. spored
	// runs as root, so this is the difference between "root runs the script spawn
	// installed" and "root runs whatever is at that path now".
	//
	// The root-owned 0700 /etc/spawn directory is the primary protection — an
	// unprivileged user cannot create or replace a file there at all. This is the
	// residual check for the case where something else has loosened the file, and
	// it is strictly more than the previous constant path offered: a constant tells
	// you where you are about to exec, not whether it is still trustworthy.
	if err := verifyHookTrustworthy(hook); err != nil {
		log.Printf("task flush: refusing to run %s: %v — no terminal record will be written", hook, err)
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
	//
	// Semgrep's dangerous-exec-command fires here because the path is not a
	// literal. It is a package variable whose ONLY writer is
	// taskproto.SetFlushScriptPathForTest — never an environment variable, a
	// config file, an EC2 tag, the task spec, or anything else crossing a trust
	// boundary. No shell is involved (no -c, no interpolation), so `why` is
	// argv[1] and cannot become code. verifyHookTrustworthy above additionally
	// refuses the exec unless the target is a regular, non-symlink file writable
	// only by its owner — a guard the previous constant path did not have, since a
	// constant tells you where you will exec, not whether the target is still
	// trustworthy. The suppression must sit on the line immediately above the
	// call; anything further up is silently ignored.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, hook, string(why))
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

// verifyHookTrustworthy reports whether path is safe for a root process to
// execute: a regular file (not a symlink, not a directory or device) that no one
// but its owner can write.
//
// Deliberately portable rather than checking uid 0 via syscall.Stat_t: that
// needs a linux/windows/other build-tag trio (see sys_*.go) for a hook that only
// ever exists on Linux, and "owner-only writable inside a root-owned directory"
// is the property that actually matters. os.Lstat, not os.Stat, so a symlink
// planted at the path is rejected instead of silently followed to its target.
func verifyHookTrustworthy(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("it is a symlink; the exec target must be the file itself")
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("it is not a regular file (mode %s)", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("it is group- or world-writable (mode %04o); anyone could choose what runs here", perm)
	}
	return nil
}
