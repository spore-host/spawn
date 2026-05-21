package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// storeNameRe matches valid plugin names for DiskStateStore path construction.
var storeNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// checkPluginName returns an error if name is not a safe plugin identifier.
func checkPluginName(name string) error {
	if !storeNameRe.MatchString(name) {
		return fmt.Errorf("invalid plugin name %q", name)
	}
	return nil
}

// PluginStatus represents the lifecycle state of a plugin instance.
type PluginStatus string

const (
	// StatusInstalling indicates install steps are running.
	StatusInstalling PluginStatus = "installing"
	// StatusConfiguring indicates configure steps are running.
	StatusConfiguring PluginStatus = "configuring"
	// StatusStarting indicates start steps are running.
	StatusStarting PluginStatus = "starting"
	// StatusRunning indicates the plugin is healthy and running.
	StatusRunning PluginStatus = "running"
	// StatusWaitingForPush indicates the plugin is waiting for a pushed value
	// before it can continue (e.g. a setup key from the local controller).
	StatusWaitingForPush PluginStatus = "waiting_for_push"
	// StatusDegraded indicates health checks are failing.
	StatusDegraded PluginStatus = "degraded"
	// StatusStopped indicates the plugin has been stopped.
	StatusStopped PluginStatus = "stopped"
	// StatusFailed indicates an unrecoverable error during install/configure/start.
	StatusFailed PluginStatus = "failed"
)

// PluginState is the persisted runtime state of a single plugin instance.
type PluginState struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Status      PluginStatus      `json:"status"`
	Config      map[string]string `json:"config"`
	Outputs     map[string]string `json:"outputs"`
	Pushed      map[string]string `json:"pushed"`
	InstalledAt time.Time         `json:"installed_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	LastHealth  *time.Time        `json:"last_health,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// StateStore manages plugin state persistence.
type StateStore interface {
	Load(name string) (*PluginState, error)
	Save(state *PluginState) error
	Delete(name string) error
	List() ([]*PluginState, error)
}

// DiskStateStore stores plugin state as JSON files under a root directory.
// Each plugin gets its own subdirectory: <dir>/<name>/state.json.
type DiskStateStore struct {
	dir string
}

// NewDiskStateStore creates a DiskStateStore rooted at dir.
func NewDiskStateStore(dir string) *DiskStateStore {
	return &DiskStateStore{dir: dir}
}

// DefaultStateStore returns a store using the default spored state directory.
func DefaultStateStore() *DiskStateStore {
	return NewDiskStateStore("/var/lib/spored/plugins")
}

func (s *DiskStateStore) pluginDir(name string) string {
	return filepath.Join(s.dir, name)
}

func (s *DiskStateStore) statePath(name string) string {
	return filepath.Join(s.pluginDir(name), "state.json")
}

// Load loads the persisted state for a plugin.
func (s *DiskStateStore) Load(name string) (*PluginState, error) {
	if err := checkPluginName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.statePath(name))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("plugin %q: state not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("load state for %s: %w", name, err)
	}
	var state PluginState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode state for %s: %w", name, err)
	}
	return &state, nil
}

// Save writes the plugin state to disk atomically via a temp file + rename.
func (s *DiskStateStore) Save(state *PluginState) error {
	if err := checkPluginName(state.Name); err != nil {
		return err
	}
	dir := s.pluginDir(state.Name)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create state dir for %s: %w", state.Name, err)
	}

	state.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state for %s: %w", state.Name, err)
	}

	// Use a random temp name to avoid collision if Save is called concurrently.
	tmpf, err := os.CreateTemp(dir, "state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp state for %s: %w", state.Name, err)
	}
	tmp := tmpf.Name()
	if _, err := tmpf.Write(data); err != nil {
		_ = tmpf.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write state for %s: %w", state.Name, err)
	}
	if err := tmpf.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close state for %s: %w", state.Name, err)
	}
	if err := os.Rename(tmp, s.statePath(state.Name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save state for %s: %w", state.Name, err)
	}
	return nil
}

// Delete removes the plugin's state directory.
func (s *DiskStateStore) Delete(name string) error {
	if err := checkPluginName(name); err != nil {
		return err
	}
	if err := os.RemoveAll(s.pluginDir(name)); err != nil {
		return fmt.Errorf("delete state for %s: %w", name, err)
	}
	return nil
}

// List returns the persisted state for all known plugins.
func (s *DiskStateStore) List() ([]*PluginState, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list plugins: %w", err)
	}

	var states []*PluginState
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := s.Load(e.Name())
		if err != nil {
			continue // Skip corrupted or incomplete state.
		}
		states = append(states, st)
	}
	return states, nil
}
