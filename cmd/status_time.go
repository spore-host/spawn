package cmd

import (
	"regexp"
	"time"
)

// statusLoc is the zone `spawn status` renders times in; nil means the
// operator's local zone. A variable so tests can pin it.
var statusLoc *time.Location

func statusLocation() *time.Location {
	if statusLoc != nil {
		return statusLoc
	}
	return time.Local
}

// formatStatusTime is the ONE way `spawn status` writes a moment: local time,
// with UTC in parentheses — "2026-10-15 10:59 MST (17:59 UTC)". The status screen
// used to say "terminates 2026-10-15 17:59 UTC" two lines above "Termination
// deadline: 2026-10-15 10:59 MST" for the same instant (#896). Where local IS
// UTC the parenthetical would repeat itself, so it is dropped; the UTC date is
// included only when it differs from the local one.
func formatStatusTime(t time.Time, loc *time.Location) string {
	local := t.In(loc)
	utc := t.UTC()
	if _, off := local.Zone(); off == 0 {
		return utc.Format("2006-01-02 15:04 UTC")
	}
	s := local.Format("2006-01-02 15:04 MST")
	if local.Format("2006-01-02") != utc.Format("2006-01-02") {
		return s + " (" + utc.Format("2006-01-02 15:04 UTC") + ")"
	}
	return s + " (" + utc.Format("15:04 UTC") + ")"
}

// sporedUTCTime matches the one timestamp shape spored's status table emits
// ("Started:" and the TTL deadline). spored runs on the instance and cannot know
// the operator's zone, so it writes UTC; spawn re-renders those timestamps in the
// same format as its own lines. Anchored to the full date-time-zone shape so
// nothing else in the output can match.
var sporedUTCTime = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2} \d{2}:\d{2}) UTC\b`)

// localizeSporedTimes rewrites spored's UTC timestamps into formatStatusTime's
// format. Display only: callers must run any parsing of spored's output against
// the original text.
func localizeSporedTimes(out string, loc *time.Location) string {
	return sporedUTCTime.ReplaceAllStringFunc(out, func(m string) string {
		t, err := time.ParseInLocation("2006-01-02 15:04", sporedUTCTime.FindStringSubmatch(m)[1], time.UTC)
		if err != nil {
			return m
		}
		return formatStatusTime(t, loc)
	})
}
