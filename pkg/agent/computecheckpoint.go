package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ComputeCheckpoint is the instance's cumulative compute, persisted on its own
// EBS volume so it survives a stop/start (#928).
//
// Why a second copy beside the spawn:compute-seconds tag: the tag is written
// over the network, through IMDS credentials and the EC2 API, and the moment
// it matters most — the shutdown a stop triggers — is the moment those are
// least dependable. When that last write was lost, the next boot started from
// whatever the tag said minutes earlier, and #928's instance came back
// reporting 22s of compute for over a minute of running. The file is written
// every monitor tick and again first thing on shutdown, needs nothing but the
// local disk, and is read back on the next boot alongside the tag.
//
// It also records which boot it describes (the kernel's boot_id) and when that
// boot's count started, which is what lets the daemon count each boot from
// the moment the kernel came up — EC2 bills from there, not from when spored
// finished installing — without double-counting when spored merely restarts
// within one boot (an in-place upgrade, a crash and Restart=on-failure).
type ComputeCheckpoint struct {
	InstanceID string `json:"instance_id"`
	// BootID identifies the boot TotalSeconds was measured in.
	BootID string `json:"boot_id"`
	// BaseSeconds is the compute carried in from boots before this one.
	BaseSeconds int64 `json:"base_seconds"`
	// CountFrom is when this boot's compute started counting.
	CountFrom time.Time `json:"count_from"`
	// TotalSeconds is BaseSeconds plus this boot's running time at WrittenAt.
	TotalSeconds int64     `json:"total_seconds"`
	WrittenAt    time.Time `json:"written_at"`
	// KernelClock says CountFromMonotonicNS and CountFromBoottimeNS are set:
	// the kernel's clocks when this boot's count started (#945). A restart
	// within the boot resumes counting on CLOCK_MONOTONIC from there, which a
	// hibernation does not advance. CountFrom, a wall time, cannot do that: a
	// hibernated instance resumes into the same boot, and wall-now minus
	// CountFrom includes the hibernation. False in a checkpoint written before
	// #945.
	KernelClock          bool  `json:"kernel_clock,omitempty"`
	CountFromMonotonicNS int64 `json:"count_from_monotonic_ns,omitempty"`
	CountFromBoottimeNS  int64 `json:"count_from_boottime_ns,omitempty"`
}

// ComputeCheckpointPath is where the daemon persists its checkpoint, or "" on
// platforms where boots cannot be told apart (see sysComputeCheckpointPath).
func ComputeCheckpointPath() string { return sysComputeCheckpointPath() }

