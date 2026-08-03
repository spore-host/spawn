// Package buildinfo resolves the version a binary reports about itself.
//
// It exists because a hand-written default version is a second source of truth
// that nothing updates. spawn's said "0.38.1" while the current release was
// v0.97.0 — 59 releases of silent drift — and spored's said "0.1.0". Both were
// wrong in the worst way: not obviously-unset, but a plausible release number
// that got believed, reported in bug reports, and compared against the update
// feed.
//
// The fix is to inject the version only at release time and, for every other
// build, fall back to the metadata the Go toolchain stamps in automatically.
// That fallback cannot go stale because nobody maintains it.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Dev is what a build with no injected version and no usable module version
// calls itself. Deliberately not a number: an obviously-not-a-release string is
// better than a plausible one, because a plausible one gets believed.
const Dev = "dev"

// dirtySuffix marks a binary built from a modified working tree — it
// corresponds to no commit, so a bug report citing its version can't be
// reproduced from it.
const dirtySuffix = "+dirty"

// Version returns the version to report, preferring the value injected at
// release time (GoReleaser's -X ldflag) and otherwise falling back to what the
// Go toolchain stamped into the binary.
func Version(injected string) string {
	return resolve(injected, mainModuleVersion(), dirty())
}

// resolve is the pure core of Version, split out so the fallback ordering is
// testable without building binaries in three different ways.
//
// moduleVersion is debug.BuildInfo.Main.Version. For `go install
// module@version` it is that version. For a build inside a checkout it is
// either "(devel)" — no information — or a pseudo-version the toolchain derives
// from the last tag plus the commit (0.97.1-0.20260803020438-48ba7f76a21a).
// Both are honest, and neither is maintained by hand.
func resolve(injected, moduleVersion string, modified bool) string {
	if v := strings.TrimSpace(injected); v != "" {
		// A release build. Trust it as-is, including its dirty-tree state: the
		// release pipeline builds from a clean tag checkout, and if it somehow
		// didn't, saying so is the release guard's job (scripts/check-version.sh),
		// not a suffix quietly appended to every published version string.
		return strings.TrimPrefix(v, "v")
	}
	if v := strings.TrimSpace(moduleVersion); v != "" && v != "(devel)" {
		return markDirty(strings.TrimPrefix(v, "v"), modified)
	}
	return markDirty(Dev, modified)
}

// markDirty appends the dirty marker unless it is already there.
//
// Idempotence is the point, not defensiveness: Go's Main.Version ALREADY ends in
// "+dirty" for a modified tree (v0.97.1-0.2026...-48ba7f76a21a+dirty), and
// vcs.modified says so a second time. Appending unconditionally produced
// "+dirty+dirty" — caught by mutating the version plumbing and watching the
// release guard print it.
func markDirty(v string, modified bool) string {
	if !modified || strings.HasSuffix(v, dirtySuffix) {
		return v
	}
	return v + dirtySuffix
}

// IsDev reports whether v carries no release number at all, so comparing it
// against the release feed would be meaningless. A real version never starts
// with "dev".
func IsDev(v string) bool {
	return strings.HasPrefix(strings.TrimSpace(v), Dev)
}

// Revision returns the commit and build time the Go toolchain recorded, or
// empty strings when the binary wasn't built from a VCS checkout.
func Revision() (commit, buildTime string) {
	commit, buildTime, _ = stamp()
	return commit, buildTime
}

func dirty() bool {
	_, _, modified := stamp()
	return modified
}

// stamp reads the VCS metadata the Go toolchain records at build time.
func stamp() (revision, buildTime string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "", false
	}
	return stampFrom(info.Settings)
}

// stampFrom is split out from stamp so the setting-key parsing is testable
// without needing a build with particular VCS state.
func stampFrom(settings []debug.BuildSetting) (revision, buildTime string, modified bool) {
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			buildTime = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return revision, buildTime, modified
}

func mainModuleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Version
}

// ModulePath is the main module's path ("github.com/spore-host/spawn"), or ""
// if unavailable. Used by the release guard's test to derive the ldflag target
// rather than hardcoding it a second time.
func ModulePath() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Path
}
