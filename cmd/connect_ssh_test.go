package cmd

import (
	"strings"
	"testing"
)

// TestBuildSSHCommandArgs_Interactive: no remote command → last arg is the
// user@host target, and the standard non-multiplexing options are present.
func TestBuildSSHCommandArgs_Interactive(t *testing.T) {
	args := buildSSHCommandArgs("ec2-user", "1.2.3.4", "key.pem", 22, "", false)

	if got := args[len(args)-1]; got != "ec2-user@1.2.3.4" {
		t.Errorf("interactive: last arg = %q, want ec2-user@1.2.3.4", got)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-i key.pem", "ControlMaster=no", "ControlPath=none", "-p 22"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
}

// TestBuildSSHCommandArgs_OneShotVerbatim: a one-shot command is appended
// exactly as given (the caller does any shell wrapping). This is what lets the
// Windows --ssh path pass a PowerShell command WITHOUT a bash -c wrapper, while
// Linux callers wrap in bash -c themselves before calling.
func TestBuildSSHCommandArgs_OneShotVerbatim(t *testing.T) {
	// Linux-style (already bash -c wrapped by the caller).
	linux := buildSSHCommandArgs("ec2-user", "1.2.3.4", "k", 22, "bash -c 'echo hi'", false)
	if got := linux[len(linux)-1]; got != "bash -c 'echo hi'" {
		t.Errorf("linux one-shot last arg = %q", got)
	}

	// Windows-style (PowerShell, no wrapper).
	win := buildSSHCommandArgs("Administrator", "1.2.3.4", "k", 22, "Get-ComputerInfo", false)
	if got := win[len(win)-1]; got != "Get-ComputerInfo" {
		t.Errorf("windows one-shot last arg = %q, want unwrapped PowerShell command", got)
	}
	if strings.Contains(strings.Join(win, " "), "bash -c") {
		t.Error("windows --ssh one-shot must NOT be wrapped in bash -c (remote shell is PowerShell)")
	}
}

// TestBuildSSHCommandArgs_Port honors a non-default port.
func TestBuildSSHCommandArgs_Port(t *testing.T) {
	args := buildSSHCommandArgs("Administrator", "h", "k", 2222, "", false)
	if !strings.Contains(strings.Join(args, " "), "-p 2222") {
		t.Errorf("custom port not honored: %v", args)
	}
}

// TestBuildSSHCommandArgs_TTY: the --tty/-t flag adds `-t` (force PTY allocation,
// which line-buffers remote stdout — #582), and WITHOUT it the argument vector is
// byte-for-byte unchanged from today so the default (block-buffered, separate
// stdout/stderr) behavior is preserved for callers piping binary output.
func TestBuildSSHCommandArgs_TTY(t *testing.T) {
	noTTY := buildSSHCommandArgs("ec2-user", "1.2.3.4", "k", 22, "long-cmd", false)
	if slicesContains(noTTY, "-t") {
		t.Errorf("default (no --tty) must NOT allocate a PTY: %v", noTTY)
	}

	withTTY := buildSSHCommandArgs("ec2-user", "1.2.3.4", "k", 22, "long-cmd", true)
	if !slicesContains(withTTY, "-t") {
		t.Errorf("--tty must add -t to force PTY allocation: %v", withTTY)
	}

	// The only difference between the two must be the single added -t: the target
	// and remote command are untouched.
	if got := withTTY[len(withTTY)-1]; got != "long-cmd" {
		t.Errorf("--tty must not disturb the remote command; last arg = %q", got)
	}
	if len(withTTY) != len(noTTY)+1 {
		t.Errorf("--tty must add exactly one arg (-t): no-tty=%v tty=%v", noTTY, withTTY)
	}
}

func slicesContains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}
