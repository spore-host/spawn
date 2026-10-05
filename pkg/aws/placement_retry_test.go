package aws

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// inUseErr is the error EC2 returns while a placement group still has members.
// Carried as a plain string because that is the form a wrapped SDK error
// presents, which is the case the classifier most needs to handle.
var inUseErr = errors.New("operation error EC2: DeletePlacementGroup, https response error " +
	"StatusCode: 400, api error InvalidPlacementGroup.InUse: The placement group " +
	"'spawn-mpi-x-us-east-1a' is in use and may not be deleted.")

// TestPlacementGroupInUseIsRecognised: if the classifier misses, the retry never
// engages and the whole fix is inert.
func TestPlacementGroupInUseIsRecognised(t *testing.T) {
	if !placementGroupInUse(inUseErr) {
		t.Error("the real InvalidPlacementGroup.InUse text is not recognised, so the retry " +
			"would never engage")
	}
	if placementGroupInUse(nil) {
		t.Error("nil must not read as in-use")
	}
	// A permissions error must NOT be retried: waiting out the budget to re-learn
	// that the caller cannot delete placement groups is worse than saying so now.
	if placementGroupInUse(errors.New("api error UnauthorizedOperation: You are not authorized")) {
		t.Error("UnauthorizedOperation must not be retried as in-use")
	}
}

// TestDeletePlacementGroupRetriesUntilMembersAreGone is the spawn#685 gate.
//
// EC2 refuses to delete a group while any instance references it, and
// TerminateInstances is asynchronous — so the delete issued right after a drain
// lost the race essentially every time. Every failed MPI launch reported
// InvalidPlacementGroup.InUse and left the group behind; nine were found in one
// region of one account, and ~35 security groups and placement groups were
// cleaned by hand in a single session.
//
// This drives the real retryPlacementGroupDelete loop, so collapsing it back to
// a single call fails here.
func TestDeletePlacementGroupRetriesUntilMembersAreGone(t *testing.T) {
	t.Cleanup(SetPGDeleteWaitForTest(2*time.Second, time.Millisecond))

	calls := 0
	// Model the real sequence: in-use while the instances shut down, then gone.
	del := func() error {
		calls++
		if calls < 4 {
			return inUseErr
		}
		return nil
	}

	if err := retryPlacementGroupDelete(context.Background(), "spawn-mpi-x", del); err != nil {
		t.Errorf("delete never succeeded after %d attempt(s): %v", calls, err)
	}
	if calls != 4 {
		t.Errorf("made %d attempt(s), want 4 — the retry did not wait out the drain", calls)
	}
}

