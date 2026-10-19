package agent

import (
	"context"
	"os/exec"
	"time"
)

// OS probes: the short-lived child processes spored runs to read the host
// (PowerShell, quser and msg.exe on Windows; who, pgrep and wall on Linux).
//
// Every one of them goes through probeCmd, which ties the child to a context so
// cancelling the monitor actually stops it. They used to be exec.Command with no
// context, so Monitor could not return until the readers of a tick already in
// flight had finished. On Windows each reader is a fresh `powershell
// -NoProfile`, a cold start measured in seconds, and Monitor returned ~14s after
// its context was cancelled (#1104); spored's shutdown, hibernate preparation
// and resume handling wait on that loop. TestNoContextlessExecInAgent keeps a
// bare exec.Command from coming back.

// sysProbeTimeout bounds one probe even when its context is never cancelled: a
// wedged PowerShell or WMI query must not hold a tick for ever. A var so tests
// can shorten it.
var sysProbeTimeout = 30 * time.Second

// sysProbeWaitDelay bounds the wait for a killed probe's output pipes to close.
// Killing the child does not close a pipe a grandchild inherited (PowerShell
// can leave one), and without WaitDelay Output would wait for that grandchild.
const sysProbeWaitDelay = 2 * time.Second

// probeCmd builds a probe bound to ctx and sysProbeTimeout. The returned cancel
// must be called once the command has finished.
func probeCmd(ctx context.Context, name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, sysProbeTimeout)
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- every caller passes a literal tool name (powershell, quser, msg, who, pgrep, wall, ps) with its own fixed flags; exec runs no shell.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = sysProbeWaitDelay
	return cmd, cancel
}

// probeOutput runs a probe and returns its stdout. A caller that wants only
// the exit status discards the output.
func probeOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd, cancel := probeCmd(ctx, name, args...)
	defer cancel()
	return cmd.Output()
}
