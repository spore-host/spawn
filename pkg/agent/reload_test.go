package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// refreshCountingProvider counts RefreshConfig calls, the tag re-reads a reload is
// supposed to cause (#1278).
type refreshCountingProvider struct {
	*stubProvider
	refreshes atomic.Int32
}

func (p *refreshCountingProvider) RefreshConfig(ctx context.Context) error {
	p.refreshes.Add(1)
	return p.stubProvider.RefreshConfig(ctx)
}

// startMonitor runs a.Monitor until the test ends. Its ticker is an hour, so
// no tick fires during the test: anything that changes is the reload's doing.
func startMonitor(t *testing.T, a *Agent) {
	t.Helper()
	a.monitorInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Monitor(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Monitor did not return after its context was cancelled")
		}
	})
}

// newReloadTestAgent is a monitor-ready agent whose provider counts refreshes
// and whose reload marker goes to a temp dir. handled receives one value per
// reload request the monitor loop handles.
func newReloadTestAgent(t *testing.T, cfg *provider.Config) (*Agent, *refreshCountingProvider, chan struct{}) {
	t.Helper()
	a := newTestAgent(t, cfg)
	p := &refreshCountingProvider{stubProvider: a.provider.(*stubProvider)}
	a.provider = p
	a.reloadMarkerPath = filepath.Join(t.TempDir(), "reloaded.json")
	handled := make(chan struct{}, 4)
	a.reloadHandled = func() { handled <- struct{}{} }
	return a, p, handled
}

func waitHandled(t *testing.T, handled chan struct{}) {
	t.Helper()
	select {
	case <-handled:
	case <-time.After(10 * time.Second):
		t.Fatal("the running monitor loop never handled the reload request (#1278: reload must reach the daemon)")
	}
}

// TestMonitor_ReloadRequestRefreshesRunningAgent is #1278's gate: `spored
// reload` used to reload a throwaway agent in its own process, so the running
// daemon kept its old config until the periodic refresh (tick 1, then every
// 5th). A reload request must reach the RUNNING agent's monitor loop and swap
// in the changed tags without waiting for a tick, and publish what it now has.
func TestMonitor_ReloadRequestRefreshesRunningAgent(t *testing.T) {
	oldCfg := &provider.Config{TTL: time.Hour}
	a, p, handled := newReloadTestAgent(t, oldCfg)
	startMonitor(t, a)

	// The tags change on the instance (spawn extend, config set).
	deadline := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	newCfg := &provider.Config{TTL: 3 * time.Hour, TTLDeadline: deadline, CostLimit: 4.5}
	p.config = newCfg

	a.RequestReload()
	waitHandled(t, handled)

	if got := p.refreshes.Load(); got != 1 {
		t.Errorf("RefreshConfig calls = %d, want exactly 1 (the reload's)", got)
	}
	if a.configRefreshTick != 0 {
		t.Errorf("configRefreshTick = %d: a tick ran, so this does not show the reload alone refreshed", a.configRefreshTick)
	}
	if a.GetConfig() != newCfg {
		t.Fatalf("the running agent's config is %+v, want the refreshed %+v", a.GetConfig(), newCfg)
	}

	m, err := ReadReloadMarker(a.reloadMarkerPath)
	if err != nil {
		t.Fatalf("reload marker: %v", err)
	}
	if m.PID != os.Getpid() || m.Seq != 1 || m.Error != "" {
		t.Errorf("marker pid/seq/error = %d/%d/%q, want %d/1/\"\"", m.PID, m.Seq, m.Error, os.Getpid())
	}
	if m.TTL != newCfg.TTL || !m.TTLDeadline.Equal(deadline) || m.CostLimit != 4.5 {
		t.Errorf("marker summary = ttl %v deadline %v cost %v, want the refreshed config", m.TTL, m.TTLDeadline, m.CostLimit)
	}
}

