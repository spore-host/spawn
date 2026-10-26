package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// taskFlushTimeout bounds the terminal-flush hook. It is a log upload plus two
// small S3 PUTs, so seconds in the normal case; the cap exists so a wedged hook
// (a hung `aws` call against a throttled endpoint) cannot delay the shutdown it
// is attached to. Deliberately well under the spot window's ~2 minutes, because
// this runs ahead of any user pre-stop hook on that path too.
//
// That is the hook's base: its log and record. A hook that stages outputs
// (spawn#1297) gets the seconds it grants on top; see flushBudget.
const taskFlushTimeout = 45 * time.Second

// taskFlushOutputsSlack covers what the hook spends past its own output
// deadline (spawn#1297): timeout(1)'s 2 s kill grace and a retry's last backoff.
const taskFlushOutputsSlack = taskproto.FlushOutputsSlackSeconds * time.Second

// spotPreStopMargin is how much of a Spot notice the pre-stop path leaves
// unused (spawn#1311): the pre-stop deadline is the interruption time minus
// this. It holds the launch Elastic IP release, which runs spotEIPReleaseLead
// before the interruption and takes the instance's egress with it on a
// multi-interface instance, plus 5 s for the notifications that follow the
// pre-stop and for clock error. EC2 gives 2 minutes and spored sees the notice
// up to 5 s late, so the pre-stop as a whole gets about 90 s.
const spotPreStopMargin = spotEIPReleaseLead + 5*time.Second

// spotPreStopDeadline is the one deadline a Spot pre-stop shares (spawn#1311).
// The task flush and the user's PreStop hook used to be bounded separately
// (up to 90 s each), so together they could outlast the notice, and whatever
// ran last was cut off by the reclaim with nothing saying so.
func spotPreStopDeadline(info *provider.InterruptionInfo) time.Time {
	return info.Time.Add(-spotPreStopMargin)
}

// spotClock is the clock the Spot notice handler measures its deadline with.
func (a *Agent) spotClock() time.Time {
	if a.spotNow != nil {
		return a.spotNow()
	}
	return time.Now()
}

// flushBudget returns the seconds of output stage-out spored grants the hook
// (passed as its $2) and the timeout it runs the hook under. declared is the
// hook's own budget (taskproto.FlushHookOutputSeconds), 0 for a hook with no
// outputs or from before #1297, which then keeps the 45 s bound.
//
// bounded says the flush must end within left, the time remaining before a
// Spot pre-stop deadline (spawn#1311). The record's 45 s are taken first,
// because the record is the run's TTL and accounting evidence; outputs get
// only what remains after it and the slack, and a flush with less than 45 s
// left runs under what there is.
func flushBudget(declared int, bounded bool, left time.Duration) (grant int, timeout time.Duration) {
	grant = min(max(declared, 0), taskproto.MaxFlushOutputsSeconds)
	timeout = taskFlushTimeout
	if bounded {
		timeout = min(taskFlushTimeout, max(left, 0))
		grant = min(grant, max(0, int((left-taskFlushTimeout-taskFlushOutputsSlack)/time.Second)))
	}
	if grant > 0 {
		timeout += time.Duration(grant)*time.Second + taskFlushOutputsSlack
	}
	return grant, timeout
}

// hookHeadBytes is how much of the hook spored reads for its declared budget.
const hookHeadBytes = 4096

