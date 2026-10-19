package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// completionAgent returns an agent whose completion file exists, whose Stop and
// Hibernate fail with stopErr, with notifications recorded and the completion
// webhook pointed at a counting server.
func completionAgent(t *testing.T, onComplete string, delay time.Duration, stopErr error) (*Agent, *stubProvider, *recordedEvents, *int32) {
	t.Helper()
	f := t.TempDir() + "/SPAWN_COMPLETE"
	if err := os.WriteFile(f, []byte(`{"status":"success","exit_code":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var hooks int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hooks, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a := newTestAgent(t, &provider.Config{
		OnComplete:           onComplete,
		CompletionFile:       f,
		CompletionDelay:      delay,
		CompletionWebhookURL: srv.URL,
		WebhookTimeout:       2 * time.Second,
	})
	sp := a.provider.(*stubProvider)
	sp.stopErr, sp.hibernateErr = stopErr, stopErr
	ev := &recordedEvents{}
	a.notifier = &Notifier{record: ev.record}
	a.warnDelay = time.Millisecond
	return a, sp, ev, &hooks
}

// A failed --on-complete stop/hibernate must not repeat the completion
// announcement on every tick (#978): one `completion` notification, one webhook
// POST, one grace period, and the action retried on the backoff, not each tick.
func TestFailedCompletionStopIsAnnouncedOnceAndBacksOff(t *testing.T) {
	for _, action := range []string{"stop", "hibernate"} {
		t.Run(action, func(t *testing.T) {
			const grace = 300 * time.Millisecond
			a, sp, ev, hooks := completionAgent(t, action, grace, errOneTimeSpot)
			calls := func() int { return sp.stopCalls + sp.hibernateCalls }
			var waits []time.Duration
			a.graceSleep = func(d time.Duration) { waits = append(waits, d) }

			for i := 0; i < 5; i++ {
				a.checkCompletion(context.Background())
			}

			if n := ev.count("completion"); n != 1 {
				t.Errorf("completion notification sent %d times over 5 ticks, want 1", n)
			}
			if n := atomic.LoadInt32(hooks); n != 1 {
				t.Errorf("completion webhook POSTed %d times over 5 ticks, want 1", n)
			}
			if len(waits) != 1 || waits[0] != grace {
				t.Errorf("grace-period waits over 5 ticks = %v, want exactly one of %v: the grace sleep was repeated", waits, grace)
			}
			if n := calls(); n != 1 {
				t.Errorf("%d %s attempts in 5 ticks inside the backoff, want 1", n, action)
			}
			if n := ev.count("completion_stop_failed"); n != 1 {
				t.Errorf("completion_stop_failed sent %d times, want 1", n)
			}

			// The backoff runs out: the action is retried, without another
			// announcement or another failure notification.
			a.stopRetry.retryAt = time.Now().Add(-time.Second)
			a.checkCompletion(context.Background())
			if n := calls(); n != 2 {
				t.Errorf("%d %s attempts after the backoff ran out, want 2", n, action)
			}
			if c, h, f := ev.count("completion"), atomic.LoadInt32(hooks), ev.count("completion_stop_failed"); c != 1 || h != 1 || f != 1 {
				t.Errorf("after the retry: completion=%d webhook=%d completion_stop_failed=%d, want 1 each", c, h, f)
			}
		})
	}
}

// While a failed on-complete stop backs off, the TTL must still be enforced:
// it is the backstop for an instance that cannot be stopped (#978).
func TestTTLStillEnforcedWhileCompletionStopBacksOff(t *testing.T) {
	a, sp, _, _ := completionAgent(t, "stop", 0, errOneTimeSpot)
	a.lastSessionTagWrite = time.Now().Add(time.Hour)
	a.lastComputeTagWrite = time.Now().Add(time.Hour)
	a.lastHeartbeatTagWrite = time.Now().Add(time.Hour)

	a.checkAndAct(context.Background()) // first attempt fails, backoff starts
	if sp.stopCalls != 1 || sp.terminated {
		t.Fatalf("first tick: stopCalls=%d terminated=%v, want 1 attempt and no terminate", sp.stopCalls, sp.terminated)
	}

	cfg := *a.config
	cfg.TTLDeadline = time.Now().Add(-time.Minute)
	a.config = &cfg
	sp.config = &cfg
	a.checkAndAct(context.Background())
	if !sp.terminated {
		t.Error("TTL expired while the on-complete stop was backing off, and nothing terminated")
	}
}

// The idle and on-complete triggers share one backoff: EC2 refusing to stop
// this instance does not depend on why spored asked, so a failed on-complete
// stop must also hold off an idle stop, and the failure is announced once.
func TestIdleStopSharesTheCompletionStopBackoff(t *testing.T) {
	a, sp, ev, _ := completionAgent(t, "stop", 0, errOneTimeSpot)
	a.checkCompletion(context.Background())

	cfg := *a.config
	cfg.IdleTimeout = 30 * time.Minute
	a.config = &cfg
	a.lastActivityTime = time.Now().Add(-time.Hour)
	a.idleTimeoutReached(context.Background(), time.Now())

	if sp.stopCalls != 1 {
		t.Errorf("stopCalls = %d, want 1: the idle stop ignored the on-complete backoff", sp.stopCalls)
	}
	if n := ev.count("idle_stop_failed") + ev.count("completion_stop_failed"); n != 1 {
		t.Errorf("%d stop-failed notifications, want 1", n)
	}
}

// A successful on-complete stop is announced once and clears the backoff.
func TestSuccessfulCompletionStop(t *testing.T) {
	a, sp, ev, hooks := completionAgent(t, "stop", 0, nil)
	if !a.checkCompletion(context.Background()) {
		t.Fatal("checkCompletion = false after an accepted stop, want true")
	}
	if !sp.stopped || ev.count("completion") != 1 || atomic.LoadInt32(hooks) != 1 || ev.count("completion_stop_failed") != 0 {
		t.Errorf("stopped=%v events=%v webhook=%d", sp.stopped, ev.events, atomic.LoadInt32(hooks))
	}
	if a.stopRetry != (stopRetry{}) {
		t.Errorf("backoff state after success = %+v, want zero", a.stopRetry)
	}
}
