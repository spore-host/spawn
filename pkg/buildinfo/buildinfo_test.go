package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name          string
		injected      string
		moduleVersion string
		modified      bool
		want          string
	}{
		{
			// The release case. Injected wins outright — that is the authoritative
			// number, and it must survive verbatim so `spawn version` on a release
			// build matches the tag (the release guard checks exactly this).
			name: "injected wins over everything", injected: "0.98.0",
			moduleVersion: "0.97.1-0.20260803020438-48ba7f76a21a", want: "0.98.0",
		},
		{
			name: "injected v-prefix trimmed", injected: "v0.98.0", want: "0.98.0",
		},
		{
			// A release build is built from a clean tag checkout, so this state
			// shouldn't arise; if it does, the injected number still stands and the
			// release guard is what complains. Appending +dirty to a published
			// version would break the tag/version match instead.
			name: "injected is not marked dirty", injected: "0.98.0", modified: true, want: "0.98.0",
		},
		{
			// `go install github.com/spore-host/spawn@v0.97.0` — the module version
			// IS the release, so report it rather than "dev".
			name: "module version used when nothing injected", moduleVersion: "v0.97.0", want: "0.97.0",
		},
		{
			// A build inside a checkout: the toolchain derives this from the last tag
			// plus the commit. Ugly but true, and better than a number someone typed.
			name: "pseudo-version passes through", moduleVersion: "v0.97.1-0.20260803020438-48ba7f76a21a",
			want: "0.97.1-0.20260803020438-48ba7f76a21a",
		},
		{
			name:          "pseudo-version from a dirty tree says so",
			moduleVersion: "v0.97.1-0.20260803020438-48ba7f76a21a", modified: true,
			want: "0.97.1-0.20260803020438-48ba7f76a21a+dirty",
		},
		{
			// What Go ACTUALLY hands us for a dirty checkout: Main.Version already
			// ends in +dirty, and vcs.modified reports the same fact again. Appending
			// unconditionally gave "+dirty+dirty" in a real build.
			name:          "dirty marker is not doubled",
			moduleVersion: "v0.97.1-0.20260803020438-48ba7f76a21a+dirty", modified: true,
			want: "0.97.1-0.20260803020438-48ba7f76a21a+dirty",
		},
		{
			// Same value, but the toolchain didn't set vcs.modified. The version
			// string is still the authority on its own dirtiness.
			name:          "pre-marked dirty version is preserved without vcs.modified",
			moduleVersion: "v0.97.1-0.20260803020438-48ba7f76a21a+dirty", modified: false,
			want: "0.97.1-0.20260803020438-48ba7f76a21a+dirty",
		},
		{
			// "(devel)" is what Go reports for a plain `go build` in a checkout with
			// no VCS-derived version. It carries no information, so it must NOT be
			// passed through as if it were a version.
			name: "(devel) is not a version", moduleVersion: "(devel)", want: "dev",
		},
		{
			name: "(devel) dirty", moduleVersion: "(devel)", modified: true, want: "dev+dirty",
		},
		{
			// The regression this package exists for: with nothing injected and no
			// usable module version, the answer is an obviously-not-a-release string.
			// If this ever returns a number again, the drift bug is back.
			name: "nothing available", want: "dev",
		},
		{
			// Whitespace-only injection is the same as not injected — an -X ldflag
			// fed an empty template variable produces exactly this.
			name: "blank injected falls through", injected: "   ", moduleVersion: "v0.97.0", want: "0.97.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.injected, tc.moduleVersion, tc.modified); got != tc.want {
				t.Errorf("resolve(%q, %q, %v) = %q, want %q",
					tc.injected, tc.moduleVersion, tc.modified, got, tc.want)
			}
		})
	}
}

// TestResolveNeverInventsAReleaseNumber is the invariant, stated separately from
// the table because it is the whole point: no combination of missing inputs may
// produce something that looks like a release. A plausible-but-false version is
// worse than an obviously-absent one, because it gets believed and reported.
func TestResolveNeverInventsAReleaseNumber(t *testing.T) {
	for _, mod := range []string{"", "   ", "(devel)"} {
		for _, dirty := range []bool{false, true} {
			got := resolve("", mod, dirty)
			if !IsDev(got) {
				t.Errorf("resolve(\"\", %q, %v) = %q, which does not read as a dev build",
					mod, dirty, got)
			}
		}
	}
}

