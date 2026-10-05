package cmd

import (
	"os"
	"strconv"
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

// TestJobArrayIDSuffixIsNotTheClock is spawn#710.
//
// The suffix used to be `time.Now().UnixNano() % 0xFFFFFF` behind a comment
// claiming randomness. Two failure modes, and this test exists because the
// previous gate (TestMemberTokenDiffersBetweenLaunches, two calls) could not
// distinguish them from a fine-grained clock:
//
//  1. On a coarse clock it simply repeats. time.Now() has MICROSECOND resolution
//     on darwin, so two consecutive calls return the identical value and two ids
//     minted in the same microsecond were equal. That gate failed 5/5 locally
//     while passing in CI on Linux — a uniqueness assertion satisfied by clock
//     granularity is not an assertion.
//  2. The clock-derived suffix wraps every 0xFFFFFF ns (~16.8 ms), so two
//     launches that far apart collide deterministically however fine the clock.
//
// A tight loop is the discriminator: it issues far more calls than a
// microsecond-resolution clock has distinct values to offer, so a clock-derived
// suffix cannot survive it on any platform.
func TestJobArrayIDSuffixIsNotTheClock(t *testing.T) {
	const n = 2000
	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		id := generateJobArrayID("gchp")
		if first, dup := seen[id]; dup {
			t.Fatalf("generateJobArrayID produced %q at both call %d and call %d.\n\n"+
				"A tight loop outruns any wall clock, so a duplicate here means the suffix is "+
				"derived from time rather than entropy. #691 made the RunInstances ClientToken "+
				"depend on this id, so a collision means EC2 either rejects the cohort with "+
				"IdempotentParameterMismatch or returns the FIRST launch's reservation — "+
				"possibly terminated instances reported as a successful launch (#710).",
				id, first, i)
		}
		seen[id] = i
	}
}

// TestJobArrayIDSuffixUsesTheFullKeyspace: 2000 draws from a 32-bit space should
// show no clustering. The old suffix was 24 bits of *wall clock*, so consecutive
// ids differed by a small, monotonically increasing amount — this catches a
// regression to any counter-like source, which a pure uniqueness test would not.
func TestJobArrayIDSuffixUsesTheFullKeyspace(t *testing.T) {
	const n = 2000
	var monotonic int
	prev := -1
	for i := 0; i < n; i++ {
		parts := strings.Split(generateJobArrayID("gchp"), "-")
		suffix := parts[len(parts)-1]
		v, err := strconv.ParseInt(suffix, 16, 64)
		if err != nil {
			t.Fatalf("suffix %q is not hex: %v", suffix, err)
		}
		if prev >= 0 && v > int64(prev) {
			monotonic++
		}
		prev = int(v)
	}
	// Random draws rise about half the time. A clock rises essentially always.
	if monotonic > n*3/4 {
		t.Errorf("%d of %d consecutive suffixes increased — the source looks like a counter or "+
			"a clock, not entropy (#710)", monotonic, n-1)
	}
}
