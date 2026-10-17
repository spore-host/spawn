//go:build linux

package agent

import (
	"time"

	"golang.org/x/sys/unix"
)

// sysSuspendedTime is how long this boot has spent suspended or hibernated:
// CLOCK_BOOTTIME minus CLOCK_MONOTONIC. The two run together except while the
// machine is suspended, when MONOTONIC stops and the kernel adds the sleep to
// BOOTTIME on resume. NTP slews both alike, so only a suspend moves the
// difference (#987; the same reading #945 uses for compute time). MONOTONIC is
// read first, so the pair can overstate the total by the nanoseconds between
// the two calls, never by more.
func sysSuspendedTime() (time.Duration, bool) {
	var mono, boot unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return 0, false
	}
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &boot); err != nil {
		return 0, false
	}
	d := time.Duration(boot.Nano() - mono.Nano())
	if d < 0 {
		d = 0
	}
	return d, true
}
