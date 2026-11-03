//go:build windows

package main

import (
	"os"
	"path/filepath"
)

// sporedLogPath is the spored daemon log file on Windows. It lives under
// %PROGRAMDATA%\spored (writable by the LocalSystem service account), falling
// back to the OS temp dir if PROGRAMDATA is unset.
func sporedLogPath() string {
	base := os.Getenv("PROGRAMDATA")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "spored")
	_ = os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "spored.log")
}

// egressProxyStatePath is where `spored egress-proxy` persists its last
// activity across restarts (#1515).
func egressProxyStatePath() string {
	base := os.Getenv("PROGRAMDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "spored", "egress-proxy-activity.json")
}
