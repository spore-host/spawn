//go:build !linux

package agent

import "time"

// sysSuspendedTime has no source outside Linux. On Windows Go's monotonic
// clock is the interrupt time, which keeps counting through sleep and
// hibernation, so neither this reading nor the wall-clock comparison in
// resumedBetween sees a resume there (#962 describes the Windows clock that
// would).
func sysSuspendedTime() (time.Duration, bool) { return 0, false }
