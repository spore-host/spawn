//go:build windows

package agent

import (
	"context"
	"os/exec"
)

// Stage-1 Windows stubs (#77). These make `GOOS=windows go build` succeed and
// keep the agent conservative (never falsely idle). Stage 2 replaces the idle/
// metrics bodies with real WMI / performance-counter implementations and the
// notify/shell bodies with msg.exe / powershell.

func sysReadCPUTimes() (idle, total int64, err error) { return 0, 0, errBadProcStat }
func sysReadNetworkBytes() (rx, tx int64)             { return -1, -1 }
func sysReadDiskIOBytes() int64                       { return 0 }
func sysIsProcessRunning(string) bool                 { return false }
func sysCountActiveSessions() int                     { return 0 }
func sysCountActivePortConnections([]int) int         { return 0 }
func sysHasActiveTerminals() bool                     { return false }
func sysHasRecentUserActivity() bool                  { return false }

// sysWarnUsers broadcasts to interactive sessions via msg.exe (best-effort).
func sysWarnUsers(message string) {
	_ = exec.Command("msg", "*", message).Run()
}

// sysShellCommand runs a user pre-stop hook through PowerShell on Windows.
func sysShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command) // nosemgrep: dangerous-exec-command
}
