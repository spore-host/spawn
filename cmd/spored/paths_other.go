//go:build !windows

package main

// sporedLogPath is the spored daemon log file on Unix-like systems.
func sporedLogPath() string { return "/var/log/spored.log" }
