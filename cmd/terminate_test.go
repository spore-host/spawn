package cmd

import (
	"os"
	"testing"
)

// TestRunTerminate_ArgValidation checks the argument/flag combinations that are
// rejected before any AWS call.
func TestRunTerminate_ArgValidation(t *testing.T) {
	prevID, prevName, prevYes := terminateJobArrayID, terminateJobArrayName, terminateYes
	t.Cleanup(func() {
		terminateJobArrayID, terminateJobArrayName, terminateYes = prevID, prevName, prevYes
	})

	t.Run("job-array mode rejects positional arg", func(t *testing.T) {
		terminateJobArrayID, terminateJobArrayName = "ja-1", ""
		err := runTerminate(terminateCmd, []string{"i-123"})
		if err == nil {
			t.Fatal("expected error when job-array mode given an instance arg")
		}
	})

	t.Run("single mode requires exactly one arg", func(t *testing.T) {
		terminateJobArrayID, terminateJobArrayName = "", ""
		if err := runTerminate(terminateCmd, []string{}); err == nil {
			t.Error("expected error with no args in single mode")
		}
	})
}

// TestConfirmTerminate covers the --yes bypass and the interactive prompt
// accept/reject branches.
func TestConfirmTerminate(t *testing.T) {
	prevYes := terminateYes
	t.Cleanup(func() { terminateYes = prevYes })

	t.Run("yes flag bypasses prompt", func(t *testing.T) {
		terminateYes = true
		if !confirmTerminate("destroy?") {
			t.Error("--yes should auto-confirm")
		}
	})

	terminateYes = false
	cases := map[string]bool{"y\n": true, "yes\n": true, "n\n": false, "\n": false, "garbage\n": false}
	for input, want := range cases {
		t.Run("stdin="+input, func(t *testing.T) {
			restore := withStdin(t, input)
			defer restore()
			if got := confirmTerminate("destroy?"); got != want {
				t.Errorf("confirmTerminate(stdin=%q) = %v, want %v", input, got, want)
			}
		})
	}
}

// withStdin replaces os.Stdin with a pipe preloaded with s, returning a restore func.
func withStdin(t *testing.T, s string) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(s); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	return func() { os.Stdin = old; _ = r.Close() }
}
