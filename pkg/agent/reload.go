package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// ReloadMarker is what the running daemon publishes when it starts taking
// reload requests and after each one it handles (#1278). `spored reload` runs
// in a separate process, so this file is how it learns that the DAEMON, not a
// throwaway agent of its own, refreshed its config, and what that config now
// is.
type ReloadMarker struct {
	// PID is the daemon that wrote the marker. `spored reload` signals a
	// process only when this matches the service's main pid.
	PID int `json:"pid"`
	// Seq is 0 when the daemon starts monitoring and n after the nth reload
	// request it handled. It only grows within one PID.
	Seq uint64    `json:"seq"`
	At  time.Time `json:"at"`
	// Error is the refresh failure, "" when the daemon adopted fresh config.
	Error string `json:"error,omitempty"`

	TTL             time.Duration `json:"ttl"`
	TTLDeadline     time.Time     `json:"ttl_deadline"`
	IdleTimeout     time.Duration `json:"idle_timeout"`
	OnComplete      string        `json:"on_complete,omitempty"`
	HibernateOnIdle bool          `json:"hibernate_on_idle"`
	TerminateOnIdle bool          `json:"terminate_on_idle"`
	CostLimit       float64       `json:"cost_limit,omitempty"`
}

// ReloadMarkerPath is where the daemon publishes its ReloadMarker, or "" where
// reload is unsupported.
func ReloadMarkerPath() string { return sysReloadMarkerPath() }

// WriteReloadMarker atomically writes m to path, world-readable like the
// compute clock: it holds no secret.
func WriteReloadMarker(path string, m ReloadMarker) error {
	if path == "" {
		return errors.New("no reload marker path on this platform")
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadReloadMarker reads a marker written by WriteReloadMarker.
func ReadReloadMarker(path string) (ReloadMarker, error) {
	var m ReloadMarker
	if path == "" {
		return m, errors.New("no reload marker path on this platform")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// reloadRequests returns the channel RequestReload feeds and Monitor drains.
func (a *Agent) reloadRequests() chan struct{} {
	a.reloadOnce.Do(func() { a.reloadReqs = make(chan struct{}, 1) })
	return a.reloadReqs
}

// RequestReload asks the monitor loop to re-read the tags now rather than at
// its next periodic refresh (#1278). It never blocks: a request already
// pending covers this one, since the refresh it triggers has not read the tags
// yet. Safe from any goroutine, including a signal handler's.
func (a *Agent) RequestReload() {
	select {
	case a.reloadRequests() <- struct{}{}:
	default:
	}
}

// handleReloadRequest runs on the monitor goroutine: it refreshes the config
// through Reload (which swaps it under configMu) and publishes the result.
func (a *Agent) handleReloadRequest(ctx context.Context) {
	err := a.Reload(ctx)
	if err != nil {
		log.Printf("Warning: reload request failed: %v", err)
	}
	a.reloadSeq++
	a.publishReloadMarker(err)
	if a.reloadHandled != nil {
		a.reloadHandled()
	}
}

// publishReloadMarker writes the current config summary and reloadErr to
// reloadMarkerPath. A no-op when there is no path.
func (a *Agent) publishReloadMarker(reloadErr error) {
	if a.reloadMarkerPath == "" {
		return
	}
	c := a.cfg()
	m := ReloadMarker{PID: os.Getpid(), Seq: a.reloadSeq, At: time.Now().UTC()}
	if reloadErr != nil {
		m.Error = reloadErr.Error()
	}
	if c != nil {
		m.TTL, m.TTLDeadline, m.IdleTimeout = c.TTL, c.TTLDeadline, c.IdleTimeout
		m.OnComplete, m.HibernateOnIdle, m.TerminateOnIdle = c.OnComplete, c.HibernateOnIdle, c.TerminateOnIdle
		m.CostLimit = c.CostLimit
	}
	if err := WriteReloadMarker(a.reloadMarkerPath, m); err != nil {
		log.Printf("Warning: could not publish reload marker to %s: %v", a.reloadMarkerPath, err)
	}
}
