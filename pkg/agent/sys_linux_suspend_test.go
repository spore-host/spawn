//go:build linux

package agent

import "testing"

// The real kernel read works and, on a machine that is not suspending under
// the test, does not move between two reads.
func TestSysSuspendedTimeReadsTheKernel(t *testing.T) {
	a, ok := sysSuspendedTime()
	if !ok {
		t.Fatal("CLOCK_BOOTTIME/CLOCK_MONOTONIC unreadable on Linux")
	}
	b, _ := sysSuspendedTime()
	if a < 0 || b-a > resumeSuspendThreshold {
		t.Errorf("suspended time %v then %v: negative, or moved by more than %v without a suspend", a, b, resumeSuspendThreshold)
	}
}
