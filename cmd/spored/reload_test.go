package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
)

// fakeDaemon is the far side of daemonReloader: a pid, a marker file, and a
// count of signals and polls. ackOnPoll > 0 makes the "daemon" publish its
// acknowledgement during that poll; 0 means it never does.
type fakeDaemon struct {
	t         *testing.T
	path      string
	pid       int
	signals   int
	polls     int
	ackOnPoll int
	ack       agent.ReloadMarker
}

func newFakeDaemon(t *testing.T, pid int, start *agent.ReloadMarker) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{t: t, path: filepath.Join(t.TempDir(), "reloaded.json"), pid: pid}
	if start != nil {
		if err := agent.WriteReloadMarker(d.path, *start); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func (d *fakeDaemon) reloader(attempts int) daemonReloader {
	return daemonReloader{
		markerPath: d.path,
		daemonPID:  func(context.Context) (int, error) { return d.pid, nil },
		signal: func(_ context.Context, pid int) error {
			if pid != d.pid {
				d.t.Errorf("signalled pid %d, want the daemon's %d", pid, d.pid)
			}
			d.signals++
			return nil
		},
		sleep: func(context.Context, time.Duration) {
			d.polls++
			if d.ackOnPoll > 0 && d.polls == d.ackOnPoll {
				if err := agent.WriteReloadMarker(d.path, d.ack); err != nil {
					d.t.Fatal(err)
				}
			}
		},
		poll:     time.Millisecond,
		attempts: attempts,
	}
}

// #1278's second gate: with no daemon there is nothing to reload, and `spored
// reload` (and so `spawn extend`) must fail instead of reporting a reload. The
// old command reloaded a throwaway agent of its own and exited 0 regardless.
func TestReload_NoDaemonFails(t *testing.T) {
	d := newFakeDaemon(t, 0, nil)
	var out bytes.Buffer
	err := reloadDaemon(context.Background(), d.reloader(5), &out)
	if !errors.Is(err, errDaemonNotRunning) {
		t.Fatalf("err = %v, want errDaemonNotRunning", err)
	}
	if d.signals != 0 || d.polls != 0 {
		t.Errorf("signals/polls = %d/%d with no daemon, want 0/0", d.signals, d.polls)
	}
	if strings.Contains(out.String(), "✓") {
		t.Errorf("reported success with no daemon:\n%s", out.String())
	}
}

func TestReload_PrintsTheDaemonsAcknowledgedConfig(t *testing.T) {
	d := newFakeDaemon(t, 42, &agent.ReloadMarker{PID: 42, Seq: 3, TTL: time.Hour})
	deadline := time.Date(2099, 1, 2, 3, 4, 0, 0, time.UTC)
	d.ack = agent.ReloadMarker{PID: 42, Seq: 4, TTL: 5 * time.Hour, TTLDeadline: deadline, CostLimit: 7}
	d.ackOnPoll = 3

	var out bytes.Buffer
	if err := reloadDaemon(context.Background(), d.reloader(10), &out); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if d.signals != 1 || d.polls != 3 {
		t.Errorf("signals/polls = %d/%d, want 1/3 (stop at the acknowledgement)", d.signals, d.polls)
	}
	for _, want := range []string{"pid 42", "2099-01-02 03:04 UTC", "$7.00"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks the daemon's %q:\n%s", want, out.String())
		}
	}
}

// A daemon that never acknowledges is a failure after the bounded wait; the
// bound is counted in polls, not time.
func TestReload_NoAcknowledgementFails(t *testing.T) {
	d := newFakeDaemon(t, 42, &agent.ReloadMarker{PID: 42, Seq: 0})
	_, err := d.reloader(4).reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("err = %v, want a did-not-acknowledge error", err)
	}
	if d.signals != 1 || d.polls != 4 {
		t.Errorf("signals/polls = %d/%d, want 1/4", d.signals, d.polls)
	}
}

// The daemon's refresh failed: it keeps the old config, so reload fails.
func TestReload_DaemonRefreshErrorFails(t *testing.T) {
	d := newFakeDaemon(t, 42, &agent.ReloadMarker{PID: 42, Seq: 1})
	d.ack = agent.ReloadMarker{PID: 42, Seq: 2, Error: "DescribeTags: 403"}
	d.ackOnPoll = 1
	_, err := d.reloader(4).reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the daemon's refresh error", err)
	}
}

// A marker from another process (an older daemon, a previous boot) proves
// nothing about the running one, which may have no reload handler: SIGHUP
// would stop it. reload must not signal it.
func TestReload_DoesNotSignalADaemonWithoutAMarker(t *testing.T) {
	for name, start := range map[string]*agent.ReloadMarker{
		"no marker":          nil,
		"other pid's marker": {PID: 41, Seq: 9},
	} {
		t.Run(name, func(t *testing.T) {
			d := newFakeDaemon(t, 42, start)
			_, err := d.reloader(4).reload(context.Background())
			if err == nil {
				t.Fatal("reload succeeded against a daemon that published no marker")
			}
			if d.signals != 0 {
				t.Errorf("signalled a daemon that does not take reload requests")
			}
		})
	}
}

// An acknowledgement must come from the running pid and be newer than the
// marker read before signalling.
func TestReload_IgnoresMarkersThatAreNotTheAcknowledgement(t *testing.T) {
	d := newFakeDaemon(t, 42, &agent.ReloadMarker{PID: 42, Seq: 5})
	d.ack = agent.ReloadMarker{PID: 43, Seq: 6}
	d.ackOnPoll = 1
	if _, err := d.reloader(3).reload(context.Background()); err == nil {
		t.Fatal("accepted another pid's marker as the acknowledgement")
	}
}

func TestForwardReloads(t *testing.T) {
	in := make(chan struct{}, 1)
	got := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); forwardReloads(ctx, in, func() { got <- struct{}{} }) }()
	in <- struct{}{}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("request not forwarded")
	}
	cancel()
	<-done
}
