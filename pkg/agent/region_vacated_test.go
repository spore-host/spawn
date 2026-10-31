package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// The region-vacated check (#260) exists only for the region_vacated
// notification, and it must never bill the last instance in a region for idle
// time (#1456). These gates cover every path that reaches terminate: TTL
// expiry, the cost limit, an idle terminate and on-complete terminate. A Spot
// interruption is terminated by EC2, not spored, and stop/hibernate do not run
// the check.

// vacatedPath drives one termination path to completion on a.
type vacatedPath struct {
	name string
	cfg  func(t *testing.T) *provider.Config
	run  func(a *Agent)
}

func vacatedPaths() []vacatedPath {
	return []vacatedPath{
		{"ttl", func(*testing.T) *provider.Config {
			return &provider.Config{TTL: time.Hour}
		}, func(a *Agent) { a.checkAndAct(context.Background()) }},
		{"cost-limit", func(*testing.T) *provider.Config {
			return &provider.Config{PricePerHour: 10, CostLimit: 5}
		}, func(a *Agent) { a.checkAndAct(context.Background()) }},
		{"idle-terminate", func(*testing.T) *provider.Config {
			return &provider.Config{IdleTimeout: 30 * time.Minute, IdleCPUPercent: 5, TerminateOnIdle: true}
		}, func(a *Agent) {
			a.lastActivityTime = time.Now().Add(-time.Hour)
			a.idleTimeoutReached(context.Background(), time.Now())
		}},
		{"on-complete-terminate", func(t *testing.T) *provider.Config {
			f := filepath.Join(t.TempDir(), "SPAWN_COMPLETE")
			if err := os.WriteFile(f, []byte(`{"status":"success","exit_code":0}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return &provider.Config{OnComplete: "terminate", CompletionFile: f, CompletionDelay: 30 * time.Second}
		}, func(a *Agent) { a.checkAndAct(context.Background()) }},
	}
}

// vacatedAgent builds a hermetic agent for path, past its TTL/cost/idle
// trigger, with the waits replaced by counters.
func vacatedAgent(t *testing.T, path vacatedPath, sp *stubProvider) (*Agent, *[]time.Duration) {
	t.Helper()
	cfg := path.cfg(t)
	sp.identity = &provider.Identity{InstanceID: "i-vacated", Region: "us-east-1", Provider: "stub"}
	sp.config = cfg
	var graces []time.Duration
	a := &Agent{
		provider:              sp,
		identity:              sp.identity,
		config:                cfg,
		startTime:             time.Now().Add(-2 * time.Hour),
		lastActivityTime:      time.Now(),
		lastSessionTagWrite:   time.Now().Add(time.Hour),
		lastComputeTagWrite:   time.Now().Add(time.Hour),
		lastHeartbeatTagWrite: time.Now().Add(time.Hour),
		warnDelay:             time.Millisecond,
		graceSleep:            func(d time.Duration) { graces = append(graces, d) },
		tagger:                &fakeTagPutter{},
	}
	return a, &graces
}

// No notifier: the check must not run at all, so it costs zero wall-clock time
// before Terminate. Not one count, on any path, even in an empty region (#1456).
func TestRegionVacatedCheckCostsNothingWithoutNotifier(t *testing.T) {
	for _, path := range vacatedPaths() {
		t.Run(path.name, func(t *testing.T) {
			sp := &stubProvider{otherInstances: 0, reportVacated: true}
			a, _ := vacatedAgent(t, path, sp)
			path.run(a)
			if !sp.terminated {
				t.Fatalf("%s did not terminate", path.name)
			}
			if sp.countCalls != 0 {
				t.Errorf("%s: %d region counts with no notifier configured, want 0: the check only feeds region_vacated", path.name, sp.countCalls)
			}
		})
	}
}

// With a notifier, region_vacated still fires when the region is empty at both
// counts, before Terminate, and is suppressed when an instance appears during
// the settle window (the rapid-relaunch false alarm #260 guards against), when
// others remain from the start, or when the count is unknown.
func TestRegionVacatedNotificationWithNotifier(t *testing.T) {
	cases := []struct {
		name       string
		sp         func() *stubProvider
		wantNotify bool
		wantCounts int
	}{
		{"vacated", func() *stubProvider { return &stubProvider{otherInstances: 0, reportVacated: true} }, true, 2},
		{"relaunch during settle", func() *stubProvider { return &stubProvider{countSeq: []int{0, 1}} }, false, 2},
		{"others remain", func() *stubProvider { return &stubProvider{otherInstances: 2} }, false, 1},
		{"unknown count", func() *stubProvider { return &stubProvider{} }, false, 1},
	}
	for _, path := range vacatedPaths() {
		for _, tc := range cases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				sp := tc.sp()
				a, _ := vacatedAgent(t, path, sp)
				ev := &recordedEvents{}
				a.notifier = &Notifier{record: ev.record}
				notifiedAtTerminate := -1
				sp.onTerminate = func() { notifiedAtTerminate = ev.count("region_vacated") }

				path.run(a)

				if !sp.terminated {
					t.Fatalf("did not terminate")
				}
				want := 0
				if tc.wantNotify {
					want = 1
				}
				if n := ev.count("region_vacated"); n != want {
					t.Errorf("region_vacated sent %d times, want %d; events=%v", n, want, ev.events)
				}
				if notifiedAtTerminate != want {
					t.Errorf("region_vacated sent %d times when Terminate was called, want %d: the instance dies at Terminate", notifiedAtTerminate, want)
				}
				if sp.countCalls != tc.wantCounts {
					t.Errorf("%d region counts, want %d", sp.countCalls, tc.wantCounts)
				}
			})
		}
	}
}

// The settle window is the waits that already precede Terminate, so it must
// be open across them: on-complete terminate starts it before the
// --completion-delay grace period, not after (#1456).
func TestRegionVacatedSettleOverlapsCompletionDelay(t *testing.T) {
	path := vacatedPaths()[3]
	if path.name != "on-complete-terminate" {
		t.Fatalf("vacatedPaths()[3] = %s", path.name)
	}
	sp := &stubProvider{otherInstances: 0, reportVacated: true}
	a, graces := vacatedAgent(t, path, sp)
	ev := &recordedEvents{}
	a.notifier = &Notifier{record: ev.record}
	probeDuringGrace := 0
	a.graceSleep = func(d time.Duration) {
		*graces = append(*graces, d)
		if a.vacatedProbe != nil {
			probeDuringGrace++
		}
	}

	path.run(a)

	if len(*graces) != 1 {
		t.Fatalf("%d grace waits, want 1", len(*graces))
	}
	if probeDuringGrace != 1 {
		t.Errorf("the region-vacated check was not running during the --completion-delay wait: its settle must overlap it")
	}
	if ev.count("region_vacated") != 1 || !sp.terminated {
		t.Errorf("region_vacated=%d terminated=%v, want 1/true", ev.count("region_vacated"), sp.terminated)
	}
}

// Terminate is never ordered after a blocking settle or a slow count: a
// DescribeInstances that has not answered by the end of the warning wait is
// abandoned (the context is cancelled, the verdict is "not vacated") rather
// than waited for. The stub's count blocks until its context is cancelled, so
// a version that waits on the check before Terminate never gets there.
func TestRegionVacatedCheckNeverHoldsTerminate(t *testing.T) {
	for _, call := range []int{1, 2} {
		for _, path := range vacatedPaths() {
			t.Run(fmt.Sprintf("%s/count%d-hangs", path.name, call), func(t *testing.T) {
				sp := &stubProvider{otherInstances: 0, reportVacated: true}
				sp.countHook = func(ctx context.Context, n int) (int, bool) {
					if n != call {
						return 0, false
					}
					<-ctx.Done()
					return -1, true // what DescribeInstances' error maps to
				}
				a, _ := vacatedAgent(t, path, sp)
				ev := &recordedEvents{}
				a.notifier = &Notifier{record: ev.record}

				path.run(a)

				if !sp.terminated {
					t.Fatalf("did not terminate")
				}
				if n := ev.count("region_vacated"); n != 0 {
					t.Errorf("region_vacated sent %d times on an unanswered count, want 0 (never cry wolf)", n)
				}
			})
		}
	}
}
