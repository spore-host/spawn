package egressproxy

import (
	"testing"
	"time"
)

// The activity tags are written at most once per ActivityPublishEvery, and
// only when the value changed (#1515 phase 5): an idle proxy writes nothing,
// and a busy one about every ten minutes, never on every tick.
func TestActivityPublisherThrottles(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	now := t0
	var p ActivityPublisher

	if _, due := p.Due(time.Time{}, now); due {
		t.Fatal("a zero activity time is due; there is nothing to publish")
	}
	v, due := p.Due(t0, now)
	if !due || v != "2026-11-03T12:00:00Z" {
		t.Fatalf("first publish = %q, %v; want the activity in RFC 3339, due", v, due)
	}
	p.Written(v)

	// Unchanged: never due, however long it has been.
	now = t0.Add(5 * time.Hour)
	if _, due := p.Due(t0, now); due {
		t.Error("an unchanged activity time is due again")
	}

	// Changed, but within the throttle of the last attempt.
	last := now.Add(-time.Minute)
	now = t0.Add(5 * time.Hour) // the last attempt was at t0, so this one is due
	if v, due := p.Due(last, now); !due || v != ActivityValue(last) {
		t.Fatalf("a changed value after the throttle: %q, %v; want due", v, due)
	}
	p.Written(ActivityValue(last))
	for _, d := range []time.Duration{time.Second, time.Minute, ActivityPublishEvery - time.Second} {
		if _, due := p.Due(now.Add(d), now.Add(d)); due {
			t.Errorf("due %v after the last write; the throttle is %v", d, ActivityPublishEvery)
		}
	}
	at := now.Add(ActivityPublishEvery)
	if _, due := p.Due(at, at); !due {
		t.Errorf("not due %v after the last write with a changed value", ActivityPublishEvery)
	}
}

// A write that failed (no Written) is retried at the throttle's pace, not on
// every tick: a refused CreateTags must not become a call a minute.
func TestActivityPublisherRetriesAFailedWriteAtTheThrottlesPace(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var p ActivityPublisher
	if _, due := p.Due(t0, t0); !due {
		t.Fatal("first publish not due")
	}
	// Not recorded as written: the write failed.
	if _, due := p.Due(t0, t0.Add(time.Minute)); due {
		t.Error("a failed write is retried a minute later; want the throttle")
	}
	if _, due := p.Due(t0, t0.Add(ActivityPublishEvery)); !due {
		t.Error("a failed write is never retried")
	}
}
