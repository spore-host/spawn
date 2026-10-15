package ttl

import (
	"strings"
	"testing"
	"time"
)

// The spellings #990 is about, and everything time.ParseDuration already took.
func TestParseDurationAccepts(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1d", 24 * time.Hour}, // the #990 repro
		{"2d", 48 * time.Hour},
		{"2d12h", 60 * time.Hour},
		{"1d30m", 24*time.Hour + 30*time.Minute},
		{"1.5d", 36 * time.Hour},
		{"0.5d", 12 * time.Hour},
		{"1d2h30m15s", 26*time.Hour + 30*time.Minute + 15*time.Second},
		{"12h1d", 36 * time.Hour}, // Go allows any segment order; so do days
		{"8h", 8 * time.Hour},
		{"90m", 90 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"1.5h", 90 * time.Minute},
		{"30s", 30 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"1us", time.Microsecond},
		{"1µs", time.Microsecond},
		{"1ns", time.Nanosecond},
		{"0", 0},
		{"0s", 0},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Everything time.ParseDuration accepts that is unsigned must parse to the same
// value, so adopting the shared parser can never change what an existing tag or
// flag means.
func TestParseDurationAgreesWithGoOnGoSyntax(t *testing.T) {
	for _, s := range []string{"1h", "1h30m", "2.5h", "90m", "45s", "100ms", "1m0s", "24h0m0s", "0", "3h2m1s500ms", ".5h", "1.h"} {
		want, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("bad fixture %q: %v", s, err)
		}
		got, err := ParseDuration(s)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; time.ParseDuration = %v", s, got, err, want)
		}
	}
}

func TestParseDurationRejects(t *testing.T) {
	for _, s := range []string{
		"", "d", "h", "1", "12", "1w", "1y", "2weeks", "abc", "1D", "1H",
		"-1h", "+1h", "-1d", "1d-2h", " 1h", "1h ", "1 h", "1h,2m",
		"1..5h", "1.2.3d", ".d", "1e3h",
		"999999999999d", "106752d", "106751d24h", // overflow (each segment fits, the sum does not)
	} {
		if d, err := ParseDuration(s); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want an error", s, d)
		}
	}
}

func TestParseTTLRejectsZero(t *testing.T) {
	for _, s := range []string{"0", "0s", "0d", "0h0m"} {
		if _, err := ParseTTL(s); err == nil || !strings.Contains(err.Error(), "greater than zero") {
			t.Errorf("ParseTTL(%q) err = %v, want a must-be-positive error", s, err)
		}
	}
	if d, err := ParseTTL("1d"); err != nil || d != 24*time.Hour {
		t.Errorf("ParseTTL(1d) = %v, %v", d, err)
	}
	if _, err := ParseTTL("1w"); err == nil {
		t.Error("ParseTTL(1w) accepted an unparseable TTL")
	}
}

// A tag value must be readable by time.ParseDuration too (older spored, the
// dashboard, other repos), and must mean the same duration.
func TestTagValueIsReadableByTimeParseDuration(t *testing.T) {
	for _, s := range []string{"1d", "2d12h", "1.5d", "8h", "1h30m", "90m", "0.5d"} {
		v, err := TagValue(s)
		if err != nil {
			t.Fatalf("TagValue(%q): %v", s, err)
		}
		want, _ := ParseDuration(s)
		got, err := time.ParseDuration(v)
		if err != nil || got != want {
			t.Errorf("TagValue(%q) = %q, which time.ParseDuration reads as %v, %v; want %v", s, v, got, err, want)
		}
	}
	// Go-native spellings are kept as typed.
	if v, _ := TagValue("8h"); v != "8h" {
		t.Errorf("TagValue(8h) = %q, want it unchanged", v)
	}
	if _, err := TagValue("1w"); err == nil {
		t.Error("TagValue(1w) accepted an unparseable value")
	}
}

// Every accepted unit parses, in integer and fractional form. Discovered from
// Units() rather than listed here.
func TestEveryUnitParses(t *testing.T) {
	us := Units()
	if len(us) == 0 {
		t.Fatal("Units() is empty")
	}
	for _, u := range us {
		for _, n := range []string{"1", "2", "1.5"} {
			if _, err := ParseDuration(n + u); err != nil {
				t.Errorf("ParseDuration(%q): %v", n+u, err)
			}
		}
	}
}
