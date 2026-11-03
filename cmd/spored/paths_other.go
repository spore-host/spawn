//go:build !windows

package main

// sporedLogPath is the spored daemon log file on Unix-like systems.
func sporedLogPath() string { return "/var/log/spored.log" }

// egressProxyStatePath is where `spored egress-proxy` persists its last
// activity across restarts (#1515), beside the compute checkpoint.
func egressProxyStatePath() string { return "/var/lib/spored/egress-proxy-activity.json" }
