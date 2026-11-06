package egressproxy

import (
	"math/rand/v2"
	"strings"
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
	// last is a minute old, so the published value stays younger than the
	// window, and the throttle holds, for ActivityPublishEvery - 1m.
	for _, d := range []time.Duration{time.Second, time.Minute, ActivityPublishEvery - time.Minute - time.Second} {
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

// After a quiet period the first new activity is published at once (#1528),
// replayed on the issue's live timeline: the proxy launched at 23:36:25,
// spored's first tick at 23:39:46 published that launch time, and the last
// real traffic was 23:39:58. The old rule held the next write until 23:49:46,
// a throttle window after the first; the tag said 23:36:25 for ten minutes.
// Now, once the published value is a window old (23:46:25), the next tick
// (23:46:46) writes 23:39:58. The writes that follow are throttled again.
func TestActivityPublisherPublishesTheFirstActivityAfterQuietAtOnce(t *testing.T) {
	at := func(hms string) time.Time {
		v, err := time.Parse(time.DateTime, "2026-11-04 "+hms)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	launch, firstTick, traffic := at("23:36:25"), at("23:39:46"), at("23:39:58")
	var p ActivityPublisher
	p.Written(mustDue(t, &p, launch, firstTick))

	tick := firstTick
	for tick = tick.Add(time.Minute); tick.Sub(launch) < ActivityPublishEvery; tick = tick.Add(time.Minute) {
		if _, due := p.Due(traffic, tick); due {
			t.Fatalf("due at %s, while the published value is %v old: under the window, the throttle holds",
				tick.Format(time.TimeOnly), tick.Sub(launch))
		}
	}
	if tick != at("23:46:46") {
		t.Fatalf("first tick past the window %s, want 23:46:46", tick.Format(time.TimeOnly))
	}
	v := mustDue(t, &p, traffic, tick)
	if v != "2026-11-04T23:39:58Z" {
		t.Fatalf("published %q at %s, want the traffic at 23:39:58", v, tick.Format(time.TimeOnly))
	}
	p.Written(v)

	// Then a burst: traffic every tick for 40 minutes. 23:39:58 is a stale
	// value, so it too ages past the window soon (23:49:58) and the 23:50:46
	// tick publishes; that keeps the promise that a tag a window old is
	// current. From there the burst is one write per window.
	var writes []string
	for now := tick.Add(time.Minute); now.Before(tick.Add(40 * time.Minute)); now = now.Add(time.Minute) {
		if v, due := p.Due(now.Add(-time.Second), now); due {
			p.Written(v)
			writes = append(writes, now.Format(time.TimeOnly))
		}
	}
	if got, want := strings.Join(writes, " "), "23:50:46 00:00:46 00:10:46 00:20:46"; got != want {
		t.Errorf("burst writes at %s, want %s", got, want)
	}
}

func mustDue(t *testing.T, p *ActivityPublisher, last, now time.Time) string {
	t.Helper()
	v, due := p.Due(last, now)
	if !due {
		t.Fatalf("Due(%s, %s) not due", last.Format(time.TimeOnly), now.Format(time.TimeOnly))
	}
	return v
}

// No change, no write, however quiet: the after-quiet rule is for a new value.
func TestActivityPublisherWritesNothingWithoutAChange(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var p ActivityPublisher
	p.Written(mustDue(t, &p, t0, t0))
	for _, d := range []time.Duration{time.Minute, ActivityPublishEvery, 3 * ActivityPublishEvery, 72 * time.Hour} {
		if _, due := p.Due(t0, t0.Add(d)); due {
			t.Errorf("unchanged activity due %v later", d)
		}
	}
}

// A refused after-quiet write does not make every tick an attempt: the retry
// falls back to the throttle's pace.
func TestActivityPublisherRetriesAFailedAfterQuietWriteAtTheThrottlesPace(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var p ActivityPublisher
	p.Written(mustDue(t, &p, t0, t0))
	edge := t0.Add(time.Hour)
	mustDue(t, &p, edge, edge) // refused: not Written
	for _, d := range []time.Duration{time.Minute, ActivityPublishEvery - time.Second} {
		if _, due := p.Due(edge, edge.Add(d)); due {
			t.Errorf("failed after-quiet write retried %v later; want the throttle", d)
		}
	}
	mustDue(t, &p, edge, edge.Add(ActivityPublishEvery))
}

// The two properties the rule exists for, over a long run of random traffic on
// spored's one-minute tick: (1) whenever the tag is at least the window old it
// is the proxy's real last activity, so the idle expiry status shows is right
// once it is near; (2) writes stay rare — never more than two in any window,
// and about one per window while the proxy is busy.
func TestActivityPublisherProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(1528, 1))
	t0 := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	var p ActivityPublisher
	var last time.Time = t0
	var writes []time.Time
	pub := ""
	busy, spell := false, 0
	for i := 0; i < 20000; i++ {
		now := t0.Add(time.Duration(i) * time.Minute)
		// Bursty traffic: busy and quiet spells of 1-40 ticks; a busy tick
		// carries traffic two times in three, at a random second.
		if spell == 0 {
			busy, spell = !busy, 1+rng.IntN(40)
		}
		spell--
		if busy && rng.IntN(3) > 0 {
			last = now.Add(-time.Duration(rng.IntN(60)) * time.Second)
		}
		if v, due := p.Due(last, now); due {
			p.Written(v)
			pub = v
			writes = append(writes, now)
		}
		pt, _ := time.Parse(time.RFC3339, pub)
		if now.Sub(pt) >= ActivityPublishEvery && pub != ActivityValue(last) {
			t.Fatalf("at %s the tag %s is %v old but the real last activity is %s",
				now.Format(time.DateTime), pub, now.Sub(pt), ActivityValue(last))
		}
	}
	for i := 2; i < len(writes); i++ {
		if writes[i].Sub(writes[i-2]) < ActivityPublishEvery {
			t.Fatalf("three writes within %v: %s, %s, %s", ActivityPublishEvery,
				writes[i-2].Format(time.TimeOnly), writes[i-1].Format(time.TimeOnly), writes[i].Format(time.TimeOnly))
		}
	}
	if len(writes) == 0 || len(writes) > 20000/10*2 {
		t.Fatalf("%d writes in %d ticks", len(writes), 20000)
	}
	t.Logf("%d writes in %d one-minute ticks", len(writes), 20000)
}
