//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSysShellCommand_RunAsUser verifies the pre-stop hook is wrapped in
// `su - <user> -c` when a username is given (so it runs with the workload user's
// $HOME/env, not root's — #63), and falls back to a bare `sh -c` when not.
func TestSysShellCommand_RunAsUser(t *testing.T) {
	cmd := sysShellCommand(context.Background(), "aws s3 sync ~/out s3://b/", "ec2-user")
	args := cmd.Args
	// exec.CommandContext sets Args[0] to the resolved/looked-up path; assert on
	// the trailing, deterministic arguments instead.
	if !strings.HasSuffix(args[0], "su") {
		t.Errorf("run-as-user should exec su, got %q", args[0])
	}
	want := []string{"-", "ec2-user", "-c", "aws s3 sync ~/out s3://b/"}
	got := args[len(args)-len(want):]
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q (full: %v)", i, got[i], want[i], args)
		}
	}
}

func TestSysShellCommand_NoUserFallsBackToRoot(t *testing.T) {
	cmd := sysShellCommand(context.Background(), "echo hi", "")
	args := cmd.Args
	if !strings.HasSuffix(args[0], "sh") {
		t.Errorf("empty user should exec sh, got %q", args[0])
	}
	want := []string{"-c", "echo hi"}
	got := args[len(args)-len(want):]
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q (full: %v)", i, got[i], want[i], args)
		}
	}
}

func TestShellQuoteArg(t *testing.T) {
	cases := map[string]string{
		"/fsx": `'/fsx'`,
		"fs-0abc.fsx.us-east-1.amazonaws.com@tcp:/q5pdvb4v": `'fs-0abc.fsx.us-east-1.amazonaws.com@tcp:/q5pdvb4v'`,
		"a'b": `'a'\''b'`,
	}
	for in, want := range cases {
		if got := shellQuoteArg(in); got != want {
			t.Errorf("shellQuoteArg(%q) = %q, want %q", in, got, want)
		}
	}
}

// The idle decision with an SSH session left open and idle for two hours, and
// every other signal idle: the instance is idle. Before #953 the PTY's existence
// ("Not idle: Active terminals present") blocked idle-stop for ever.
func TestIsIdle_IdlePTYDoesNotBlockIdleStop(t *testing.T) {
	now := time.Now()
	origRoot, origLines := sessionDevRoot, loginTTYLines
	t.Cleanup(func() { sessionDevRoot, loginTTYLines = origRoot, origLines })
	sessionDevRoot = fakeDev(t, now, map[string]time.Duration{"pts/0": 2 * time.Hour})
	loginTTYLines = func(context.Context) []string { return []string{"pts/0"} }

	stubCPUTimes(t, [2]int64{1000, 1000}, [2]int64{1990, 2000}, [2]int64{2980, 3000})
	stubNetworkBytes(t, [2]int64{5000, 5000})
	stubDiskIOBytes(t, 4096)
	a := newTestAgent(t, nil)
	_, _ = a.cpuUsage(context.Background())
	_, _, _ = a.networkBytes(context.Background())
	_, _, _ = a.diskIOBytes(context.Background())

	if !a.isIdle(context.Background()) {
		t.Error("an idle PTY blocked the idle decision (#953)")
	}

	// And typing in it does block it.
	if err := os.Chtimes(filepath.Join(sessionDevRoot, "pts", "0"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if a.isIdle(context.Background()) {
		t.Error("a PTY with input just now did not count as an active session")
	}
}
