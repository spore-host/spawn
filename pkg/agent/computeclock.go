package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ComputeClock is the daemon's compute clock, published so a separate
// `spored status` process can report real running time (#896).
//
// Why it has to be published: `spored status` is a fresh process. Its Agent's
// startTime is "now", so TotalComputeSeconds was the spawn:compute-seconds tag
// plus roughly zero — a checkpoint the daemon writes once a minute at best and
// every five minutes after the first ten. On a brand-new instance that tag is
// still 0, which is how `Compute cost: $0.00 (0s × ...)` appeared at 47s elapsed.
// The same zero startTime made the boot term swallow all of elapsed, so the
// breakdown said "boot" for time the instance had spent computing.
//
// The daemon knows the two numbers status needs — when it started and the
// compute it inherited from earlier boots — so it writes them down once at
// startup and status adopts them.
//
// StartedAt alone cannot be measured from in another process: it comes back
// from JSON with no monotonic reading, so time.Since(StartedAt) is wall time,
// and after a hibernate-and-resume — same boot, /run intact — that includes
// the hibernation (#945). So the clock also carries the kernel's clocks at the
// start, and status measures running time on CLOCK_MONOTONIC from there.
type ComputeClock struct {
	InstanceID  string    `json:"instance_id"`
	StartedAt   time.Time `json:"started_at"`
	BaseSeconds int64     `json:"base_seconds"`
	// KernelClock says BootID, StartMonotonicNS and StartBoottimeNS are set
	// (see kernelClocks). The readings mean nothing outside the boot that took
	// them, so BootID travels with them. False in a clock published before #945.
	KernelClock      bool   `json:"kernel_clock,omitempty"`
	BootID           string `json:"boot_id,omitempty"`
	StartMonotonicNS int64  `json:"start_monotonic_ns,omitempty"`
	StartBoottimeNS  int64  `json:"start_boottime_ns,omitempty"`
}

// ComputeClockPath is where the daemon publishes its clock, or "" on platforms
// with no boot-scoped location for it (see sysComputeClockPath).
func ComputeClockPath() string { return sysComputeClockPath() }

// ComputeClock returns this agent's clock: when it started counting and the
// compute it carried over from before.
func (a *Agent) ComputeClock() ComputeClock {
	id := ""
	if a.identity != nil {
		id = a.identity.InstanceID
	}
	c := ComputeClock{InstanceID: id, StartedAt: a.computeFrom().UTC(), BaseSeconds: a.computeSecondsBase}
	if a.kernelOriginKnown && a.bootID != "" {
		c.KernelClock, c.BootID = true, a.bootID
		c.StartMonotonicNS = int64(a.kernelOrigin.Monotonic)
		c.StartBoottimeNS = int64(a.kernelOrigin.Boottime)
	}
	return c
}

// ComputeClockLive reports whether TotalComputeSeconds counts real running time
// — true for the daemon itself and for a query agent that adopted the daemon's
// clock. False means it is only the last spawn:compute-seconds checkpoint, which
// can lag by minutes and is 0 until the daemon's first write.
func (a *Agent) ComputeClockLive() bool { return a.liveClock }

// WriteComputeClock atomically writes c to path, world-readable (status may run
// unprivileged over SSM or SSH).
func WriteComputeClock(path string, c ComputeClock) error {
	if path == "" {
		return errors.New("no compute clock path on this platform")
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadComputeClock reads a clock written by WriteComputeClock.
func ReadComputeClock(path string) (ComputeClock, error) {
	var c ComputeClock
	if path == "" {
		return c, errors.New("no compute clock path on this platform")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// AdoptComputeClock makes this agent count compute from the daemon's clock
// rather than its own construction time. It refuses a clock that cannot
// describe this boot of this instance, because adopting a stale one would report
// stopped time as compute — the opposite error, and a costlier one:
//
//   - a different instance id (an image baked from a running spore);
//   - a start in the future, or none at all;
//   - a start before the current kernel booted, when the boot time is knowable
//     (a clock left over from before a stop/start);
//   - kernel clock readings from another boot, or ones later than now.
//
// A clock with kernel readings is measured on CLOCK_MONOTONIC, so the time
// the instance spent hibernated is not counted as compute and is reported by
// HibernatedTime instead (#945). A clock without them, from a spored before
// #945, is measured on the wall clock as it always was.
//
// bootTime is injected for tests; pass the zero time when it is unknown.
func (a *Agent) AdoptComputeClock(c ComputeClock, bootTime time.Time) error {
	now := time.Now()
	var origin kernelClocks
	if c.KernelClock {
		origin = kernelClocks{Monotonic: time.Duration(c.StartMonotonicNS), Boottime: time.Duration(c.StartBoottimeNS)}
		kernelNow, ok := readKernelClocks()
		thisBoot := readBootID()
		switch {
		case !ok:
			return errors.New("compute clock is on the kernel clocks, which cannot be read here")
		case c.BootID == "" || thisBoot == "" || c.BootID != thisBoot:
			return fmt.Errorf("compute clock is from boot %q, not this boot %q", c.BootID, thisBoot)
		case !origin.validOrigin(kernelNow):
			return errors.New("compute clock's kernel reading is not from earlier in this boot")
		}
	}
	switch {
	case c.StartedAt.IsZero():
		return errors.New("compute clock has no start time")
	case a.identity != nil && c.InstanceID != a.identity.InstanceID:
		return fmt.Errorf("compute clock belongs to %s, not %s", c.InstanceID, a.identity.InstanceID)
	case c.StartedAt.After(now.Add(time.Minute)):
		return fmt.Errorf("compute clock starts in the future (%s)", c.StartedAt.Format(time.RFC3339))
	case !bootTime.IsZero() && c.StartedAt.Before(bootTime.Add(-time.Minute)):
		return fmt.Errorf("compute clock predates this boot (started %s, booted %s)",
			c.StartedAt.Format(time.RFC3339), bootTime.UTC().Format(time.RFC3339))
	case c.BaseSeconds < 0:
		return errors.New("compute clock has a negative base")
	}
	a.computeStart = c.StartedAt
	a.computeSecondsBase = c.BaseSeconds
	a.kernelOrigin, a.kernelOriginKnown = origin, c.KernelClock
	a.liveClock = true
	return nil
}

// AdoptPublishedComputeClock reads the daemon's clock from its platform path and
// adopts it, returning why not when it cannot.
func (a *Agent) AdoptPublishedComputeClock() error {
	c, err := ReadComputeClock(ComputeClockPath())
	if err != nil {
		return err
	}
	return a.AdoptComputeClock(c, sysBootTime())
}
