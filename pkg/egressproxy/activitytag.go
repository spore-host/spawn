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

// ActivityPublisher decides when spored rewrites [ActivityTag]. It writes only
// when the value it would write differs from the last one written, and:
//
//   - after a quiet period, at once: when the published value is older than
//     [ActivityPublishEvery], the first new activity is written on the tick
//     that sees it (#1528). So once the tag says the proxy has been idle for
//     the throttle window or longer, it is within a tick of the truth, and
//     the edge that decides the idle expiry reaches the tag in seconds, not
//     minutes;
//   - otherwise, at most once per [ActivityPublishEvery] since the last
//     attempt. A proxy in steady use writes once every ten minutes, and an
//     idle one never. An after-quiet write can carry a value already minutes
//     old, which then ages past the window early, so there can be two writes
//     in one window, never three.
//
// A tag younger than the window can still trail the proxy's real last
// activity by up to its own age, which is why `spawn egress-proxy status`
// reports it as a lower bound. The value has one-second resolution. Not safe
// for concurrent use; spored calls it from one loop.
type ActivityPublisher struct {
	// Every overrides [ActivityPublishEvery] when positive. Tests only.
	Every time.Duration

	attempted      time.Time // when the last write was attempted
	attemptedValue string    // what it tried to write
	written        string    // the last value that reached the tags
	writtenTime    time.Time // written, parsed
}

// ActivityValue is how t is written into [ActivityTag].
func ActivityValue(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Due reports whether last should be written now, and the value to write. A
// write that is due counts as an attempt whether or not it then succeeds (see
// [ActivityPublisher.Written]), so a refused CreateTags is retried at the
// throttle's pace, not every tick: the after-quiet exemption applies only when
// the previous attempt succeeded.
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
	if !p.attempted.IsZero() && now.Sub(p.attempted) < every && !p.quiet(now, every) {
		return "", false
	}
	p.attempted, p.attemptedValue = now, v
	return v, true
}

// quiet reports whether the published value is at least every old and the
// last attempt was the write that published it.
func (p *ActivityPublisher) quiet(now time.Time, every time.Duration) bool {
	return p.written != "" && p.attemptedValue == p.written && now.Sub(p.writtenTime) >= every
}

// Written records that v reached the instance's tags.
func (p *ActivityPublisher) Written(v string) {
	p.written = v
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		p.writtenTime = t
	}
}
