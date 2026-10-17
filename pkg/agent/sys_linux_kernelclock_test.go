//go:build linux

package agent

import "testing"

// On Linux both kernel clocks are readable, and CLOCK_BOOTTIME — which adds
// suspended time back — is never behind CLOCK_MONOTONIC (#945).
func TestSysKernelClocks_ReadsBothClocks(t *testing.T) {
	k, ok := sysKernelClocks()
	if !ok {
		t.Fatal("CLOCK_MONOTONIC/CLOCK_BOOTTIME unreadable")
	}
	if k.Monotonic <= 0 || k.Boottime < k.Monotonic {
		t.Errorf("readings %+v: want 0 < monotonic <= boottime", k)
	}
}

// The suspended time resume detection reads (#987) comes from the same kernel
// clocks and, on a machine that is not suspending under the test, does not move
// between two reads.
func TestReadSuspendedTimeReadsTheKernel(t *testing.T) {
	a, ok := readSuspendedTime()
	if !ok {
		t.Fatal("CLOCK_BOOTTIME/CLOCK_MONOTONIC unreadable on Linux")
	}
	b, _ := readSuspendedTime()
	if a < 0 || b-a > resumeSuspendThreshold {
		t.Errorf("suspended time %v then %v: negative, or moved by more than %v without a suspend", a, b, resumeSuspendThreshold)
	}
}
