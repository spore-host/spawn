package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// golangci-lint is a ratchet here (#1028): .golangci.yml enables only the
// linters that passed when it was committed, and the set may grow but never
// shrink. docs/lint-ratchet.md is the record of that set. These gates make the
// two agree, so dropping a linter (or switching off the gate that runs them)
// fails a test instead of quietly lowering the bar.

const (
	lintConfigPath = ".golangci.yml"
	lintRecordPath = "docs/lint-ratchet.md"
)

type golangciConfig struct {
	Version string `yaml:"version"`
	Linters struct {
		Default    string   `yaml:"default"`
		Enable     []string `yaml:"enable"`
		Disable    []string `yaml:"disable"`
		Exclusions struct {
			Paths []string         `yaml:"paths"`
			Rules []map[string]any `yaml:"rules"`
		} `yaml:"exclusions"`
	} `yaml:"linters"`
	Formatters struct {
		Enable     []string `yaml:"enable"`
		Exclusions struct {
			Paths []string `yaml:"paths"`
		} `yaml:"exclusions"`
	} `yaml:"formatters"`
}

func loadGolangciConfig(t *testing.T) golangciConfig {
	t.Helper()
	var c golangciConfig
	if err := yaml.Unmarshal([]byte(readRepoFile(t, lintConfigPath)), &c); err != nil {
		t.Fatalf("%s is not valid YAML: %v", lintConfigPath, err)
	}
	return c
}

// recordedSet reads the bullet list between `<!-- ratchet:<name>:begin -->` and
// `<!-- ratchet:<name>:end -->` in docs/lint-ratchet.md. Only lines of the exact
// form "- `linter`" count, so prose that merely mentions a linter does not.
func recordedSet(t *testing.T, name string) []string {
	t.Helper()
	doc := readRepoFile(t, lintRecordPath)
	begin, end := "<!-- ratchet:"+name+":begin -->", "<!-- ratchet:"+name+":end -->"
	i, j := strings.Index(doc, begin), strings.Index(doc, end)
	if i < 0 || j < i {
		t.Fatalf("%s has no %s ... %s block", lintRecordPath, begin, end)
	}
	item := regexp.MustCompile("^- `([a-z0-9_]+)`")
	var out []string
	for _, line := range strings.Split(doc[i+len(begin):j], "\n") {
		if m := item.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		// Refusing to pass vacuously: an empty record would accept any config.
		t.Fatalf("%s: the %q block lists no linters — the record is broken, not the repo clean", lintRecordPath, name)
	}
	return out
}

