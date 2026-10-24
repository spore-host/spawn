package reaperconfig

import (
	"strings"
	"testing"
	"time"
)

func TestParseModeAcceptsExactlyTheThreeModes(t *testing.T) {
	for in, want := range map[string]string{"off": ModeOff, " REPORT ": ModeReport, "Reap": ModeReap} {
		if got, ok := ParseMode(in); !ok || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "reep", "true", "yes", "on"} {
		if _, ok := ParseMode(in); ok {
			t.Errorf("ParseMode(%q) accepted an unrecognised value", in)
		}
	}
}

func TestParseMaxAge(t *testing.T) {
	if d, err := ParseMaxAge("3d"); err != nil || d != 72*time.Hour {
		t.Errorf("ParseMaxAge(3d) = %v, %v", d, err)
	}
	for _, in := range []string{"", "0", "forever", "1w", "-2h"} {
		if _, err := ParseMaxAge(in); err == nil {
			t.Errorf("ParseMaxAge(%q) accepted", in)
		}
	}
}

// A notify URL is usually a webhook whose path is the credential, so no
// rejection may echo it.
// REAPER_TTL_GRACE (#1186): 0 restores reaping at the deadline; the ceiling
// exists because the heartbeat that earns the grace is instance-writable.
func TestParseTTLGrace(t *testing.T) {
	for in, want := range map[string]time.Duration{"0": 0, "10m": 10 * time.Minute, "1h": time.Hour, " 5m ": 5 * time.Minute} {
		if d, err := ParseTTLGrace(in); err != nil || d != want {
			t.Errorf("ParseTTLGrace(%q) = %v, %v; want %v", in, d, err, want)
		}
	}
	for _, in := range []string{"", "61m", "2h", "-1m", "soon"} {
		if _, err := ParseTTLGrace(in); err == nil {
			t.Errorf("ParseTTLGrace(%q) accepted", in)
		}
	}
	if DefaultTTLGrace <= 0 || DefaultTTLGrace > MaxTTLGrace {
		t.Errorf("DefaultTTLGrace %s must be within (0, %s]", DefaultTTLGrace, MaxTTLGrace)
	}
}

func TestValidateNeverEchoesTheNotifyURL(t *testing.T) {
	for _, in := range []string{"ftp://h/s3cr3t", "s3cr3t", "https:///s3cr3t", "http://[::1/s3cr3t"} {
		err := Validate(EnvNotifyURL, in)
		if err == nil {
			t.Errorf("Validate(%q) accepted", in)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("error leaks the URL: %v", err)
		}
	}
	if err := Validate(EnvNotifyURL, "https://hooks.example.invalid/x"); err != nil {
		t.Errorf("valid URL rejected: %v", err)
	}
}

func TestValidateEmptyRemovesOnlySettableVariables(t *testing.T) {
	for _, k := range Settable() {
		if err := Validate(k, ""); err != nil {
			t.Errorf("Validate(%s, \"\") = %v; empty means remove", k, err)
		}
	}
	if err := Validate("REAPER_DRY_RUN", ""); err == nil {
		t.Error("REAPER_DRY_RUN is not settable here; arming is `spawn reaper arm`")
	}
}
