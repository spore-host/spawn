//go:build !windows

package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// SIGHUP is the daemon's reload request on Unix (#1278). Without the handler
// its default action stops the process, which here is the test binary.
func TestNotifyReload_SIGHUP(t *testing.T) {
	ch := notifyReload()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("SIGHUP did not produce a reload request")
	}
}

func TestParseMainPID(t *testing.T) {
	for in, want := range map[string]int{"MainPID=1234\n": 1234, "MainPID=0": 0} {
		if got, err := parseMainPID(in); err != nil || got != want {
			t.Errorf("parseMainPID(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "Foo=1", "MainPID=x", "MainPID=-1"} {
		if _, err := parseMainPID(bad); err == nil {
			t.Errorf("parseMainPID(%q) accepted", bad)
		}
	}
}
