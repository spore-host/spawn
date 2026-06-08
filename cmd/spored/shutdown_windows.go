//go:build windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// stopCh is closed when a stop is requested. When spored runs as a Windows
// Service the SCM handler (service_windows.go) closes it on Stop/Shutdown; when
// run interactively it's closed on Ctrl-C / SIGTERM. waitForShutdown blocks on
// whichever fires first.
var stopCh = make(chan struct{})

// requestStop closes stopCh exactly once (safe to call from the SCM handler).
var stopOnce = make(chan struct{}, 1)

func requestStop() {
	select {
	case stopOnce <- struct{}{}:
		close(stopCh)
	default:
		// already requested
	}
}

// waitForShutdown blocks until stop is requested via the SCM handler or a
// console signal. Under the service, the SCM path drives requestStop(); when
// interactive, signals do.
func waitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stopCh:
	case <-sigChan:
		requestStop()
	}
}
