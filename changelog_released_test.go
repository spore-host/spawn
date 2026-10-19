package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A released section of CHANGELOG.md is the record of what shipped, so it must
// stay what it was when its version was tagged (#1101).
//
// It kept changing. A PR opened before a release adds its entry under
// [Unreleased]; the release PR then inserts `## [X.Y.Z]` directly beneath that
// heading; when the older PR merges, git's 3-way merge places its lines by
// their surrounding context, which now sits inside the released section. The
// merge is clean, CI was green, and the notes for an already-tagged release
// silently changed: #1093 and #1090 under [0.132.1], the same at v0.129.0, a
// squash-merge deleting entries from [0.132.2], and four v0.127.0 fixes still
// recorded under [0.126.0]. The structural changelog tests pass all of these.
//
// So every `## [X.Y.Z]` section must equal the same section in
// `git show vX.Y.Z:CHANGELOG.md`. Tags before v0.43.0 have no CHANGELOG.md and
// are skipped; the newest section may have no tag yet, because the release PR
// that adds it merges before the tag is pushed.
//
// It needs the tags. The CI jobs that run it fetch them; under CI=true missing
// tags FAIL rather than skip, or the gate could never fire where it matters
// (CLAUDE.md, "Writing a gate", rule 1). Locally it skips with a message.

// acceptedReleasedEdits are released sections that differ from their tag and
// have been accepted as they now stand. Each is pinned by the SHA-256 of its
// current text, so the section is immutable again in its accepted form: any
// further edit fails, as does an entry whose section has been restored (remove
// it then). Add one only with the reason, never to make a merge go green; the
// fix for an entry that landed in a released section is to move it back to
// [Unreleased].
var acceptedReleasedEdits = map[string]struct{ sha256, why string }{
	"0.126.0": {
		sha256: "754d9814235734afd6ecca61128907f951b3329a73579a37bff558afcb307e68",
		why: "four entries (#476 via #785, #781, #772 via #778, #774 via #775) merged after the tag and " +
			"landed here, though they shipped in 0.127.0. They cannot be moved to [0.127.0] without " +
			"breaking that section's own match with v0.127.0, and restoring the tag's text would drop " +
			"them from the changelog entirely.",
	},
	"0.97.0": {
		sha256: "16f82dca39c950ebf4dbacb924744e44b5b42d802aae1ead7b827e92ba584190",
		why: "the tag's section had `### Added` and `### Fixed` twice each; consolidating them moved the " +
			"#465 entry into the remaining `### Fixed` group. Its text is unchanged, and restoring the " +
			"tag's text would bring back the duplicate groups.",
	},
}

var (
	semverHeading = regexp.MustCompile(`^## \[(\d+\.\d+\.\d+)\]`)
	anyHeading    = regexp.MustCompile(`^## \[`)
	linkDef       = regexp.MustCompile(`^\[[^\]]+\]:\s`)
)

// releasedSection returns the text of the `## [version]` section of a
// changelog, heading included, and whether it was found. It ends at the next
// release heading or the compare-link block, with trailing blank lines and
// carriage returns dropped, so a CRLF checkout compares equal.
func releasedSection(changelog, version string) (string, bool) {
	var out []string
	in := false
	for _, l := range strings.Split(strings.ReplaceAll(changelog, "\r\n", "\n"), "\n") {
		if in && (anyHeading.MatchString(l) || linkDef.MatchString(l)) {
			break
		}
		if !in {
			if m := semverHeading.FindStringSubmatch(l); m != nil && m[1] == version {
				in = true
			} else {
				continue
			}
		}
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n"), in
}

func sectionSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// skipOrFailWithoutTags fails under CI and skips elsewhere.
func skipOrFailWithoutTags(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("CI") == "true" {
		t.Fatalf("%s; under CI the released-section gate must run, so the job has to fetch tags (#1101)", msg)
	}
	t.Skipf("%s; skipping the released-section gate locally (run `git fetch --tags` to enable it)", msg)
}

// tagChangelogs reads CHANGELOG.md at every given tag in one `git cat-file`
// process. A tag whose tree has no CHANGELOG.md maps to "", present=false.
func tagChangelogs(t *testing.T, versions []string) map[string]*string {
	t.Helper()
	var in bytes.Buffer
	for _, v := range versions {
		fmt.Fprintf(&in, "refs/tags/v%s:CHANGELOG.md\n", v)
	}
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git cat-file --batch: %v", err)
	}
	r := bufio.NewReader(bytes.NewReader(out))
	got := map[string]*string{}
	for _, v := range versions {
		header, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("git cat-file: reading the header for v%s: %v", v, err)
		}
		f := strings.Fields(header)
		if len(f) == 2 && f[1] == "missing" {
			got[v] = nil
			continue
		}
		if len(f) != 3 || f[1] != "blob" {
			t.Fatalf("git cat-file: unexpected header for v%s: %q", v, header)
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			t.Fatalf("git cat-file: bad size for v%s: %q", v, header)
		}
		body := make([]byte, size+1) // content plus the trailing newline
		if _, err := io.ReadFull(r, body); err != nil {
			t.Fatalf("git cat-file: reading v%s: %v", v, err)
		}
		s := string(body[:size])
		got[v] = &s
	}
	return got
}

