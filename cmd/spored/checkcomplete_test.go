package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckCompleteCode covers the standardized --check-complete exit codes (#26).
func TestCheckCompleteCode(t *testing.T) {
	dir := t.TempDir()

	t.Run("absent file => running (2)", func(t *testing.T) {
		if got := checkCompleteCode(filepath.Join(dir, "nope")); got != 2 {
			t.Errorf("got %d, want 2", got)
		}
	})

	t.Run("empty file => complete (0)", func(t *testing.T) {
		p := filepath.Join(dir, "empty")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := checkCompleteCode(p); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})

	t.Run("success metadata => complete (0)", func(t *testing.T) {
		p := filepath.Join(dir, "ok.json")
		if err := os.WriteFile(p, []byte(`{"status":"success","message":"done"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := checkCompleteCode(p); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})

	t.Run("failed metadata => failed (1)", func(t *testing.T) {
		for _, status := range []string{"failed", "failure", "error", "FAILED"} {
			p := filepath.Join(dir, "fail-"+status+".json")
			if err := os.WriteFile(p, []byte(`{"status":"`+status+`"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := checkCompleteCode(p); got != 1 {
				t.Errorf("status %q: got %d, want 1", status, got)
			}
		}
	})

	t.Run("non-JSON content => complete (0)", func(t *testing.T) {
		p := filepath.Join(dir, "plain.txt")
		if err := os.WriteFile(p, []byte("all done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := checkCompleteCode(p); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
}
