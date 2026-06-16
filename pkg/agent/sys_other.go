//go:build !linux && !windows

package agent

import (
	"context"
	"fmt"
	"os/exec"
)

// Conservative stubs so darwin/dev builds compile. spored only ever runs on
// Linux or Windows instances in production; these keep `go build`/`make check`
// green on a developer Mac without pretending to measure anything. Idle signals
// return "active/unknown" so a dev build never falsely concludes idle (#77).

func sysReadCPUTimes() (idle, total int64, err error) { return 0, 0, errBadProcStat }
func sysReadNetworkBytes() (rx, tx int64)             { return -1, -1 }
func sysReadDiskIOBytes() int64                       { return 0 }
func sysIsProcessRunning(string) bool                 { return false }
func sysCountActiveSessions() int                     { return 0 }
func sysCountActivePortConnections([]int) int         { return 0 }
func sysHasActiveTerminals() bool                     { return false }
func sysHasRecentUserActivity() bool                  { return false }
func sysWarnUsers(string)                             {}

// sysShellCommand runs a pre-stop hook on non-linux/non-windows builds (dev /
// darwin). It runs the hook as the given user via `su - <user> -c` when set,
// mirroring the Linux behavior (#63); empty falls back to the root shell.
func sysShellCommand(ctx context.Context, command, user string) *exec.Cmd {
	if user != "" {
		return exec.CommandContext(ctx, "su", "-", user, "-c", command) // nosemgrep: dangerous-exec-command -- pre-stop hook runs as the instance's own user
	}
	return exec.CommandContext(ctx, "sh", "-c", command) // nosemgrep: dangerous-exec-command
}

// sysMountLustre is a no-op on non-linux/non-windows (dev/darwin) builds — spored
// only mounts FSx Lustre on Linux instances (#194).
func sysMountLustre(ctx context.Context, dnsName, mountName, mountPoint string) error {
	_ = ctx
	_, _, _ = dnsName, mountName, mountPoint
	return fmt.Errorf("FSx Lustre mounting is only supported on Linux")
}
