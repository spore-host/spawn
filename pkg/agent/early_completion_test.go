package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// lateTagProvider models EC2 tag eventual consistency (#270): spawn:on-complete
// is invisible to the first visibleAt-1 RefreshConfig calls and visible from
// call visibleAt on.
type lateTagProvider struct {
	*stubProvider
	visibleAt int
	refreshes int
	before    *provider.Config
	after     *provider.Config
}

func (p *lateTagProvider) RefreshConfig(context.Context) error { p.refreshes++; return nil }
func (p *lateTagProvider) GetConfig(context.Context) (*provider.Config, error) {
	if p.refreshes >= p.visibleAt {
		return p.after, nil
	}
	return p.before, nil
}

// earlyCompletionAgent returns an agent whose first tag refresh (tick 1) does
// not see on-complete, and whose completion file is the default path (because
// the completion-file tag is not visible either).
func earlyCompletionAgent(t *testing.T) (*Agent, *lateTagProvider, string) {
	t.Helper()
	signal := t.TempDir() + "/SPAWN_COMPLETE"
	ttl := time.Now().Add(4 * time.Hour)
	before := &provider.Config{TTLDeadline: ttl}
	after := &provider.Config{TTLDeadline: ttl, OnComplete: "terminate", CompletionFile: signal}

	a := newTestAgent(t, before)
	p := &lateTagProvider{stubProvider: a.provider.(*stubProvider), visibleAt: 2, before: before, after: after}
	a.provider = p
	a.defaultCompletionFile = signal
	a.warnDelay = time.Millisecond
	a.graceSleep = func(time.Duration) {}
	a.notifier = &Notifier{record: (&recordedEvents{}).record}
	a.lastSessionTagWrite = time.Now().Add(time.Hour)
	a.lastComputeTagWrite = time.Now().Add(time.Hour)
	a.lastHeartbeatTagWrite = time.Now().Add(time.Hour)
	return a, p, signal
}

// The reported race: tick 1 refreshes before spawn:on-complete is visible, the
// short task then writes its completion file, and the tag becomes visible on
// the next read. on_complete must fire on the very next tick, not wait for the
// tick-5 refresh (or, before that existed, the TTL).
func TestCompletionBeforeOnCompleteTagIsVisibleStillActsNextTick(t *testing.T) {
	a, p, signal := earlyCompletionAgent(t)

	a.checkAndAct(context.Background()) // tick 1: scheduled refresh, tag not yet visible
	if a.config.OnComplete != "" || p.terminated {
		t.Fatalf("setup: tick 1 should see no on-complete and do nothing (on-complete=%q terminated=%v)", a.config.OnComplete, p.terminated)
	}

	if err := os.WriteFile(signal, []byte(`{"status":"success","exit_code":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.checkAndAct(context.Background()) // tick 2: not a scheduled refresh tick
	if !p.terminated {
		t.Fatalf("on_complete=terminate did not fire on the tick after the completion file appeared (refreshes=%d); it would wait for the tick-5 refresh", p.refreshes)
	}
	if p.refreshes != 2 {
		t.Errorf("want exactly one extra refresh triggered by the signal (2 total), got %d", p.refreshes)
	}
}

// Without a completion signal, nothing changes: ticks 2-4 do not re-read tags.
func TestNoCompletionSignalKeepsTheRefreshCadence(t *testing.T) {
	a, p, _ := earlyCompletionAgent(t)
	for i := 0; i < 4; i++ {
		a.checkAndAct(context.Background())
	}
	if p.refreshes != 1 {
		t.Errorf("4 ticks without a completion signal: %d refreshes, want 1 (tick 1 only)", p.refreshes)
	}
}

// A workload that writes the file with no on-complete ever configured costs a
// bounded number of extra refreshes, not one per tick forever.
func TestEarlyCompletionRefreshesAreCapped(t *testing.T) {
	a, p, signal := earlyCompletionAgent(t)
	p.visibleAt = 1 << 30 // never visible
	if err := os.WriteFile(signal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const ticks = 4 * maxEarlyCompletionRefreshes // 20 ticks: scheduled refreshes at 1,5,10,15,20
	for i := 0; i < ticks; i++ {
		a.checkAndAct(context.Background())
	}
	scheduled := 1 + ticks/5
	if want := scheduled + maxEarlyCompletionRefreshes; p.refreshes != want {
		t.Errorf("%d ticks: %d refreshes, want %d scheduled + %d early = %d", ticks, p.refreshes, scheduled, maxEarlyCompletionRefreshes, want)
	}
	if p.terminated {
		t.Error("terminated with no on-complete configured")
	}
}
