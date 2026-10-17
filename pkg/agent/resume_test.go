package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/pluginruntime"
	"github.com/spore-host/spawn/pkg/provider"
)

// fakeSuspend stands in for the kernel's suspended-time reading.
type fakeSuspend struct {
	mu sync.Mutex
	d  time.Duration
}

func (f *fakeSuspend) read() (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.d, true
}

func (f *fakeSuspend) hibernate(d time.Duration) {
	f.mu.Lock()
	f.d += d
	f.mu.Unlock()
}

func useFakeSuspend(t *testing.T) *fakeSuspend {
	t.Helper()
	f := &fakeSuspend{d: 3 * time.Second}
	prev := readSuspendedTime
	readSuspendedTime = f.read
	t.Cleanup(func() { readSuspendedTime = prev })
	return f
}

// quietCounters makes every counter read as a quiet machine: CPU fully idle,
// no network or disk traffic. The idle decision needs two samples of each.
func quietCounters(t *testing.T) {
	t.Helper()
	noSessions(t) // a quiet machine has no active sessions either, on every OS
	var mu sync.Mutex
	var jiffies int64
	cpu, net, disk := readCPUTimes, readNetworkBytes, readDiskIOBytes
	readCPUTimes = func() (int64, int64, error) {
		mu.Lock()
		defer mu.Unlock()
		jiffies += 100
		return jiffies, jiffies, nil
	}
	readNetworkBytes = func() (int64, int64) { return 1000, 1000 }
	readDiskIOBytes = func() int64 { return 5000 }
	t.Cleanup(func() { readCPUTimes, readNetworkBytes, readDiskIOBytes = cpu, net, disk })
}

// Both signals, and the near-misses each must not take for a resume.
func TestResumedBetween(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tick := func(wall time.Duration, mono time.Duration, susp time.Duration, ok bool) tickReading {
		return tickReading{wall: t0.Add(wall), mono: mono, suspended: susp, suspendedOK: ok}
	}
	base := tick(0, time.Hour, 10*time.Second, true)
	for _, tc := range []struct {
		name string
		now  tickReading
		want bool
	}{
		{"an ordinary tick", tick(time.Minute, time.Hour+time.Minute, 10*time.Second, true), false},
		{"hibernated two hours, kernel accounted it", tick(2*time.Hour+time.Minute, time.Hour+time.Minute, 2*time.Hour+10*time.Second, true), true},
		{"kernel suspended time moved by clock-read noise", tick(time.Minute, time.Hour+time.Minute, 10*time.Second+time.Millisecond, true), false},
		{"hibernated, kernel did not account it: wall ran ahead", tick(2*time.Hour, time.Hour+time.Minute, 10*time.Second, true), true},
		{"hibernated, no kernel reading: wall ran ahead", tick(2*time.Hour, time.Hour+time.Minute, 0, false), true},
		{"an NTP step of 30s is not a resume", tick(90*time.Second, time.Hour+time.Minute, 10*time.Second, true), false},
		{"a wall clock stepped back is not a resume", tick(-time.Hour, time.Hour+time.Minute, 10*time.Second, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _, _ := resumedBetween(base, tc.now); got != tc.want {
				t.Errorf("resumedBetween = %v, want %v", got, tc.want)
			}
		})
	}
}

// resumeAgent is an agent with DNS, a plugin runtime and a public-IP lookup,
// as a hibernate would leave it: everything Cleanup removed, and every
// once-per-boot flag set.
type resumeFixture struct {
	a       *Agent
	sp      *stubProvider
	ev      *recordedEvents
	susp    *fakeSuspend
	store   *plugin.DiskStateStore
	mu      sync.Mutex
	dnsReqs []dns.DNSUpdateRequest
	dnsURL  string
}

func (f *resumeFixture) dns() []dns.DNSUpdateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dns.DNSUpdateRequest(nil), f.dnsReqs...)
}

