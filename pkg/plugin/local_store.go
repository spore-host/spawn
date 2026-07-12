package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// instanceIDRe matches a safe instance path component. --instance accepts either
// an EC2 instance ID (i-…) or a user-chosen Name tag, so this is permissive but
// excludes path separators and dots that could escape the store root.
var instanceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// LocalRecord captures what the controller needs to later tear down a plugin's
// local (controller-side) footprint — the mutagen sync it created, the Globus
// endpoint it registered, etc. It is written by `spawn plugin install` after
// local provision runs and read by `spawn plugin remove` / `spawn terminate` so
// they can replay the plugin's local deprovision steps.
//
// Outputs is the key field: a plugin's deprovision step may reference a value
// captured during provision (e.g. Globus's {{ outputs.endpoint_id }}), and that
// value lives only in controller memory during install — spored persists it on
// the instance, which is exactly what's being torn down. Persisting it here on
// the controller is what makes local deprovision possible.
type LocalRecord struct {
	Name        string            `json:"name"`
	Ref         string            `json:"ref"`
	InstanceID  string            `json:"instance_id"`
	Instance    map[string]string `json:"instance"` // instance.* template values used at provision (id/name/ip)
	Config      map[string]string `json:"config"`
	Outputs     map[string]string `json:"outputs"`
	Deprovision []Step            `json:"deprovision"`         // spec's local deprovision steps, captured at install
	Reconcile   []Step            `json:"reconcile,omitempty"` // spec's local reconcile steps (re-point on IP change)
	InstalledAt time.Time         `json:"installed_at"`
}

// LocalStore persists LocalRecords on the controller under a root directory,
// laid out as <root>/<instance-id>/<plugin>.json.
type LocalStore struct {
	dir string
}

// NewLocalStore creates a LocalStore rooted at dir.
func NewLocalStore(dir string) *LocalStore {
	return &LocalStore{dir: dir}
}

// DefaultLocalStore returns a LocalStore rooted at ~/.spawn/plugins. It errors
// only if the user's home directory can't be resolved.
func DefaultLocalStore() (*LocalStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	return NewLocalStore(filepath.Join(home, ".spawn", "plugins")), nil
}

func (s *LocalStore) instanceDir(instanceID string) string {
	return filepath.Join(s.dir, instanceID)
}

func (s *LocalStore) recordPath(instanceID, name string) string {
	return filepath.Join(s.instanceDir(instanceID), name+".json")
}

// Save writes a record. The record's InstanceID and Name must be safe path
// components.
func (s *LocalStore) Save(rec *LocalRecord) error {
	if err := checkPluginName(rec.Name); err != nil {
		return err
	}
	if !instanceIDRe.MatchString(rec.InstanceID) {
		return fmt.Errorf("invalid instance identifier %q", rec.InstanceID)
	}
	dir := s.instanceDir(rec.InstanceID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create local plugin state dir: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal local record: %w", err)
	}
	path := s.recordPath(rec.InstanceID, rec.Name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write local record: %w", err)
	}
	return nil
}

// Load reads the record for a single plugin on an instance. It returns an error
// wrapping os.ErrNotExist when no record is present.
func (s *LocalStore) Load(instanceID, name string) (*LocalRecord, error) {
	if err := checkPluginName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.recordPath(instanceID, name))
	if err != nil {
		return nil, err
	}
	var rec LocalRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse local record: %w", err)
	}
	return &rec, nil
}

// List returns all records for an instance. A missing instance directory yields
// an empty slice, not an error.
func (s *LocalStore) List(instanceID string) ([]*LocalRecord, error) {
	entries, err := os.ReadDir(s.instanceDir(instanceID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []*LocalRecord
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		name := e.Name()[:len(e.Name())-len(".json")]
		rec, err := s.Load(instanceID, name)
		if err != nil {
			continue // skip unreadable/foreign files
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// Delete removes the record for a plugin on an instance. A missing record is not
// an error. When the instance directory is left empty it is removed too.
func (s *LocalStore) Delete(instanceID, name string) error {
	if err := checkPluginName(name); err != nil {
		return err
	}
	if err := os.Remove(s.recordPath(instanceID, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove local record: %w", err)
	}
	// Best-effort cleanup of an emptied instance dir.
	if entries, err := os.ReadDir(s.instanceDir(instanceID)); err == nil && len(entries) == 0 {
		_ = os.Remove(s.instanceDir(instanceID))
	}
	return nil
}
