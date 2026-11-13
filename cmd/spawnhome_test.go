//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// #1593: the DCV session pages embed a session token, so ~/.spawn, the
// sessions directory and every page in it are owner-only, including ones an
// older spawn left 0755/0644.
func TestSessionFilesAreOwnerOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessions := filepath.Join(home, ".spawn", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(home, ".spawn"), 0o755)
	_ = os.Chmod(sessions, 0o755)
	old := filepath.Join(sessions, "paraview-old.html")
	if err := os.WriteFile(old, []byte("const AUTH_TOKEN = 'x'"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(old, 0o644)

	path, err := writeSessionHTML("i-0123", "s", "", "203.0.113.1", "paraview", "d", "g5.xlarge", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{
		filepath.Join(home, ".spawn"): 0o700,
		sessions:                      0o700,
		old:                           0o600,
		path:                          0o600,
	} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o: another local user could read the session token", p, got, want)
		}
	}
}

// The cleanup log was the path that created ~/.spawn 0755.
func TestCleanupLogKeepsSpawnHomeOwnerOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, f := openCleanupLog()
	if f == nil {
		t.Fatal("no cleanup log")
	}
	_ = f.Close()
	for p, want := range map[string]os.FileMode{filepath.Join(home, ".spawn"): 0o700, path: 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", p, got, want)
		}
	}
}
