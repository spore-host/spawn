package reaperdeploy

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/iam"
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
// carries 30 days of history (set by deploy since #1156), which bounds the answer but does not spoil
// it: "idle for at least 30 days" is a sufficient basis for the only decision
// this feeds.
type LogsAPI interface {
	FilterLogEvents(context.Context, *cwlogs.FilterLogEventsInput, ...func(*cwlogs.Options)) (*cwlogs.FilterLogEventsOutput, error)
	// Deploy creates the group and sets its retention; teardown deletes it
	// (#1156). These run under the caller's credentials, not the function's.
	CreateLogGroup(context.Context, *cwlogs.CreateLogGroupInput, ...func(*cwlogs.Options)) (*cwlogs.CreateLogGroupOutput, error)
	PutRetentionPolicy(context.Context, *cwlogs.PutRetentionPolicyInput, ...func(*cwlogs.Options)) (*cwlogs.PutRetentionPolicyOutput, error)
	DeleteLogGroup(context.Context, *cwlogs.DeleteLogGroupInput, ...func(*cwlogs.Options)) (*cwlogs.DeleteLogGroupOutput, error)
}

// LogGroupName is where the in-account reaper writes.
func LogGroupName() string { return "/aws/lambda/" + FunctionName }

// idleWindow is how far back the check looks. Matches DefaultLogRetentionDays,
// which `spawn reaper deploy` now sets (#1156): looking further would silently
// return "never worked" for a reaper whose evidence has simply expired, which
// is a different claim. A shorter retention only makes the answer smaller,
// because idleness is measured from the first run still in the logs.
const idleWindow = DefaultLogRetentionDays * 24 * time.Hour

// ActionSentinels are the words the reaper logs, one line per resource, when it
// actually reclaims something: an instance or filesystem REAPED, a Spot
// request CANCELLED, a Spot replacement TERMINATED, a subdomain or DNS lease
// EXPIRING. Matching only REAPED counted every Spot and DNS action as idle
// (#1155). TestActionSentinelsCoverTheReapersLogLines discovers the sentinels
// from the reaper's source and fails on one that is not classified here.
var ActionSentinels = []string{"REAPED", "CANCELLED", "TERMINATED", "EXPIRING"}

// actionPattern is the CloudWatch filter for any action sentinel (OR terms).
func actionPattern() string {
	terms := make([]string, len(ActionSentinels))
	for i, s := range ActionSentinels {
		terms[i] = `?"` + s + `"`
	}
	return strings.Join(terms, " ")
}

// actionLine reports whether a log message records an action, rather than
// merely containing a sentinel word. The filter above is a substring match, so
// it also returns the "SPOT REPLACEMENT NOT TERMINATED" alarm and any "WOULD …"
// dry-run line that happens to quote one; neither is work done.
var actionLine = regexp.MustCompile(`(?:^|[^A-Za-z_])((?:NOT|WOULD) )?(` +
	strings.Join(ActionSentinels, "|") + `)(?:[^A-Za-z_]|$)`)

func isActionLine(msg string) bool {
	for _, m := range actionLine.FindAllStringSubmatch(msg, -1) {
		if m[1] == "" {
			return true
		}
	}
	return false
}

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
	// FirstRan is the earliest invocation still in the window.
	FirstRan time.Time
	// Created is when this deployment began: the execution role's creation
	// date, since Lambda reports no creation time for a function (LastModified
	// moves on every update). Runs logged before it belong to an earlier
	// deployment, whose log group a teardown used to leave behind.
	Created time.Time
	// Why explains an undetermined result.
	Why string
}

// IdleFor reports how long the reaper has gone without reclaiming anything.
// Zero when it has worked, or when nothing could be determined.
//
// Measured from the first run in the window or the deployment's creation,
// whichever is later. Not from LastRan: the gap since the last RUN is nearly
// zero for a healthy schedule and would read as "not idle". And not the whole
// window, which is what this used to return for any reaper that had run at all
// — so a reaper deployed an hour ago read as idle for 30 days, `--if-idle-for
// 720h` removed it, and any value above 30 days could never trigger (#1155).
func (i Idleness) IdleFor(now time.Time) time.Duration {
	if !i.Determined || i.EverWorked || !i.Ran {
		return 0
	}
	since := i.FirstRan
	if since.IsZero() {
		return 0 // ran, but when is unknown: claim nothing
	}
	if i.Created.After(since) {
		since = i.Created
	}
	if d := now.Sub(since); d > 0 {
		return d
	}
	return 0
}

