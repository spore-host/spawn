//go:build !linux && !windows

package agent

import (
	"context"
	"os/exec"
	"time"
)

// Conservative stubs so darwin/dev builds compile. spored only ever runs on
// Linux or Windows instances in production; these keep `go build`/`make check`
// green on a developer Mac without pretending to measure anything. Idle signals
// return "active/unknown" so a dev build never falsely concludes idle (#77).

func sysReadCPUTimes(context.Context) (idle, total int64, err error) { return 0, 0, errBadProcStat }
func sysReadNetworkBytes(context.Context) (rx, tx int64)             { return -1, -1 }
func sysReadDiskIOBytes(context.Context) int64                       { return -1 }
func sysIsProcessRunning(context.Context, string) bool               { return false }
func sysCountActiveSessions(context.Context) int                     { return 0 }
func sysCountActivePortConnections(context.Context, []int) int       { return 0 }
func sysWarnUsers(context.Context, string)                           {}
func sysWriteConsole(string) error                                   { return nil }

// sysShellCommand runs a pre-stop hook on non-linux/non-windows builds (dev /
// darwin). It runs the hook as the given user via `su - <user> -c` when set,
// mirroring the Linux behavior (#63); empty falls back to the root shell.
func sysShellCommand(ctx context.Context, command, user string) *exec.Cmd {
	if user != "" {
		return exec.CommandContext(ctx, "su", "-", user, "-c", command) // nosemgrep: dangerous-exec-command -- pre-stop hook runs as the instance's own user
	}
	return exec.CommandContext(ctx, "sh", "-c", command) // nosemgrep: dangerous-exec-command
}

// sysMountLustre fails on non-linux/non-windows (dev/darwin) builds — spored
// only mounts FSx Lustre on Linux instances (#194). See errLustreUnsupported.
func sysMountLustre(ctx context.Context, dnsName, mountName, mountPoint string) error {
	_ = ctx
	_, _, _ = dnsName, mountName, mountPoint
	return errLustreUnsupported
}

// No compute clock outside Linux/Windows: spored does not run there for real.
func sysComputeClockPath() string { return "" }
func sysIdleStatePath() string    { return "" }

// sysReloadMarkerPath is "" on a dev build: `spored reload` needs the systemd
// or Windows service it signals (#1278).
func sysReloadMarkerPath() string { return "" }
func sysBootTime() time.Time      { return time.Time{} }

// sysAcceleratorFamilies: no accelerators on a dev build.
func sysAcceleratorFamilies() []string { return nil }

// No durable compute checkpoint outside Linux (#928): there is no boot id to
// tell a restart within a boot from a new boot, and a checkpoint that cannot
// say which boot it describes cannot be reused safely. The spawn:compute-seconds
// tag still carries compute across a stop/start there.
func sysComputeCheckpointPath() string { return "" }
func sysBootID() string                { return "" }

// No kernel clocks outside Linux and Windows: compute is measured on the wall
// clock, as before #945, and resume detection (#987) falls back to comparing
// the wall clock with Go's monotonic clock.
func sysKernelClocks() (kernelClocks, bool) { return kernelClocks{}, false }