// diffSets reports what is in want but not got, and in got but not want.
func diffSets(want, got []string) (missing, extra []string) {
	w, g := map[string]bool{}, map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	for _, s := range got {
		g[s] = true
	}
	for s := range w {
		if !g[s] {
			missing = append(missing, s)
		}
	}
	for s := range g {
		if !w[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

func TestGolangciConfigOnlyWidens(t *testing.T) {
	c := loadGolangciConfig(t)

	// `default: none` is what makes `enable` the whole set. Under `standard` or
	// `all`, the effective set would depend on golangci-lint's version and on a
	// `disable` list, and comparing `enable` with the record would prove nothing.
	if c.Version != "2" {
		t.Errorf("%s: version = %q, want \"2\" (the schema these gates parse)", lintConfigPath, c.Version)
	}
	if c.Linters.Default != "none" {
		t.Errorf("%s: linters.default = %q, want \"none\" so that linters.enable is the complete set the ratchet records", lintConfigPath, c.Linters.Default)
	}
	if len(c.Linters.Disable) > 0 {
		t.Errorf("%s: linters.disable = %v; with default: none, a linter is off by not being enabled — record it in %s instead", lintConfigPath, c.Linters.Disable, lintRecordPath)
	}
	// Narrowing is done by choosing linters, never by hiding code from them.
	if len(c.Linters.Exclusions.Paths) > 0 || len(c.Formatters.Exclusions.Paths) > 0 {
		t.Errorf("%s: exclusions.paths %v %v exclude code from every linter; fix the findings or leave the linter off", lintConfigPath, c.Linters.Exclusions.Paths, c.Formatters.Exclusions.Paths)
	}
	if len(c.Linters.Exclusions.Rules) > 0 {
		t.Errorf("%s: linters.exclusions.rules is set (%d rules); a rule narrows an enabled linter, and this gate cannot tell how far — scope it in that linter's settings with a comment saying why", lintConfigPath, len(c.Linters.Exclusions.Rules))
	}

	for _, set := range []struct {
		name string
		got  []string
	}{
		{"enabled", c.Linters.Enable},
		{"formatters", c.Formatters.Enable},
	} {
		missing, extra := diffSets(recordedSet(t, set.name), set.got)
		if len(missing) > 0 {
			t.Errorf("%s no longer enables %v, which %s records as part of the floor. The ratchet only widens: fix the findings instead. "+
				"If dropping one is a reviewed decision, remove it from the %q block in %s in the same PR and say why there.",
				lintConfigPath, missing, lintRecordPath, set.name, lintRecordPath)
		}
		if len(extra) > 0 {
			t.Errorf("%s enables %v, which %s does not record. Move them into its %q block (and out of the disabled table) so the floor rises with them.",
				lintConfigPath, extra, lintRecordPath, set.name)
		}
	}
}

// TestLintGateRunsInCI: a ratchet nothing runs is a file, not a gate. CI must
// run `make lint`, that target must run scripts/lint.sh (every module), and the
// golangci-lint version CI installs must be the one the Makefile pins.
func TestLintGateRunsInCI(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("ci.yml is not valid YAML: %v", err)
	}
	var ciVersion string
	var runsLint bool
	installRe := regexp.MustCompile(`(?m)^\s*VER=([0-9][0-9.]*)\s*$`)
	for _, name := range sortedJobNames(wf.Jobs) {
		for _, step := range wf.Jobs[name].Steps {
			run, _ := step["run"].(string)
			if strings.TrimSpace(run) == "make lint" {
				runsLint = true
			}
			if strings.Contains(run, "golangci-lint-${VER}-linux-amd64.tar.gz") {
				if !regexp.MustCompile(`(?m)^\s*echo "\$SHA256  .*" \| sha256sum -c -\s*$`).MatchString(run) {
					t.Errorf("ci.yml job %s installs golangci-lint without verifying a pinned sha256", name)
				}
				if m := installRe.FindStringSubmatch(run); m != nil {
					ciVersion = m[1]
				}
			}
		}
	}
	if !runsLint {
		t.Error("no ci.yml step runs `make lint`, so .golangci.yml gates nothing")
	}
	if ciVersion == "" {
		t.Error("ci.yml has no golangci-lint install step with a `VER=` pin")
	}

	mk := readRepoFile(t, "Makefile")
	m := regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION\s*:?=\s*(\S+)\s*$`).FindStringSubmatch(mk)
	if m == nil {
		t.Fatal("Makefile does not pin GOLANGCI_LINT_VERSION")
	}
	if m[1] != ciVersion {
		t.Errorf("Makefile pins golangci-lint %s but ci.yml installs %s; local and CI findings would differ", m[1], ciVersion)
	}
	// The recipe lines of the `lint:` target, up to the next blank line or target.
	target := regexp.MustCompile(`(?ms)^lint:\n((?:\t[^\n]*\n|[^\n]*\\\n)*)`).FindStringSubmatch(mk)
	if target == nil || !regexp.MustCompile(`(?m)^\t@?scripts/lint\.sh\s*$`).MatchString(target[1]) {
		t.Error("the Makefile's lint target does not run scripts/lint.sh, so CI's `make lint` would not lint every module")
	}
}
