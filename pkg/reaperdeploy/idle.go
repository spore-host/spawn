package reaperdeploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

// LogsAPI is the slice of CloudWatch Logs the idle check needs.
//
// Reading LOGS rather than asking the function is the whole point. A reaper that
// tracked its own idleness would need somewhere durable to put the answer, and a
// reaper that removed itself when idle would need lambda:DeleteFunction,
// iam:DeleteRole, events:DeleteRule and s3:DeleteBucket on top of the 11
// EC2/FSx/SSM actions it has today.
//
// That escalation is the thing to avoid. #613's audit of the in-account reaper is
// quotable precisely because the policy is minimal — "the inline policy contains
// no sts:AssumeRole at all" — and it was deployed into an account whose
// organization forbids external trust. Granting that function iam:DeleteRole
// would make the next audit of it materially worse, to save a human running one
// command.
//
// So idleness is DERIVED from what the function already writes, under the
// AWSLambdaBasicExecutionRole it already has. Zero new permissions. The log group
// carries 30 days of history (#770), which bounds the answer but does not spoil
// it: "idle for at least 30 days" is a sufficient basis for the only decision
// this feeds.
type LogsAPI interface {
	FilterLogEvents(context.Context, *cwlogs.FilterLogEventsInput, ...func(*cwlogs.Options)) (*cwlogs.FilterLogEventsOutput, error)
}

// LogGroupName is where the in-account reaper writes.
func LogGroupName() string { return "/aws/lambda/" + FunctionName }

// idleWindow is how far back the check looks. Matches the 30-day log retention
// applied in #770: looking further would silently return "never worked" for a
// reaper whose evidence has simply expired, which is a different claim.
const idleWindow = 30 * 24 * time.Hour

// Idleness is what could be determined about whether the reaper has had anything
// to do.
//
// Determined is separate from the answer for the same reason it is on
// ReaperCoverage: "it has done nothing" and "I could not tell" are different, and
// conflating them is spawn#624, where a check that fired in every account carried
// no information. Here the stakes are higher — this one feeds a decision to
// DELETE things.
type Idleness struct {
	// Determined is false when the log group could not be read, in which case
	// nothing may be claimed and certainly nothing removed.
	Determined bool
	// EverWorked is true when a run within the window reclaimed something.
	EverWorked bool
	// LastWorked is when that was. Zero when EverWorked is false.
	LastWorked time.Time
	// Ran is true when the function ran at all in the window. A reaper that is
	// idle because it is NOT RUNNING is a different problem from one that runs
	// and finds nothing — the first is a broken schedule, the second is an unused
	// feature, and they have opposite remedies.
	Ran bool
	// LastRan is the most recent invocation seen.
	LastRan time.Time
	// Why explains an undetermined result.
	Why string
}

// IdleFor reports how long the reaper has gone without reclaiming anything.
// Zero when it has worked, or when nothing could be determined.
func (i Idleness) IdleFor(now time.Time) time.Duration {
	if !i.Determined || i.EverWorked {
		return 0
	}
	if i.Ran {
		// It has been running and finding nothing for at least as long as we can
		// see. Measure from the window, not from LastRan: the gap since the last
		// RUN is nearly zero for a healthy schedule and would read as "not idle".
		return now.Sub(now.Add(-idleWindow))
	}
	return 0
}

// DetectIdleness reads the reaper's own log group and reports whether it has had
// anything to do.
//
// "Did work" is keyed on the REAPED sentinel the reaper already emits per
// reclaimed resource, not on the summary counters. The summary is a single line
// per run with counters inside it, so matching it would require parsing; the
// REAPED lines are one per action and are what the existing metric filters key
// on too (template.yaml's FilterPattern entries).
func (d *Deployer) DetectIdleness(ctx context.Context, now time.Time) Idleness {
	if d.Logs == nil {
		return Idleness{Why: "no logs client configured"}
	}
	start := now.Add(-idleWindow).UnixMilli()

	// Did it reclaim anything?
	worked, err := d.latestMatch(ctx, start, "REAPED")
	if err != nil {
		return Idleness{Why: err.Error()}
	}
	// Did it run at all? Keyed on the summary line every invocation emits.
	ran, err := d.latestMatch(ctx, start, "ttl-reaper done")
	if err != nil {
		return Idleness{Why: err.Error()}
	}

	out := Idleness{Determined: true}
	if !worked.IsZero() {
		out.EverWorked, out.LastWorked = true, worked
	}
	if !ran.IsZero() {
		out.Ran, out.LastRan = true, ran
	}
	return out
}

// latestMatch returns the timestamp of the most recent event matching pattern,
// or the zero time when there is none.
func (d *Deployer) latestMatch(ctx context.Context, startMillis int64, pattern string) (time.Time, error) {
	var latest time.Time
	var token *string
	// Bounded: this is a yes/no with a timestamp, not an audit. A reaper in a busy
	// account can emit a great many REAPED lines, and paging all of them to learn
	// "the newest is recent" would be wasteful.
	for page := 0; page < 20; page++ {
		out, err := d.Logs.FilterLogEvents(ctx, &cwlogs.FilterLogEventsInput{
			LogGroupName:  awssdk.String(LogGroupName()),
			StartTime:     awssdk.Int64(startMillis),
			FilterPattern: awssdk.String(`"` + pattern + `"`),
			NextToken:     token,
		})
		if err != nil {
			// A missing log group means the function has never run, which is a
			// legitimate answer rather than a failure — but it is NOT "idle", and
			// the caller must not treat it as grounds for removal.
			if strings.Contains(err.Error(), "ResourceNotFoundException") {
				return time.Time{}, nil
			}
			return time.Time{}, fmt.Errorf("read %s: %w", LogGroupName(), err)
		}
		for _, e := range out.Events {
			if e.Timestamp == nil {
				continue
			}
			if t := time.UnixMilli(*e.Timestamp); t.After(latest) {
				latest = t
			}
		}
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	return latest, nil
}

// IdlenessAdvice is the line to show someone whose reaper has had nothing to do.
// Empty when it has worked, or when nothing could be determined.
//
// It describes what is true and what removing would mean, and stops there. The
// decision is the operator's: an idle reaper in a user's account IS a trace under
// "leave no trace", and an idle reaper is also exactly what a correctly-protected
// account looks like when nobody has launched anything. Those are
// indistinguishable from here, which is why this reports rather than acts.
func IdlenessAdvice(i Idleness, now time.Time) string {
	switch {
	case !i.Determined:
		return fmt.Sprintf("could not tell whether the reaper has had anything to do (%s)", i.Why)

	case i.EverWorked:
		return ""

	case !i.Ran:
		// The dangerous case to get wrong. Nothing reclaimed AND nothing ran is a
		// broken schedule, not an unused feature — and removing it would delete a
		// reaper that was never given the chance to work.
		return "the reaper has not run at all in the last 30 days, which is a broken schedule " +
			"rather than an idle one — check `spawn reaper status` for the rule state before " +
			"concluding it is unused"

	default:
		return fmt.Sprintf("the reaper has run regularly for at least %d days and reclaimed "+
			"nothing. If this account is finished with spawn, `spawn reaper teardown` removes it "+
			"and its role, schedule and artifact bucket — leaving no trace. If it is not, this is "+
			"simply what a protected account with no expired instances looks like.",
			int(idleWindow.Hours()/24))
	}
}
