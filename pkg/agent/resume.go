package agent

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

// A stopped instance boots a fresh spored. A hibernated one resumes the SAME
// process, with all its memory, so without help spored carries its
// pre-hibernate state into the new run (#987):
//
//   - preStopDone and taskFlushDone are still set, so the next stop, hibernate
//     or TTL terminate skips the user's pre-stop hook and the task record;
//   - Cleanup deleted the DNS name, and the hourly renewal would put it back
//     with the public IP from the first boot, which a resumed instance without
//     an EIP no longer has;
//   - lastActivityTime is from before the hibernation, so the first idle tick
//     finds the timeout long past and hibernates again within about a minute;
//   - the plugins Cleanup recorded as stopped stay stopped.
//
// spored detects the resume itself, on each monitor tick, rather than through a
// systemd sleep hook: a hook is another unit to install, and it only helps if
// it runs. The kernel clocks need neither.

// readSuspendedTime is how long this boot has spent suspended or hibernated:
// CLOCK_BOOTTIME minus CLOCK_MONOTONIC on Linux. It is read through
// readKernelClocks, the same kernel-clock reader that keeps hibernated time out
// of compute (#945), so resume detection and the compute clock can never
// disagree about whether, or for how long, the machine slept. Tests simulate a
// hibernation by standing in for that one reader.
func readSuspendedTime() (time.Duration, bool) {
	k, ok := readKernelClocks()
	if !ok {
		return 0, false
	}
	return k.suspended(), true
}

const (
	// resumeSuspendThreshold is how much the kernel's suspended time must grow
	// between two ticks to count as a resume. The reading only moves during a
	// suspend, so this only has to clear the nanoseconds between two clock
	// reads.
	resumeSuspendThreshold = time.Second

	// resumeWallJumpThreshold is how far the wall clock must run ahead of the
	// monotonic clock between two ticks to count as a resume, for a kernel that
	// did not account the sleep. Large, because an NTP step also moves the wall
	// clock; an EC2 hibernation is minutes at the least.
	resumeWallJumpThreshold = 5 * time.Minute
)

// tickReading is the clocks at one monitor tick.
type tickReading struct {
	wall        time.Time     // wall clock, monotonic reading stripped
	mono        time.Duration // Go's monotonic clock (CLOCK_MONOTONIC on Linux), since spored started
	suspended   time.Duration // the kernel's suspended time this boot
	suspendedOK bool
}

// resumedBetween reports whether the machine was suspended between two ticks,
// for about how long, and which signal said so. Pure, so both signals are
// tested without a hibernation.
func resumedBetween(prev, now tickReading) (bool, time.Duration, string) {
	if prev.suspendedOK && now.suspendedOK {
		if d := now.suspended - prev.suspended; d >= resumeSuspendThreshold {
			return true, d, "kernel suspended time"
		}
	}
	// Differently shaped, for a kernel without the sleep accounted in
	// CLOCK_BOOTTIME: Go's monotonic clock stops in a suspend, the wall clock
	// does not (once it is corrected after the resume).
	if !prev.wall.IsZero() {
		if d := now.wall.Sub(prev.wall) - (now.mono - prev.mono); d >= resumeWallJumpThreshold {
			return true, d, "wall clock ran ahead of the monotonic clock"
		}
	}
	return false, 0, ""
}

// readTick takes this tick's clock readings.
func (a *Agent) readTick() tickReading {
	r := tickReading{wall: time.Now().Round(0), mono: time.Since(a.startTime)}
	r.suspended, r.suspendedOK = readSuspendedTime()
	return r
}

// checkResume compares this tick's clocks with the last tick's, and resets the
// per-boot state when the machine was suspended in between. The first call only
// takes the baseline. Called from the monitor loop only.
func (a *Agent) checkResume(ctx context.Context) bool {
	now := a.readTick()
	prev, had := a.lastTick, a.lastTickSet
	a.lastTick, a.lastTickSet = now, true
	if !had {
		return false
	}
	resumed, gap, how := resumedBetween(prev, now)
	if !resumed {
		return false
	}
	a.onResume(ctx, gap, how)
	return true
}

