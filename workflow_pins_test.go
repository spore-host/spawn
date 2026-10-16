package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every action CI runs is pinned to a full commit SHA with its version in a
// trailing comment. A floating tag (`actions/checkout@v4`) lets whoever controls
// the tag change what our CI executes; a SHA cannot move. The comment is what
// Dependabot rewrites alongside the SHA, and the only part of the pin a reviewer
// can read — so it has to be true, which is the second test here.
var (
	usesLine = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(.*)$`)
	// owner/repo[/path]@<40-hex SHA> # vX.Y.Z — groups: owner/repo, SHA, version.
	pinnedRef = regexp.MustCompile(`^([\w.-]+/[\w.-]+)(?:/[\w./-]+)?@([0-9a-f]{40}) # (v\d+\.\d+\.\d+)$`)
)

const pinTable = "scripts/action-pins.txt"

type actionPin struct {
	where   string // file:line
	ref     string // the whole `uses:` value
	repo    string
	sha     string
	version string
}

// workflowPins discovers every `uses:` in .github/workflows. It fails rather
// than returning nothing: a gate whose discovery broke would otherwise pass.
func workflowPins(t *testing.T) []actionPin {
	t.Helper()
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, err := filepath.Glob(filepath.Join(".github", "workflows", pat))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found under .github/workflows; a gate that checks nothing must not pass")
	}
	var pins []actionPin
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // the repo's own workflows
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(b), "\n") {
			m := usesLine.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			p := actionPin{where: f + ":" + strconv.Itoa(i+1), ref: strings.TrimSpace(m[1])}
			if pm := pinnedRef.FindStringSubmatch(p.ref); pm != nil {
				p.repo, p.sha, p.version = pm[1], pm[2], pm[3]
			}
			pins = append(pins, p)
		}
	}
	if len(pins) == 0 {
		t.Fatal("found no `uses:` lines in any workflow; the pattern or the layout changed")
	}
	return pins
}

func TestActionsArePinnedToSHAs(t *testing.T) {
	for _, p := range workflowPins(t) {
		if strings.HasPrefix(p.ref, "./") || strings.HasPrefix(p.ref, "docker://") {
			continue // local action or image, not a mutable remote tag
		}
		if p.sha == "" {
			t.Errorf("%s: %q is not pinned as owner/repo@<40-hex SHA> # vX.Y.Z", p.where, p.ref)
		}
	}
}

// TestActionPinCommentsMatchTheirSHAs: the `# vX.Y.Z` beside a SHA is a claim
// nothing else checks. actions/checkout@3d3c42e… was commented `# v6` in every
// workflow while it was in fact v7.0.1 (spawn#994) — the pin was safe, the label
// misled every reviewer who read it. The truth comes from GitHub, so it can't be
// computed offline; scripts/refresh-action-pins.sh records it in
// scripts/action-pins.txt and this test holds every comment to that record.
func TestActionPinCommentsMatchTheirSHAs(t *testing.T) {
	table := readPinTable(t)
	const fix = "run scripts/refresh-action-pins.sh and commit " + pinTable
	used := map[string]bool{}
	for _, p := range workflowPins(t) {
		if p.sha == "" {
			continue // TestActionsArePinnedToSHAs reports it
		}
		key := p.repo + "@" + p.sha
		used[key] = true
		want, ok := table[key]
		switch {
		case !ok:
			t.Errorf("%s: %s is not in %s, so its `# %s` comment is unverified — %s",
				p.where, key, pinTable, p.version, fix)
		case want != p.version:
			t.Errorf("%s: %s is commented `# %s` but that SHA is %s (per %s) — correct the comment, or if the table is the thing that is wrong, %s",
				p.where, key, p.version, want, pinTable, fix)
		}
	}
	for key := range table {
		if !used[key] {
			t.Errorf("%s lists %s, which no workflow pins any more — %s", pinTable, key, fix)
		}
	}
}

// readPinTable parses scripts/action-pins.txt into owner/repo@sha -> vX.Y.Z.
func readPinTable(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(pinTable)
	if err != nil {
		t.Fatalf("open %s: %v — generate it with scripts/refresh-action-pins.sh", pinTable, err)
	}
	defer func() { _ = f.Close() }()
	entry := regexp.MustCompile(`^([\w.-]+/[\w.-]+) ([0-9a-f]{40}) (v\d+\.\d+\.\d+)$`)
	table := map[string]string{}
	s := bufio.NewScanner(f)
	for n := 1; s.Scan(); n++ {
		l := s.Text()
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		m := entry.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("%s:%d: malformed entry %q (want `owner/repo <sha> vX.Y.Z`)", pinTable, n, l)
		}
		table[m[1]+"@"+m[2]] = m[3]
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if len(table) == 0 {
		t.Fatalf("%s has no entries; a table that lists nothing verifies nothing", pinTable)
	}
	return table
}
