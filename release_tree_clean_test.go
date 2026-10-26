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

	var probes []string
	for _, p := range outputs {
		probes = append(probes, createProbe(t, p))
	}

	t.Logf("probing %v", probes)
	args := append([]string{"status", "--porcelain", "--untracked-files=all", "--"}, probes...)
	out, err := exec.Command("git", args...).CombinedOutput()
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

// createProbe makes an empty file at rel (unless something is already there,
// which is then what gets checked), and registers cleanup that removes only
// what it created, so a developer's real dist/ or build output survives.
func createProbe(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.FromSlash(rel)
	if _, err := os.Lstat(p); err == nil {
		return rel
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	// The outermost missing ancestor is what to remove afterwards.
	top := p
	for d := filepath.Dir(p); d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		top = d
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(top) })
	return rel
}
