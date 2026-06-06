package cmd

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSporedUnit_NoPrivateTmp guards against regressing #66.
//
// spored watches the host-side completion file (default /tmp/SPAWN_COMPLETE),
// which users, `spawn connect`, and nf-spawn write to the real /tmp. systemd's
// PrivateTmp=true gives the daemon an isolated /tmp where it can never see that
// file, silently disabling --on-complete and --pre-stop. Every place that emits
// the spored systemd unit must therefore NOT set PrivateTmp=true.
//
// We scan the source files that define the unit for an active PrivateTmp=true
// directive (ignoring comment lines, which may mention it to explain the ban).
func TestSporedUnit_NoPrivateTmp(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..") // .../spawn

	// Files that emit (or template) the spored systemd unit.
	files := []string{
		filepath.Join(repoRoot, "cmd", "launch.go"),
		filepath.Join(repoRoot, "scripts", "spored.service"),
		filepath.Join(repoRoot, "scripts", "install-spored.sh"),
	}

	for _, f := range files {
		fh, err := os.Open(f) //nolint:gosec // test reads known repo files
		if err != nil {
			t.Fatalf("open %s: %v", f, err)
		}
		scanner := bufio.NewScanner(fh)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := strings.TrimSpace(scanner.Text())
			// Skip comments (Go //, shell/systemd #) — they may mention the ban.
			if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.Contains(line, "PrivateTmp=true") {
				t.Errorf("%s:%d sets PrivateTmp=true — this hides /tmp/SPAWN_COMPLETE "+
					"from spored and breaks --on-complete/--pre-stop (#66):\n  %s", f, lineNo, line)
			}
		}
		_ = fh.Close()
		if err := scanner.Err(); err != nil {
			t.Fatalf("scan %s: %v", f, err)
		}
	}
}
