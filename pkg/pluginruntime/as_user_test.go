package pluginruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// TestRunCommand_AsUser_NoLocalUserRunsAsRoot verifies that an as_user step with
// no known local user falls back to running normally (as spored's uid), rather
// than failing — older instances have no spawn:local-username.
func TestRunCommand_AsUser_NoLocalUserRunsAsRoot(t *testing.T) {
	out := filepath.Join(t.TempDir(), "ran")
	e := NewRemoteExecutor("") // no local user
	step := plugin.Step{Type: "run", AsUser: true, Run: "echo ok > " + out}
	if err := e.runCommand(context.Background(), step); err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("step did not run (fallback-to-root path): %v", err)
	}
}

// TestRunCommand_NonAsUser_RunsNormally is the baseline: a plain step runs.
func TestRunCommand_NonAsUser_RunsNormally(t *testing.T) {
	out := filepath.Join(t.TempDir(), "ran")
	e := NewRemoteExecutor("someuser") // set, but step doesn't opt in
	step := plugin.Step{Type: "run", Run: "echo ok > " + out}
	if err := e.runCommand(context.Background(), step); err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("plain step did not run: %v", err)
	}
}

// TestRunCommand_AsUser_InvalidUsernameRejected ensures a bogus local username
// is rejected rather than shelled into su.
func TestRunCommand_AsUser_InvalidUsernameRejected(t *testing.T) {
	e := NewRemoteExecutor("bad user;rm -rf") // invalid per security.ValidateUsername
	step := plugin.Step{Type: "run", AsUser: true, Run: "true"}
	if err := e.runCommand(context.Background(), step); err == nil {
		t.Error("expected an error for an invalid as_user username, got nil")
	}
}
