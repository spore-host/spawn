package egressproxy

import (
	"time"
)

// The proxy's activity, published as tags (#1515 phase 5), so `spawn
// egress-proxy status` can say how long a proxy has been idle and when its idle
// period ends without running anything on it over SSM. spored writes both
// through its own tag path (pkg/agent, ec2:CreateTags on itself, which the
// spored role already holds for spawn:last-heartbeat), throttled by
// [ActivityPublisher].
//
// They are instance-written, so they are a report, never a decision: nothing
// reuses, keeps or ends a proxy because of them. spored's idle expiry reads its
// in-process activity, and the deadline is the spawn:ttl-deadline the reaper
// enforces.
const (
	// ActivityTag is the last time the proxy carried a request or a byte,
	// RFC 3339 in UTC.
	ActivityTag = "spawn:egress-proxy-last-activity"
	// IdleTag is the idle period spored enforces on the proxy, as a Go
	// duration ("72h0m0s").
	IdleTag = "spawn:egress-proxy-idle"
	// ActivityTagsSince is the first spawn release whose proxies publish
	// [ActivityTag] and [IdleTag]. A proxy without them predates it.
	ActivityTagsSince = "0.158.0"
	// ActivityPublishEvery is the most often [ActivityPublisher] lets the
	// tags be written.
	ActivityPublishEvery = 10 * time.Minute
)

// ActivityPublisher decides when spored rewrites [ActivityTag]: at most once
// per [ActivityPublishEvery], and only when the value it would write differs
// from the last one written. The value has one-second resolution, so a proxy
// in use writes about every ten minutes and an idle one never. Not safe for
// concurrent use; spored calls it from one loop.
type ActivityPublisher struct {
	// Every overrides [ActivityPublishEvery] when positive. Tests only.
	Every time.Duration

	attempted time.Time
	written   string
}

// ActivityValue is how t is written into [ActivityTag].
func ActivityValue(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Due reports whether last should be written now, and the value to write. A
// write that is due counts as an attempt whether or not it then succeeds (see
// [ActivityPublisher.Written]), so a refused CreateTags is retried at the
// throttle's pace, not every tick.
func (p *ActivityPublisher) Due(last, now time.Time) (string, bool) {
	if last.IsZero() {
		return "", false
	}
	v := ActivityValue(last)
	if v == p.written {
		return "", false
	}
	every := p.Every
	if every <= 0 {
		every = ActivityPublishEvery
	}
	if !p.attempted.IsZero() && now.Sub(p.attempted) < every {
		return "", false
	}
	p.attempted = now
	return v, true
}

// Written records that v reached the instance's tags.
func (p *ActivityPublisher) Written(v string) { p.written = v }
