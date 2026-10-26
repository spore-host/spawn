package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A release build must leave the checkout clean (#1266).
//
// Go stamps every binary with the VCS state of the tree it was built from, and
// it decides "modified" by running `git status --porcelain`: ANY line of output,
// an untracked file included, sets vcs.modified=true and turns the module
// version into vX.Y.Z+dirty. GoReleaser refuses to start on a dirty tree, then
// writes its own output into dist/ inside the checkout before building — so
// every binary after the first saw an untracked dist/, and v0.139.1 shipped as
// `v0.139.1+dirty`, misreporting what was built to anything that reads build
// info (SBOM and provenance tooling). truffle, lagotto and calque ignore dist/
// and come out clean; spawn did not.
//
// The gate runs the mechanism itself: it creates a file at every path a release
// writes into the tree and asserts `git status --porcelain --untracked-files=all`
// reports none of them. The paths are DISCOVERED from .goreleaser.yaml (its
// `dist:` directory, default "dist", and the `-o`/`zip` outputs of each
// `before:` hook) rather than listed, so a new hook output is covered without
// anyone remembering to add it — and one the parser cannot read fails here
// rather than passing unchecked.
func TestReleaseBuildLeavesTheTreeClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git not on PATH in CI; this gate must run there")
		}
		t.Skip("git not available")
	}
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").CombinedOutput(); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("not a git checkout in CI: %v: %s", err, out)
		}
		t.Skip("not a git checkout")
	}

	outputs, err := releaseTreeOutputs(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}

	tree, probes := probeReleaseTree(t, outputs)
	t.Logf("probing %v in %s", probes, tree)
	args := append([]string{"--git-dir=" + gitDir(t), "--work-tree=" + tree,
		"status", "--porcelain", "--untracked-files=all", "--"}, probes...)
	cmd := exec.Command("git", args...)
	// status must not refresh the shared index on the way: the work tree it
	// sees is not the checkout's.
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("a release build writes these paths into the checkout and git reports them, "+
			"so every binary built after them is stamped vcs.modified=true and versioned +dirty "+
			"(#1266). Ignore them in .gitignore:\n%s", s)
	}
}

// releaseTreeOutputs returns, slash-separated and relative to the repo root,
// one representative file path for each place a release build writes into the
// checkout: inside GoReleaser's dist directory (top level and nested, as its
// per-target build directories are), and every output of a before-hook.
func releaseTreeOutputs(cfgPath string) ([]string, error) {
	b, err := os.ReadFile(cfgPath) //nolint:gosec // the repo's own release config
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Dist   string `yaml:"dist"`
		Before struct {
			Hooks []yaml.Node `yaml:"hooks"`
		} `yaml:"before"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	dist := cfg.Dist
	if dist == "" {
		dist = "dist" // GoReleaser's default
	}
	dist = strings.TrimPrefix(path.Clean(filepath.ToSlash(dist)), "./")
	if path.IsAbs(dist) || strings.HasPrefix(dist, "../") {
		// Outside the checkout: nothing for git to see.
		dist = ""
	}
	var outs []string
	if dist != "" {
		outs = append(outs, dist+"/artifacts.json", dist+"/spawn_linux_amd64_v1/probe")
	}

	for i, n := range cfg.Before.Hooks {
		// A hook is a string or a mapping with `cmd:` (and optionally `dir:`).
		var cmd, dir string
		switch n.Kind {
		case yaml.ScalarNode:
			cmd = n.Value
		case yaml.MappingNode:
			var h struct {
				Cmd string `yaml:"cmd"`
				Dir string `yaml:"dir"`
			}
			if err := n.Decode(&h); err != nil {
				return nil, err
			}
			cmd, dir = h.Cmd, h.Dir
		}
		got, err := hookOutputs(cmd, dir)
		if err != nil {
			return nil, fmt.Errorf(".goreleaser.yaml before.hooks[%d]: %w", i, err)
		}
		outs = append(outs, got...)
	}
	if len(cfg.Before.Hooks) > 0 && len(outs) == 0 {
		return nil, errors.New("found before-hooks but no outputs: the parse has broken")
	}
	return outs, nil
}

// hookOutputs reads the files a before-hook writes: `go build -o X` and
// `zip ... ARCHIVE`, resolved against any `cd DIR` earlier in the same &&-chain.
// A `go build` with no -o writes a binary named after the package into the
// working directory, which this cannot name, so it is an error rather than a
// silent pass.
func hookOutputs(cmd, dir string) ([]string, error) {
	s := strings.TrimSpace(cmd)
	if rest, ok := strings.CutPrefix(s, "sh -c "); ok {
		s = strings.Trim(strings.TrimSpace(rest), `"'`)
	}
	cwd := dir
	var outs []string
	for _, seg := range strings.Split(s, "&&") {
		f := strings.Fields(seg)
		// Drop leading VAR=value environment assignments.
		for len(f) > 0 && strings.Contains(f[0], "=") && !strings.HasPrefix(f[0], "-") {
			f = f[1:]
		}
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "cd" && len(f) == 2:
			cwd = path.Join(cwd, f[1])
		case f[0] == "go" && len(f) > 1 && f[1] == "build":
			o := ""
			for j := 2; j < len(f)-1; j++ {
				if f[j] == "-o" {
					o = f[j+1]
				}
			}
			if o == "" {
				return nil, errors.New("`go build` without -o; cannot tell where it writes: " + strings.TrimSpace(seg))
			}
			outs = append(outs, path.Join(cwd, o))
		case f[0] == "zip":
			a := ""
			for _, x := range f[1:] {
				if !strings.HasPrefix(x, "-") {
					a = x
					break
				}
			}
			if a == "" {
				return nil, errors.New("`zip` with no archive name: " + strings.TrimSpace(seg))
			}
			outs = append(outs, path.Join(cwd, a))
		}
	}
	return outs, nil
}

