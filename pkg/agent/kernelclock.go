package agent

import (
	"math"
	"time"
)

// kernelClocks is one reading of the kernel's two since-boot clocks (#945).
//
// Why compute is measured on these and not on the wall clock: a hibernated
// instance resumes into the SAME boot. Its boot_id, its /run and every
// timestamp spored wrote down before the hibernation survive, so measuring
// "running time" as wall-now minus a recorded wall start counts the hibernated
// interval as compute, which EC2 does not bill. The two clocks tell the
// intervals apart without needing to catch the hibernation as it happens (a
// sleep hook can be missed, and EC2's transition times need the network):
//
//   - Monotonic (CLOCK_MONOTONIC) is running time since boot. It stops while
//     the machine is suspended or hibernated, and it is comparable across
//     processes within one boot, so it can be written down and read back.
//   - Boottime (CLOCK_BOOTTIME) is the same clock with the suspended time
//     added back. Boottime − Monotonic is how long this boot has spent
//     suspended, which is what status reports as hibernated.
//
// Both are zero at kernel boot, so "count from the kernel's boot" is the
// reading {0, 0}. On Windows the same roles are played by the unbiased and the
// biased interrupt time (#962, interruptTimeClocks).
type kernelClocks struct {
	Monotonic time.Duration
	Boottime  time.Duration
}

// suspended is how long the boot had spent suspended at this reading.
func (k kernelClocks) suspended() time.Duration {
	if d := k.Boottime - k.Monotonic; d > 0 {
		return d
	}
	return 0
}

// interruptTimeClocks maps Windows' two interrupt-time counts, in 100ns units
// since boot, onto the kernel clock roles (#962): the unbiased count, which
// excludes sleep and hibernation, is Monotonic; the biased count, which
// includes them, is Boottime. See sysKernelClocks in sys_windows.go for the
// documentation each role rests on. It lives here, not there, so the mapping is
// tested on every platform. A pair no real system produces (biased behind
// unbiased, or too large for a time.Duration) reports false.
func interruptTimeClocks(unbiased, biased uint64) (kernelClocks, bool) {
	const unit = 100 // ns per interrupt-time unit
	if biased < unbiased || biased > math.MaxInt64/unit {
		return kernelClocks{}, false
	}
	return kernelClocks{
		Monotonic: time.Duration(unbiased) * unit,
		Boottime:  time.Duration(biased) * unit,
	}, true
}

// readKernelClocks reads both clocks now, or reports false where the platform
// has none spored can use (see sysKernelClocks). A variable so tests can stand
// in for the kernel and simulate a hibernation.
var readKernelClocks = sysKernelClocks

// readBootID is sysBootID, a variable for the same reason.
var readBootID = sysBootID

// SetKernelClocksForTest replaces the kernel clock and boot id readers for
// the rest of a test, so a caller outside this package (spored status) can
// simulate a hibernation. ok=false simulates a platform without the clocks.
// Restore with the returned func; not safe for parallel tests.
func SetKernelClocksForTest(bootID string, read func() (monotonic, boottime time.Duration, ok bool)) (restore func()) {
	prevClocks, prevBoot := readKernelClocks, readBootID
	readKernelClocks = func() (kernelClocks, bool) {
		m, b, ok := read()
		return kernelClocks{Monotonic: m, Boottime: b}, ok
	}
	readBootID = func() string { return bootID }
	return func() { readKernelClocks, readBootID = prevClocks, prevBoot }
}

// kernelRunningSince is the running time — suspended time excluded — between
// the reading origin and now. Never negative.
func kernelRunningSince(origin, now kernelClocks) time.Duration {
	if d := now.Monotonic - origin.Monotonic; d > 0 {
		return d
	}
	return 0
}

// kernelSuspendedSince is the suspended time between origin and now.
func kernelSuspendedSince(origin, now kernelClocks) time.Duration {
	if d := now.suspended() - origin.suspended(); d > 0 {
		return d
	}
	return 0
}

// validOrigin reports whether origin can be a reading taken earlier in the boot
// that produced now. A reading from another boot, or a corrupt one, fails.
func (k kernelClocks) validOrigin(now kernelClocks) bool {
	return k.Monotonic >= 0 && k.Boottime >= k.Monotonic &&
		k.Monotonic <= now.Monotonic && k.Boottime <= now.Boottime
}

// thisBootRunning is how long this boot's compute has been counting: on the
// kernel's monotonic clock where the agent has an origin on it, so a
// hibernation is never counted (#945); otherwise on the wall clock from
// computeFrom, as before.
func (a *Agent) thisBootRunning() time.Duration {
	if a.kernelOriginKnown {
		if now, ok := readKernelClocks(); ok {
			return kernelRunningSince(a.kernelOrigin, now)
		}
	}
	return time.Since(a.computeFrom())
}

// HibernatedTime is how long the instance has spent hibernated (suspended)
// since this boot's compute started counting. EC2 bills EBS for that time but
// not compute, so `spored status` reports it beside stopped time (#945). Zero
// when it cannot be measured: a platform without the kernel clocks, or a query
// agent that has not adopted a daemon clock carrying them.
func (a *Agent) HibernatedTime() time.Duration {
	if !a.kernelOriginKnown {
		return 0
	}
	now, ok := readKernelClocks()
	if !ok {
		return 0
	}
	return kernelSuspendedSince(a.kernelOrigin, now)
}
