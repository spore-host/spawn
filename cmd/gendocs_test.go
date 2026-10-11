package cmd

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/libs/docgen"
)

// The committed reference fragments must correspond EXACTLY to the commands that
// exist — in both directions.
//
// #795: `make check-docs` runs `spawn gen-docs` and then
// `git diff --exit-code docs-gen/`. That catches a fragment whose CONTENT
// drifted. It cannot catch a fragment whose COMMAND is gone, because nothing
// rewrote or removed the file, so there is no diff:
//
//	$ make check-docs
//	wrote 53 reference fragments to docs-gen    # was 54
//	$ echo $?
//	0                                           # passed
//	$ git ls-files docs-gen/ | grep alerts
//	docs-gen/alerts.md                          # still tracked, still shipped
//
// The count dropped and the gate was still happy. Published docs would have gone
// on describing `spawn alerts create|list|delete|history` after the command was
// deleted — the gate whose job is "docs match the CLI" reporting success while
// they did not.
//
// Set equality is deliberately chosen over "prune on generate". Pruning fixes
// this instance; an assertion that the two sides agree cannot pass while they
// disagree, whatever the cause — a fragment deleted by hand, a command added
// without regenerating, or a future generator that writes somewhere new.
func TestDocsGenFragmentsMatchTheCommandSetExactly(t *testing.T) {
	frags, err := docgen.Generate(rootCmd, docgen.Options{CLIName: "spawn"})
	if err != nil {
		t.Fatalf("docgen.Generate: %v", err)
	}
	if len(frags) == 0 {
		// Refusing to pass vacuously: with no generated set, "the directory
		// matches" is trivially true and checks nothing.
		t.Fatal("docgen.Generate produced no fragments — the generator is broken, not the CLI")
	}

	dir := filepath.Join(repoRootFromCmd(t), "docs-gen")
	onDisk, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(onDisk) == 0 {
		t.Fatalf("no fragments found in %s — the gate is reading the wrong directory", dir)
	}

	want := map[string]bool{}
	for name := range frags {
		want[name] = true
	}
	have := map[string]bool{}
	for _, p := range onDisk {
		have[filepath.Base(p)] = true
	}

	// Direction 1: a fragment on disk with no command behind it. This is the
	// half #795 was filed for.
	var orphaned []string
	for name := range have {
		if !want[name] {
			orphaned = append(orphaned, name)
		}
	}
	// Direction 2: a command with no committed fragment. Equally wrong, and
	// previously only caught because `git diff` saw an untracked new file —
	// which says nothing if the file was never generated.
	var missing []string
	for name := range want {
		if !have[name] {
			missing = append(missing, name)
		}
	}

	sort.Strings(orphaned)
	sort.Strings(missing)

	if len(orphaned) > 0 {
		t.Errorf("docs-gen/ contains %d fragment(s) for commands that do not exist: %s\n"+
			"Run `make gen-docs` (which now prunes) and commit the deletion.",
			len(orphaned), strings.Join(orphaned, ", "))
	}
	if len(missing) > 0 {
		t.Errorf("%d command(s) have no committed fragment: %s\n"+
			"Run `make gen-docs` and commit the new files.",
			len(missing), strings.Join(missing, ", "))
	}
}

// gen-docs must refuse to prune when the generated set is implausibly small.
//
// Without the floor, a generator that returned almost nothing — a broken tree
// walk, an i18n failure leaving every Short empty — would delete the entire
// published reference, and `git diff --exit-code` would then report it
// faithfully but far too late. This is the same rule as
// scripts/nested-modules.sh refusing to pass on an empty discovery.
func TestGenDocsRefusesToPruneOnAnImplausiblyEmptySet(t *testing.T) {
	src := filepath.Join(repoRootFromCmd(t), "cmd", "gendocs.go")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	s := string(b)

	// The declaration and the comparison, not the bare name: the comment above
	// the constant explains the floor, and a substring match would be satisfied
	// by that prose alone. Five gates in this repo have been fooled that way.
	for _, want := range []string{
		"const minPlausibleFragments = 10",
		"len(frags) < minPlausibleFragments",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("cmd/gendocs.go does not contain %q — pruning without a floor can "+
				"delete the whole reference when the generator breaks", want)
		}
	}
	if !strings.Contains(s, "refusing to prune") {
		t.Error("the floor does not refuse — it must return an error, not warn and continue")
	}
}
