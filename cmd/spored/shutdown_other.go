//go:build !windows

package main

import (
	"os"
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
