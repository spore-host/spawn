// Package arrayrec persists a lightweight local launch record for a job array so
// `spawn array retry --failed` can faithfully relaunch missing/failed indexes.
//
// A job array has no server-side record (unlike sweeps, which live in DynamoDB):
// its members are discovered by grouping EC2 on the spawn:job-array-* tags. But a
// surviving member's tags don't carry the full launch config — spawn:command
// only rides when ≤256 chars, and AMI/subnet/security-group/user-data are never
// tagged (pkg/aws/tags.go). So retry needs the original config from somewhere.
//
// This package writes it to a local file at launch (default
// ~/.config/spore/arrays/<array-id>.json, plus a <name>.json pointer to the
// latest id for that name). It is deliberately local-only and SDK-free — no
// infra-account credentials, matching the array group's "no server-side record"
// ethos. The documented tradeoff: retry works only from the machine that
// launched the array.
package arrayrec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// Record is the persisted launch config for one job array. Base is the full
// per-launch LaunchConfig whose UserData already embeds storage mounts and the
// command (see cmd/launch_jobarray.go); the scalar fields alongside it are the
// job-array knobs the per-member builder needs that don't live on Base.
type Record struct {
	ArrayID       string           `json:"array_id"`
	Name          string           `json:"name"`
	Size          int              `json:"size"`
	Region        string           `json:"region"`
	Command       string           `json:"command,omitempty"`
	InstanceNames string           `json:"instance_names,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	Base          aws.LaunchConfig `json:"base"`
}

// DefaultDir is the base directory records live in, ~/.config/spore/arrays.
// Returns an error if the home directory can't be resolved.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "spore", "arrays"), nil
}

// Save writes the record to <dir>/<array-id>.json and rewrites the
// <dir>/<name>.json pointer to the latest id for that name (both 0600). The dir
// is created if absent. Callers treat a Save error as non-fatal at launch (the
// launch already succeeded; only retry-from-this-machine is lost).
func Save(dir string, r Record) error {
	if r.ArrayID == "" || r.Name == "" {
		return fmt.Errorf("record needs both ArrayID and Name")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create array-record dir: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal array record: %w", err)
	}
	if err := os.WriteFile(idPath(dir, r.ArrayID), data, 0600); err != nil {
		return fmt.Errorf("write array record: %w", err)
	}
	// The name pointer is a tiny JSON holding the latest array-id for this name,
	// so retry can resolve a human name to the most recent launch. Relaunching an
	// array with the same name overwrites it — retry always targets the latest.
	ptr, err := json.Marshal(namePointer{ArrayID: r.ArrayID})
	if err != nil {
		return fmt.Errorf("marshal name pointer: %w", err)
	}
	if err := os.WriteFile(namePath(dir, r.Name), ptr, 0600); err != nil {
		return fmt.Errorf("write name pointer: %w", err)
	}
	return nil
}

// LoadByID reads the record for an exact array id.
func LoadByID(dir, arrayID string) (Record, error) {
	data, err := os.ReadFile(idPath(dir, arrayID))
	if err != nil {
		return Record{}, fmt.Errorf("read array record %q: %w", arrayID, err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, fmt.Errorf("parse array record %q: %w", arrayID, err)
	}
	return r, nil
}

// LoadByName resolves a job-array name to its latest record via the name
// pointer. Returns a clear error when no local record exists (retry must run
// from the machine that launched the array).
func LoadByName(dir, name string) (Record, error) {
	data, err := os.ReadFile(namePath(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, fmt.Errorf("no local launch record for job array %q — retry must run from the machine that launched it", name)
		}
		return Record{}, fmt.Errorf("read name pointer %q: %w", name, err)
	}
	var ptr namePointer
	if err := json.Unmarshal(data, &ptr); err != nil {
		return Record{}, fmt.Errorf("parse name pointer %q: %w", name, err)
	}
	return LoadByID(dir, ptr.ArrayID)
}

type namePointer struct {
	ArrayID string `json:"array_id"`
}

func idPath(dir, arrayID string) string { return filepath.Join(dir, safeFile(arrayID)+".json") }
func namePath(dir, name string) string  { return filepath.Join(dir, "name-"+safeFile(name)+".json") }

// safeFile makes a job-array name/id safe to use as a single path segment. Names
// aren't strictly validated at launch, so an arbitrary string (with slashes,
// dots, etc.) must not escape the record dir or collide with the id file. It
// replaces any char outside [A-Za-z0-9._-] with '_'; the mapping need not be
// reversible — LoadByName resolves through the name pointer to the exact id, and
// LoadByID is given the id from that pointer, so both sides apply safeFile
// identically.
func safeFile(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}
