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
}

// resolveComputeStart decides the compute carried in from earlier boots (base)
// and when this boot's count starts (from), so that TotalComputeSeconds =
// base + time.Since(from).
//
// The rules, each chosen so that the error, when there is one, undercounts
// rather than counting a stretch twice:
//
//   - A checkpoint from THIS boot means spored restarted within the boot. Its
//     base and start are reused as they are: the boot's running time is still
//     being counted from where it began.
//   - A checkpoint from an EARLIER boot carries that boot's total. The tag may be
//     newer (the file write failed) or older (the tag write was lost on
//     shutdown, #928), and neither includes this boot, so the larger wins. This
//     boot counts from the kernel's boot time.
//   - No usable checkpoint and no tag: nothing has been counted on this
//     instance, so this boot counts from the kernel's boot time too.
//   - No usable checkpoint but a non-zero tag: the tag may already include part
//     of this boot (an older spored ran earlier in it), so this boot counts only
//     from spored's own start. That is how every spored counted before #928.
func resolveComputeStart(in computeStartInputs) (base int64, from time.Time, source string) {
	bootStart := in.SporedAt
	if !in.BootTime.IsZero() && !in.BootTime.After(in.SporedAt) {
		bootStart = in.BootTime
		if !in.LaunchTime.IsZero() && bootStart.Before(in.LaunchTime) {
			bootStart = in.LaunchTime // clock skew; billing cannot predate launch
		}
	}

	cp := in.Checkpoint
	usable := cp != nil && in.BootID != "" && cp.BootID != "" && cp.InstanceID == in.InstanceID &&
		cp.BaseSeconds >= 0 && cp.TotalSeconds >= cp.BaseSeconds
	switch {
	case usable && cp.BootID == in.BootID && !cp.CountFrom.IsZero() && !cp.CountFrom.After(in.SporedAt):
		return cp.BaseSeconds, cp.CountFrom, "checkpoint (same boot)"
	case usable && cp.BootID != in.BootID:
		base := cp.TotalSeconds
		if in.TagSeconds > base {
			base = in.TagSeconds
		}
		return base, bootStart, "checkpoint (earlier boot)"
	case in.TagSeconds <= 0:
		return 0, bootStart, "first boot"
	default:
		return in.TagSeconds, in.SporedAt, "tag"
	}
}

// computeCheckpoint is the agent's checkpoint as of now.
func (a *Agent) computeCheckpoint() ComputeCheckpoint {
	id := ""
	if a.identity != nil {
		id = a.identity.InstanceID
	}
	return ComputeCheckpoint{
		InstanceID:   id,
		BootID:       a.bootID,
		BaseSeconds:  a.computeSecondsBase,
		CountFrom:    a.computeFrom().UTC(),
		TotalSeconds: a.TotalComputeSeconds(),
		WrittenAt:    time.Now().UTC(),
	}
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
