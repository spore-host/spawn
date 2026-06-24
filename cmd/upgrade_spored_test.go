package cmd

import (
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.64.0", "0.63.1", 1},
		{"0.63.1", "0.64.0", -1},
		{"0.64.0", "0.64.0", 0},
		{"v0.64.0", "0.64.0", 0}, // leading v tolerated
		{"1.0.0", "0.99.99", 1},
		{"0.64.1", "0.64.0", 1},
		{"0.64.0-rc1", "0.64.0", 0}, // pre-release suffix ignored per segment
		{"0.65", "0.64.9", 1},       // short version (missing patch) still compares
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestBuildSporedUpgradeScript(t *testing.T) {
	s := buildSporedUpgradeScript("0.64.0")

	// Must target the VERSIONED artifact path (deterministic), not the floating
	// "latest" path, and pin the requested version into TARGET_VERSION.
	if !strings.Contains(s, "spawn/versions/${TARGET_VERSION}") {
		t.Errorf("script does not pull the versioned artifact path:\n%s", s)
	}
	if !strings.Contains(s, `TARGET_VERSION="0.64.0"`) {
		t.Errorf("script does not pin the requested version:\n%s", s)
	}
	// Regional bucket with us-east-1 fallback (mirrors bootstrap.go).
	if !strings.Contains(s, "spawn-binaries-${REGION}") || !strings.Contains(s, "spawn-binaries-us-east-1") {
		t.Error("script missing regional+fallback bucket logic")
	}
	// Atomic rename over the busy binary (never write in place → no ETXTBSY).
	if !strings.Contains(s, "mv -f") || !strings.Contains(s, "/usr/local/bin/spored") {
		t.Error("script must atomically mv into /usr/local/bin/spored")
	}
	// Graceful stop (triggers compute-seconds flush in agent.Cleanup) then start.
	if !strings.Contains(s, "systemctl stop spored") || !strings.Contains(s, "systemctl start spored") {
		t.Error("script must stop then start spored")
	}
	// Health check + rollback on failure.
	if !strings.Contains(s, "is-active") || !strings.Contains(s, "rolling back") {
		t.Error("script must health-check and roll back on failure")
	}
	// Checksum verification.
	if !strings.Contains(s, "sha256sum") || !strings.Contains(s, ".sha256") {
		t.Error("script must verify the SHA256 checksum")
	}
	// Fail-fast shell.
	if !strings.HasPrefix(s, "set -euo pipefail") {
		t.Error("script must start with set -euo pipefail")
	}
}

func TestParseSporedVersion(t *testing.T) {
	cases := []struct {
		name, out, want string
	}{
		{"tagged line", "Lifecycle status\n  spored:           v0.63.1\n  ttl: 24h\n", "0.63.1"},
		{"no v prefix", "  spored: 0.63.1\n", "0.63.1"},
		{"absent", "  ttl: 24h\n  idle-timeout: 1h\n", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := parseSporedVersion(c.out); got != c.want {
			t.Errorf("%s: parseSporedVersion=%q want %q", c.name, got, c.want)
		}
	}
}