// DetectIdleness reads the reaper's own log group and reports whether it has had
// anything to do.
//
// "Did work" is keyed on the per-action sentinel lines (ActionSentinels), not on
// the summary counters. The summary is a single line per run with counters
// inside it, so matching it would require parsing; the sentinel lines are one
// per action and are what the existing metric filters key on too
// (template.yaml's FilterPattern entries).
func (d *Deployer) DetectIdleness(ctx context.Context, now time.Time) Idleness {
	if d.Logs == nil {
		return Idleness{Why: "no logs client configured"}
	}
	if d.IAM == nil {
		return Idleness{Why: "no IAM client configured, so the deployment's age is unknown"}
	}
	start := now.Add(-idleWindow).UnixMilli()

	// Did it reclaim anything?
	_, worked, err := d.matchRange(ctx, start, actionPattern(), isActionLine)
	if err != nil {
		return Idleness{Why: err.Error()}
	}
	// Did it run at all, and since when? Keyed on the summary line every
	// invocation emits.
	firstRan, lastRan, err := d.matchRange(ctx, start, `"ttl-reaper done"`, nil)
	if err != nil {
		return Idleness{Why: err.Error()}
	}

	out := Idleness{Determined: true}
	if !worked.IsZero() {
		out.EverWorked, out.LastWorked = true, worked
	}
	if !lastRan.IsZero() {
		out.Ran, out.LastRan, out.FirstRan = true, lastRan, firstRan
		// Only needed to bound a run-but-idle answer, so only read then.
		role, err := d.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: awssdk.String(RoleName)})
		if err != nil || role.Role == nil || role.Role.CreateDate == nil {
			// Without it, runs of an earlier deployment could pass for this
			// one's. Claim nothing rather than overstate.
			why := "role has no creation date"
			if err != nil {
				why = err.Error()
			}
			return Idleness{Why: "read role " + RoleName + ": " + why}
		}
		out.Created = *role.Role.CreateDate
	}
	return out
}

// matchRange returns the earliest and latest timestamps of events matching
// pattern (and keep, when non-nil), or zero times when there are none.
func (d *Deployer) matchRange(ctx context.Context, startMillis int64, pattern string, keep func(string) bool) (earliest, latest time.Time, err error) {
	var token *string
	// Bounded: this is a yes/no with timestamps, not an audit. A reaper in a busy
	// account can emit a great many lines. If the bound truncates, earliest can
	// only come out LATER than the truth, which makes the reaper look less idle
	// — the safe direction for an answer that feeds a deletion.
	for page := 0; page < 20; page++ {
		out, err := d.Logs.FilterLogEvents(ctx, &cwlogs.FilterLogEventsInput{
			LogGroupName:  awssdk.String(LogGroupName()),
			StartTime:     awssdk.Int64(startMillis),
			FilterPattern: awssdk.String(pattern),
			NextToken:     token,
		})
		if err != nil {
			// A missing log group means the function has never run, which is a
			// legitimate answer rather than a failure — but it is NOT "idle", and
			// the caller must not treat it as grounds for removal.
			if strings.Contains(err.Error(), "ResourceNotFoundException") {
				return time.Time{}, time.Time{}, nil
			}
			return time.Time{}, time.Time{}, fmt.Errorf("read %s: %w", LogGroupName(), err)
		}
		for _, e := range out.Events {
			if e.Timestamp == nil {
				continue
			}
			if keep != nil && !keep(awssdk.ToString(e.Message)) {
				continue
			}
			t := time.UnixMilli(*e.Timestamp)
			if t.After(latest) {
				latest = t
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	return earliest, latest, nil
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
		return fmt.Sprintf("the reaper has not run at all in the last %d days, which is a broken schedule "+
			"rather than an idle one — check `spawn reaper status` for the rule state before "+
			"concluding it is unused", int(idleWindow.Hours()/24))

	default:
		// The measured duration, not the window: "at least 30 days" was printed
		// for a reaper deployed an hour ago (#1155).
		return fmt.Sprintf("the reaper has run for %s and reclaimed nothing in that time. "+
			"If this account is finished with spawn, `spawn reaper teardown` removes it "+
			"and its role, schedule, artifact bucket and logs — leaving no trace. If it is not, "+
			"this is simply what a protected account with no expired instances looks like.",
			humanDuration(i.IdleFor(now)))
	}
}

// humanDuration renders an idle duration in days, or hours under two days.
func humanDuration(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	h := int(d.Round(time.Hour).Hours())
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}
