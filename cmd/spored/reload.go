package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/provider"
)

// `spored reload` and `spored config set` run in their own short-lived process.
// Before #1278 they reloaded a throwaway agent of their own and reported ITS
// config, while the running daemon kept its old config until its periodic
// refresh. They now ask the daemon itself: SIGHUP on Unix, the SCM ParamChange
// control on Windows. The daemon refreshes on its monitor goroutine and
// publishes an agent.ReloadMarker, which is the only acknowledgement trusted.

// reloadWait is how long `spored reload` waits for the daemon to acknowledge:
// reloadPollAttempts polls reloadPollInterval apart (about 15 s). A refresh is
// one DescribeTags call.
const (
	reloadPollInterval = 250 * time.Millisecond
	reloadPollAttempts = 60
)

var errDaemonNotRunning = errors.New("spored is not running: no daemon to reload (its config is read from the tags when it starts)")

// daemonReloader asks the running daemon to reload and waits for its
// acknowledgement. The fields are the seams: production wiring is
// newDaemonReloader (platform-specific); tests substitute fakes and count
// polls instead of timing them.
type daemonReloader struct {
	markerPath string
	// daemonPID is the running service's main pid, 0 when it is not running.
	daemonPID func(ctx context.Context) (int, error)
	// signal delivers the reload request to pid.
	signal   func(ctx context.Context, pid int) error
	sleep    func(ctx context.Context, d time.Duration)
	poll     time.Duration
	attempts int
}

// reload signals the daemon and returns the marker it published for this
// request. Every path that does not end in a fresh, error-free marker from the
// running daemon's pid is an error.
func (r daemonReloader) reload(ctx context.Context) (agent.ReloadMarker, error) {
	if r.markerPath == "" {
		return agent.ReloadMarker{}, errors.New("spored reload is not supported on this platform (it signals the systemd or Windows service)")
	}
	pid, err := r.daemonPID(ctx)
	if err != nil {
		return agent.ReloadMarker{}, fmt.Errorf("find the running spored: %w", err)
	}
	if pid == 0 {
		return agent.ReloadMarker{}, errDaemonNotRunning
	}
	prior, err := agent.ReadReloadMarker(r.markerPath)
	if err != nil || prior.PID != pid {
		// An older spored has no reload handler, and SIGHUP's default action
		// would stop it; on Windows it rejects the control. Don't signal it.
		return agent.ReloadMarker{}, fmt.Errorf("the running spored (pid %d) has not published %s, so it does not take reload requests (it predates #1278, or is still starting); it applies tag changes at its next periodic refresh, within about 5 minutes", pid, r.markerPath)
	}
	if err := r.signal(ctx, pid); err != nil {
		return agent.ReloadMarker{}, fmt.Errorf("signal spored (pid %d): %w", pid, err)
	}
	for i := 0; i < r.attempts; i++ {
		r.sleep(ctx, r.poll)
		if ctx.Err() != nil {
			return agent.ReloadMarker{}, ctx.Err()
		}
		m, err := agent.ReadReloadMarker(r.markerPath)
		if err != nil || m.PID != pid || m.Seq <= prior.Seq {
			continue
		}
		if m.Error != "" {
			return m, fmt.Errorf("spored (pid %d) could not refresh its config and is still running on the previous one: %s", pid, m.Error)
		}
		return m, nil
	}
	return agent.ReloadMarker{}, fmt.Errorf("spored (pid %d) did not acknowledge the reload within %v; it applies tag changes at its next periodic refresh, within about 5 minutes", pid, time.Duration(r.attempts)*r.poll)
}

func sleepFor(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// reloadDaemon is `spored reload`: it reports the DAEMON's refreshed config,
// and fails when the daemon did not refresh.
func reloadDaemon(ctx context.Context, r daemonReloader, w io.Writer) error {
	_, _ = fmt.Fprintln(w, "Reloading configuration...")
	m, err := r.reload(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "✓ spored (pid %d) reloaded its configuration\n", m.PID)
	_, _ = fmt.Fprintln(w, "\nCurrent configuration:")
	_, _ = fmt.Fprintf(w, "  TTL:              %s\n", formatTTLLine(markerConfig(m), time.Now()))
	_, _ = fmt.Fprintf(w, "  Idle Timeout:     %v\n", m.IdleTimeout)
	_, _ = fmt.Fprintf(w, "  On Complete:      %s\n", m.OnComplete)
	_, _ = fmt.Fprintf(w, "  Hibernate:        %v\n", m.HibernateOnIdle)
	_, _ = fmt.Fprintf(w, "  Terminate idle:   %v\n", m.TerminateOnIdle)
	if m.CostLimit > 0 {
		_, _ = fmt.Fprintf(w, "  Cost limit:       $%.2f\n", m.CostLimit)
	}
	return nil
}

// markerConfig is the part of a daemon's config its marker carries, in the
// shape formatTTLLine reads.
func markerConfig(m agent.ReloadMarker) *provider.Config {
	return &provider.Config{TTL: m.TTL, TTLDeadline: m.TTLDeadline}
}

// forwardReloads hands each reload request the platform delivers (SIGHUP, the
// SCM ParamChange control) to the agent's monitor loop.
func forwardReloads(ctx context.Context, in <-chan struct{}, request func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-in:
			request()
		}
	}
}
