package aws

import (
	"context"
	"errors"
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
