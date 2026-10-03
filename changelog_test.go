package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// CHANGELOG.md is policy, not decoration: CLAUDE.md requires an entry under
// `## [Unreleased]` in the SAME PR as any user-facing change, and the release ritual
// promotes that section verbatim into a dated one. Nothing checked either half, and
// both broke within a week:
//
//   - #627 merged with no changelog entry at all. Nobody noticed until the next
//     release found an EMPTY [Unreleased] section and the entries had to be
//     reconstructed from the diff at tag time — exactly when you least want to be
//     reverse-engineering what a change did.
//   - Three PRs each inserted their own `### Added` at the top of [Unreleased].
//     That merges cleanly for git and badly for Keep a Changelog: the section ended
//     up with duplicate `### Added` and `### Fixed` headers, which had to be
//     consolidated by hand at release time. It then happened a second time.
//
// These tests cover the format half, which is checkable from the file alone. The
// "did this PR update the changelog" half needs the base branch, so it lives in CI
// (.github/workflows/ci.yml, "Changelog gate").

const changelogPath = "CHANGELOG.md"

// keepAChangelogGroups are the only group headings the format defines, plus
// Documentation, which this repo uses by convention for docs-only changes.
var keepAChangelogGroups = map[string]bool{
	"Added": true, "Changed": true, "Deprecated": true,
	"Removed": true, "Fixed": true, "Security": true,
	"Documentation": true,
}

var (
	releaseHeading = regexp.MustCompile(`^## \[([^\]]+)\]`)
	groupHeading   = regexp.MustCompile(`^### (.+)$`)
)

func readChangelog(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(changelogPath)
	if err != nil {
		t.Fatalf("read %s: %v", changelogPath, err)
	}
	return strings.Split(string(b), "\n")
}

// section returns the lines belonging to the named release section.
func section(t *testing.T, lines []string, want string) []string {
	t.Helper()
	start := -1
	for i, l := range lines {
		m := releaseHeading.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if start >= 0 {
			return lines[start+1 : i] // next heading ends the section we were in
		}
		if m[1] == want {
			start = i
		}
	}
	if start < 0 {
		t.Fatalf("%s has no `## [%s]` section", changelogPath, want)
	}
	return lines[start+1:]
}

// TestChangelogHasNoDuplicateGroups is the one that actually bit, twice. Two PRs each
// adding `### Added` at the top of [Unreleased] merge without conflict, leaving a
// section with the same heading twice — valid Markdown, invalid Keep a Changelog, and
// invisible until someone reads the rendered file.
//
// Scoped to [Unreleased] ON PURPOSE. Running it over the whole file reports duplicate
// groups in roughly a dozen shipped releases — this has been happening for a long
// time, not just in the three PRs that prompted the gate. Gating frozen history would
// force either a mass rewrite of released notes (noisy, and those sections describe
// what actually shipped) or leave a permanently-red test, which is worse than no test
// because it gets disabled. The job here is to stop NEW occurrences in the one section
// still being edited, which is also the only place the problem causes work: a
// duplicate in [Unreleased] has to be consolidated by hand at release time.
func TestChangelogHasNoDuplicateGroups(t *testing.T) {
	seen := map[string]bool{}
	for _, l := range section(t, readChangelog(t), "Unreleased") {
		m := groupHeading.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		g := strings.TrimSpace(m[1])
		if seen[g] {
			t.Errorf("[Unreleased] has `### %s` more than once — merge the entries into one group "+
				"(two PRs each inserting their own group merges cleanly for git and badly for the format)", g)
		}
		seen[g] = true
	}
}

// TestChangelogGroupsAreValid catches a typo'd or invented heading, which would
// otherwise silently become a section nobody's tooling or eye looks for. [Unreleased]
// only, for the same reason as above — history already contains `### CI` and
// `### Changed (internal refactors — no behavior change)`.
func TestChangelogGroupsAreValid(t *testing.T) {
	for _, l := range section(t, readChangelog(t), "Unreleased") {
		m := groupHeading.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		g := strings.TrimSpace(m[1])
		if !keepAChangelogGroups[g] {
			valid := make([]string, 0, len(keepAChangelogGroups))
			for k := range keepAChangelogGroups {
				valid = append(valid, k)
			}
			sort.Strings(valid)
			t.Errorf("[Unreleased] has unknown group `### %s`; Keep a Changelog defines: %s",
				g, strings.Join(valid, ", "))
		}
	}
}

// TestChangelogHasUnreleasedSection: the release ritual promotes [Unreleased] into a
// dated section. If it is missing, the next change has nowhere to go and the next
// release has nothing to promote.
func TestChangelogHasUnreleasedSection(t *testing.T) {
	found := false
	for _, l := range readChangelog(t) {
		if strings.HasPrefix(l, "## [Unreleased]") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("%s must keep a `## [Unreleased]` section, even when empty — it is where the next "+
			"change goes and what the next release promotes", changelogPath)
	}
}

// TestChangelogUnreleasedHasNoEntriesWithoutAGroup: a bullet sitting directly under
// `## [Unreleased]` with no `###` above it renders, but is uncategorised and gets
// promoted into a dated section that way.
func TestChangelogUnreleasedHasNoEntriesWithoutAGroup(t *testing.T) {
	inGroup := false
	for _, l := range section(t, readChangelog(t), "Unreleased") {
		if groupHeading.MatchString(l) {
			inGroup = true
			continue
		}
		if strings.HasPrefix(l, "- ") && !inGroup {
			t.Errorf("[Unreleased] has an entry before any `###` group:\n    %s", strings.TrimSpace(l))
		}
	}
}

// TestChangelogCompareLinksCoverEveryRelease. The links at the bottom are how a
// reader gets from a version to its diff; a release promoted without adding one
// leaves a dead reference, and the omission is easy to make because it is a separate
// edit at the very end of the file.
func TestChangelogCompareLinksCoverEveryRelease(t *testing.T) {
	lines := readChangelog(t)

	// Only single-version headings get a compare link. A heading like
	// `## [0.36.0 – 0.36.13]` summarises a SPAN of releases and has no one diff to
	// point at; demanding a link for it would be a false positive that teaches people
	// to ignore this test.
	singleVersion := regexp.MustCompile(`^\d+\.\d+\.\d+$`)

	var versions []string
	links := map[string]bool{}
	linkDef := regexp.MustCompile(`^\[([^\]]+)\]:\s+http`)
	for _, l := range lines {
		if m := releaseHeading.FindStringSubmatch(l); m != nil {
			if m[1] == "Unreleased" || singleVersion.MatchString(m[1]) {
				versions = append(versions, m[1])
			}
		}
		if m := linkDef.FindStringSubmatch(l); m != nil {
			links[m[1]] = true
		}
	}

	if len(versions) == 0 {
		t.Fatal("no release sections found")
	}
	for _, v := range versions {
		if !links[v] {
			t.Errorf("section [%s] has no compare link at the bottom of the file — a release promoted "+
				"without one leaves a dead reference", v)
		}
	}
}
