//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// Tier 0 regression coverage for spawn#527: `spawn list --state all` used to
// pass "all" through as a literal EC2 instance-state-name filter value. Since
// no instance is ever literally in state "all", the filter matched nothing —
// exit 0, a well-formed empty JSON array, reading exactly like a clean
// account even when instances (including leaked ones) exist. The fix makes
// "all"/"any" mean "no state filter" (a genuine superset of the default,
// including terminated/shutting-down), and makes any other unrecognized
// --state value a hard, non-zero-exit error instead of a silent empty list.

// TestTier0_ListStateAll_ReturnsRunningAndTerminated is the primary
// regression test: given one running instance and one terminated instance,
// `--state all` must return BOTH. Before the fix this returned zero
// instances (the literal-"all" filter matched nothing).
func TestTier0_ListStateAll_ReturnsRunningAndTerminated(t *testing.T) {
	env := startSpawnSubstrate(t)

	// A single --count 2 launch (one setupSSHKey/ImportKeyPair round-trip)
	// rather than two separate `launch` invocations: Substrate's ImportKeyPair
	// treats a second import of the same key material within one env as
	// InvalidKeyPair.Duplicate, which spawn doesn't tolerate on retry here.
	pair := env.launchOK("pair", "--instance-type", "t3.small", "--count", "2", "--job-array-name", "pairgrp")
	if len(pair) != 2 {
		t.Fatalf("--count 2 launched %d instances, want 2", len(pair))
	}
	runningID := pair[0]["instance_id"].(string)
	terminatedID := pair[1]["instance_id"].(string)

	env.runOK("terminate", terminatedID, "-y")
	env.requireState(terminatedID, "terminated")

	seen := map[string]bool{}
	for _, inst := range mustJSONArray(t, env.runOK("list", "--state", "all", "-o", "json")) {
		if id, _ := inst["instance_id"].(string); id != "" {
			seen[id] = true
		}
	}
	if !seen[runningID] {
		t.Errorf("--state all missing the running instance %s", runningID)
	}
	if !seen[terminatedID] {
		t.Errorf("--state all missing the terminated instance %s "+
			"(spawn#527 regression: \"all\" must mean no filter, not a literal unmatchable filter value)", terminatedID)
	}

	// "any" is documented as an alias for "all" and must behave identically.
	seenAny := map[string]bool{}
	for _, inst := range mustJSONArray(t, env.runOK("list", "--state", "any", "-o", "json")) {
		if id, _ := inst["instance_id"].(string); id != "" {
			seenAny[id] = true
		}
	}
	if !seenAny[runningID] || !seenAny[terminatedID] {
		t.Errorf("--state any did not behave as an alias for --state all: seen=%v", seenAny)
	}
}

// TestTier0_ListNoFlag_ExcludesTerminated verifies the no-flag default is
// unchanged by the fix: it must show the running instance but exclude the
// terminated one (terminated is only visible via --state all/any or
// --state terminated explicitly).
func TestTier0_ListNoFlag_ExcludesTerminated(t *testing.T) {
	env := startSpawnSubstrate(t)

	pair := env.launchOK("defaultpair", "--instance-type", "t3.small", "--count", "2", "--job-array-name", "defaultgrp")
	if len(pair) != 2 {
		t.Fatalf("--count 2 launched %d instances, want 2", len(pair))
	}
	runningID := pair[0]["instance_id"].(string)
	terminatedID := pair[1]["instance_id"].(string)

	env.runOK("terminate", terminatedID, "-y")
	env.requireState(terminatedID, "terminated")

	seen := map[string]bool{}
	for _, inst := range mustJSONArray(t, env.runOK("list", "-o", "json")) {
		if id, _ := inst["instance_id"].(string); id != "" {
			seen[id] = true
		}
	}
	if !seen[runningID] {
		t.Errorf("default list is missing the running instance %s", runningID)
	}
	if seen[terminatedID] {
		t.Errorf("default list should exclude the terminated instance %s, but it was present", terminatedID)
	}
}

// TestTier0_ListStateBogus_ErrorsNotEmpty is the most important negative
// assertion in this regression suite: an unrecognized --state value must
// exit non-zero with an error naming the valid states, NOT exit 0 with "[]".
// A bug where "all" silently degraded to some other wrong-but-plausible
// filter would still pass a weaker "output is valid JSON" check — this test
// specifically asserts the failure mode, not just the output shape.
func TestTier0_ListStateBogus_ErrorsNotEmpty(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.launchOK("irrelevant", "--instance-type", "t3.small")

	stdout, stderr, code := env.run("list", "--state", "bogus", "-o", "json")
	if code == 0 {
		t.Fatalf("spawn list --state bogus exited 0, want non-zero; stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if strings.TrimSpace(stdout) == "[]" {
		t.Fatalf("spawn list --state bogus produced an empty JSON array instead of erroring — "+
			"this is exactly the spawn#527 failure mode (silent false-clean result)\nstdout:\n%s", stdout)
	}
	combined := stderr + stdout
	for _, want := range []string{"pending", "running", "stopping", "stopped"} {
		if !strings.Contains(combined, want) {
			t.Errorf("error message should name valid state %q; got:\n%s", want, combined)
		}
	}
}

// TestTier0_ListStateWrongCase_Errors verifies the alias match is exact-case:
// "ALL" is not accepted as a synonym for "all" — it's just another
// unrecognized value and must error the same way a typo would.
func TestTier0_ListStateWrongCase_Errors(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.launchOK("irrelevant2", "--instance-type", "t3.small")

	_, _, code := env.run("list", "--state", "ALL", "-o", "json")
	if code == 0 {
		t.Fatalf("spawn list --state ALL (wrong case) exited 0, want non-zero")
	}
}

// TestTier0_ListStateTypo_Errors verifies a near-miss typo of a real state
// ("runing" for "running") errors rather than silently returning nothing.
func TestTier0_ListStateTypo_Errors(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.launchOK("irrelevant3", "--instance-type", "t3.small")

	_, _, code := env.run("list", "--state", "runing", "-o", "json")
	if code == 0 {
		t.Fatalf("spawn list --state runing (typo) exited 0, want non-zero")
	}
}
