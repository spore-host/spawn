package agent

import (
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// Windows' interrupt-time counts are 100ns units since boot. The unbiased count
// excludes sleep and hibernation, so it is the running clock; the biased count
// includes them (#962).
func TestInterruptTimeClocks_MapsUnbiasedToMonotonic(t *testing.T) {
	k, ok := interruptTimeClocks(6_000_000_000, 294_000_000_000) // 10m running, 8h10m since boot
	if !ok {
		t.Fatal("a valid pair was rejected")
	}
	if k.Monotonic != 10*time.Minute {
		t.Errorf("Monotonic = %v, want 10m0s: the unbiased count, in 100ns units, is the running clock", k.Monotonic)
	}
	if k.Boottime != 8*time.Hour+10*time.Minute {
		t.Errorf("Boottime = %v, want 8h10m0s: the biased count includes hibernation", k.Boottime)
	}
	if k.suspended() != 8*time.Hour {
		t.Errorf("suspended = %v, want 8h0m0s", k.suspended())
	}
}

// A pair no system produces is not a reading: the biased count can never be
// behind the unbiased one, and a count past time.Duration's range would wrap.
func TestInterruptTimeClocks_RejectsImpossiblePairs(t *testing.T) {
	for _, c := range []struct{ unbiased, biased uint64 }{
		{unbiased: 200, biased: 100},
		{unbiased: 0, biased: 1 << 62},
	} {
		if k, ok := interruptTimeClocks(c.unbiased, c.biased); ok {
			t.Errorf("interruptTimeClocks(%d, %d) = %+v, true; want false", c.unbiased, c.biased, k)
		}
	}
}

// fakeInterruptTime stands in for QueryUnbiasedInterruptTime and
// QueryInterruptTime, in their 100ns units.
type fakeInterruptTime struct{ unbiased, biased uint64 }

func (w *fakeInterruptTime) run(d time.Duration) {
	w.unbiased += uint64(d / 100)
	w.biased += uint64(d / 100)
}
func (w *fakeInterruptTime) hibernate(d time.Duration) { w.biased += uint64(d / 100) }

// A Windows spored: no boot id, no boot time, no compute clock file and no
// checkpoint, so it counts in-process from its own start. It runs 10 minutes,
// the instance hibernates for 8 hours and resumes. Its count, which is what it
// writes to spawn:compute-seconds and enforces --cost-limit against, must be
// the 10 minutes, not 8h10m (#962). Go's monotonic clock on Windows is the
// biased interrupt time, so measuring on it counted the hibernation.
func TestWindowsDaemon_HibernationIsNotCountedAsCompute(t *testing.T) {
	w := &fakeInterruptTime{}
	w.run(3 * time.Minute) // Windows boot and service start
	restore := SetKernelClocksForTest("", func() (time.Duration, time.Duration, bool) {
		k, ok := interruptTimeClocks(w.unbiased, w.biased)
		return k.Monotonic, k.Boottime, ok
	})
	t.Cleanup(restore)

	now := time.Now()
	kernelNow, kernelOK := readKernelClocks()
	start := resolveComputeStart(computeStartInputs{
		InstanceID: hibInstance, BootID: readBootID(), SporedAt: now,
		TagSeconds: 70, Kernel: kernelNow, KernelOK: kernelOK,
	})
	a := &Agent{
		identity: &provider.Identity{InstanceID: hibInstance}, bootID: readBootID(),
		computeSecondsBase: start.Base, computeStart: start.From, startTime: now,
		kernelOrigin: start.Origin, kernelOriginKnown: start.OriginKnown,
	}
	if !a.kernelOriginKnown {
		t.Fatalf("daemon has no origin on the interrupt-time clocks (%s): it would count on Go's biased clock", start.Source)
	}

	w.run(10 * time.Minute)
	w.hibernate(hibernation)
	if got := a.TotalComputeSeconds(); !withinSecond(got, 70+600) {
		t.Errorf("total after a hibernation = %ds, want %ds: the hibernation was counted as compute", got, 70+600)
	}
	if got := a.HibernatedTime(); got != hibernation {
		t.Errorf("hibernated = %v, want %v", got, hibernation)
	}
	// Without a boot id the readings mean nothing to another process, so they
	// are not published for spored status.
	if c := a.ComputeClock(); c.KernelClock {
		t.Errorf("compute clock published kernel readings with no boot id: %+v", c)
	}
}
