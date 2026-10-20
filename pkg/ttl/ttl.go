// Package ttl is the single place that computes an EC2 instance's TTL
// deadline from a duration, so every writer of spawn:ttl/spawn:ttl-deadline
// (launch, `spawn extend`, `spored config set ttl`) derives the deadline the
// same way pkg/agent.NewAgent does when it synthesizes one for a
// pre-deadline instance: launch_time + ttl.
//
// Before this package existed, `spored config set ttl` (cmd/spored/main.go)
// wrote spawn:ttl alone and never touched spawn:ttl-deadline — the field
// pkg/agent's enforcement loop actually reads (agent.go's remaining-time
// calculation prefers TTLDeadline whenever it is non-zero, which is true for
// every instance a current spawn has ever launched). The command reported
// full success — tag written, reload triggered, "TTL changed" logged,
// checkmark printed — while the deadline that actually terminates the
// instance never moved (spawn#553). This was especially dangerous for
// SHORTENING a TTL: `spawn extend` can only ever move the deadline later
// (it's monotonic by construction), so `config set ttl` was the only
// candidate path for tightening an over-provisioned TTL, and it silently did
// nothing.
package ttl

import (
	"errors"
	"fmt"
	"time"
)

// ErrLaunchTimeUnknown is returned by DeadlineForNewTTL when launchTime is
// the zero value. Every spawn:ttl-deadline in this codebase is anchored to
// the instance's launch time, never to wall-clock "now" — an unknown launch
// time means there is no coherent anchor to compute a deadline from. Callers
// must refuse the operation outright (spawn#553's fix #1) rather than fall
// back to `now`, which would silently redefine what spawn:ttl means for this
// one instance versus every other reader of the tag.
var ErrLaunchTimeUnknown = errors.New("launch time is unknown; refusing to compute a TTL deadline without it")

// DeadlineForNewTTL resolves the absolute deadline that corresponds to
// setting an instance's TOTAL TTL (duration since launch) to newTTL. This is
// the anchor `spored config set ttl <duration>` must use: it treats newTTL as
// the new total-from-launch, exactly like a fresh `spawn launch --ttl` and
// exactly like agent.NewAgent's own pre-deadline-tag fallback
// (launchTime.Add(TTL)). The result can land in the past when newTTL is
// shorter than the time already elapsed — that is not an error, it is the
// whole point of supporting a shortened TTL (spawn#553): the instance is due
// for termination on the very next monitor tick, which is what an operator
// asking to tighten an over-provisioned TTL actually wants.
func DeadlineForNewTTL(launchTime time.Time, newTTL time.Duration) (time.Time, error) {
	if launchTime.IsZero() {
		return time.Time{}, ErrLaunchTimeUnknown
	}
	return launchTime.Add(newTTL), nil
}

// Tags renders the {spawn:ttl, spawn:ttl-deadline} tag pair for a resolved
// absolute deadline, anchored to launchTime. spawn:ttl is always computed as
// deadline-launchTime — the total duration from launch — never a raw
// caller-supplied argument, so it stays consistent with what every other
// reader of the tag expects: agent.NewAgent's pre-deadline-tag synthesis, and
// `spawn stop`/`hibernate`'s remaining-TTL preservation. (Writing the raw
// extend/set argument instead was spawn#506's bug for `spawn extend`; the
// same mistake is avoided here for the shared write path both `extend` and
// `config set ttl` now go through.)
//
// Callers write both tags in a single CreateTags call — EC2's CreateTags
// applies every tag in one API request, which is as close to atomic as the
// tag-write mechanism allows — so spawn:ttl and spawn:ttl-deadline can never
// be observed half-updated relative to each other.
func Tags(launchTime, deadline time.Time) map[string]string {
	total := deadline.Sub(launchTime).Round(time.Second)
	return map[string]string{
		"spawn:ttl":          total.String(),
		"spawn:ttl-deadline": deadline.UTC().Format(time.RFC3339),
	}
}

