package progress

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

// TestQuietProgress_NoStdout verifies a quiet progress tracker writes nothing to
// stdout, so machine-readable output (-o json) stays clean (#21).
func TestQuietProgress_NoStdout(t *testing.T) {
	out := captureStdout(t, func() {
		p := NewQuietProgress()
		p.Start("spawn.progress.detecting_ami")
		p.Complete("spawn.progress.detecting_ami")
		p.Start("spawn.progress.launching_instance")
		p.Error("spawn.progress.launching_instance", errors.New("boom"))
		p.Skip("spawn.progress.setup_ssh_key")
		p.DisplaySuccess("i-123", "1.2.3.4", "ssh ec2-user@1.2.3.4", nil)
	})
	if out != "" {
		t.Errorf("quiet progress wrote to stdout:\n%q", out)
	}
}

// TestProgress_WritesWhenNotQuiet confirms the normal tracker does emit the TUI,
// so the quiet guard is actually doing something (not vacuously passing).
func TestProgress_WritesWhenNotQuiet(t *testing.T) {
	out := captureStdout(t, func() {
		p := NewProgress()
		p.Start("spawn.progress.detecting_ami")
		p.Complete("spawn.progress.detecting_ami")
	})
	if out == "" {
		t.Error("non-quiet progress produced no stdout; expected TUI output")
	}
}

// TestQuietProgress_StillTracksState verifies suppressing output does not break
// step bookkeeping — Start/Complete still update the underlying steps.
func TestQuietProgress_StillTracksState(t *testing.T) {
	p := NewQuietProgress()
	p.Start("spawn.progress.detecting_ami")
	p.Complete("spawn.progress.detecting_ami")
	for _, s := range p.steps {
		if s.Name == "spawn.progress.detecting_ami" && s.Status != "complete" {
			t.Errorf("step status = %q, want complete", s.Status)
		}
	}
}

func TestGetSymbol(t *testing.T) {
	for _, status := range []string{"pending", "running", "complete", "error", "skipped", "unknown"} {
		if got := getSymbol(status); got == "" {
			t.Errorf("getSymbol(%q) returned empty", status)
		}
	}
}
