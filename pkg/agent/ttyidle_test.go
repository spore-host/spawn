package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// fakeDev builds a /dev with the named terminals, each with its own last-input
// time (atime). mtime is set to now for all of them: output to a terminal (a
// job printing progress in a detached tmux) is not input and must not count.
func fakeDev(t *testing.T, now time.Time, ttys map[string]time.Duration) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pts"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ptmx is always present and is not a terminal.
	if err := os.WriteFile(filepath.Join(root, "pts", "ptmx"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, idle := range ttys {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, nil, 0o620); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-idle), now); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The documented intent of countActiveSessions, asserted from its doc comment:
// "input within the last 5 minutes, so idle SSH/RDP connections don't block idle
// detection". Before #953 a PTY's existence blocked idle-stop however long it
// had been idle.
func TestSessionTTYs_OnlyRecentInputCounts(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		ttys  map[string]time.Duration
		login []string
		want  int
	}{
		{
			// The issue's fixture: an SSH session left open, nobody typing.
			name: "PTY open, idle 2h, so idle",
			ttys: map[string]time.Duration{"pts/0": 2 * time.Hour},
			want: 0,
		},
		{
			// A long job left running in a detached tmux: its pane PTY is written
			// to (mtime) but nobody types (atime).
			name: "detached tmux pane printing output",
			ttys: map[string]time.Duration{"pts/3": 6 * time.Hour},
			want: 0,
		},
		{
			name: "someone typing 30s ago",
			ttys: map[string]time.Duration{"pts/0": 30 * time.Second, "pts/1": 3 * time.Hour},
			want: 1,
		},
		{
			name: "just under and just over the 5-minute bar",
			ttys: map[string]time.Duration{"pts/0": sessionMaxIdle - 10*time.Second, "pts/1": sessionMaxIdle + 10*time.Second},
			want: 1,
		},
		{
			// A serial-console login is only found through utmp (`who`), and its
			// PTY-backed duplicate is counted once.
			name:  "serial console login, and a who line duplicating a PTY",
			ttys:  map[string]time.Duration{"ttyS0": 1 * time.Minute, "pts/0": 1 * time.Minute},
			login: []string{"ttyS0", "pts/0"},
			want:  2,
		},
		{
			name: "no terminals at all",
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := fakeDev(t, now, tc.ttys)
			got := countActiveTTYs(sessionTTYs(dev, tc.login), ttyInputTime, now, sessionMaxIdle)
			if got != tc.want {
				t.Errorf("active sessions = %d, want %d", got, tc.want)
			}
		})
	}
}

// The same through the production entry point, with the seams pointed at a
// fixture: an idle PTY counts for nothing.
func TestCountTTYSessions_IdlePTYIsNotASession(t *testing.T) {
	now := time.Now()
	origRoot, origLines := sessionDevRoot, loginTTYLines
	t.Cleanup(func() { sessionDevRoot, loginTTYLines = origRoot, origLines })
	sessionDevRoot = fakeDev(t, now, map[string]time.Duration{"pts/0": 2 * time.Hour, "pts/1": 40 * time.Minute})
	loginTTYLines = func() []string { return []string{"pts/0"} }
	if n := countTTYSessions(now); n != 0 {
		t.Errorf("countTTYSessions = %d with only idle PTYs, want 0 (#953)", n)
	}
}

func TestSessionTTYs_DiscoversPTYsAndRejectsEscapes(t *testing.T) {
	now := time.Now()
	dev := fakeDev(t, now, map[string]time.Duration{"pts/0": 0, "pts/12": 0})
	got := sessionTTYs(dev, []string{"../etc/passwd", "pts/12", ":0"})
	want := []string{filepath.Join(dev, "pts", "0"), filepath.Join(dev, "pts", "12"), filepath.Join(dev, ":0")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionTTYs = %v, want %v", got, want)
	}
}

// A terminal that disappeared (logout between listing and stat) is no session;
// one that cannot be read for another reason is assumed active.
func TestCountActiveTTYs_ErrorHandling(t *testing.T) {
	now := time.Now()
	gone := func(string) (time.Time, error) { return time.Time{}, os.ErrNotExist }
	if n := countActiveTTYs([]string{"/dev/pts/9"}, gone, now, sessionMaxIdle); n != 0 {
		t.Errorf("a vanished terminal counted as %d sessions", n)
	}
	broken := func(string) (time.Time, error) { return time.Time{}, errors.New("permission denied") }
	if n := countActiveTTYs([]string{"/dev/pts/9"}, broken, now, sessionMaxIdle); n != 1 {
		t.Errorf("an unreadable terminal counted as %d sessions, want 1 (assume active)", n)
	}
}

// `who` prints NAME LINE TIME [COMMENT]; the line is the second field whatever
// follows it.
func TestParseWhoLines(t *testing.T) {
	out := []byte("ec2-user pts/0        2026-10-15 10:00 (203.0.113.7)\n" +
		"root     ttyS0        2026-10-15 09:12\n\n")
	if got, want := parseWhoLines(out), []string{"pts/0", "ttyS0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parseWhoLines = %v, want %v", got, want)
	}
}