func newResumeFixture(t *testing.T, cfg *provider.Config) *resumeFixture {
	t.Helper()
	f := &resumeFixture{susp: useFakeSuspend(t)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req dns.DNSUpdateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.dnsReqs = append(f.dnsReqs, req)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(dns.DNSUpdateResponse{Success: true})
	}))
	t.Cleanup(srv.Close)
	f.dnsURL = srv.URL
	t.Setenv("SPORE_DNS_SIGV4", "0")
	client, err := dns.NewClient(context.Background(), "spore.host", srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	cfg.DNSName = "box"
	f.a = newTestAgent(t, cfg)
	f.sp = f.a.provider.(*stubProvider)
	f.ev = &recordedEvents{}
	f.a.notifier = &Notifier{record: f.ev.record}
	f.a.warnDelay = time.Millisecond
	f.a.dnsClient = client
	f.a.nextDNSRefresh = time.Now().Add(time.Hour)
	f.a.lookupPublicIP = func(context.Context) (string, error) { return "5.6.7.8", nil }
	// Keep checkAndAct hermetic: skip the real-AWS tag writes.
	f.a.lastSessionTagWrite = time.Now().Add(time.Hour)
	f.a.lastComputeTagWrite = time.Now().Add(time.Hour)
	f.a.lastHeartbeatTagWrite = time.Now().Add(time.Hour)

	f.store = plugin.NewDiskStateStore(t.TempDir())
	if err := f.store.Save(&plugin.PluginState{Name: "globus", Version: "1.0.0", Status: plugin.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	f.a.pluginRuntime = pluginruntime.NewRuntimeWithStore(f.store, f.a.identity, "")
	return f
}

// After a resume, spored must reset what a fresh boot would (#987): the
// pre-stop hook and task record are re-armed, DNS is registered again with the
// CURRENT public IP, the idle timer starts again, and the plugins Cleanup
// stopped are restored.
func TestResumeResetsPerBootState(t *testing.T) {
	f := newResumeFixture(t, &provider.Config{
		IdleTimeout: 30 * time.Minute, IdleCPUPercent: 5, HibernateOnIdle: true,
	})
	a := f.a
	a.checkResume(context.Background()) // baseline

	a.lastActivityTime = time.Now().Add(-time.Hour)
	a.idleTimeoutReached(context.Background(), time.Now()) // hibernates: Cleanup ran
	if !f.sp.hibernated {
		t.Fatal("setup: the idle hibernate did not happen")
	}
	a.preStopDone, a.taskFlushDone, a.idleWarned = true, true, true
	a.netSampled, a.diskSampled = true, true
	a.prevCPUTotal = 12345
	a.stopRetry = stopRetry{failures: 3, retryAt: time.Now().Add(time.Hour)}
	if st, _ := f.store.Load("globus"); st.Status != plugin.StatusStopped {
		t.Fatalf("setup: plugin %q after the hibernate's Cleanup, want stopped", st.Status)
	}
	before := len(f.dns())

	f.susp.hibernate(8 * time.Hour)
	if !a.checkResume(context.Background()) {
		t.Fatal("an 8-hour jump in the kernel's suspended time was not taken for a resume")
	}

	if a.preStopDone || a.taskFlushDone {
		t.Errorf("preStopDone=%v taskFlushDone=%v after a resume, want both re-armed", a.preStopDone, a.taskFlushDone)
	}
	if idle := time.Since(a.lastActivity()); idle > time.Minute {
		t.Errorf("idle for %v after a resume, want the timer restarted", idle)
	}
	if a.idleWarned {
		t.Error("idleWarned still set after a resume")
	}
	if a.stopRetry != (stopRetry{}) {
		t.Errorf("stop backoff %+v survived the resume", a.stopRetry)
	}
	if a.netSampled || a.diskSampled || a.prevCPUTotal != 0 {
		t.Errorf("counter baselines from before the hibernation kept: net=%v disk=%v cpu=%d",
			a.netSampled, a.diskSampled, a.prevCPUTotal)
	}
	reqs := f.dns()[before:]
	if len(reqs) != 1 || reqs[0].Action != "UPSERT" || reqs[0].IPAddress != "5.6.7.8" {
		t.Errorf("DNS after the resume = %+v, want one UPSERT with the new IP 5.6.7.8", reqs)
	}
	if st, _ := f.store.Load("globus"); st.Status != plugin.StatusRunning {
		t.Errorf("plugin %q after the resume, want running", st.Status)
	}
}

// The issue's scenario end to end, through the monitor tick: hibernate on
// idle, resume, and the next tick must not hibernate again; the next real
// idle hibernate must run the pre-stop hook again and delete the record at the
// new IP.
func TestResumedInstanceIsNotReHibernatedAndRunsPreStopAgain(t *testing.T) {
	quietCounters(t)
	hookLog := filepath.Join(t.TempDir(), "prestop.log")
	f := newResumeFixture(t, &provider.Config{
		IdleTimeout: 30 * time.Minute, IdleCPUPercent: 5, HibernateOnIdle: true,
		PreStop: "echo ran >> " + hookLog,
	})
	a := f.a
	a.checkResume(context.Background())
	a.evaluateIdle() // the counters' first samples, so the next tick reads idle
	if s := a.evaluateIdle(); !s.idle {
		t.Fatalf("setup: the quiet machine does not read idle: %s", s.reason)
	}
	a.lastActivityTime = time.Now().Add(-time.Hour)
	a.idleTimeoutReached(context.Background(), time.Now())
	if f.sp.hibernateCalls != 1 {
		t.Fatalf("setup: %d hibernate calls, want 1", f.sp.hibernateCalls)
	}

	f.susp.hibernate(2 * time.Hour)
	a.checkAndAct(context.Background())
	if f.sp.hibernateCalls != 1 {
		t.Errorf("the first tick after the resume hibernated again (%d calls)", f.sp.hibernateCalls)
	}

	// Idle for real now: the hook runs a second time and the record deleted is
	// the one at the new IP.
	before := len(f.dns())
	a.lastActivityTime = time.Now().Add(-time.Hour)
	a.idleTimeoutReached(context.Background(), time.Now())
	if f.sp.hibernateCalls != 2 {
		t.Fatalf("%d hibernate calls, want 2", f.sp.hibernateCalls)
	}
	b, _ := os.ReadFile(hookLog)
	// The hook runs under powershell on Windows, whose `>>` appends UTF-16;
	// dropping the NULs reads either encoding of an ASCII "ran".
	if n := strings.Count(strings.ReplaceAll(string(b), "\x00", ""), "ran"); n != 2 {
		t.Errorf("pre-stop hook ran %d times over two hibernates, want 2", n)
	}
	var deleted []string
	for _, r := range f.dns()[before:] {
		if r.Action == "DELETE" {
			deleted = append(deleted, r.IPAddress)
		}
	}
	if len(deleted) != 1 || deleted[0] != "5.6.7.8" {
		t.Errorf("DELETEs at the second hibernate = %v, want one at the new IP 5.6.7.8", deleted)
	}
}

// An on-complete hibernate leaves the completion file on disk. After the
// resume that file is spent; a completion written again is acted on.
func TestResumeAfterOnCompleteHibernate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "SPAWN_COMPLETE")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newResumeFixture(t, &provider.Config{OnComplete: "hibernate", CompletionFile: file})
	a := f.a
	a.checkResume(context.Background())
	if !a.checkCompletion(context.Background()) || f.sp.hibernateCalls != 1 {
		t.Fatalf("setup: on-complete hibernate calls = %d, want 1", f.sp.hibernateCalls)
	}

	f.susp.hibernate(time.Hour)
	a.checkResume(context.Background())
	if a.checkCompletion(context.Background()) || f.sp.hibernateCalls != 1 {
		t.Errorf("the stale completion file hibernated the resumed instance again (%d calls)", f.sp.hibernateCalls)
	}

	// A new job finishes: the file is written again.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	if !a.checkCompletion(context.Background()) || f.sp.hibernateCalls != 2 {
		t.Errorf("a new completion after the resume was not acted on (%d calls)", f.sp.hibernateCalls)
	}
	if n := f.ev.count("completion"); n != 2 {
		t.Errorf("completion announced %d times over two completions, want 2", n)
	}
}

// Without a suspend, ticks change nothing: no DNS write, no reset.
func TestNoResumeWithoutASuspend(t *testing.T) {
	f := newResumeFixture(t, &provider.Config{IdleTimeout: 30 * time.Minute, IdleCPUPercent: 5})
	a := f.a
	a.checkResume(context.Background())
	a.preStopDone = true
	for i := 0; i < 3; i++ {
		if a.checkResume(context.Background()) {
			t.Fatal("a tick without a suspend was taken for a resume")
		}
	}
	if !a.preStopDone || len(f.dns()) != 0 {
		t.Errorf("preStopDone=%v dns=%v, want untouched", a.preStopDone, f.dns())
	}
}
