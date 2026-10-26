package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"

	"gopkg.in/yaml.v3"
)

// Every command in this module must be a command some release produces (#811).
//
// `cmd/orchestrator` was a second binary, 679 non-test lines plus 654 of tests,
// with a systemd unit in `deployment/` whose `ExecStart` pointed at
// `/usr/local/bin/spawn-orchestrator` and a `--config` pointing at
// `/etc/spawn/orchestrator-config.yaml` that the repo never contained.
// `.goreleaser.yaml` builds `spawn` and `spored` and nothing else, so the binary
// could not be installed by any supported route. It compiled, it was vetted, it
// was tested, and `make check` was green on all of it for as long as it existed —
// because nothing anywhere asserted that a `package main` is a thing users can
// get.
//
// The set is DISCOVERED from the filesystem rather than listed (rule 4). A gate
// that named the commands it knew about would have reported a clean bill of
// health for exactly the one nobody remembered.
func TestEveryCommandIsBuiltByARelease(t *testing.T) {
	built, err := goreleaserMainPackages(".goreleaser.yaml")
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	// A discovery that found nothing has broken, not proved the repo clean.
	if len(built) == 0 {
		t.Fatal("parsed no `main:` entries out of .goreleaser.yaml — the parse has broken, " +
			"which is not the same as the release building nothing")
	}

	found := map[string]bool{}
	err = filepath.Walk(".", func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			if treewalk.Vanished(".", path, werr) {
				return nil
			}
			return werr
		}
		if info.IsDir() {
			// lambda/* are separate modules deployed by their own scripts, not by
			// GoReleaser; test/ holds harnesses; docs-gen is generated.
			switch info.Name() {
			// testdata: the go tool never builds it, so a `package main` there
			// is a test fixture (internal/iamcoverage's), not a command.
			case ".git", "lambda", "test", "docs-gen", "node_modules", "web", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // walking this module's own source
		if rerr != nil {
			return rerr
		}
		// The clause on its own line, so a `package main` inside a string literal
		// or a comment explaining this gate does not register as a command.
		if !packageMainDecl.Match(b) {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		found[dir] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("found no `package main` directories at all, not even the repo root — the walk " +
			"has broken rather than the repo having no commands")
	}

	var unbuilt []string
	for dir := range found {
		if !built[dir] {
			unbuilt = append(unbuilt, dir)
		}
	}
	sort.Strings(unbuilt)
	if len(unbuilt) > 0 {
		var have []string
		for d := range built {
			have = append(have, d)
		}
		sort.Strings(have)
		t.Errorf("%d command(s) are not built by any release: %s\n"+
			"  .goreleaser.yaml builds: %s\n"+
			"  A `package main` nothing ships cannot be installed by any supported route, so it "+
			"accumulates green CI while being unreachable. Either add a build entry, or delete it.",
			len(unbuilt), strings.Join(unbuilt, ", "), strings.Join(have, ", "))
	}
}

// packageMainDecl matches the package clause itself, anchored to the start of a
// line, never the bare words (rule 3).
var packageMainDecl = regexp.MustCompile(`(?m)^package main\s*$`)

// goreleaserMainPackages returns the set of directories GoReleaser builds, keyed
// the same way filepath.Dir reports them ("." for the root).
//
// Parsed with a regexp rather than a YAML unmarshal to keep this test free of a
// yaml dependency in the root module; the shape being read is two fixed keys.
var goreleaserMain = regexp.MustCompile(`(?m)^\s*main:\s*(\S+)\s*$`)

func goreleaserMainPackages(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the repo's own release config
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range goreleaserMain.FindAllStringSubmatch(string(b), -1) {
		v := strings.Trim(m[1], `"'`)
		v = strings.TrimPrefix(v, "./")
		if v == "" || v == "." {
			v = "."
		}
		out[filepath.ToSlash(filepath.Clean(v))] = true
	}
	return out, nil
}

// scripts/release-goos-vet.sh (#925) vets every GOOS the release builds, and
// discovers that set by parsing .goreleaser.yaml with awk so that it needs
// nothing beyond the Go toolchain. A hand-rolled parser can drift from the
// format, and a drifted one reports a SUBSET as clean, which is the failure the
// discovery exists to prevent. So check its answer against a real YAML decode
// of the same file, by a differently-shaped route.
func TestReleaseGOOSDiscoveryAgreesWithYAML(t *testing.T) {
	const script = "scripts/release-goos-vet.sh"
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// Read both inputs through the test process, so `go test`'s cache keys on
	// them and an edit to either re-runs this test.
	if _, err := os.ReadFile(script); err != nil { //nolint:gosec // the repo's own script
		t.Fatalf("read %s: %v", script, err)
	}
	b, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}

	var cfg struct {
		Builds []struct {
			ID   string   `yaml:"id"`
			Goos []string `yaml:"goos"`
		} `yaml:"builds"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("decode .goreleaser.yaml: %v", err)
	}
	if len(cfg.Builds) == 0 {
		t.Fatal("decoded no builds from .goreleaser.yaml; the decode has broken, not the release")
	}
	want := map[string]bool{}
	for _, b := range cfg.Builds {
		if len(b.Goos) == 0 {
			t.Errorf("build %q lists no goos, so GoReleaser applies its default set, which the "+
				"vet gate will not guess at; list it explicitly", b.ID)
		}
		for _, g := range b.Goos {
			want[g] = true
		}
	}
	var wantList []string
	for g := range want {
		wantList = append(wantList, g)
	}
	sort.Strings(wantList)

	out, err := exec.Command("bash", script, "--list").CombinedOutput() //nolint:gosec // fixed repo script
	if err != nil {
		t.Fatalf("%s --list failed: %v\n%s", script, err, out)
	}
	got := strings.Fields(string(out))
	if strings.Join(got, ",") != strings.Join(wantList, ",") {
		t.Errorf("%s discovers GOOS %v, but .goreleaser.yaml decodes to %v; the awk parse has "+
			"drifted from the file, so the vet gate is checking the wrong set", script, got, wantList)
	}
}
