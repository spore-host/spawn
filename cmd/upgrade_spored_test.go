package cmd

import (
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/launchconfig"
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

// #1412: with no --version, upgrade-spored brings an instance to the spored
// this spawn ships (the one its launches install), not to whatever was
// released last; a dev build, which ships none, asks for the latest ("").
func TestUpgradeSporedDefaultsToThisSpawnsVersion(t *testing.T) {
	cases := []struct{ flag, own, want string }{
		{"", "0.148.0", "0.148.0"},
		{"", "", ""},
		{"0.140.0", "0.148.0", "0.140.0"},
		{"v0.140.0", "", "0.140.0"},
	}
	for _, c := range cases {
		got, err := upgradeSporedTarget(c.flag, c.own)
		if err != nil || got != c.want {
			t.Errorf("upgradeSporedTarget(%q, %q) = %q, %v; want %q", c.flag, c.own, got, err, c.want)
		}
	}
	for _, bad := range []string{"latest", "0.1", "0.1.0'; reboot; '", "0.1.0-rc1"} {
		if _, err := upgradeSporedTarget(bad, "0.148.0"); err == nil {
			t.Errorf("--version %q was accepted; it names no published spored", bad)
		}
	}
}

// #1359: an upgrade that would leave an instance's launch-fixed settings
// where the target spored does not read them is refused, in both directions;
// one with nothing in the other place goes ahead.
func TestUpgradeSporedRefusesToStrandLaunchSettings(t *testing.T) {
	first := launchconfig.FirstSporedVersion
	tagsOnly := map[string]string{"spawn:managed": "true", "spawn:version": "0.149.0", "spawn:notify-url": "https://n", "spawn:active-ports": "8888"}
	object := map[string]string{"spawn:managed": "true", "spawn:version": first, launchconfig.Tag: strings.Repeat("a", 64)}
	plain := map[string]string{"spawn:managed": "true", "spawn:version": "0.149.0", "spawn:ttl": "1h"}
	for _, tc := range []struct {
		name, target string
		tags         map[string]string
		refuse       string
	}{
		{"tags-only instance, new spored", first, tagsOnly, "--version 0.149.0"},
		{"tags-only instance, later spored", "0.151.2", tagsOnly, "spawn:active-ports, spawn:notify-url"},
		{"tags-only instance, old spored", "0.149.0", tagsOnly, ""},
		{"object instance, new spored", "0.151.0", object, ""},
		{"object instance, old spored", "0.149.0", object, "does not read"},
		{"nothing to strand, new spored", first, plain, ""},
		{"nothing to strand, old spored", "0.148.0", plain, ""},
	} {
		err := launchConfigUpgradeGuard(tc.tags, tc.target)
		switch {
		case tc.refuse == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)):
			t.Errorf("%s: err = %v, want a refusal mentioning %q", tc.name, err, tc.refuse)
		}
	}
}

// #1423: the spored the tags-only refusal tells the user to pin comes from
// spawn:version only on an instance spawn itself launched. spawn-ts writes its
// own library version there (0.7.0, no such spored), so its hint names the
// spored the instance reports, or the frozen tags-reading spored.
func TestUpgradeSporedTagsOnlyHintNamesARealSpored(t *testing.T) {
	settings := map[string]string{"spawn:managed": "true", "spawn:notify-url": "https://n"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range settings {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, tc := range []struct {
		name      string
		tags      map[string]string
		want, not string
	}{
		{"spawn-ts instance", with(map[string]string{"spawn:created-by": "spawn-ts", "spawn:version": "0.7.0", "spawn:spored-version": "0.149.0"}),
			"--version 0.149.0", "--version 0.7.0"},
		{"spawn-ts instance on an older spored", with(map[string]string{"spawn:created-by": "spawn-ts", "spawn:version": "0.7.0", "spawn:spored-version": "0.141.2"}),
			"--version 0.141.2", "--version 0.7.0"},
		{"spawn instance", with(map[string]string{"spawn:created-by": "spawn", "spawn:version": "0.142.0", "spawn:spored-version": "0.141.2"}),
			"--version 0.142.0", ""},
		{"neither version", with(nil), "--version 0.149.0", ""},
		{"spawn instance with a dev version", with(map[string]string{"spawn:created-by": "spawn", "spawn:version": "dev"}),
			"--version 0.149.0", "--version dev"},
	} {
		err := launchConfigUpgradeGuard(tc.tags, launchconfig.FirstSporedVersion)
		if err == nil {
			t.Errorf("%s: not refused", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to name %q", tc.name, err, tc.want)
		}
		if tc.not != "" && strings.Contains(err.Error(), tc.not) {
			t.Errorf("%s: err = %v, names %q", tc.name, err, tc.not)
		}
	}
}
