package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Everything shipped under deployment/ must be reachable from something a reader
// would find (#811).
//
// `deployment/prometheus/` and `deployment/grafana/` — twelve files of working
// scrape config, alert rules and dashboards — were referenced by no doc, no
// Makefile target, no script and no workflow. The only way to discover them was
// to list the directory. That is how the tree came to contain an Alertmanager
// receiver pointing at a webhook no code served, and a scrape job filtering on a
// tag nothing sets, with nobody noticing either: unreferenced config is not read,
// and config that is not read does not get corrected.
//
// The same shape as `deployment/spawn-orchestrator.service`, whose `ExecStart`
// named a binary no release built.
//
// Checked per top-level subtree rather than per file, because a dashboard JSON
// does not need its own mention — the tree it lives in does.
func TestEveryDeploymentSubtreeIsReferenced(t *testing.T) {
	entries, err := os.ReadDir("deployment")
	if err != nil {
		t.Fatalf("read deployment/: %v", err)
	}
	// Rule 4: an empty discovery has broken rather than proved the tree clean.
	if len(entries) == 0 {
		t.Fatal("deployment/ listed no entries — the read has broken, not the directory emptied")
	}

	// Where a reader could plausibly find a pointer. Discovered, not listed:
	// every doc, script, workflow, Makefile and template in the repo.
	haystack := referenceHaystack(t)
	if len(haystack) < 10_000 {
		t.Fatalf("collected only %d bytes of searchable text; the collection has broken, which "+
			"would make every subtree look referenced", len(haystack))
	}

	var orphaned []string
	for _, e := range entries {
		name := e.Name()
		// A top-level file is its own subtree.
		needle := "deployment/" + name
		if strings.Contains(haystack, needle) {
			continue
		}
		orphaned = append(orphaned, needle)
	}
	sort.Strings(orphaned)
	if len(orphaned) > 0 {
		t.Errorf("%d deployment subtree(s) are referenced by nothing: %s\n"+
			"  Config nobody can find is config nobody reads, and config nobody reads stops "+
			"being true without anyone noticing — deployment/prometheus/ shipped a receiver "+
			"for a server that was never started and a scrape filter on a tag nothing sets. "+
			"Reference it from a doc that explains what it is and what it needs, or delete it.",
			len(orphaned), strings.Join(orphaned, ", "))
	}
}

// referenceHaystack concatenates every file a pointer could live in, excluding
// deployment/ itself so a tree cannot vouch for itself.
func referenceHaystack(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(".", func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			switch info.Name() {
			// deployment/ excluded: a file inside the tree referencing its sibling
			// is not discoverability. node_modules/docs-gen/.git are noise.
			case "deployment", ".git", "node_modules", "docs-gen":
				return filepath.SkipDir
			}
			return nil
		}
		// CHANGELOG.md is NOT discoverability, and including it made this gate
		// useless on its first run: the only thing in the repo mentioning
		// deployment/prometheus was a changelog entry, so the gate passed while the
		// tree was exactly as unfindable as before. A changelog records that
		// something happened; it does not tell a reader what the config is or what
		// it needs. Same reasoning excludes docs marked SUPERSEDED — a historical
		// document pointing at a live tree is not a pointer anyone should follow.
		if info.Name() == "CHANGELOG.md" {
			return nil
		}
		// _test.go excluded, and this file is why: its own comments name
		// deployment/prometheus/ and deployment/grafana/ as the examples the gate
		// exists for, so on the second run the gate VOUCHED FOR ITSELF and passed
		// with the explanatory doc deleted. A test is not somewhere a reader goes
		// to learn what a config tree is for.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".md"),
			strings.HasSuffix(path, ".sh"),
			strings.HasSuffix(path, ".yml"),
			strings.HasSuffix(path, ".yaml"),
			strings.HasSuffix(path, ".go"),
			info.Name() == "Makefile":
			c, rerr := os.ReadFile(path) //nolint:gosec // walking this repo's own files
			if rerr != nil {
				return rerr
			}
			b.Write(c)
			b.WriteByte('\n')
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return b.String()
}