// probeReleaseTree builds, in a temporary directory, a work tree holding the
// checkout's tracked .gitignore files and a probe file at every release output,
// and returns it with the probe paths. git status then runs against the
// checkout's own index and excludes but this work tree.
//
// The probes must not go into the checkout itself (#1334): `go test ./...`
// runs packages in parallel, and a dozen gates in other packages walk the
// whole tree. Creating and removing dist/ and the before-hook outputs under
// them made those walks fail on entries that vanished between ReadDir and
// Lstat.
func probeReleaseTree(t *testing.T, outputs []string) (string, []string) {
	t.Helper()
	tree := t.TempDir()
	out, err := exec.Command("git", "ls-files", "-z", "--", ":(glob)**/.gitignore").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	ignores := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(ignores) == 0 || ignores[0] == "" {
		t.Fatal("found no tracked .gitignore; the discovery has broken")
	}
	for _, rel := range ignores {
		b, err := os.ReadFile(filepath.FromSlash(rel)) //nolint:gosec // the repo's own tracked file
		if err != nil {
			t.Fatal(err)
		}
		writeProbe(t, filepath.Join(tree, filepath.FromSlash(rel)), b)
	}
	var probes []string
	for _, rel := range outputs {
		// Something already at the path in the checkout (a developer's real
		// dist/, a tracked file) is what gets checked, so carry its content.
		var b []byte
		if fi, err := os.Lstat(filepath.FromSlash(rel)); err == nil && fi.Mode().IsRegular() {
			if b, err = os.ReadFile(filepath.FromSlash(rel)); err != nil { //nolint:gosec // path from the repo's release config
				t.Fatal(err)
			}
		}
		writeProbe(t, filepath.Join(tree, filepath.FromSlash(rel)), b)
		probes = append(probes, rel)
	}
	return tree, probes
}

func writeProbe(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// gitDir is the checkout's git directory (a linked worktree's own, so its
// index is the one consulted).
func gitDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatalf("git rev-parse --absolute-git-dir: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestReleaseTreeProbesStayOutOfTheCheckout holds probeReleaseTree to writing
// nothing into the checkout, the property that keeps the release gate from
// racing every tree-walking test in the other packages (#1334). Every output
// path, and every ancestor directory of one, that was absent before must be
// absent after.
func TestReleaseTreeProbesStayOutOfTheCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git not on PATH in CI; this gate must run there")
		}
		t.Skip("git not available")
	}
	outputs, err := releaseTreeOutputs(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var absent []string
	for _, rel := range outputs {
		for p := filepath.FromSlash(rel); p != "." && p != string(filepath.Separator); p = filepath.Dir(p) {
			if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
				absent = append(absent, p)
			}
		}
	}
	if len(absent) == 0 {
		t.Skip("every release output already exists in this checkout; nothing to observe")
	}
	tree, _ := probeReleaseTree(t, outputs)
	for _, p := range absent {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("probing the release tree created %s in the checkout; probes belong in %s (#1334)", p, tree)
		}
	}
	for _, rel := range outputs {
		if _, err := os.Lstat(filepath.Join(tree, filepath.FromSlash(rel))); err != nil {
			t.Errorf("probe tree has no %s: %v", rel, err)
		}
	}
}
