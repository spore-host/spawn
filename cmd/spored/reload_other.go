//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spore-host/spawn/pkg/agent"
)

// notifyReload subscribes to SIGHUP, the daemon's reload request (#1278). It is
// called first thing in runDaemon, so a SIGHUP that arrives while the agent is
// still being built is queued rather than stopping the process (SIGHUP's
// default action).
func notifyReload() <-chan struct{} {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	out := make(chan struct{}, 1)
	go func() {
		for range sig {
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()
	return out
}

func newDaemonReloader() daemonReloader {
	return daemonReloader{
		markerPath: agent.ReloadMarkerPath(),
		daemonPID:  systemdMainPID,
		signal: func(_ context.Context, pid int) error {
			return syscall.Kill(pid, syscall.SIGHUP)
		},
		sleep:    sleepFor,
		poll:     reloadPollInterval,
		attempts: reloadPollAttempts,
	}
}

// systemdMainPID is spored.service's main pid, 0 when the unit is not running
// or does not exist. reload signals that one process rather than running
// `systemctl kill -s HUP spored`: by default that signals every process in the
// unit's cgroup, which includes the plugin steps spored runs, and SIGHUP would
// stop them. `--value` would be tidier but needs systemd 230, newer than
// Amazon Linux 2's.
func systemdMainPID(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=MainPID", "spored").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return 0, fmt.Errorf("systemctl show spored: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return 0, fmt.Errorf("systemctl show spored: %w", err)
	}
	return parseMainPID(string(out))
}

// parseMainPID reads `systemctl show --property=MainPID` output.
func parseMainPID(out string) (int, error) {
	v, ok := strings.CutPrefix(strings.TrimSpace(out), "MainPID=")
	if !ok {
		return 0, fmt.Errorf("unexpected systemctl output %q", out)
	}
	pid, err := strconv.Atoi(v)
	if err != nil || pid < 0 {
		return 0, fmt.Errorf("unexpected MainPID %q", v)
	}
	return pid, nil
}