func TestChangelogReleasedSectionsMatchTheirTags(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		skipOrFailWithoutTags(t, "git is not installed")
	}
	tagOut, err := exec.Command("git", "tag", "--list", "v*").Output()
	if err != nil {
		skipOrFailWithoutTags(t, "cannot list tags (%v)", err)
	}
	tags := map[string]bool{}
	for _, tag := range strings.Fields(string(tagOut)) {
		tags[strings.TrimPrefix(tag, "v")] = true
	}
	if len(tags) == 0 {
		skipOrFailWithoutTags(t, "this checkout has no v* tags")
	}

	b, err := os.ReadFile(changelogPath)
	if err != nil {
		t.Fatal(err)
	}
	current := string(b)

	// Discover the versions from the headings, newest first.
	var versions []string
	for _, l := range strings.Split(current, "\n") {
		if m := semverHeading.FindStringSubmatch(strings.TrimRight(l, "\r")); m != nil {
			versions = append(versions, m[1])
		}
	}
	if len(versions) == 0 {
		t.Fatal("found no `## [X.Y.Z]` sections; the discovery is broken")
	}

	var tagged []string
	for i, v := range versions {
		switch {
		case tags[v]:
			tagged = append(tagged, v)
		case i == 0:
			t.Logf("[%s] has no tag yet: the release being cut, not checked", v)
		default:
			t.Errorf("[%s] has no tag v%s, and only the newest section may be untagged; "+
				"if the checkout is missing tags, run `git fetch --tags`", v, v)
		}
	}

	atTag := tagChangelogs(t, tagged)
	compared := 0
	for _, v := range tagged {
		old := atTag[v]
		if old == nil {
			continue // a tag from before CHANGELOG.md existed (v0.42.0 and earlier)
		}
		want, ok := releasedSection(*old, v)
		if !ok {
			t.Errorf("v%s's CHANGELOG.md has no `## [%s]` section to compare with", v, v)
			continue
		}
		got, _ := releasedSection(current, v)
		compared++
		accepted, isAccepted := acceptedReleasedEdits[v]
		switch {
		case got == want && isAccepted:
			t.Errorf("[%s] matches v%s again; remove its acceptedReleasedEdits entry", v, v)
		case got == want:
		case isAccepted && sectionSHA256(got) == accepted.sha256:
		case isAccepted:
			t.Errorf("[%s] was accepted as edited (%s) and has changed again (sha256 %s, accepted %s)",
				v, accepted.why, sectionSHA256(got), accepted.sha256)
		default:
			t.Errorf("[%s] differs from `git show v%s:CHANGELOG.md`. A released section records what "+
				"shipped; an entry that merged into it after the tag belongs under [Unreleased] (#1101).\n%s",
				v, v, lineDiff(want, got))
		}
	}
	if compared == 0 {
		t.Fatalf("compared no sections with their tags (%d tagged); the discovery is broken", len(tagged))
	}
	for v := range acceptedReleasedEdits {
		if !tags[v] {
			t.Errorf("acceptedReleasedEdits names %s, which has no tag", v)
		}
	}
}

// lineDiff is a minimal report of the lines only one side has, enough to see
// which entries moved without a diff library.
func lineDiff(want, got string) string {
	count := func(s string) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(s, "\n") {
			m[l]++
		}
		return m
	}
	w, g := count(want), count(got)
	var b strings.Builder
	for _, l := range strings.Split(want, "\n") {
		if g[l] < w[l] {
			fmt.Fprintf(&b, "  - (at tag only) %s\n", l)
			g[l]++
		}
	}
	w2, g2 := count(want), count(got)
	for _, l := range strings.Split(got, "\n") {
		if w2[l] < g2[l] {
			fmt.Fprintf(&b, "  + (now only)    %s\n", l)
			w2[l]++
		}
	}
	if b.Len() == 0 {
		return "  (same lines, different order or grouping)"
	}
	return b.String()
}
