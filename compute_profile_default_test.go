package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

// No non-test Go file may use the spore.host team's dev profile name as a
// value (#1538).
//
// pkg/config's GetComputeProfile returned it when SPAWN_COMPUTE_PROFILE was
// unset, so a detached sweep's members (and schedules, batch queues and
// collect-results) acted in whatever account a local profile of that name
// pointed at, whatever AWS_PROFILE or --profile said: live, AWS_PROFILE=aws
// launched two instances in the team's dev account. The compute default is now
// the caller's own credentials; a team that wants a dedicated compute account
// says so in ~/.spawn/config.yaml (compute_profile).
//
// Matched on the code, not the text: only string literals (and '+' runs of
// them, so "spore-host-" + "dev" is caught) whose whole value is the profile
// name. Comments, and prose in help text that names it as an example, do not
// trip it. Every non-test .go file is scanned, nested modules included.
const teamDevProfileName = "spore-host-" + "dev"

// teamDevProfileAllowed is where the name is a value on purpose, and why. A
// new entry needs the same: a reason it is not a spawn default.
var teamDevProfileAllowed = map[string]string{
	// The real-AWS e2e tiers (1-3) target the team's dev account when the
	// caller names no profile and no keys, as the Makefile and test/e2e/README
	// document. spawnChildEnv exports it to the spawn binary as AWS_PROFILE,
	// so the binary, the verification calls and the reaper agree on one
	// account. Test harness only (build-tagged e2e_tier1-3).
	"test/e2e/helpers.go": "e2e harness default, exported to the binary under test",
	// `//go:build integration` helpers for the root integration tests, which
	// run against the team's dev account. Never linked into the spawn binary.
	"pkg/testutil/integration_helpers.go": "integration-test helper, build-tagged, not in the binary",
}

func TestNoHardcodedTeamComputeProfile(t *testing.T) {
	scanned := 0
	used := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(".", path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "bin", "build", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		scanned++
		rel := filepath.ToSlash(path)
		for _, s := range stringRuns(f) {
			if strings.TrimSpace(s.text) != teamDevProfileName {
				continue
			}
			if _, ok := teamDevProfileAllowed[rel]; ok {
				used[rel] = true
				continue
			}
			t.Errorf("%s: %q as a value. Compute defaults to the caller's own credentials (#1538); "+
				"a dedicated compute account is SPAWN_COMPUTE_PROFILE or compute_profile in ~/.spawn/config.yaml, "+
				"never a name in code", fset.Position(s.pos), s.text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d Go files — the discovery is broken, not the repo clean", scanned)
	}
	for path, why := range teamDevProfileAllowed {
		if !used[path] {
			t.Errorf("allowlist entry %s (%s) no longer names the profile: remove it", path, why)
		}
	}
}