// hookOutputSeconds reads the output budget the hook at path declares.
// Unreadable means none, so the hook keeps the base bound.
func hookOutputSeconds(path string) int {
	f, err := os.Open(path) //nolint:gosec // the hook path is a package constant, trust-checked by the caller
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, hookHeadBytes)
	n, _ := io.ReadFull(f, head)
	return taskproto.FlushHookOutputSeconds(head[:n])
}

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
// Not covered here: an instance killed WITHOUT spored's involvement. spored
// never runs its pre-stop path there, so the terminating side runs the hook
// over SSM instead: `spawn terminate` (#802) and the TTL reaper (#1186) both
// send taskproto.FlushCommand. A direct `aws ec2 terminate-instances` or a hard
// host failure still gets nothing.
//
// deadline is the Spot pre-stop deadline (spotPreStopDeadline), or the zero
// time on every other path. With one, the flush is sized to end by it, and is
// not run at all once it has passed.
func (a *Agent) flushTaskRecord(why taskproto.ExitReason, deadline time.Time) {
	if a.taskFlushDone {
		return
	}

	// A plain `spawn launch` has no task identity or results prefix, so no hook is
	// installed and there is nothing to do. Stat rather than config: the bootstrap
	// writes the file only on the task path, which keeps this from needing a flag
	// plumbed through every launch surface.
	//
	// A TASK instance without its hook is different (#1186): the record it was
	// meant to write will not exist, and that has to be visible after the
	// instance is gone, so it is reported like a failure.
	hook := taskproto.FlushScriptPath()
	if _, err := os.Stat(hook); err != nil {
		if c := a.cfg(); c != nil && c.TaskID != "" && why != taskproto.ExitCompleted {
			a.taskFlushDone = true
			a.reportFlushOutcome(why, flushOutcome{status: flushSkipped, reason: "hook-missing",
				output: fmt.Sprintf("%s: %v", hook, err)})
		}
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
		if why != taskproto.ExitCompleted {
			a.taskFlushDone = true
			a.reportFlushOutcome(why, flushOutcome{status: flushFailed, reason: "hook-refused", output: err.Error()})
		}
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

	bounded := !deadline.IsZero()
	var left time.Duration
	if bounded {
		if left = deadline.Sub(a.spotClock()); left <= 0 {
			a.reportFlushOutcome(why, flushOutcome{status: flushSkipped, reason: "no-time-left",
				output: fmt.Sprintf("the Spot notice's pre-stop deadline %s had passed", deadline.Format(time.RFC3339))})
			return
		}
	}
	grant, timeout := flushBudget(hookOutputSeconds(hook), bounded, left)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	log.Printf("task flush: writing a terminal completion record (reason: %s, output budget %d s, timeout %v)", why, grant, timeout)
	args := []string{string(why)}
	if grant > 0 {
		args = append(args, strconv.Itoa(grant))
	}

	// Run as root (spored's own identity) and NOT via `su` to the instance user,
	// unlike the user pre-stop hook. The hook's job is to reach S3 with the
	// instance profile's credentials, which come from IMDS and are identical for
	// either user, and the file is root-owned 0700 precisely so an unprivileged
	// user cannot influence what root executes here.
	//
	// The reason token is passed as a separate argv element, never interpolated
	// into a shell string: it originates in this package as a typed constant, and
	// keeping it out of a shell means it stays that way. The output grant, when
	// there is one, is argv[2], a decimal spored formats itself.
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
	cmd := exec.CommandContext(ctx, hook, args...)
	// WaitDelay (#1186): the context kills the hook, but not an `aws` child it
	// started, and that child holds the output pipe open, so without this Wait
	// blocks until the child exits and the 45 s cap does not bound anything.
	cmd.WaitDelay = a.flushWaitDelay()
	out, err := cmd.CombinedOutput()
	a.reportFlushOutcome(why, classifyFlush(ctx, cmd, out, err))
}

// taskFlushWaitDelay is how long the hook's output pipes may stay open after it
// exits or is killed (exec.Cmd.WaitDelay). Short: it only has to outlast the
// hook writing its last lines.
const taskFlushWaitDelay = 5 * time.Second

func (a *Agent) flushWaitDelay() time.Duration {
	if a.taskFlushWaitDelay > 0 {
		return a.taskFlushWaitDelay
	}
	return taskFlushWaitDelay
}

// The flush outcomes recorded in the spawn:flush-status tag (#1186).
const (
	flushOK      = "ok"
	flushFailed  = "failed"
	flushSkipped = "skipped"
)

// flushStatusTag records the flush outcome on the instance. Tags stay readable
// for about an hour after termination, so `aws ec2 describe-tags` (or spawn
// status) can say what happened when the results prefix is empty.
const flushStatusTag = "spawn:flush-status"

// flushOutcome is what one flush attempt came to.
type flushOutcome struct {
	status string // flushOK, flushFailed or flushSkipped
	reason string // a short token: hook-missing, hook-refused, timeout, upload-failed, exit-N, start-failed
	output string // the hook's combined output, or the error that stopped it running
}

func (o flushOutcome) tagValue() string {
	if o.reason == "" {
		return o.status
	}
	return o.status + ":" + o.reason
}

// classifyFlush turns the hook's exit into a flushOutcome.
func classifyFlush(ctx context.Context, cmd *exec.Cmd, out []byte, err error) flushOutcome {
	o := flushOutcome{output: string(out)}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		o.status, o.reason = flushFailed, "timeout"
	case err == nil:
		o.status = flushOK
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success():
		// The hook finished cleanly; only a child it left behind held the pipe.
		o.status = flushOK
	case errors.As(err, &exitErr) && exitErr.ExitCode() == taskproto.FlushExitUploadFailed:
		o.status, o.reason = flushFailed, "upload-failed"
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		o.status, o.reason = flushFailed, fmt.Sprintf("exit-%d", exitErr.ExitCode())
	case cmd.ProcessState == nil:
		o.status, o.reason = flushFailed, "start-failed"
		o.output += err.Error()
	default:
		o.status, o.reason = flushFailed, "killed"
		o.output += err.Error()
	}
	return o
}

