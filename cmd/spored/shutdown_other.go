//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// waitForShutdown blocks until the process is asked to stop. On Unix-like
// systems that's SIGINT or SIGTERM (systemd sends SIGTERM on `systemctl stop`).
func waitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
}

// powerOff shuts the machine down from the OS. Used to retire a Spot
// replacement (#995), which carries no spawn tags and so is outside spored's
// tag-scoped TerminateInstances grant. Fixed argv, no shell.
func powerOff(ctx context.Context) error {
	return exec.CommandContext(ctx, "shutdown", "-h", "now").Run()
}
