package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spore-host/spawn/pkg/observability/metrics"
)

// noSessions points the session check at an empty /dev, so a terminal on the
// machine running the tests cannot decide them.
func noSessions(t *testing.T) {
	t.Helper()
	origRoot, origLines, origCounter := sessionDevRoot, loginTTYLines, sessionCounter
	t.Cleanup(func() { sessionDevRoot, loginTTYLines, sessionCounter = origRoot, origLines, origCounter })
	sessionDevRoot = t.TempDir()
	loginTTYLines = func(context.Context) []string { return nil }
	// The Linux sources above are not the Windows one (quser); pin the count too.
	sessionCounter = func(context.Context) int { return 0 }
}

// counters is a fake host: cumulative CPU jiffies, network bytes and disk
// bytes the test advances between ticks. Atomic, so that a reader on another
// goroutine is a race only in the agent, not in the fake.
type counters struct {
	cpuIdle, cpuTotal, net, disk atomic.Int64
	reads                        atomic.Int64
}

func installCounters(t *testing.T) *counters {
	t.Helper()
	c := &counters{}
	origCPU, origNet, origDisk := readCPUTimes, readNetworkBytes, readDiskIOBytes
	t.Cleanup(func() { readCPUTimes, readNetworkBytes, readDiskIOBytes = origCPU, origNet, origDisk })
	readCPUTimes = func(context.Context) (int64, int64, error) {
		c.reads.Add(1)
		return c.cpuIdle.Load(), c.cpuTotal.Load(), nil
	}
	readNetworkBytes = func(context.Context) (int64, int64) { c.reads.Add(1); return c.net.Load(), 0 }
	readDiskIOBytes = func(context.Context) int64 { c.reads.Add(1); return c.disk.Load() }
	c.cpuTotal.Store(1000)
	return c
}

// idleCPUMinute advances the CPU counters by one quiet minute (1% busy).
func (c *counters) idleCPUMinute() {
	c.cpuIdle.Add(5940)
	c.cpuTotal.Add(6000)
}

func (c *counters) busyCPUMinute() {
	c.cpuIdle.Add(600)
	c.cpuTotal.Add(6000)
}

func scrape(a *Agent) {
	ch := make(chan prometheus.Metric, 256)
	go func() {
		metrics.NewCollector(a).Collect(ch)
		close(ch)
	}()
	for range ch {
	}
}

// THE bug in #952. Traffic just over the idle threshold in one tick, three
// quarters of it before a scrape. A scrape in the middle of the tick used to
// take its own sample, so the monitor loop's delta covered only the quarter
// since the scrape and the instance read as idle: a false idle-stop. The
// monitor loop must own sampling.
func TestScrapeDoesNotShortenTheMonitorLoopWindow(t *testing.T) {
	noSessions(t)
	c := installCounters(t)
	a := newTestAgent(t, nil)

	a.evaluateIdle(context.Background()) // first tick: baseline
	c.idleCPUMinute()
	total := idleNetworkBytesThreshold + idleNetworkBytesThreshold/4
	before := total * 3 / 4
	c.net.Add(before)
	scrape(a) // a Prometheus scrape mid-tick
	c.net.Add(total - before)

	s := a.evaluateIdle(context.Background())
	if !s.netMeasured || s.net != total {
		t.Fatalf("monitor tick measured %d network bytes (measured=%v), want all %d "+
			"since the previous tick — the scrape moved the baseline (#952)", s.net, s.netMeasured, total)
	}
	if s.idle {
		t.Errorf("%d bytes of traffic in one tick, over the %d threshold, read as idle (%s)", total, idleNetworkBytesThreshold, s.reason)
	}
}

// A scrape reads; it does not sample. The counters are touched by the monitor
// loop's ticks only.
func TestScrapeTakesNoSample(t *testing.T) {
	noSessions(t)
	c := installCounters(t)
	a := newTestAgent(t, nil)
	a.evaluateIdle(context.Background())
	before := c.reads.Load()
	for i := 0; i < 3; i++ {
		scrape(a)
		_ = a.IsIdle()
		_, _ = a.GetCPUUsageMeasured()
		_, _, _ = a.GetNetworkBytesMeasured()
		_, _, _ = a.GetDiskIOBytesMeasured()
	}
	if n := c.reads.Load() - before; n != 0 {
		t.Errorf("readers took %d counter samples; only the monitor loop may sample (#952)", n)
	}
}

// Every tick samples every counter, even when an earlier check decides "not
// idle". Before, a busy CPU returned before the network was sampled, so the
// next network delta covered two ticks.
func TestEveryTickSamplesEveryCounter(t *testing.T) {
	noSessions(t)
	c := installCounters(t)
	a := newTestAgent(t, nil)
	a.evaluateIdle(context.Background())

	c.busyCPUMinute()
	c.net.Add(10_000_000) // a big transfer during the busy tick
	c.disk.Add(10_000_000)
	if s := a.evaluateIdle(context.Background()); s.idle {
		t.Fatalf("busy tick read as idle: %s", s.reason)
	}

	c.idleCPUMinute()
	c.net.Add(5_000)
	c.disk.Add(4_096)
	s := a.evaluateIdle(context.Background())
	if s.net != 5_000 || s.disk != 4_096 {
		t.Errorf("quiet tick measured net=%d disk=%d, want 5000 and 4096: the window "+
			"included the previous tick, which returned early at the CPU check", s.net, s.disk)
	}
	if !s.idle {
		t.Errorf("quiet tick after a busy one read as not idle: %s", s.reason)
	}
}

// Before the first evaluation every reader reports unknown and not idle,
// rather than a zero that reads as idle.
func TestReadersBeforeTheFirstTickAreUnknown(t *testing.T) {
	a := newTestAgent(t, nil)
	if a.IsIdle() {
		t.Error("IsIdle before any evaluation = true")
	}
	if _, measured := a.GetCPUUsageMeasured(); measured {
		t.Error("CPU reported measured before any sample")
	}
	if v, measured, why := a.GetNetworkBytesMeasured(); measured || why == "" || v <= idleNetworkBytesThreshold {
		t.Errorf("network before any sample = (%d, %v, %q), want unknown over the threshold", v, measured, why)
	}
	if v, measured, why := a.GetDiskIOBytesMeasured(); measured || why == "" || v <= idleDiskBytesThreshold {
		t.Errorf("disk before any sample = (%d, %v, %q), want unknown over the threshold", v, measured, why)
	}
	if _, err := a.GetCPUUsageContext(t.Context()); err == nil {
		t.Error("the metrics CPU reader exported a value before any sample")
	}
}

// Run under -race (CI's `go test -race ./pkg/agent/...`, `make test-race`):
// the monitor loop's ticks and concurrent scrapes and status reads must not
// share unsynchronised state (#952).
func TestMonitorTicksAndScrapesDoNotRace(t *testing.T) {
	noSessions(t)
	c := installCounters(t)
	a := newTestAgent(t, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // the monitor loop
		defer wg.Done()
		for i := 0; i < 50; i++ {
			c.idleCPUMinute()
			c.net.Add(1000)
			a.evaluateIdle(context.Background())
			a.setLastActivity(time.Now())
		}
		close(stop)
	}()
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() { // the HTTP scrape, and other off-loop readers
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.IsIdle()
				_, _ = a.GetCPUUsageContext(t.Context())
				_, _, _ = a.GetNetworkBytesMeasured()
				_, _, _ = a.GetDiskIOBytesMeasured()
				_ = a.GetLastActivityTime()
				_ = a.GetConfig()
			}
		}()
	}
	scrape(a)
	wg.Wait()
}