// onResume resets what a fresh boot would have reset. It is the hibernate
// counterpart of the boot path in newAgent.
func (a *Agent) onResume(ctx context.Context, suspended time.Duration, how string) {
	log.Printf("Resumed after about %v suspended (%s); resetting per-boot state", suspended.Round(time.Second), how)

	// The next stop, hibernate or terminate runs the user's pre-stop hook and
	// writes the task record again.
	a.preStopDone = false
	a.taskFlushDone = false

	// The idle timer starts now. The hibernated time was not idle time on a
	// running instance, and counting it re-hibernates within a minute.
	a.setLastActivity(time.Now())
	a.idleWarned = false

	// Whatever EC2 refused before is a new question now.
	a.stopRetry = stopRetry{}

	// The counter baselines are from before the hibernation, so the next delta
	// would span it. Start again: the first tick reads unknown, which is active.
	a.prevCPUIdle, a.prevCPUTotal = 0, 0
	a.netSampled = false
	a.diskSampled = false

	// The completion file that triggered an on-complete hibernate is still on
	// disk. Acting on it again would hibernate the instance the user has just
	// started, so that file is spent; a new completion (the file written again)
	// is acted on as usual.
	if a.completionAnnounced {
		if fi, err := os.Stat(a.cfg().CompletionFile); err == nil {
			a.completionSpent = fi.ModTime()
		}
		a.completionAnnounced = false
	}

	// A resumed instance without an EIP has a new public IP.
	a.refreshPublicIP(ctx)

	// Put back what Cleanup removed before the hibernate: DNS (with the IP just
	// read), the job-array registration and the plugins. When the hibernate was
	// started outside spored, Cleanup did not run and these are refreshes.
	a.restoreAfterCleanup(ctx)
}

// publicIP is the instance's current public IP: identity.PublicIP until a
// resume re-reads it. identity is shared, unguarded, with the provider, the
// plugin runtime and goroutines off the monitor loop, so it is never mutated;
// the new IP lives here, under ipMu, because Cleanup reads it from the
// shutdown goroutine.
func (a *Agent) publicIP() string {
	a.ipMu.Lock()
	defer a.ipMu.Unlock()
	if a.currentIPSet {
		return a.currentIP
	}
	if a.identity == nil {
		return ""
	}
	return a.identity.PublicIP
}

// refreshPublicIP re-reads the public IP after a resume. On a failed read the
// old one is kept: a stale name is better than none, and the hourly renewal
// tries again with it.
func (a *Agent) refreshPublicIP(ctx context.Context) {
	if err := a.readPublicIP(ctx); err != nil {
		log.Printf("Warning: could not re-read the public IP after a resume: %v; keeping %s", err, a.publicIP())
	}
}

// readPublicIP reads the public IP and makes it the current value; on an
// error the current value is unchanged. Not on EC2 (and with no test lookup),
// it does nothing.
func (a *Agent) readPublicIP(ctx context.Context) error {
	lookup := a.lookupPublicIP
	if lookup == nil {
		if a.identity == nil || a.identity.Provider != "ec2" {
			return nil
		}
		lookup = imdsPublicIP
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ip, err := lookup(lctx)
	if err != nil {
		return err
	}
	if old := a.publicIP(); ip != old {
		log.Printf("Public IP changed: %q -> %q", old, ip)
	}
	a.ipMu.Lock()
	a.currentIP, a.currentIPSet = ip, true
	a.ipMu.Unlock()
	return nil
}

// imdsPublicIP reads public-ipv4 from IMDS, as the EC2 provider does at boot.
func imdsPublicIP(ctx context.Context) (string, error) {
	out, err := imds.NewFromConfig(aws.Config{}).GetMetadata(ctx, &imds.GetMetadataInput{Path: "public-ipv4"})
	if err != nil {
		return "", fmt.Errorf("IMDS public-ipv4: %w", err)
	}
	defer func() { _ = out.Content.Close() }()
	b, err := io.ReadAll(out.Content)
	if err != nil {
		return "", fmt.Errorf("IMDS public-ipv4: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
