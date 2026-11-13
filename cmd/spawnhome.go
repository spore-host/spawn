package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// spawnHomeSubdir returns ~/.spawn/<parts...>, creating it and ~/.spawn owner-only
// (0700). Both are tightened to 0700 when they exist looser: an older spawn
// made ~/.spawn 0755 (cleanup's log), and under it sit the DCV session pages,
// each embedding a session token, the audit log and the SSH keys (#1593). On a
// shared login or HPC host another local user could otherwise read a session
// page and take over the desktop. The chmod is best-effort: a directory spawn
// does not own keeps its mode, and the launch goes ahead.
func spawnHomeSubdir(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	base := filepath.Join(home, ".spawn")
	dir := filepath.Join(append([]string{base}, parts...)...)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		tightenToOwnerOnly(d)
		if d == base || !strings.HasPrefix(d, base) {
			break
		}
	}
	return dir, nil
}

// tightenToOwnerOnly chmods path to 0700 (a directory) or 0600 (a file) when it
// grants anything to group or other. Best-effort; Windows has no such bits.
func tightenToOwnerOnly(path string) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o077 == 0 {
		return
	}
	mode := os.FileMode(0o600)
	if fi.IsDir() {
		mode = 0o700
	}
	_ = os.Chmod(path, mode)
}
