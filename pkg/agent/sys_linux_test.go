//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"
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
