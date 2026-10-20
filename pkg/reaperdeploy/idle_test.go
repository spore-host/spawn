package reaperdeploy

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// fakeLogs is a small CloudWatch Logs: it holds events and evaluates the filter
// patterns DetectIdleness sends — a quoted term, or ?"A" ?"B" OR terms — as a
// substring match, which is how CloudWatch treats them. Matching the real
// semantics rather than keying on the exact pattern string is what lets a test
// see a sentinel word inside a line that is not an action.
type fakeLogs struct {
	events []logEvent
	err    error
	calls  []string

	groupExists  bool
	createErr    error
	retention    int32
	deleted      bool
	deleteErr    error
	createdGroup bool
}

type logEvent struct {
	at  time.Time
	msg string
}

var quotedTerm = regexp.MustCompile(`"([^"]+)"`)

func (f *fakeLogs) FilterLogEvents(_ context.Context, in *cwlogs.FilterLogEventsInput, _ ...func(*cwlogs.Options)) (*cwlogs.FilterLogEventsOutput, error) {
	pat := awssdk.ToString(in.FilterPattern)
	f.calls = append(f.calls, "FilterLogEvents "+pat)
	if f.err != nil {
		return nil, f.err
	}
	var terms []string
	for _, m := range quotedTerm.FindAllStringSubmatch(pat, -1) {
		terms = append(terms, m[1])
	}
	out := &cwlogs.FilterLogEventsOutput{}
	for _, e := range f.events {
		if in.StartTime != nil && e.at.UnixMilli() < *in.StartTime {
			continue
		}
		for _, term := range terms {
			if strings.Contains(e.msg, term) {
				ms, msg := e.at.UnixMilli(), e.msg
				out.Events = append(out.Events, cwtypes.FilteredLogEvent{Timestamp: &ms, Message: &msg})
				break
			}
		}
	}
	return out, nil
}

func (f *fakeLogs) CreateLogGroup(context.Context, *cwlogs.CreateLogGroupInput, ...func(*cwlogs.Options)) (*cwlogs.CreateLogGroupOutput, error) {
	f.calls = append(f.calls, "CreateLogGroup")
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.groupExists {
		return nil, &cwtypes.ResourceAlreadyExistsException{Message: awssdk.String("The specified log group already exists")}
	}
	f.groupExists, f.createdGroup = true, true
	return &cwlogs.CreateLogGroupOutput{}, nil
}

func (f *fakeLogs) PutRetentionPolicy(_ context.Context, in *cwlogs.PutRetentionPolicyInput, _ ...func(*cwlogs.Options)) (*cwlogs.PutRetentionPolicyOutput, error) {
	f.calls = append(f.calls, "PutRetentionPolicy")
	// The real API creates nothing: retention on an absent group fails (#827).
	if !f.groupExists {
		return nil, &cwtypes.ResourceNotFoundException{Message: awssdk.String("The specified log group does not exist")}
	}
	f.retention = awssdk.ToInt32(in.RetentionInDays)
	return &cwlogs.PutRetentionPolicyOutput{}, nil
}

func (f *fakeLogs) DescribeLogGroups(context.Context, *cwlogs.DescribeLogGroupsInput, ...func(*cwlogs.Options)) (*cwlogs.DescribeLogGroupsOutput, error) {
	f.calls = append(f.calls, "DescribeLogGroups")
	out := &cwlogs.DescribeLogGroupsOutput{}
	if f.groupExists {
		g := cwtypes.LogGroup{LogGroupName: awssdk.String(LogGroupName())}
		if f.retention > 0 {
			g.RetentionInDays = awssdk.Int32(f.retention)
		}
		out.LogGroups = []cwtypes.LogGroup{g}
	}
	return out, nil
}

func (f *fakeLogs) DeleteLogGroup(context.Context, *cwlogs.DeleteLogGroupInput, ...func(*cwlogs.Options)) (*cwlogs.DeleteLogGroupOutput, error) {
	f.calls = append(f.calls, "DeleteLogGroup")
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if !f.groupExists {
		return nil, &cwtypes.ResourceNotFoundException{Message: awssdk.String("The specified log group does not exist")}
	}
	f.groupExists, f.deleted = false, true
	return &cwlogs.DeleteLogGroupOutput{}, nil
}

// runs returns one "ttl-reaper done" summary line every interval from first to
// last, inclusive — what a healthy schedule writes.
func runs(first, last time.Time, every time.Duration) []logEvent {
	var out []logEvent
	for t := first; !t.After(last); t = t.Add(every) {
		out = append(out, logEvent{t, "2026/10/19 12:00:00 ttl-reaper done in 1.2s: {Scanned:3 Reaped:0}"})
	}
	return out
}

// idleDeployer has a role created long ago, so the creation bound does not
// interfere unless a test sets one.
func idleDeployer(l LogsAPI) *Deployer {
	return &Deployer{Logs: l, IAM: &fakeIAM{roleExists: true, created: time.Now().Add(-365 * 24 * time.Hour)}}
}

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
		d := idleDeployer(&fakeLogs{events: runs(now.Add(-40*time.Minute), now.Add(-10*time.Minute), 10*time.Minute)})
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
		d := idleDeployer(&fakeLogs{})
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
		d := idleDeployer(&fakeLogs{events: append(runs(now.Add(-4*time.Hour), now.Add(-10*time.Minute), 10*time.Minute),
			logEvent{when.Add(-time.Hour), "2026/10/19 10:00:00 REAPED i-1 (spore) in self/us-east-1 — expired"},
			logEvent{when, "2026/10/19 11:00:00 REAPED i-2 (spore) in self/us-east-1 — expired"},
		)})
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
	if got := (Idleness{Determined: true, Ran: true, FirstRan: now.Add(-time.Hour)}).IdleFor(now); got == 0 {
		t.Error("IdleFor on a determined, running, never-worked Idleness = 0; the previous " +
			"assertion would then prove nothing")
	}
}
