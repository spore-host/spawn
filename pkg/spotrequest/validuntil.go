package spotrequest

import (
	"fmt"
	"time"
)

// ValidUntilTag records, on the instance and on its request, when spawn set a
// persistent Spot request to expire (its ValidUntil, #950). The CLI reads it to
// warn when `spawn extend` pushes the deadline past it, and to explain a failed
// `spawn start` after it.
const ValidUntilTag = "spawn:spot-valid-until"

// OriginalInstanceTag is written on a persistent request after RunInstances,
// naming the instance spawn launched from it (#950). The TTL reaper's sweep
// compares it with the request's current InstanceId. A difference means the
// original was terminated outside spawn and EC2 launched a replacement.
const OriginalInstanceTag = "spawn:original-instance-id"

// DefaultMaxLifetime bounds a persistent request whose launch has no TTL
// deadline. It matches the TTL reaper's default max-age ceiling
// (pkg/ttl.DefaultReaperMaxAge), which terminates any instance older than
// that whatever its deadline says, so the request never needs to outlive it.
// Kept as its own constant so this package doesn't import pkg/ttl.
// TestDefaultMaxLifetimeMatchesReaperCeiling keeps the two in step.
const DefaultMaxLifetime = 7 * 24 * time.Hour

// ValidUntil is the ValidUntil spawn gives a persistent Spot request (#950): the
// TTL deadline, or launch plus [DefaultMaxLifetime] when there is no deadline.
//
// It is a backstop for a request left open by a terminate that bypassed spawn.
// Such a request keeps launching replacement instances, and EC2 has no other
// end date for a persistent request. Past ValidUntil the request is no longer
// active. It launches nothing more, and a running instance is not terminated,
// but a stopped one can no longer be restarted through it. That is why
// `spawn extend` warns. (This comes from EC2's API documentation; it has not
// been observed on a live request.)
func ValidUntil(deadline, launch time.Time) time.Time {
	if !deadline.IsZero() {
		return deadline.UTC()
	}
	return launch.Add(DefaultMaxLifetime).UTC()
}

// parseValidUntil reads [ValidUntilTag] from a persistent Spot instance's tags.
// ok is false for anything else: on-demand, one-time, launched before #950.
func parseValidUntil(tags map[string]string) (time.Time, bool) {
	if tags[TypeTag] != TypePersistent {
		return time.Time{}, false
	}
	v, err := time.Parse(time.RFC3339, tags[ValidUntilTag])
	if err != nil {
		return time.Time{}, false
	}
	return v, true
}

// ExtendWarning is what `spawn extend` prints when the new deadline outlives the
// instance's Spot request. "" when it doesn't, or the instance isn't persistent
// Spot. EC2 has no API to move a Spot request's end date, so this is a warning,
// not a fix.
func ExtendWarning(tags map[string]string, newDeadline time.Time) string {
	vu, ok := parseValidUntil(tags)
	if !ok || !newDeadline.After(vu) {
		return ""
	}
	return fmt.Sprintf("this instance's persistent Spot request expires at %s (spawn sets it to the deadline "+
		"at launch, and EC2 cannot change it). Until the new deadline the instance keeps running, but once "+
		"the request has expired a stop (the idle timeout, spawn stop, a Spot interruption) may not be "+
		"restartable: EC2 restarts a stopped Spot instance only through its request",
		vu.UTC().Format("2006-01-02 15:04 UTC"))
}

// StartFailureNote adds the likely reason to a failed `spawn start` when the
// instance's Spot request has expired. "" otherwise.
func StartFailureNote(tags map[string]string, now time.Time) string {
	vu, ok := parseValidUntil(tags)
	if !ok || now.Before(vu) {
		return ""
	}
	return fmt.Sprintf("its persistent Spot request expired at %s, and EC2 restarts a stopped Spot instance "+
		"only through an active request. Launch a new instance; terminate this one with spawn terminate",
		vu.UTC().Format("2006-01-02 15:04 UTC"))
}
