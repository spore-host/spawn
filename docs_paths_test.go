package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A doc that names a file must either name one that exists, or say it is
// historical (#848).
//
// `docs/spawn-audit-2026-07-11.md` is framed as a live prioritized findings
// report — "Phase 0 — Do now" — and cites `cmd/launch.go` five times, including
// its line count. That file does not exist; it was split up. A reader following
// the document's own top-priority item finds nothing, and cannot tell whether the
// finding was fixed, renamed, or never real.
//
// The forcing function is the point: a doc is either current, in which case its
// paths resolve, or it is history, in which case it says so. There is no third
// state where it quietly describes a repo that no longer exists. The model already
// in the tree is `docs/pooled-task-execution-design.md`, which carries
// `Status: SHIPPED`.
//
// Deliberately only backtick-quoted paths with a file extension. Prose like "the
// cmd directory" is not a claim about a specific file, and treating it as one is
// how a gate ends up with an allowlist of things that are obviously fine
// (CLAUDE.md rule 6).
var docPathRef = regexp.MustCompile(
	"`((?:cmd|pkg|lambda|internal|scripts|deployment|test|api|examples)/[A-Za-z0-9._/-]+\\.(?:go|sh|yaml|yml|py|md|json|txt))`")

// historicalMarkers let a doc opt out by declaring itself history. Checked in the
// first part of the file, so a marker has to be prominent rather than buried.
var historicalMarkers = []string{"SUPERSEDED", "HISTORICAL", "Status: SHIPPED"}

func TestDocsDoNotCitePathsThatDoNotExist(t *testing.T) {
	var docs []string
	err := filepath.Walk("docs", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".md") {
			docs = append(docs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	docs = append(docs, "README.md")
	if len(docs) < 5 {
		t.Fatalf("found only %d docs; the walk has broken rather than the repo having shrunk",
			len(docs))
	}

	checked := 0
	for _, doc := range docs {
		b, rerr := os.ReadFile(doc) //nolint:gosec // walking the repo's own docs
		if rerr != nil {
			t.Fatalf("read %s: %v", doc, rerr)
		}
		body := string(b)

		// A doc that declares itself historical may cite whatever it described.
		head := body
		if len(head) > 600 {
			head = head[:600]
		}
		historical := false
		for _, m := range historicalMarkers {
			if strings.Contains(strings.ToUpper(head), strings.ToUpper(m)) {
				historical = true
				break
			}
		}

		var missing []string
		for _, p := range docPathRef.FindAllStringSubmatch(body, -1) {
			checked++
			if _, serr := os.Stat(p[1]); serr != nil {
				missing = append(missing, p[1])
			}
		}
		if len(missing) == 0 || historical {
			continue
		}
		sort.Strings(missing)
		t.Errorf("%s cites %d path(s) that do not exist: %s\n"+
			"  Either the doc is current — in which case fix the paths — or it describes a repo "+
			"that has moved on, in which case mark it %s near the top so a reader knows not to "+
			"act on it. docs/pooled-task-execution-design.md is the model.",
			doc, len(missing), strings.Join(uniqueStrings(missing), ", "), historicalMarkers[0])
	}

	if checked == 0 {
		t.Fatal("matched no backtick-quoted repo paths in any doc — the pattern has stopped " +
			"matching, which is not the same as every path being valid")
	}
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
