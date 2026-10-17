package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Session idle time for the idle check (#953).
//
// countActiveSessions' contract is that a session counts only while someone is
// typing in it: "input within the last 5 minutes, so idle SSH/RDP connections
// don't block idle detection." This file is how Linux keeps it. It is not
// build-tagged, so the logic is tested on the darwin machines most development
// happens on; sys_linux.go wires it to /dev.
//
// The idle time of a terminal is the atime of its device node. The tty layer
// sets it on every read of input (tty_update_time, rounded to 8 seconds), so it
// is the time a process last consumed a keystroke from that terminal. It is
// also exactly what w(1) prints in its IDLE column, read directly rather than
// parsed out of w's output: w's columns move when FROM is not printed, which is
// procps-ng's default build, and a parser that took the wrong column read every
// session as active (#953, follow-up comment).
//
// Before #953 the session check above was undone by two presence checks: any
// numeric entry in /dev/pts, and any still-open login in `last -s -5min`. A PTY
// exists for every open SSH session however long it has been idle, and for every
// detached tmux or screen session, `docker exec -t` and SSM shell, so leaving any
// of them behind blocked --idle-timeout for ever.

// sessionMaxIdle is how recent a terminal's last input must be for its session
// to count as active.
const sessionMaxIdle = 5 * time.Minute

// sessionDevRoot is where terminal device nodes live; a var so tests can point
// it at a fixture.
var sessionDevRoot = "/dev"

// loginTTYLines returns the terminal ("line") of each logged-in user, relative
// to sessionDevRoot, for example "pts/0" or "ttyS0". It adds the console and
// serial logins, which are not under /dev/pts, to the PTYs found there. A var so
// tests can replace the `who` call.
var loginTTYLines = whoLines

// whoLines lists the LINE column of `who`, which is always the second field
// (NAME LINE TIME [COMMENT]). An error or no utmp gives nil: the PTYs in
// /dev/pts are still examined.
func whoLines() []string {
	out, err := exec.Command("who").Output()
	if err != nil {
		return nil
	}
	return parseWhoLines(out)
}

func parseWhoLines(out []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			lines = append(lines, f[1])
		}
	}
	return lines
}

// sessionTTYs returns the device paths whose input time decides whether a
// session is active: every PTY under devRoot/pts, which covers SSH, SSM, tmux,
// screen and docker -t whether or not they wrote utmp, plus each login line
// (console and serial logins). Duplicates are removed, and a line that would
// leave devRoot is dropped.
func sessionTTYs(devRoot string, loginLines []string) []string {
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(devRoot, "pts")); err == nil {
		for _, e := range entries {
			// ptmx is the multiplexer, not a terminal; PTYs are numbered.
			if _, err := strconv.Atoi(e.Name()); err == nil {
				add(filepath.Join(devRoot, "pts", e.Name()))
			}
		}
	}
	root := filepath.Clean(devRoot)
	for _, line := range loginLines {
		p := filepath.Join(root, line)
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			continue
		}
		add(p)
	}
	return paths
}

// ttyInputTime returns when the terminal at path last read input: its atime.
func ttyInputTime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fileAtime(fi), nil
}

// countActiveTTYs counts the terminals whose last input is within maxIdle of
// now. Presence alone never counts. A terminal that has gone (the session ended
// between listing and stat) is skipped; one whose time cannot be read for any
// other reason counts as active, the conservative direction for a check that
// stops instances.
func countActiveTTYs(paths []string, inputTime func(string) (time.Time, error), now time.Time, maxIdle time.Duration) int {
	active := 0
	for _, p := range paths {
		t, err := inputTime(p)
		if err != nil {
			if !os.IsNotExist(err) {
				active++
			}
			continue
		}
		if now.Sub(t) < maxIdle {
			active++
		}
	}
	return active
}

// countTTYSessions is the Linux session count: terminals with input in the last
// sessionMaxIdle.
func countTTYSessions(now time.Time) int {
	return countActiveTTYs(sessionTTYs(sessionDevRoot, loginTTYLines()), ttyInputTime, now, sessionMaxIdle)
}
