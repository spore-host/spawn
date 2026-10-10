package reaperdeploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

type fakeLogs struct {
	// byPattern maps a filter pattern to the event timestamps it should return.
	byPattern map[string][]time.Time
	err       error
	calls     []string
}

func (f *fakeLogs) FilterLogEvents(_ context.Context, in *cwlogs.FilterLogEventsInput, _ ...func(*cwlogs.Options)) (*cwlogs.FilterLogEventsOutput, error) {
	pat := awssdk.ToString(in.FilterPattern)
	f.calls = append(f.calls, pat)
	if f.err != nil {
		return nil, f.err
	}
	out := &cwlogs.FilterLogEventsOutput{}
	for _, t := range f.byPattern[pat] {
		ms := t.UnixMilli()
		out.Events = append(out.Events, cwtypes.FilteredLogEvent{Timestamp: &ms})
	}
	return out, nil
}

func idleDeployer(l LogsAPI) *Deployer { return &Deployer{Logs: l} }

// TestIdlenessDistinguishesNotRunningFromFindingNothing is the property that
// makes this safe to act on.
//
// "Reclaimed nothing" has two causes with OPPOSITE remedies: the feature is
// unused (removal is right), or the schedule is broken so it never got the chance
// (removal destroys a reaper that would have worked). Conflating them is how an
// idle check becomes a liability — it is the #624 shape, with deletion attached.
func TestIdlenessDistinguishesNotRunningFromFindingNothing(t *testing.T) {
	now := time.Now()

	t.Run("ran regularly, reclaimed nothing", func(t *testing.T) {
		d := idleDeployer(&fakeLogs{byPattern: map[string][]time.Time{
			`"ttl-reaper done"`: {now.Add(-10 * time.Minute)},
			`"REAPED"`:          nil,
		}})
		got := d.DetectIdleness(context.Background(), now)
		if !got.Determined || got.EverWorked || !got.Ran {
			t.Fatalf("want determined/idle/running, got %+v", got)
		}
		advice := IdlenessAdvice(got, now)
		if !strings.Contains(advice, "teardown") {
			t.Errorf("an idle-but-running reaper should mention teardown:\n%s", advice)
		}
		if strings.Contains(advice, "broken schedule") {
			t.Errorf("a RUNNING reaper must not be reported as a broken schedule:\n%s", advice)
		}
	})

	t.Run("never ran", func(t *testing.T) {
		d := idleDeployer(&fakeLogs{byPattern: map[string][]time.Time{}})
		got := d.DetectIdleness(context.Background(), now)
		if got.Ran {
			t.Fatal("Ran should be false when no invocation was logged")
		}
		advice := IdlenessAdvice(got, now)
		if !strings.Contains(advice, "broken schedule") {
			t.Errorf("a reaper that never ran must be called a broken schedule, not idle:\n%s", advice)
		}
		if strings.Contains(advice, "teardown") {
			t.Errorf("a reaper that never ran must NOT be offered for teardown — removing it would "+
				"delete something that was never given the chance to work:\n%s", advice)
		}
		// And it must not count as idle for any duration.
		if d := got.IdleFor(now); d != 0 {
			t.Errorf("IdleFor on a never-ran reaper = %s, want 0", d)
		}
	})

	t.Run("reclaimed something", func(t *testing.T) {
		when := now.Add(-2 * time.Hour)
		d := idleDeployer(&fakeLogs{byPattern: map[string][]time.Time{
			`"ttl-reaper done"`: {now.Add(-10 * time.Minute)},
			`"REAPED"`:          {when.Add(-time.Hour), when},
		}})
		got := d.DetectIdleness(context.Background(), now)
		if !got.EverWorked {
			t.Fatal("EverWorked should be true")
		}
		// The NEWEST reap, not the first seen.
		if got.LastWorked.Unix() != when.Unix() {
			t.Errorf("LastWorked = %s, want the most recent (%s)", got.LastWorked, when)
		}
		if IdlenessAdvice(got, now) != "" {
			t.Errorf("a working reaper should produce no advice, got %q", IdlenessAdvice(got, now))
		}
		if d := got.IdleFor(now); d != 0 {
			t.Errorf("IdleFor on a working reaper = %s, want 0", d)
		}
	})
}

