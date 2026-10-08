package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A Lambda handler must not start a goroutine that outlives it.
//
// The runtime FREEZES the execution environment when the handler returns, so a
// goroutine still running at that point never resumes — or resumes much later
// against a cancelled context on an unrelated invocation. `cleanupPlacementGroup`
// opened with a 30-second sleep and was started with `go` immediately before
// `return nil` at all four of its call sites, so the placement group was NEVER
// deleted, for the life of the feature (#752). #685 found nine orphaned
// placement groups in one region of one account and was diagnosed as a CLI-side
// ordering bug; this was a second, unidentified mechanism producing them.
//
// The specific lethal shape is `go` + a sleep: a fire-and-forget goroutine doing
// a single fast API call usually completes before the freeze, which is why the
// availability-tracking ones elsewhere in this file are tolerated. A sleep makes
// non-completion a certainty rather than a race.
//
// Scans both orchestrator Lambdas, since they had the same bug independently.
func TestNoSleepingGoroutinesInLambdaHandlers(t *testing.T) {
	for _, rel := range []string{"main.go", "../pipeline-orchestrator/main.go"} {
		path := filepath.Clean(rel)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.Split(string(b), "\n")

		// Which top-level functions SLEEP anywhere in their body. Needed because
		// the real bug was `go cleanupPlacementGroup(...)` — a named function
		// whose sleep sat 1200 lines from the call site, so a
		// "sleep within N lines of the go" check never saw it. My first version of
		// this gate had exactly that hole and passed against a deliberate revert
		// of the original bug.
		sleepers := map[string]bool{}
		funcDecl := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z_][A-Za-z0-9_]*)\(`)
		current := ""
		for _, line := range lines {
			if m := funcDecl.FindStringSubmatch(line); m != nil {
				current = m[1]
				continue
			}
			if current != "" && !strings.HasPrefix(strings.TrimSpace(line), "//") &&
				strings.Contains(line, "time.Sleep(") {
				sleepers[current] = true
			}
		}

		// A `go` starting either an inline func that sleeps, or a named function
		// known to sleep.
		goInline := regexp.MustCompile(`^\s*go\s+func\s*\(`)
		goNamed := regexp.MustCompile(`^\s*go\s+([A-Za-z_][A-Za-z0-9_]*)\(`)
		sleep := regexp.MustCompile(`time\.Sleep\(`)

		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if goInline.MatchString(line) {
				for j := i + 1; j < min(i+8, len(lines)); j++ {
					if !strings.HasPrefix(strings.TrimSpace(lines[j]), "//") && sleep.MatchString(lines[j]) {
						t.Errorf("%s:%d starts an inline goroutine that sleeps (%s:%d) — the Lambda "+
							"runtime freezes on handler return, so it will never finish. Do the work "+
							"synchronously, or hand it to the TTL reaper.", path, i+1, path, j+1)
					}
				}
				continue
			}
			if m := goNamed.FindStringSubmatch(line); m != nil && sleepers[m[1]] {
				t.Errorf("%s:%d starts goroutine %s(), whose body sleeps — the Lambda runtime "+
					"freezes on handler return, so it will never finish (#752). Call it "+
					"synchronously, or hand the work to the TTL reaper.", path, i+1, m[1])
			}
		}

		// And no function reachable as a cleanup should contain a bare long sleep
		// at all: a flat wait for TerminateInstances is a race in one direction and
		// dead time in the other, since termination has no fixed duration.
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if m := regexp.MustCompile(`time\.Sleep\((\d+)\s*\*\s*time\.Second\)`).FindStringSubmatch(line); m != nil {
				if m[1] == "30" {
					t.Errorf("%s:%d has a flat 30s sleep — use "+
						"spawnaws.RetryPlacementGroupDelete, which retries only "+
						"InvalidPlacementGroup.InUse against a budget (#752)", path, i+1)
				}
			}
		}
	}
}

// The fix must actually use the shared policy rather than reimplementing it. A
// local reimplementation would pass the scan above while losing the
// InUse-only classifier — and then a permissions error would be retried for the
// full budget instead of surfacing at once.
func TestCleanupUsesTheSharedRetryPolicy(t *testing.T) {
	for _, path := range []string{"main.go", "../pipeline-orchestrator/main.go"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(b), "RetryPlacementGroupDelete") {
			t.Errorf("%s does not call spawnaws.RetryPlacementGroupDelete — the retry "+
				"budget, interval and InUse-only classifier live there and should not be "+
				"reimplemented", path)
		}
	}
}
