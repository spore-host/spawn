package agent

import (
	"errors"
	"testing"
)

// stubCPUTimes replaces the CPU-times source for one test.
func stubCPUTimes(t *testing.T, samples ...[2]int64) {
	t.Helper()
	i := 0
	orig := readCPUTimes
	readCPUTimes = func() (int64, int64, error) {
		if i >= len(samples) {
			return samples[len(samples)-1][0], samples[len(samples)-1][1], nil
		}
		s := samples[i]
		i++
		return s[0], s[1], nil
	}
	t.Cleanup(func() { readCPUTimes = orig })
}

// Three states used to collapse into one float, two of them sentinels a caller
// could not tell from data — and the third, the zero-delta case, returned the
// most REASSURING possible value (0.0) from a no-information state while its two
// siblings returned the most conservative one (#734).
func TestCPUUsage_UnknownStatesAreReportedAsUnknown(t *testing.T) {
	t.Run("unreadable source", func(t *testing.T) {
		orig := readCPUTimes
		readCPUTimes = func() (int64, int64, error) { return 0, 0, errors.New("no /proc/stat") }
		t.Cleanup(func() { readCPUTimes = orig })

		a := newTestAgent(t, nil)
		usage, ok := a.cpuUsage()
		if ok {
			t.Error("an unreadable source reported a measurement")
		}
		if usage != 100.0 {
			t.Errorf("usage = %v, want 100 (assume active)", usage)
		}
	})

	t.Run("first call has no previous sample", func(t *testing.T) {
		stubCPUTimes(t, [2]int64{100, 1000})
		a := newTestAgent(t, nil)
		usage, ok := a.cpuUsage()
		if ok {
			t.Error("the first call reported a measurement; there is no delta to measure")
		}
		if usage != 100.0 {
			t.Errorf("usage = %v, want 100 (assume active)", usage)
		}
	})

	// THE bug. Two calls microseconds apart see identical jiffies, and the old
	// code returned 0.0 — so `spored status`, which samples twice in one process,
	// logged `Not idle: CPU usage 100.00%` and printed `CPU: 0.0%` on a box
	// running at 100%.
	t.Run("zero elapsed time between samples", func(t *testing.T) {
		stubCPUTimes(t, [2]int64{100, 1000}, [2]int64{100, 1000})
		a := newTestAgent(t, nil)
		_, _ = a.cpuUsage() // prime
		usage, ok := a.cpuUsage()
		if ok {
			t.Error("a zero-length interval reported a measurement")
		}
		if usage == 0.0 {
			t.Error("zero elapsed time reported 0% CPU — that is #734: the most reassuring " +
				"possible value from a no-information state, and it is what feeds isIdle")
		}
		if usage != 100.0 {
			t.Errorf("usage = %v, want 100 — consistent with the other two unknown branches", usage)
		}
	})
}

// A genuine delta must still be reported as a measurement, with the right value.
func TestCPUUsage_RealDeltaIsMeasured(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second [2]int64 // {idle, total}
		want          float64
	}{
		// 100 idle of 200 total elapsed -> 50% busy.
		{"half busy", [2]int64{100, 1000}, [2]int64{200, 1200}, 50.0},
		// No idle time at all -> 100% busy. The value the demo instance really had.
		{"fully busy", [2]int64{100, 1000}, [2]int64{100, 1200}, 100.0},
		// All idle -> 0% busy. A REAL zero, which must stay distinguishable from
		// the unknown-zero above.
		{"fully idle", [2]int64{100, 1000}, [2]int64{300, 1200}, 0.0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubCPUTimes(t, tc.first, tc.second)
			a := newTestAgent(t, nil)
			_, _ = a.cpuUsage()
			usage, ok := a.cpuUsage()
			if !ok {
				t.Fatal("a real delta was not reported as a measurement")
			}
			if usage != tc.want {
				t.Errorf("usage = %v, want %v", usage, tc.want)
			}
		})
	}
}

// A measured 0% and an unknown 0% must not be the same answer. This is the whole
// point of the second return value: `CPU: 0.0%` meant both, and on the reported
// instance it meant the second while looking like the first.
func TestCPUUsage_RealZeroIsDistinguishableFromUnknown(t *testing.T) {
	stubCPUTimes(t, [2]int64{100, 1000}, [2]int64{300, 1200})
	a := newTestAgent(t, nil)
	_, _ = a.cpuUsage()
	measured, ok := a.cpuUsage()
	if !ok || measured != 0.0 {
		t.Fatalf("precondition: got (%v, %v), want (0, true)", measured, ok)
	}

	stubCPUTimes(t, [2]int64{100, 1000}, [2]int64{100, 1000})
	b := newTestAgent(t, nil)
	_, _ = b.cpuUsage()
	unknown, ok := b.cpuUsage()
	if ok {
		t.Fatal("the unknown case claimed to be measured")
	}
	if unknown == measured {
		t.Errorf("an unknown reading (%v) is identical to a measured 0%% — a caller "+
			"cannot tell an idle box from an unmeasurable one", unknown)
	}
}

// HasPriorComputeTime is the discriminator for #735's stopped-time term: it is
// true only when a previous run's compute total was carried over through the tag,
// which is the only evidence that the instance was ever actually stopped.
func TestHasPriorComputeTime(t *testing.T) {
	a := newTestAgent(t, nil)
	if a.HasPriorComputeTime() {
		t.Error("a fresh agent claims prior compute time, so a first boot would " +
			"report its boot duration as stopped time (#735)")
	}
	a.computeSecondsBase = 3600
	if !a.HasPriorComputeTime() {
		t.Error("carried-over compute time was not recognised")
	}
}
