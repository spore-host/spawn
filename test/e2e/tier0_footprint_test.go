//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// flatten collapses runs of whitespace so an assertion on wording survives the
// paragraph being re-wrapped. The first version of the test below searched for a
// contiguous "does NOT mean the account is empty" and failed on output that
// contained exactly that, wrapped across a newline — testing the line breaks
// rather than the sentence.
func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

// Tier 0 coverage for `spawn footprint` (#653).
//
// The command's value is not the list — it is that a zero or partial result
// cannot be MISREAD. Three separate hand-built footprint inventories during #653
// were each wrong, and the earlier #708 failure was the same shape: `spawn
// orphans` printed "No orphaned spawn-managed resources" in an account holding
// 21 of them. So what Tier 0 pins here is the wording that prevents that
// reading, not the row count.

// TestTier0_Footprint_EmptyAccountSaysWhatItLookedAt is the #708 guard.
//
// Against a fresh account the honest answer is "nothing MATCHED", and the report
// must say what it searched for — otherwise "Nothing found" reads as "the account
// is clean", which is exactly the false conclusion that let 21 orphans sit for
// three months.
func TestTier0_Footprint_EmptyAccountSaysWhatItLookedAt(t *testing.T) {
	env := startSpawnSubstrate(t)

	out := env.runOK("footprint", "--region", "us-east-1")
	flat := flatten(out)

	// It must never claim to be exhaustive.
	for _, want := range []string{
		"nothing is deleted",
		"does NOT mean the account is empty",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("footprint output is missing %q, so an empty result could be read as a clean\n"+
				"account — the #708 failure this wording exists to prevent:\n%s", want, out)
		}
	}
}

// TestTier0_Footprint_IsReadOnly pins the property the help text promises.
//
// A footprint report feeds deletion decisions, so it must not make any itself.
// Asserted by launching an instance, running footprint, and confirming the
// instance is still there — a report that deleted what it enumerated would be
// the worst possible version of this command.
func TestTier0_Footprint_IsReadOnly(t *testing.T) {
	env := startSpawnSubstrate(t)

	launched := env.launchOK("fp-readonly", "--instance-type", "t3.small")
	if len(launched) == 0 {
		t.Fatal("launch returned no instances")
	}
	id, _ := launched[0]["instance_id"].(string)
	if id == "" {
		t.Fatalf("launch result missing instance_id: %+v", launched[0])
	}

	env.runOK("footprint", "--region", "us-east-1")

	// Still listed afterwards.
	out := env.runOK("list", "--region", "us-east-1")
	if !strings.Contains(out, id) {
		t.Errorf("after `spawn footprint`, instance %s is no longer listed — footprint must be\n"+
			"read-only:\n%s", id, out)
	}
}

// TestTier0_Footprint_FindsATaggedInstance confirms the tagged half is wired to
// the same discovery path `resources` and `cleanup` use, rather than being a
// second, divergent implementation.
func TestTier0_Footprint_FindsATaggedInstance(t *testing.T) {
	env := startSpawnSubstrate(t)

	launched := env.launchOK("fp-tagged", "--instance-type", "t3.small")
	if len(launched) == 0 {
		t.Fatal("launch returned no instances")
	}
	id, _ := launched[0]["instance_id"].(string)

	out := env.runOK("footprint", "--region", "us-east-1")
	if !strings.Contains(out, id) {
		t.Errorf("footprint did not report the spawn:managed instance %s. The tagged half shares\n"+
			"DiscoverManagedResources with `resources`/`cleanup`, so a miss here means they have\n"+
			"diverged:\n%s", id, out)
	}
	if !strings.Contains(out, "spawn:managed") {
		t.Errorf("footprint output does not name the spawn:managed signal, so a reader cannot tell\n"+
			"a tag match from a name guess:\n%s", out)
	}
}

// TestTier0_Footprint_JSONIsParseable pins the machine-readable shape, which
// exists so an operator can diff two footprints over time rather than eyeballing
// a table.
func TestTier0_Footprint_JSONIsParseable(t *testing.T) {
	env := startSpawnSubstrate(t)

	out := env.runOK("footprint", "--region", "us-east-1", "-o", "json")
	// Named top-level keys rather than a bare array, so an empty account is
	// distinguishable from a failed scan.
	for _, want := range []string{`"tagged"`, `"control_plane"`, `"global"`} {
		if !strings.Contains(out, want) {
			t.Errorf("footprint --json is missing the %s key; a consumer cannot tell an empty\n"+
				"account from a failed scan without it:\n%s", want, out)
		}
	}
}