func TestIsDev(t *testing.T) {
	cases := map[string]bool{
		"dev":                                  true,
		"dev+dirty":                            true,
		"  dev  ":                              true,
		"0.97.0":                               false,
		"0.97.1-0.20260803020438-48ba7f76a21a": false,
		"0.97.0+dirty":                         false,
		"":                                     false, // no version at all is not the same claim as "a dev build"
	}
	for in, want := range cases {
		if got := IsDev(in); got != want {
			t.Errorf("IsDev(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStampFrom(t *testing.T) {
	settings := []debug.BuildSetting{
		{Key: "-compiler", Value: "gc"},
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "48ba7f76a21a"},
		{Key: "vcs.time", Value: "2026-08-03T02:04:38Z"},
		{Key: "vcs.modified", Value: "true"},
	}
	rev, ts, mod := stampFrom(settings)
	if rev != "48ba7f76a21a" {
		t.Errorf("revision = %q, want 48ba7f76a21a", rev)
	}
	if ts != "2026-08-03T02:04:38Z" {
		t.Errorf("buildTime = %q, want 2026-08-03T02:04:38Z", ts)
	}
	if !mod {
		t.Error("modified = false, want true")
	}

	// Absent keys must read as "unknown, clean" rather than defaulting to dirty:
	// a build with no VCS metadata at all (a release tarball, `go build` outside a
	// repo) has no evidence of modification, and claiming +dirty there would be a
	// false accusation on every such build.
	rev, ts, mod = stampFrom([]debug.BuildSetting{{Key: "-compiler", Value: "gc"}})
	if rev != "" || ts != "" || mod {
		t.Errorf("stampFrom(no vcs keys) = (%q, %q, %v), want (\"\", \"\", false)", rev, ts, mod)
	}

	// Anything other than the literal "true" is not dirty. Go writes exactly
	// "true"/"false"; be strict rather than treating any non-empty value as dirty.
	if _, _, mod = stampFrom([]debug.BuildSetting{{Key: "vcs.modified", Value: "false"}}); mod {
		t.Error("vcs.modified=false read as dirty")
	}
}

// TestVersionReadsRealBuildMetadata exercises the non-pure path: with nothing
// injected, the running test binary must resolve to something honest via
// debug.ReadBuildInfo. Which value depends on how the test was built, so assert
// the property rather than a literal — a test binary in a checkout gets a
// pseudo-version, one built elsewhere gets "dev".
func TestVersionReadsRealBuildMetadata(t *testing.T) {
	got := Version("")
	if got == "" {
		t.Fatal("Version(\"\") is empty; it must always report something")
	}
	if got == "(devel)" {
		t.Errorf("Version(\"\") = %q; (devel) must be translated, not passed through", got)
	}
	if !IsDev(got) && (got[0] < '0' || got[0] > '9') {
		t.Errorf("Version(\"\") = %q, want either a dev marker or a version starting with a digit", got)
	}

	// And an injected value must win over whatever the toolchain stamped, since
	// that is the mechanism the release relies on.
	if got := Version("1.2.3"); got != "1.2.3" {
		t.Errorf("Version(%q) = %q, want the injected value", "1.2.3", got)
	}
}

// TestModulePathIsThisModule guards the assumption the release ldflag depends
// on: the -X target is spelled with the module path, so if the module were
// renamed without updating .goreleaser.yaml the injection would silently stop
// working. (The release guard catches it too, at tag time; this catches it in
// ordinary CI.)
func TestModulePathIsThisModule(t *testing.T) {
	if got := ModulePath(); got != "github.com/spore-host/spawn" {
		t.Errorf("ModulePath() = %q; if the module was renamed, update the -X ldflags in .goreleaser.yaml and scripts/check-release-version.sh", got)
	}
}
