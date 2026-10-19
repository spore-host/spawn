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