// WriteComputeCheckpoint atomically writes c to path and syncs it, so a
// shutdown that follows immediately cannot leave a torn or unflushed file.
func WriteComputeCheckpoint(path string, c ComputeCheckpoint) error {
	if path == "" {
		return errors.New("no compute checkpoint path on this platform")
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // fixed daemon-owned path
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadComputeCheckpoint reads a checkpoint written by WriteComputeCheckpoint.
func ReadComputeCheckpoint(path string) (ComputeCheckpoint, error) {
	var c ComputeCheckpoint
	if path == "" {
		return c, errors.New("no compute checkpoint path on this platform")
	}
	data, err := os.ReadFile(path) //nolint:gosec // fixed daemon-owned path
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// computeStartInputs is everything resolveComputeStart decides from.
type computeStartInputs struct {
	InstanceID string
	BootID     string    // this boot's id, "" when unknown
	BootTime   time.Time // when this kernel booted, zero when unknown
	LaunchTime time.Time // the instance's original launch, zero when unknown
	SporedAt   time.Time // when this spored started
	TagSeconds int64     // spawn:compute-seconds
	Checkpoint *ComputeCheckpoint
	Kernel     kernelClocks // the kernel clocks at SporedAt
	KernelOK   bool         // Kernel was read; false where the platform has no such clocks
}

// computeStart is resolveComputeStart's decision.
type computeStart struct {
	Base   int64     // compute carried in from earlier boots
	From   time.Time // when this boot's count started, on the wall clock
	Source string    // which rule decided, for the log
	// Origin is the kernel clocks at From, when OriginKnown. Running time is
	// then measured on CLOCK_MONOTONIC from Origin, so hibernated time is not
	// counted (#945); without it, on the wall clock from From.
	Origin      kernelClocks
	OriginKnown bool
}

// resolveComputeStart decides the compute carried in from earlier boots (Base)
// and when this boot's count starts (From, and Origin on the kernel clocks), so
// that TotalComputeSeconds = Base + running time since then.
//
// The rules, each chosen so that the error, when there is one, undercounts
// rather than counting a stretch twice:
//
//   - A checkpoint from THIS boot means spored restarted within the boot. Its
//     base and start are reused as they are: the boot's running time is still
//     being counted from where it began. The start is reused on the kernel's
//     monotonic clock, never on the wall clock, because a hibernated instance
//     resumes into the same boot and the wall clock counts the hibernation
//     (#945). A same-boot checkpoint written before #945 has no kernel reading,
//     so its TotalSeconds is carried and counting resumes from spored's start:
//     that loses the seconds since its last write, which is the right way to
//     be wrong.
//   - A checkpoint from an EARLIER boot carries that boot's total. The tag may be
//     newer (the file write failed) or older (the tag write was lost on
//     shutdown, #928), and neither includes this boot, so the larger wins. This
//     boot counts from the kernel's boot time.
//   - No usable checkpoint and no tag: nothing has been counted on this
//     instance, so this boot counts from the kernel's boot time too.
//   - No usable checkpoint but a non-zero tag: the tag may already include part
//     of this boot (an older spored ran earlier in it), so this boot counts only
//     from spored's own start. That is how every spored counted before #928.
//
// "The kernel's boot time" is the reading {0, 0} on the kernel clocks, so a
// hibernation before spored started (late in a boot) is not counted either.
func resolveComputeStart(in computeStartInputs) computeStart {
	// Counting from spored's own start: the kernel clocks as they read now.
	atSpored := computeStart{From: in.SporedAt, Origin: in.Kernel, OriginKnown: in.KernelOK}

	bootStart := atSpored
	if !in.BootTime.IsZero() && !in.BootTime.After(in.SporedAt) {
		bootStart = computeStart{From: in.BootTime, OriginKnown: in.KernelOK}
		if !in.LaunchTime.IsZero() && bootStart.From.Before(in.LaunchTime) {
			// Clock skew; billing cannot predate launch. Move the kernel origin
			// by the same amount, capped at now.
			skew := in.LaunchTime.Sub(in.BootTime)
			if in.KernelOK && skew > in.Kernel.Monotonic {
				skew = in.Kernel.Monotonic
			}
			bootStart.From = in.LaunchTime
			bootStart.Origin = kernelClocks{Monotonic: skew, Boottime: skew}
		}
	}
	with := func(c computeStart, base int64, source string) computeStart {
		c.Base, c.Source = base, source
		return c
	}

	cp := in.Checkpoint
	usable := cp != nil && in.BootID != "" && cp.BootID != "" && cp.InstanceID == in.InstanceID &&
		cp.BaseSeconds >= 0 && cp.TotalSeconds >= cp.BaseSeconds
	sameBoot := usable && cp.BootID == in.BootID
	switch {
	case sameBoot && cp.KernelClock && in.KernelOK:
		origin := kernelClocks{
			Monotonic: time.Duration(cp.CountFromMonotonicNS),
			Boottime:  time.Duration(cp.CountFromBoottimeNS),
		}
		if !origin.validOrigin(in.Kernel) {
			// Not a reading from earlier in this boot: carry what it recorded.
			return with(atSpored, cp.TotalSeconds, "checkpoint (same boot, bad kernel reading)")
		}
		from := cp.CountFrom
		if from.IsZero() || from.After(in.SporedAt) {
			from = in.SporedAt.Add(-kernelRunningSince(origin, in.Kernel))
		}
		return computeStart{Base: cp.BaseSeconds, From: from, Origin: origin, OriginKnown: true,
			Source: "checkpoint (same boot)"}
	case sameBoot && in.KernelOK:
		return with(atSpored, cp.TotalSeconds, "checkpoint (same boot, no kernel reading)")
	case sameBoot && !cp.CountFrom.IsZero() && !cp.CountFrom.After(in.SporedAt):
		// No kernel clocks here at all: the wall clock is all there is.
		return computeStart{Base: cp.BaseSeconds, From: cp.CountFrom, Source: "checkpoint (same boot)"}
	case usable && cp.BootID != in.BootID:
		base := cp.TotalSeconds
		if in.TagSeconds > base {
			base = in.TagSeconds
		}
		return with(bootStart, base, "checkpoint (earlier boot)")
	case in.TagSeconds <= 0:
		return with(bootStart, 0, "first boot")
	default:
		return with(atSpored, in.TagSeconds, "tag")
	}
}

// computeCheckpoint is the agent's checkpoint as of now.
func (a *Agent) computeCheckpoint() ComputeCheckpoint {
	id := ""
	if a.identity != nil {
		id = a.identity.InstanceID
	}
	c := ComputeCheckpoint{
		InstanceID:   id,
		BootID:       a.bootID,
		BaseSeconds:  a.computeSecondsBase,
		CountFrom:    a.computeFrom().UTC(),
		TotalSeconds: a.TotalComputeSeconds(),
		WrittenAt:    time.Now().UTC(),
	}
	if a.kernelOriginKnown {
		c.KernelClock = true
		c.CountFromMonotonicNS = int64(a.kernelOrigin.Monotonic)
		c.CountFromBoottimeNS = int64(a.kernelOrigin.Boottime)
	}
	return c
}

// WriteComputeCheckpoint persists the agent's cumulative compute to its local
// checkpoint file. A no-op for a query agent, on a platform with no path, or
// when the boot cannot be identified (a checkpoint that cannot say which boot
// it describes cannot be safely reused). A failure is logged once, not every
// tick.
func (a *Agent) WriteComputeCheckpoint() {
	if a.checkpointPath == "" || a.bootID == "" {
		return
	}
	if err := WriteComputeCheckpoint(a.checkpointPath, a.computeCheckpoint()); err != nil {
		if !a.checkpointWarned {
			log.Printf("Warning: could not write compute checkpoint %s: %v", a.checkpointPath, err)
			a.checkpointWarned = true
		}
		return
	}
	a.checkpointWarned = false
}

// computeTagWriteAttempts bounds the retries of the shutdown tag flush. It runs
// inside Cleanup's 10s context, so attempts are cheap to allow and the context
// is what actually stops them.
const computeTagWriteAttempts = 3

// putComputeSecondsTag writes spawn:compute-seconds through the agent's tag
// writer, retrying a failure (a stop's shutdown is when a transient credential
// or network error is likeliest) and returning the last error. It used to make
// one attempt and discard the error, so a lost final write was invisible.
func (a *Agent) putComputeSecondsTag(ctx context.Context, total int64) error {
	tp := a.tagger
	if tp == nil {
		tp = &ec2TagPutter{region: a.identity.Region}
	}
	tags := map[string]string{"spawn:compute-seconds": strconv.FormatInt(total, 10)}
	var err error
	for i := 0; i < computeTagWriteAttempts; i++ {
		if err = tp.putTags(ctx, a.identity.InstanceID, tags); err == nil {
			return nil
		}
		if i == computeTagWriteAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (after %d attempts: %v)", ctx.Err(), i+1, err)
		case <-time.After(time.Duration(i+1) * 500 * time.Millisecond):
		}
	}
	return err
}