// TestDeletePlacementGroupDoesNotRetryOtherErrors: only InUse is a race worth
// waiting on. Anything else is a standing condition, and burning the budget on
// it just delays the error the caller needs to read.
func TestDeletePlacementGroupDoesNotRetryOtherErrors(t *testing.T) {
	t.Cleanup(SetPGDeleteWaitForTest(10*time.Second, time.Second))

	calls := 0
	denied := errors.New("api error UnauthorizedOperation: You are not authorized")
	del := func() error { calls++; return denied }

	start := time.Now()
	err := retryPlacementGroupDelete(context.Background(), "spawn-mpi-x", del)
	if !errors.Is(err, denied) {
		t.Errorf("error = %v, want the original UnauthorizedOperation returned unwrapped", err)
	}
	if calls != 1 {
		t.Errorf("made %d attempt(s), want 1", calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s on a non-retryable error; it should return at once", elapsed)
	}
}

// TestDeletePlacementGroupGivesUpWithinBudget: an empty placement group is FREE,
// so the only cost of giving up is quota pressure. Blocking a CLI exit for
// minutes over a free resource would be a worse trade — provided the error says
// what was left behind.
func TestDeletePlacementGroupGivesUpWithinBudget(t *testing.T) {
	t.Cleanup(SetPGDeleteWaitForTest(50*time.Millisecond, 5*time.Millisecond))

	del := func() error { return inUseErr } // never drains

	start := time.Now()
	err := retryPlacementGroupDelete(context.Background(), "spawn-mpi-stuck", del)
	if err == nil {
		t.Fatal("a group that never drains must eventually return an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s to give up on a 50ms budget; the CLI would appear hung", elapsed)
	}
	// The message has to name the group — the operator's next step is a manual
	// delete, and they need the name to run it.
	if !strings.Contains(err.Error(), "spawn-mpi-stuck") {
		t.Errorf("error %q does not name the placement group left behind", err)
	}
	if !errors.Is(err, inUseErr) {
		t.Error("the underlying InUse error should stay wrapped, so callers can classify it")
	}
}

// TestDeletePlacementGroupHonoursContext: a Ctrl-C during the wait must return
// promptly rather than sit out the remaining budget.
func TestDeletePlacementGroupHonoursContext(t *testing.T) {
	t.Cleanup(SetPGDeleteWaitForTest(time.Minute, 10*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	del := func() error { cancel(); return inUseErr }

	start := time.Now()
	if err := retryPlacementGroupDelete(ctx, "spawn-mpi-x", del); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s to notice cancellation (interval is 10s)", elapsed)
	}
}

// TestDeletePlacementGroupTakesARegion is the second half of spawn#685's leak.
//
// CreatePlacementGroup pinned the client to the launch region; DeletePlacementGroup
// used ec2.NewFromConfig(c.cfg) — the DEFAULT region — and had no region parameter
// at all. So a cohort launched with --region in a non-default region created its
// group in one region and tried to delete it in another: the delete failed
// InvalidPlacementGroup.Unknown, which is not retryable, so the group was abandoned
// on the first attempt no matter how long the retry waited. (And a same-named group
// genuinely present in the default region would have been deleted instead.)
//
// The signature is the gate: a region-less Delete cannot be correct next to a
// region-taking Create, and the compiler now enforces that every caller supplies
// one.
func TestDeletePlacementGroupTakesARegion(t *testing.T) {
	var _ func(context.Context, string, string) error = (&Client{}).DeletePlacementGroup
	var _ func(context.Context, string, string) error = (&Client{}).DeletePlacementGroupWithRetry
}

// TestNoRegionalEC2CallUsesTheDefaultConfig generalises it: the default-region
// client is the shape of the bug, not the symptom. c.cfg carries whatever region
// the ambient AWS config had, which has nothing to do with where the caller asked
// to launch — so a regional EC2 call built on it silently targets the wrong
// region. Everything regional must go through regionalEC2.
func TestNoRegionalEC2CallUsesTheDefaultConfig(t *testing.T) {
	// GetEnabledRegions is genuinely account-global: DescribeRegions returns the
	// same answer from any region, and there is no caller region to honour.
	allowed := map[string]bool{"GetEnabledRegions": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read pkg/aws: %v", err)
	}
	funcRE := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)\(`)
	found := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		src := string(b)
		for _, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, "ec2.NewFromConfig(c.cfg)") {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // the comment in placement.go describing this very bug
			}
			found++
			// Attribute it to the enclosing function so the message is actionable.
			idx := strings.Index(src, line)
			var fn string
			for _, m := range funcRE.FindAllStringSubmatchIndex(src[:idx], -1) {
				fn = src[m[2]:m[3]]
			}
			if allowed[fn] {
				continue
			}
			t.Errorf("%s: %s builds an EC2 client from the client's DEFAULT region. "+
				"c.cfg's region is whatever the ambient AWS config had, not where the "+
				"caller asked to operate — so this silently targets the wrong region "+
				"(spawn#685). Use c.regionalEC2(region).", name, fn)
		}
	}
	if found == 0 {
		t.Error("found no ec2.NewFromConfig(c.cfg) at all, not even the allowed one — " +
			"the matcher is stale and this gate would pass vacuously")
	}
}
