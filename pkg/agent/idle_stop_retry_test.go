package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/provider"
)

// What EC2 returns for StopInstances on a one-time Spot instance.
var errOneTimeSpot = errors.New("failed to stop: api error UnsupportedOperation: " +
	"You can't stop the Spot Instance 'i-test123' because it is associated with a one-time Spot Instance request")

type recordedEvents struct {
	mu     sync.Mutex
	events []string
}

func (r *recordedEvents) record(event, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordedEvents) count(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == event {
			n++
		}
	}
	return n
}

// idleAgent returns an agent past its idle timeout whose Stop fails, with
// notifications recorded.
func idleAgent(t *testing.T, cfg *provider.Config, stopErr error) (*Agent, *stubProvider, *recordedEvents) {
	t.Helper()
	a := newTestAgent(t, cfg)
	sp := a.provider.(*stubProvider)
	sp.stopErr, sp.hibernateErr = stopErr, stopErr
	ev := &recordedEvents{}
	a.notifier = &Notifier{record: ev.record}
	a.warnDelay = time.Millisecond
	a.lastActivityTime = time.Now().Add(-2 * a.config.IdleTimeout)
	return a, sp, ev
}

// A failed idle stop must not be repeated on every one-minute tick, and must
// never be announced as `idle_stopped` (#948).
func TestFailedIdleStopBacksOffAndIsNotAnnouncedAsStopped(t *testing.T) {
	for _, hibernate := range []bool{false, true} {
		name := map[bool]string{false: "stop", true: "hibernate"}[hibernate]
		t.Run(name, func(t *testing.T) {
			a, sp, ev := idleAgent(t, &provider.Config{
				IdleTimeout: 30 * time.Minute, IdleCPUPercent: 5, HibernateOnIdle: hibernate,
			}, errOneTimeSpot)
			calls := func() int { return sp.stopCalls + sp.hibernateCalls }

			start := time.Now()
			// Ten one-minute ticks, all idle and past the timeout.
			for i := 0; i < 10; i++ {
				a.idleTimeoutReached(context.Background(), start.Add(time.Duration(i)*time.Minute))
			}
			// First attempt at t=0, then backoff 5m: attempt at t=5m, then 10m.
			if n := calls(); n != 2 {
				t.Errorf("%d %s attempts in 10 idle minutes, want 2 (t=0 and after the 5-minute backoff)", n, name)
			}
			for _, e := range []string{"idle_stopped", "idle_hibernated"} {
				if n := ev.count(e); n != 0 {
					t.Errorf("%q sent %d times although nothing stopped", e, n)
				}
			}
			if n := ev.count("idle_stop_failed"); n != 1 {
				t.Errorf("idle_stop_failed sent %d times over two failures, want once", n)
			}
		})
	}
}

// Success is announced, once, after the provider accepted the request.
func TestSuccessfulIdleStopIsAnnounced(t *testing.T) {
	a, sp, ev := idleAgent(t, nil, nil)
	a.idleTimeoutReached(context.Background(), time.Now())
	if !sp.stopped || ev.count("idle_stopped") != 1 || ev.count("idle_stop_failed") != 0 {
		t.Errorf("stopped=%v events=%v, want stopped and one idle_stopped", sp.stopped, ev.events)
	}
}

// After a failed attempt the backoff wait grows and is capped.
func TestIdleActionRetryDelay(t *testing.T) {
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := stopRetryDelay(i + 1); got != w {
			t.Errorf("delay after failure %d = %v, want %v", i+1, got, w)
		}
	}
	if d := stopRetryDelay(1000); d != stopRetryMax {
		t.Errorf("delay after many failures = %v, want the cap %v", d, stopRetryMax)
	}
}

// The DNS name is removed before the stop, because a hibernating guest gets no
// later chance. When the stop fails the instance is still running, so the name
// must be put back (#948).
func TestFailedStopRestoresDNS(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hibernate bool
		stopErr   error
		want      []string
	}{
		{"stop fails: record deleted, then restored", false, errOneTimeSpot, []string{"DELETE", "UPSERT"}},
		{"hibernate fails: record deleted, then restored", true, errOneTimeSpot, []string{"DELETE", "UPSERT"}},
		{"stop succeeds: record deleted only", false, nil, []string{"DELETE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var actions []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req dns.DNSUpdateRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				mu.Lock()
				actions = append(actions, req.Action)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(dns.DNSUpdateResponse{Success: true})
			}))
			t.Cleanup(srv.Close)
			t.Setenv("SPORE_DNS_SIGV4", "0")
			client, err := dns.NewClient(context.Background(), "spore.host", srv.URL)
			if err != nil {
				t.Fatal(err)
			}

			a, _, _ := idleAgent(t, &provider.Config{
				IdleTimeout: 30 * time.Minute, IdleCPUPercent: 5, DNSName: "box",
				HibernateOnIdle: tc.hibernate,
			}, tc.stopErr)
			a.dnsClient = client
			a.nextDNSRefresh = time.Now().Add(time.Hour) // the lease is not due

			a.idleTimeoutReached(context.Background(), time.Now())

			mu.Lock()
			defer mu.Unlock()
			if len(actions) != len(tc.want) {
				t.Fatalf("DNS actions = %v, want %v", actions, tc.want)
			}
			for i := range tc.want {
				if actions[i] != tc.want[i] {
					t.Fatalf("DNS actions = %v, want %v", actions, tc.want)
				}
			}
		})
	}
}
