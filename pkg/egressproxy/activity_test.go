package egressproxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestActivityRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "activity.json")
	if got, err := LoadActivity(path); err != nil || !got.IsZero() {
		t.Fatalf("missing file: %v, %v; want zero, nil", got, err)
	}
	at := time.Date(2026, 11, 1, 9, 30, 0, 123, time.UTC)
	if err := SaveActivity(path, at, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := LoadActivity(path)
	if err != nil || !got.Equal(at) {
		t.Fatalf("LoadActivity = %v, %v; want %v", got, err, at)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary file was left behind")
	}
	if err := os.WriteFile(path, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadActivity(path); err == nil {
		t.Error("a corrupt file loaded without error")
	}
}

func TestInitialActivity(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	launch := now.Add(-20 * 24 * time.Hour)
	used := now.Add(-3 * 24 * time.Hour)
	cases := []struct {
		name              string
		persisted, anchor time.Time
		want              time.Time
	}{
		{"restart keeps the persisted activity", used, launch, used},
		{"never used: idle counts from launch, not from this restart", time.Time{}, launch, launch},
		{"persisted before launch (another instance's file)", launch.Add(-time.Hour), launch, launch},
		{"nothing known: now", time.Time{}, time.Time{}, now},
		{"persisted only", used, time.Time{}, used},
		{"a clock stepped back cannot postpone expiry", now.Add(time.Hour), launch, now},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InitialActivity(tc.persisted, tc.anchor, now); !got.Equal(tc.want) {
				t.Errorf("InitialActivity = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInitialActivitySeedsTheProxy(t *testing.T) {
	at := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	if got := New(Config{InitialActivity: at}).LastActivity(); !got.Equal(at) {
		t.Errorf("LastActivity = %v, want the seed %v", got, at)
	}
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if got := New(Config{Now: func() time.Time { return now }}).LastActivity(); !got.Equal(now) {
		t.Errorf("unseeded LastActivity = %v, want the start time %v", got, now)
	}
}