// TestIdlenessUndeterminedNeverImpliesIdle is the guard against the worst
// outcome: deleting a reaper because its logs could not be read.
func TestIdlenessUndeterminedNeverImpliesIdle(t *testing.T) {
	d := idleDeployer(&fakeLogs{err: errors.New("AccessDeniedException")})
	got := d.DetectIdleness(context.Background(), time.Now())
	if got.Determined {
		t.Fatal("a failed read must not be Determined")
	}
	if d := got.IdleFor(time.Now()); d != 0 {
		t.Errorf("IdleFor on an undetermined read = %s, want 0 — a non-zero value here would let "+
			"--if-idle-for delete a reaper on the strength of a permission error", d)
	}
	advice := IdlenessAdvice(got, time.Now())
	if !strings.Contains(advice, "could not tell") {
		t.Errorf("want a hedge, got %q", advice)
	}
	for _, bad := range []string{"teardown", "broken schedule"} {
		if strings.Contains(advice, bad) {
			t.Errorf("an undetermined read must not claim %q:\n%s", bad, advice)
		}
	}
}

// TestIdlenessNilLogsIsUndetermined covers the optional-client path: an existing
// caller that builds a Deployer without Logs must get "undetermined", not "idle".
func TestIdlenessNilLogsIsUndetermined(t *testing.T) {
	got := (&Deployer{}).DetectIdleness(context.Background(), time.Now())
	if got.Determined || got.IdleFor(time.Now()) != 0 {
		t.Errorf("a Deployer with no Logs client must report undetermined, got %+v", got)
	}
}

// TestIdlenessMissingLogGroupIsNotIdle: a function that has never been invoked
// has no log group. That is "never ran", not "idle" — and must not be removable.
func TestIdlenessMissingLogGroupIsNotIdle(t *testing.T) {
	d := idleDeployer(&fakeLogs{err: errors.New("ResourceNotFoundException: The specified log group does not exist")})
	got := d.DetectIdleness(context.Background(), time.Now())
	if !got.Determined {
		t.Fatal("a missing log group is a definite answer, not an unreadable one")
	}
	if got.Ran || got.EverWorked {
		t.Errorf("want neither Ran nor EverWorked, got %+v", got)
	}
	if got.IdleFor(time.Now()) != 0 {
		t.Error("a never-invoked reaper must not be idle for any duration")
	}
	if !strings.Contains(IdlenessAdvice(got, time.Now()), "broken schedule") {
		t.Error("want the broken-schedule wording")
	}
}

// TestIdleForRequiresDeterminedIndependently tests the method's own invariant
// rather than going through DetectIdleness.
//
// Written because a deliberate revert of the `!i.Determined` guard in IdleFor
// PASSED the tests above. The reason is that DetectIdleness returns early on
// error, so an undetermined result always has Ran=false and the guard is
// unreachable through the constructor — the tests could not distinguish a real
// guard from a redundant one.
//
// It is not redundant. Idleness is an exported struct and IdleFor is an exported
// method, so a caller can hand it {Determined:false, Ran:true} — and the answer
// must be 0, because the alternative is --if-idle-for deleting a reaper on the
// strength of a read that failed. Testing the invariant where it lives makes the
// guard load-bearing instead of decorative.
func TestIdleForRequiresDeterminedIndependently(t *testing.T) {
	now := time.Now()

	if got := (Idleness{Determined: false, Ran: true}).IdleFor(now); got != 0 {
		t.Errorf("IdleFor on an undetermined-but-ran Idleness = %s, want 0. A non-zero answer here "+
			"lets --if-idle-for act on a reaper whose idleness was never established.", got)
	}
	// And the determined equivalent must be non-zero, or the test above would
	// pass against a method that always returns 0.
	if got := (Idleness{Determined: true, Ran: true}).IdleFor(now); got == 0 {
		t.Error("IdleFor on a determined, running, never-worked Idleness = 0; the previous " +
			"assertion would then prove nothing")
	}
}