// reportFlushOutcome makes the flush outcome visible after the instance is gone
// (#1186). A TTL-killed task once left an empty results prefix and nothing
// anywhere saying why: the hook's output and status went only to spored's
// journal, which dies with the disk.
//
// Every outcome is logged in full and written to the spawn:flush-status tag. A
// failure or skip is also written to the serial console with the hook's output
// and the command log's tail (both readable with get-console-output after
// termination), and sent as a pre_stop_failed notification naming the flush.
//
// Bounded and best-effort: this sits in front of the terminate that protects
// the user's bill. The tag write has its own short timeout, the console write
// is a local device write, and the notification is fire-and-forget.
func (a *Agent) reportFlushOutcome(why taskproto.ExitReason, o flushOutcome) {
	value := o.tagValue()
	if out := strings.TrimSpace(o.output); out != "" {
		log.Printf("task flush output:\n%s", out)
	}
	if o.status == flushOK {
		log.Printf("task flush: terminal record written (%s)", value)
	} else {
		// Worth a loud line: the user is about to lose the instance, and if this
		// failed then the diagnostics they would reach for are not in S3.
		log.Printf("task flush: %s (reason: %s) — this task's terminal record may not be in S3", value, why)
	}

	if a.tagger != nil && a.identity != nil && a.identity.InstanceID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), flushReportTimeout)
		if err := a.tagger.putTags(ctx, a.identity.InstanceID, map[string]string{flushStatusTag: truncateTagValue(value, 255)}); err != nil {
			log.Printf("task flush: could not write %s=%s: %v", flushStatusTag, value, err)
		}
		cancel()
	}

	if o.status == flushOK {
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n%s %s (reason: %s) ===\n", consoleFlushStartPrefix, value, why)
	if tail := lastLines(o.output, consoleTailLines); tail != "" {
		b.WriteString(tail)
		if !strings.HasSuffix(tail, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString(consoleFlushEndMarker + "\n")
	if err := a.writeConsole(b.String()); err != nil {
		log.Printf("task flush: could not write to the serial console: %v", err)
	}
	// The log is what did not reach S3, so put its tail where it survives too.
	a.writeCommandLogToConsole("task flush " + value)

	// pre_stop_failed, not an event of its own: the flush is the first step of
	// the pre-stop path, and spore-bot formats only the events it lists
	// (spored_events_test.go), so a new name would reach users as a generic
	// fallback message.
	a.notifier.Notify(context.Background(), "pre_stop_failed", preStopDetail("task flush "+value, tailBytes(o.output, 512)))
}

// The frame around a flush report on the serial console. Distinct from the
// command-log block's markers (taskproto.ConsoleLogStartPrefix), which `spawn
// logs` extracts, so this block is never mistaken for the log.
const (
	consoleFlushStartPrefix = "=== task flush report:"
	consoleFlushEndMarker   = "=== end task flush report ==="
)

// flushReportTimeout bounds the spawn:flush-status tag write.
const flushReportTimeout = 5 * time.Second

// lastLines returns at most the last n lines of s.
func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

// tailBytes returns at most the last n bytes of s, trimmed.
func tailBytes(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
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
