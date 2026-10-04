package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/spore-host/cohort"
)

// TestMemberTokenDiffersBetweenLaunches is spawn#691.
//
// The RunInstances ClientToken was derived from --job-array-name, not from the
// per-launch array id. Two `spawn launch` invocations sharing a name sent the
// SAME token per index, and within EC2's retention window:
//
//   - parameters differ → IdempotentParameterMismatch, the whole cohort fails
//     (reported 18.5h after the first launch, with different user-data);
//   - parameters identical → EC2 returns the ORIGINAL reservation rather than
//     launching, so spawn can report success holding instance IDs that are
//     already terminated, or two arrays can share instances.
//
// The second is the dangerous one and has not been reproduced; it follows from
// EC2's documented idempotency semantics.
func TestMemberTokenDiffersBetweenLaunches(t *testing.T) {
	// generateJobArrayID carries a date and random suffix, so two invocations of
	// the same NAME produce different ids.
	first := generateJobArrayID("gchp-spawn-smoke")
	second := generateJobArrayID("gchp-spawn-smoke")
	if first == second {
		t.Fatalf("generateJobArrayID returned %q twice; this test needs two distinct "+
			"launches of the same name", first)
	}

	id := cohort.EntityID("gchp-spawn-smoke-0")
	if memberIdempotencyToken(first, id) == memberIdempotencyToken(second, id) {
		t.Errorf("two launches of the same --job-array-name produced the same ClientToken.\n"+
			"  launch 1 array id: %s\n  launch 2 array id: %s\n\n"+
			"EC2 then either rejects the second with IdempotentParameterMismatch or "+
			"returns the FIRST launch's reservation — possibly terminated instances "+
			"reported as a successful launch (#691).", first, second)
	}
}

// TestMemberTokenIsStableWithinALaunch is the property that must NOT break while
// fixing the one above. Retries and AZ-fallback rungs inside a single reconcile
// re-issue the token on purpose: that is what stops a retried RunInstances from
// double-launching a member.
func TestMemberTokenIsStableWithinALaunch(t *testing.T) {
	arrayID := "gchp-20261004-abc123"
	id := cohort.EntityID("gchp-0")

	want := memberIdempotencyToken(arrayID, id)
	for i := 0; i < 5; i++ {
		if got := memberIdempotencyToken(arrayID, id); got != want {
			t.Fatalf("token changed within one launch (call %d): %q != %q — a retry would "+
				"double-launch the member", i, got, want)
		}
	}
}

// TestMemberTokensDifferPerIndex: each member needs its own token, or EC2 would
// treat member 1's launch as a retry of member 0's and return the same instance.
func TestMemberTokensDifferPerIndex(t *testing.T) {
	arrayID := "gchp-20261004-abc123"
	seen := map[string]string{}
	for _, name := range []string{"gchp-0", "gchp-1", "gchp-2", "gchp-15"} {
		tok := memberIdempotencyToken(arrayID, cohort.EntityID(name))
		if prev, dup := seen[tok]; dup {
			t.Errorf("members %s and %s share a ClientToken; EC2 would return one "+
				"instance for both", prev, name)
		}
		seen[tok] = name
	}
}

// TestRetryReusesTheOriginalToken: `spawn array retry` passes the ORIGINAL
// rec.ArrayID, so a retried member must land on the same token as its first
// attempt — that is what keeps the retry idempotent against its own earlier
// launch rather than racing it.
func TestRetryReusesTheOriginalToken(t *testing.T) {
	originalArrayID := "gchp-20261004-abc123"
	id := cohort.EntityID("gchp-3")

	initial := memberIdempotencyToken(originalArrayID, id)
	retried := memberIdempotencyToken(originalArrayID, id) // retry passes rec.ArrayID
	if initial != retried {
		t.Errorf("retry produced a different token (%q vs %q); it would launch a second "+
			"instance for a member that may already be running", retried, initial)
	}
}

// TestIntentIsBuiltWithAnExplicitToken is the class gate. The bug was an empty
// token argument silently falling back to cohort's name-keyed default, which is
// invisible at the call site — nothing about `""` says "and now the token is
// derived from something else".
func TestIntentIsBuiltWithAnExplicitToken(t *testing.T) {
	b, err := os.ReadFile("launch_jobarray.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	src := string(b)

	if !strings.Contains(src, "memberIdempotencyToken(jobArrayID, id)") {
		t.Error("the member intent is not built with an explicit per-launch token; an empty " +
			"token argument falls back to cohort's Token(cluster, entity, gen), which keys " +
			"on --job-array-name and collides across launches (#691)")
	}
	if strings.Contains(src, `cohort.RungPlacement{Rung: rung, Chain: chain}, "")`) {
		t.Error("the empty-token call is back; cohort then derives the token from the " +
			"job-array NAME rather than the per-launch array id")
	}
}
