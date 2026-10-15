//go:build windows

package agent

import (
	"testing"
	"time"
)

// On Windows both interrupt-time counts are readable, the biased one is never
// behind the unbiased one, and the unbiased one advances while running (#962).
// This cannot show that the unbiased count stops through an EC2 hibernation;
// that needs a hibernated Windows instance.
func TestSysKernelClocks_ReadsBothInterruptTimes(t *testing.T) {
	first, ok := sysKernelClocks()
	if !ok {
		t.Fatal("QueryUnbiasedInterruptTime/QueryInterruptTime unreadable")
	}
	if first.Monotonic <= 0 || first.Boottime < first.Monotonic {
		t.Errorf("readings %+v: want 0 < monotonic <= boottime", first)
	}
	time.Sleep(100 * time.Millisecond)
	second, ok := sysKernelClocks()
	if !ok {
		t.Fatal("second reading failed")
	}
	// The counts advance a clock tick (~15.6ms) at a time.
	if d := second.Monotonic - first.Monotonic; d < 50*time.Millisecond || d > 10*time.Second {
		t.Errorf("unbiased interrupt time advanced %v over a 100ms sleep", d)
	}
}
