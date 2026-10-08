package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The termination time decides whether an absent log means "the job did not
// fail" or "the capture has not populated yet". Those send a user in opposite
// directions, so a wrong time is worse than none — hence a zero time on anything
// unexpected rather than a guess.
func TestParseTransitionTime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		wantOK bool
	}{
		{"EC2's documented format", "User initiated (2026-10-08 16:48:02 GMT)", true},
		{"spot reclaim with a time", "Server.SpotInstanceTermination (2026-10-08 09:00:00 GMT)", true},
		{"no timestamp at all", "Client.InstanceInitiatedShutdown", false},
		{"empty", "", false},
		{"parens but not a date", "User initiated (soon)", false},
		{"unterminated paren", "User initiated (2026-10-08 16:48:02 GMT", false},
		{"reversed parens", "User initiated )2026-10-08 16:48:02 GMT(", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTransitionTime(tc.reason)
			if tc.wantOK && got.IsZero() {
				t.Errorf("parseTransitionTime(%q) = zero, want a parsed time", tc.reason)
			}
			if !tc.wantOK && !got.IsZero() {
				t.Errorf("parseTransitionTime(%q) = %v, want zero — a wrong time produces a "+
					"confidently wrong 'try again shortly'", tc.reason, got)
			}
		})
	}
}

// The cache must expire. "Leave no trace" applies to the user's disk too: an
// unexpiring pile of console dumps in ~/.spawn is the same un-reaped-footprint
// problem this project treats as a design flaw elsewhere.
func TestPruneConsoleCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".spawn", "cache", "console")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	fresh := filepath.Join(dir, "i-fresh.log")
	stale := filepath.Join(dir, "i-stale.log")
	for _, p := range []string{fresh, stale} {
		if err := os.WriteFile(p, []byte("console"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	// One day past the TTL.
	old := time.Now().Add(-consoleCacheTTL - 24*time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	pruneConsoleCache()

	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("pruned a fresh entry: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("kept an entry older than the %s TTL — the cache would grow without "+
			"bound", consoleCacheTTL)
	}
}

// The TTL must outlast its SOURCE, or the cache teaches you not to trust it. AWS
// served console output for instances DescribeInstances had entirely forgotten
// more than twelve hours later; a 6h cache would expire first.
func TestConsoleCacheTTLOutlastsMeasuredSourceRetention(t *testing.T) {
	const measuredSourceRetention = 12 * time.Hour
	if consoleCacheTTL <= measuredSourceRetention {
		t.Errorf("consoleCacheTTL = %s, which is not longer than the %s of source "+
			"retention actually measured — the cache would expire before AWS does",
			consoleCacheTTL, measuredSourceRetention)
	}
}

// Pruning must not explode when the cache has never been written.
func TestPruneConsoleCacheMissingDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pruneConsoleCache() // must not panic
}

// `logs` must be a top-level command, not buried under `array`. The whole point
// is that a user who does not know get-console-output exists can still answer
// "why did my job fail".
func TestLogsCommandIsRegisteredAtTopLevel(t *testing.T) {
	var found bool
	for _, c := range rootCmd.Commands() {
		if strings.HasPrefix(c.Use, "logs") {
			found = true
		}
	}
	if !found {
		t.Error("no top-level `logs` command registered")
	}
}

// The failure paths must POINT AT `spawn logs`, or the command is undiscoverable
// and the user is back to needing to know `get-console-output` exists.
//
// These are the two places someone lands when asking "why did it vanish?": the
// generic not-found path, and `terminate` re-run on an instance that already
// self-terminated.
func TestFailurePathsPointAtSpawnLogs(t *testing.T) {
	for _, f := range []string{"utils.go", "terminate.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(b), "spawn logs ") {
			t.Errorf("%s reports a gone instance without pointing at `spawn logs` — the "+
				"user has to already know the verb, which defeats the point of adding it", f)
		}
	}
}
