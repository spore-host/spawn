package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// externalIdleAgent is an agent whose monitor tick does no tag writes, with a
// fake clock for the external idle check.
func externalIdleAgent(t *testing.T, cfg *provider.Config, clock *time.Time) (*Agent, *stubProvider, *recordedEvents) {
	t.Helper()
	a := newTestAgent(t, cfg)
	sp := a.provider.(*stubProvider)
	ev := &recordedEvents{}
	a.notifier = &Notifier{record: ev.record}
	a.warnDelay = time.Millisecond
	a.lastSessionTagWrite = time.Now().Add(time.Hour)
	a.lastComputeTagWrite = time.Now().Add(time.Hour)
	a.lastHeartbeatTagWrite = time.Now().Add(time.Hour)
	a.externalIdleClock = func() time.Time { return *clock }
	return a, sp, ev
}

// The egress proxy's idle expiry (#1515): once nothing has been proxied for
// the timeout, the monitor tick terminates the instance through the same
// terminate the TTL uses, never a stop.
func TestExternalIdleTerminatesThroughTheTerminatePath(t *testing.T) {
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	last := clock
	// spored's own idle settings would stop (or hibernate) the instance; the
	// external signal must replace them, not race them.
	a, sp, ev := externalIdleAgent(t, &provider.Config{
		IdleTimeout: time.Minute, IdleCPUPercent: 101, HibernateOnIdle: true,
	}, &clock)
	a.lastActivityTime = clock.Add(-24 * time.Hour)
	a.SetExternalIdle(ExternalIdle{Name: "egress proxy", LastActivity: func() time.Time { return last }, Timeout: 14 * 24 * time.Hour})

	clock = clock.Add(14*24*time.Hour - time.Second)
	a.checkAndAct(context.Background())
	if sp.terminated || sp.stopped || sp.hibernated {
		t.Fatalf("acted a second before the idle period ended: terminated=%v stopped=%v hibernated=%v",
			sp.terminated, sp.stopped, sp.hibernated)
	}

	clock = clock.Add(2 * time.Second)
	a.checkAndAct(context.Background())
	if !sp.terminated {
		t.Fatal("not terminated after the idle period")
	}
	if sp.stopped || sp.hibernated {
		t.Errorf("stopped=%v hibernated=%v: an idle proxy must terminate", sp.stopped, sp.hibernated)
	}
	if n := ev.count("ttl_expired"); n != 1 {
		t.Errorf("ttl_expired (the \"has terminated\" event) sent %d times, want 1", n)
	}
}

// Activity resets the clock: a proxy used within the period is left running.
func TestExternalIdleActivityKeepsTheInstance(t *testing.T) {
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	last := clock
	a, sp, _ := externalIdleAgent(t, &provider.Config{}, &clock)
	a.SetExternalIdle(ExternalIdle{Name: "egress proxy", LastActivity: func() time.Time { return last }, Timeout: time.Hour})
	for i := 0; i < 5; i++ {
		clock = clock.Add(50 * time.Minute)
		last = clock.Add(-time.Minute)
		a.checkAndAct(context.Background())
	}
	if sp.terminated {
		t.Error("terminated although the proxy was used every 50 minutes")
	}
}

// A refused terminate is retried on the next tick, like the TTL's, and
// announced only once accepted.
func TestExternalIdleRetriesARefusedTerminate(t *testing.T) {
	clock := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	a, sp, ev := externalIdleAgent(t, &provider.Config{}, &clock)
	a.SetExternalIdle(ExternalIdle{Name: "egress proxy", LastActivity: func() time.Time { return clock.Add(-2 * time.Hour) }, Timeout: time.Hour})
	sp.terminateErr = errors.New("api error")
	a.checkAndAct(context.Background())
	if ev.count("ttl_expired") != 0 {
		t.Error("a refused terminate was announced")
	}
	sp.terminateErr = nil
	a.checkAndAct(context.Background())
	if !sp.terminated || ev.count("ttl_expired") != 1 {
		t.Errorf("terminated=%v events=%v after the retry", sp.terminated, ev.events)
	}
}