// TestMonitor_PublishesMarkerAtStart: `spored reload` signals only a daemon
// whose pid is in the marker, so the daemon must publish one (seq 0) before any
// request arrives. Without it, reload refuses to signal a new daemon at all.
func TestMonitor_PublishesMarkerAtStart(t *testing.T) {
	a, p, _ := newReloadTestAgent(t, &provider.Config{TTL: time.Hour})
	startMonitor(t, a)
	var m ReloadMarker
	var err error
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if m, err = ReadReloadMarker(a.reloadMarkerPath); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("Monitor published no reload marker at start: %v", err)
	}
	if m.Seq != 0 || m.PID != os.Getpid() || m.TTL != time.Hour {
		t.Errorf("start marker = %+v, want seq 0, this pid, ttl 1h", m)
	}
	if got := p.refreshes.Load(); got != 0 {
		t.Errorf("the start marker cost %d tag refreshes, want 0", got)
	}
}

// TestMonitor_ReloadFailureIsPublished: a failed tag refresh keeps the old
// config and says so in the marker, so `spored reload` exits non-zero instead
// of reporting the stale config as reloaded (#505, #1278).
func TestMonitor_ReloadFailureIsPublished(t *testing.T) {
	oldCfg := &provider.Config{TTL: time.Hour}
	a, p, handled := newReloadTestAgent(t, oldCfg)
	startMonitor(t, a)
	p.refreshConfigErr = errors.New("DescribeTags: 403")
	p.config = &provider.Config{TTL: 9 * time.Hour}

	a.RequestReload()
	waitHandled(t, handled)

	if a.GetConfig() != oldCfg {
		t.Error("a failed refresh swapped in new config")
	}
	m, err := ReadReloadMarker(a.reloadMarkerPath)
	if err != nil {
		t.Fatalf("reload marker: %v", err)
	}
	if m.Seq != 1 || m.Error == "" || m.TTL != time.Hour {
		t.Errorf("marker = %+v, want seq 1, a non-empty error and the old 1h TTL", m)
	}
}

// TestRequestReload_NeverBlocks: the signal handler calls RequestReload; a
// burst of requests while one is pending must coalesce, not block.
func TestRequestReload_NeverBlocks(t *testing.T) {
	a := newTestAgent(t, nil)
	for i := 0; i < 5; i++ {
		a.RequestReload()
	}
	if n := len(a.reloadRequests()); n != 1 {
		t.Errorf("pending requests = %d, want 1", n)
	}
}

// TestReload_SwapsUnderConfigMu: Reload now runs on the daemon's monitor loop
// while the metrics scrape and other goroutines read the config, so it must
// read and swap through cfg()/setConfig (#175, #1278). It used to assign
// a.config directly. Under -race (CI runs pkg/agent with it) the concurrent
// reader below reports that; without -race, the held lock shows it.
func TestReload_SwapsUnderConfigMu(t *testing.T) {
	a := newTestAgent(t, &provider.Config{TTL: time.Hour})
	stub := a.provider.(*stubProvider)
	stub.config = &provider.Config{TTL: 2 * time.Hour}

	t.Run("concurrent readers", func(t *testing.T) {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = a.GetConfig().TTL
				}
			}
		}()
		for i := 0; i < 200; i++ {
			if err := a.Reload(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		close(stop)
		wg.Wait()
	})

	t.Run("waits for the write lock", func(t *testing.T) {
		next := &provider.Config{TTL: 5 * time.Hour}
		stub.config = next
		a.configMu.Lock()
		done := make(chan error, 1)
		go func() { done <- a.Reload(context.Background()) }()
		select {
		case <-done:
			a.configMu.Unlock()
			t.Fatal("Reload completed while configMu was held: it bypasses the lock")
		case <-time.After(100 * time.Millisecond):
		}
		if a.config == next {
			a.configMu.Unlock()
			t.Fatal("Reload swapped the config while configMu was held")
		}
		a.configMu.Unlock()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if a.GetConfig() != next {
			t.Error("Reload did not adopt the refreshed config once the lock was free")
		}
	})
}
