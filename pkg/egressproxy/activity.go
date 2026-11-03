package egressproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// activityRecord is the on-disk form of the last-activity time.
type activityRecord struct {
	LastActivity time.Time `json:"last_activity"`
	WrittenAt    time.Time `json:"written_at"`
}

// LoadActivity reads the last-activity time SaveActivity wrote to path. A
// missing file is not an error: it returns the zero time.
func LoadActivity(path string) (time.Time, error) {
	b, err := os.ReadFile(path) //nolint:gosec // spored's own state file, at a path it chose
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	var rec activityRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return time.Time{}, fmt.Errorf("egress proxy activity file %s: %w", path, err)
	}
	return rec.LastActivity, nil
}

// SaveActivity atomically writes t to path as the last-activity time, so a
// crash mid-write leaves the previous value rather than a torn file.
func SaveActivity(path string, t, now time.Time) error {
	b, err := json.Marshal(activityRecord{LastActivity: t.UTC(), WrittenAt: now.UTC()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // spored's own state file
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// InitialActivity is the activity time a starting proxy begins from: the later
// of the persisted last activity and anchor.
//
// anchor is when the proxy's idle clock may start at the earliest: the
// instance's launch time, not this process's start. Using the process start
// would reset the idle clock on every spored restart, an in-place upgrade or
// the reboot that applies OS updates, and a proxy that is rebooted more often
// than its idle period would never expire. A persisted time later than now
// (a clock that stepped back) is clamped to now, so it cannot postpone expiry.
func InitialActivity(persisted, anchor, now time.Time) time.Time {
	t := anchor
	if persisted.After(t) {
		t = persisted
	}
	if t.IsZero() || t.After(now) {
		t = now
	}
	return t
}
