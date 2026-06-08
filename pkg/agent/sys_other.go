//go:build !linux && !windows

package agent

import (
	"context"
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

func sysShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command) // nosemgrep: dangerous-exec-command
}