// SetTags computes the tags for setting an instance's TTL to an absolute new
// total duration from launch (`spored config set ttl <duration>` /
// `spawn instance-config <id> set ttl <duration>`) — as opposed to
// `spawn extend`'s ADD-to-the-current-deadline semantics, computed
// separately in cmd/extend.go. This is the fix for spawn#553: it recomputes
// spawn:ttl-deadline from spawn:launch-time + newTTL and returns it alongside
// spawn:ttl so a caller writes both tags together, whether the new TTL is
// longer OR SHORTER than the current remaining time — shortening a TTL has
// no other supported path in spawn (extend is monotonic-forward-only), and
// silently failing to support it was the operational hole #553 reports: an
// operator who explicitly asks to tighten an over-provisioned TTL needs the
// deadline to actually move earlier.
func SetTags(launchTime time.Time, newTTL time.Duration) (map[string]string, time.Time, error) {
	deadline, err := DeadlineForNewTTL(launchTime, newTTL)
	if err != nil {
		return nil, time.Time{}, err
	}
	return Tags(launchTime, deadline), deadline, nil
}

// DefaultReaperMaxAge mirrors the ttl-reaper's REAPER_MAX_AGE default
// (lambda/ttl-reaper/main.go's defaultMaxAge and the MaxAge parameter default in
// its template).
//
// Duplicated here rather than imported because the reaper is a separate Go
// module and the CLI cannot read a deployment parameter it does not own. The
// duplication is gated: TestDefaultReaperMaxAgeMatchesTheReaper reads the
// reaper's source and fails if the two drift, so this cannot quietly become a
// second source of truth — the defect #790 was about.
const DefaultReaperMaxAge = 7 * 24 * time.Hour

// ExceedsDefaultCeiling reports whether a requested TTL is longer than the
// reaper's default max-age ceiling, in which case the instance will be reclaimed
// at the ceiling rather than at the requested deadline.
//
// The ceiling applies even inside a live deadline, because spawn:ttl-deadline is
// writable by the instance itself and so cannot be the only bound (#76). That
// makes a long --ttl silently ineffective, which is worth saying at launch rather
// than discovering at the ceiling.
func ExceedsDefaultCeiling(requested time.Duration) bool {
	return requested > DefaultReaperMaxAge
}

// CeilingWarning is the line to show when ExceedsDefaultCeiling is true. Empty
// otherwise, so a caller can print unconditionally.
func CeilingWarning(requested time.Duration) string {
	if !ExceedsDefaultCeiling(requested) {
		return ""
	}
	return fmt.Sprintf(
		"warning: --ttl %s exceeds the TTL reaper's default max-age ceiling of %s, so the "+
			"instance will be reclaimed at ~%s regardless of the deadline.\n"+
			"  The ceiling applies even inside a live deadline because spawn:ttl-deadline is "+
			"writable from the instance, so it cannot be the only bound.\n"+
			"  For a longer-lived instance, raise REAPER_MAX_AGE on the reaper deployment "+
			"(spawn reaper deploy --max-age, for a reaper in your own account).",
		requested, DefaultReaperMaxAge, DefaultReaperMaxAge)
}

// LaunchAnchor is the instant an instance's age, TTL and wall-clock cost are
// measured from: the EARLIER of its spawn:launch-time tag (tagValue, RFC3339)
// and the EC2 LaunchTime (ec2Launch), whichever are present and parseable.
// The zero time means neither is.
//
// It is the one rule for both enforcement (the TTL reaper's max-age ceiling and
// its spawn:launch-time + spawn:ttl fallback deadline) and display (`spawn
// status`, `spawn list`, `spawn cost`, `spawn extend`), so the age a user is
// shown is the age the reaper acts on (#1029).
//
// Why not EC2's LaunchTime alone: it is the most recent START, so it resets on
// every start of a stopped instance and an instance launched three days ago
// would look an hour old. The tag, written once by the CLI at launch, is the
// earlier of the two in that case.
//
// Why not the tag alone (GHSA-c93q-4jh7-6pvj): an instance that rewrote its
// own spawn:launch-time to "now" would never age past the max-age ceiling.
// Taking the minimum keeps the legitimate case and makes a forged later value
// harmless.
func LaunchAnchor(tagValue string, ec2Launch time.Time) time.Time {
	var t time.Time
	if tagValue != "" {
		if parsed, err := time.Parse(time.RFC3339, tagValue); err == nil {
			t = parsed
		}
	}
	if !ec2Launch.IsZero() && (t.IsZero() || ec2Launch.Before(t)) {
		t = ec2Launch
	}
	return t
}
