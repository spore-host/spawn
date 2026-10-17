package agent

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

// IdleState is the daemon's last idle evaluation, published for `spored
// status` (#1090).
//
// Since #952 only the daemon's monitor loop samples the idle counters. `spored
// status` is a separate one-shot process with an Agent of its own, so a reading
// it takes itself has a single sample and no interval to measure over: every
// status screen, at every age, said CPU, network and disk were "unknown (single
// sample)", and why an instance was not idle-stopping was only in
// /var/log/spored.log. The daemon therefore publishes each tick's evaluation
// here and status reads it, as it already does for the compute clock (#896).
type IdleState struct {
	Taken  time.Time `json:"taken"`
	Idle   bool      `json:"idle"`
	Reason string    `json:"reason"` // the "Not idle: ..." / "System is idle ..." log line

	CPUPercent      float64 `json:"cpu_percent"`
	CPUMeasured     bool    `json:"cpu_measured"`
	NetworkBytes    int64   `json:"network_bytes"`
	NetworkMeasured bool    `json:"network_measured"`
	NetworkWhy      string  `json:"network_why,omitempty"`
	DiskBytes       int64   `json:"disk_bytes"`
	DiskMeasured    bool    `json:"disk_measured"`
	DiskWhy         string  `json:"disk_why,omitempty"`

	// LastActivity is when the idle check last saw activity, so a reader can
	// say how long the instance has been continuously idle.
	LastActivity time.Time `json:"last_activity"`
}

// IdleStateFreshFor is how old a published state may be and still describe
// the instance now: two monitor ticks and a margin. An older one means the
// daemon is not running, or not ticking, and its reading is history.
const IdleStateFreshFor = 150 * time.Second

// Fresh reports whether s was taken within IdleStateFreshFor of now. A state
// from the future (clock stepped back) is not fresh either.
func (s IdleState) Fresh(now time.Time) bool {
	age := now.Sub(s.Taken)
	return !s.Taken.IsZero() && age >= 0 && age <= IdleStateFreshFor
}

// idleStatePath is where the daemon publishes its IdleState, or "" where there
// is no boot-scoped location (see sysIdleStatePath). A var so tests can use a
// temporary directory.
var idleStatePath = sysIdleStatePath

// IdleStatePath is the published IdleState's location, or "".
func IdleStatePath() string { return idleStatePath() }

// idleStateOf converts one evaluation for publishing.
func idleStateOf(s idleSnapshot, lastActivity time.Time) IdleState {
	return IdleState{
		Taken: s.taken, Idle: s.idle, Reason: s.reason,
		CPUPercent: s.cpu, CPUMeasured: s.cpuMeasured,
		NetworkBytes: s.net, NetworkMeasured: s.netMeasured, NetworkWhy: s.netWhy,
		DiskBytes: s.disk, DiskMeasured: s.diskMeasured, DiskWhy: s.diskWhy,
		LastActivity: lastActivity,
	}
}

// WriteIdleState writes s atomically (temp file and rename), so a reader never
// sees a torn file. Not fsynced: the path is on tmpfs, and a write that reached
// a disk every minute would itself be disk I/O counted against idle (#1093).
func WriteIdleState(path string, s IdleState) error {
	if path == "" {
		return errors.New("no idle state path on this platform")
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // fixed daemon-owned path, world-readable by design
		return err
	}
	return os.Rename(tmp, path)
}

// ReadIdleState reads a state written by WriteIdleState.
func ReadIdleState(path string) (IdleState, error) {
	if path == "" {
		return IdleState{}, os.ErrNotExist
	}
	data, err := os.ReadFile(path) //nolint:gosec // fixed daemon-owned path
	if err != nil {
		return IdleState{}, err
	}
	var s IdleState
	if err := json.Unmarshal(data, &s); err != nil {
		return IdleState{}, err
	}
	return s, nil
}

// publishIdleState writes this tick's evaluation for `spored status`.
// Best-effort: a status without it falls back to a one-shot reading and says
// so. A failure is logged once, not every minute.
func (a *Agent) publishIdleState(s idleSnapshot) {
	p := IdleStatePath()
	if p == "" {
		return
	}
	if err := WriteIdleState(p, idleStateOf(s, a.lastActivity())); err != nil {
		if !a.idleStateWarned {
			a.idleStateWarned = true
			log.Printf("Warning: could not publish idle state to %s: %v (spored status will show a one-shot reading)", p, err)
		}
		return
	}
	a.idleStateWarned = false
}
