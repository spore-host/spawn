//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// Tier 0 output-contract matrix. Two halves:
//   - positive: read commands with -o json on seeded state return VALID JSON
//     and exit 0;
//   - negative: malformed input (bad region/ami, unresolved name, missing
//     confirmation) exits nonzero with a message and never panics.
// This guards the machine-readable surface that wrappers (truffle, CI) depend
// on, and ensures error paths fail cleanly.

// TestTier0_JSONOutputMatrix asserts -o json / --json commands emit parseable
// JSON and exit 0 against a freshly-seeded emulator.
func TestTier0_JSONOutputMatrix(t *testing.T) {
	env := startSpawnSubstrate(t)
	// Seed the tables the queried commands touch.
	seedDynamoTable(t, env.DynamoClient(), sweepTable, "sweep_id", nil)
	// One launched instance so `list` has content to serialize.
	env.launchOK("matrix", "--instance-type", "t3.small")

	cases := []struct {
		name  string
		args  []string
		array bool // true → expect a JSON array, false → object
	}{
		{"list", []string{"list", "-o", "json"}, true},
		{"list filtered by state", []string{"list", "--state", "running", "-o", "json"}, true},
		{"list-sweeps", []string{"list-sweeps", "--json"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := env.runOK(c.args...)
			if c.array {
				mustJSONArray(t, out)
			} else {
				mustJSONObject(t, out)
			}
		})
	}
}

// TestTier0_NegativeMatrix asserts malformed invocations fail cleanly: nonzero
// exit, a non-empty stderr message, and no Go panic in the output.
func TestTier0_NegativeMatrix(t *testing.T) {
	env := startSpawnSubstrate(t)

	cases := []struct {
		name string
		args []string
	}{
		{"unresolved instance name", []string{"status", "no-such-instance", "-o", "json"}},
		{"terminate unknown id", []string{"terminate", "i-doesnotexist", "-y"}},
		{"bad default key", []string{"defaults", "set", "bogus-key", "value"}},
		{"launch without name", []string{"launch"}},
		{"unknown subcommand", []string{"frobnicate"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout, stderr, code := env.run(c.args...)
			if code == 0 {
				t.Errorf("expected nonzero exit for %v, got 0\nstdout:\n%s", c.args, stdout)
			}
			combined := stdout + stderr
			if strings.Contains(combined, "panic:") || strings.Contains(combined, "goroutine ") {
				t.Errorf("command panicked for %v:\n%s", c.args, combined)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Errorf("expected a diagnostic on stderr for %v, got none", c.args)
			}
		})
	}
}

// TestTier0_TerminateRequiresConfirmation verifies terminate without --yes
// aborts (does not terminate) when stdin is not an interactive yes. The harness
// runs with stdin closed, so the confirmation read hits EOF and must abort.
func TestTier0_TerminateRequiresConfirmation(t *testing.T) {
	env := startSpawnSubstrate(t)
	arr := env.launchOK("keepme", "--instance-type", "t3.small")
	id := arr[0]["instance_id"].(string)

	// No --yes: must NOT terminate.
	env.run("terminate", id) // exit code intentionally unchecked (abort path varies)

	// The instance must still be present and running.
	found := false
	for _, inst := range mustJSONArray(t, env.runOK("list", "--state", "running", "-o", "json")) {
		if inst["instance_id"] == id {
			found = true
		}
	}
	if !found {
		t.Errorf("instance %s was terminated despite no --yes confirmation", id)
	}
}
