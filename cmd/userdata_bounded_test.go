package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// No generated user-data may contain an UNBOUNDED wait.
//
// cmd/burst.go shipped `while [ ! -f /usr/local/bin/spored ]; do sleep 5; done`
// — no deadline, no failure path (#752). A failed spored install (bad download,
// checksum mismatch, wrong architecture, no network) hung the boot forever with
// no output and no completion record.
//
// That matters more than an ordinary hang: an instance with no spored has NO
// TTL, idle or cost enforcement in-instance (#50), so a silently hung boot is
// also a silently unbounded bill. Every other generated script here caps its
// waits — pkg/userdata/queue.go at 300s with a message naming cloud-init,
// pkg/launcher/bootstrap.go's readiness barrier at 600s with a named failure.
//
// Scans the Go files that EMBED shell, since the scripts live in string
// literals and no shell linter ever sees them.
func TestGeneratedUserDataHasNoUnboundedWait(t *testing.T) {
	roots := []string{".", "../pkg/userdata", "../pkg/launcher", "../pkg/taskproto"}

	// A `while` whose condition has no counter/deadline term, followed by a sleep
	// before the loop closes. The bounded form always carries a `-lt`/`-le`
	// comparison or a `break` on a deadline.
	whileLine := regexp.MustCompile(`^\s*while\s+(\[|!|:)`)
	sleepLine := regexp.MustCompile(`^\s*sleep\s+\d+`)
	// A COMPARISON bounds a loop. Incrementing a counter does not — and the first
	// version of this gate accepted a bare `WAITED=$((WAITED + 5))` in the body
	// as proof of boundedness, so it passed against a deliberate revert that
	// removed only the comparison from the while condition. The counter was still
	// being incremented, forever.
	comparison := regexp.MustCompile(`-lt|-le|-ge|-gt`)

	checked := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
				strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, e.Name())
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				continue
			}
			lines := strings.Split(string(b), "\n")
			for i, line := range lines {
				if !whileLine.MatchString(line) || comparison.MatchString(line) {
					continue
				}
				// Look ahead for a sleep before the loop closes, and for any
				// bounding term inside the body.
				hasSleep, isBounded := false, false
				for j := i + 1; j < min(i+10, len(lines)); j++ {
					body := lines[j]
					if strings.TrimSpace(body) == "done" {
						break
					}
					if sleepLine.MatchString(body) {
						hasSleep = true
					}
					// Only a comparison, or a break reachable from one, bounds it.
					if comparison.MatchString(body) || strings.Contains(body, "break") {
						isBounded = true
					}
				}
				if hasSleep && !isBounded {
					t.Errorf("%s:%d generates an UNBOUNDED wait: %q\n"+
						"A failed prerequisite would hang the boot forever. Cap it like "+
						"pkg/userdata/queue.go does (MAX_WAIT + a non-zero exit naming "+
						"cloud-init) — and remember an instance with no spored has no TTL "+
						"enforcement, so a hung boot is an unbounded bill.",
						path, i+1, strings.TrimSpace(line))
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no `while` loops in any generated shell — the scan is broken, not the repo")
	}
	t.Logf("checked %d generated wait loop(s)", checked)
}
