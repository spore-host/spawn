package main

import (
	"bufio"
	"go/build/constraint"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"

	"gopkg.in/yaml.v3"
)

// The Windows CI job (#971) is wiring: a ci.yml job, a script and a skip list.
// Each piece is trivially deletable and its absence is silent — Windows-only
// tests simply go back to being compiled and never run, which is how spawn
// shipped a Windows spored whose runtime readings (WMI/perf-counter names,
// PowerShell parsing) had only ever been checked by reading the code (#919,
// #937). These gates make removing or hollowing out the job fail a test.

const (
	windowsTestScript = "scripts/windows-tests.sh"
	windowsSkipList   = "scripts/windows-test-skips.txt"
)

type ciWorkflow struct {
	Env  map[string]any   `yaml:"env"`
	Jobs map[string]ciJob `yaml:"jobs"`
}

type ciJob struct {
	RunsOn          any              `yaml:"runs-on"`
	ContinueOnError any              `yaml:"continue-on-error"`
	Env             map[string]any   `yaml:"env"`
	Steps           []map[string]any `yaml:"steps"`
}

// runners normalises runs-on, which may be a string or a list of labels.
func (j ciJob) runners() []string {
	switch v := j.RunsOn.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (j ciJob) onWindows() bool {
	for _, r := range j.runners() {
		if strings.HasPrefix(strings.ToLower(r), "windows") {
			return true
		}
	}
	return false
}

// windowsJobs returns the ci.yml jobs that run on a Windows runner, failing if
// there are none.
func windowsJobs(t *testing.T) (ciWorkflow, map[string]ciJob) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("ci.yml is not valid YAML: %v", err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatal("parsed no jobs out of ci.yml — the parse has broken, not the workflow")
	}
	out := map[string]ciJob{}
	for name, j := range wf.Jobs {
		if j.onWindows() {
			out[name] = j
		}
	}
	if len(out) == 0 {
		t.Fatalf("no ci.yml job runs on a Windows runner (#971). spawn ships a Windows "+
			"spored; without one its Windows-only tests are compiled and never run. "+
			"Jobs found: %v", sortedJobNames(wf.Jobs))
	}
	return wf, out
}

func sortedJobNames(m map[string]ciJob) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The script must be INVOKED at command position, not mentioned: a comment or
// an echo naming it would otherwise satisfy the gate (CLAUDE.md rule 3).
var invokesWindowsScript = regexp.MustCompile(`(?m)^\s*(bash\s+)?(\./)?scripts/windows-tests\.sh(\s|$)`)

// TestCIRunsUnitTestsOnWindows: a job on a Windows runner runs the script, and
// nothing lets it go green while failing.
func TestCIRunsUnitTestsOnWindows(t *testing.T) {
	_, jobs := windowsJobs(t)

	running := 0
	for name, j := range jobs {
		if b, ok := j.ContinueOnError.(bool); ok && b {
			t.Errorf("job %s has continue-on-error: true, so a Windows test failure "+
				"cannot fail CI — indistinguishable from not running", name)
		}
		for _, s := range j.Steps {
			run, _ := s["run"].(string)
			if !invokesWindowsScript.MatchString(run) {
				continue
			}
			running++
			if b, ok := s["continue-on-error"].(bool); ok && b {
				t.Errorf("job %s runs %s with continue-on-error: true, so its failures "+
					"never fail CI", name, windowsTestScript)
			}
		}
	}
	if running == 0 {
		t.Errorf("no Windows job runs %s; the job exists but tests nothing", windowsTestScript)
	}

	// And the script must actually run the short unit tests, over the package
	// list it computes — not, say, `go vet` or `go build`.
	script, err := os.ReadFile(windowsTestScript)
	if err != nil {
		t.Fatalf("read %s: %v", windowsTestScript, err)
	}
	if !regexp.MustCompile(`(?m)^\s*go test -short\b.*"\$\{pkgs\[@\]\}"`).Match(script) {
		t.Errorf("%s does not run `go test -short ... \"${pkgs[@]}\"`; the Windows job "+
			"must execute the discovered packages' unit tests", windowsTestScript)
	}
	if !strings.Contains(string(script), windowsSkipList) {
		t.Errorf("%s does not read %s, so the skip list this test validates is not the "+
			"one the job uses", windowsTestScript, windowsSkipList)
	}
}

// awsCredentialEnv matches environment variables that would hand the job AWS
// credentials or a profile to find them with.
var awsCredentialEnv = regexp.MustCompile(`^AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN|PROFILE|ROLE_ARN|WEB_IDENTITY_TOKEN_FILE|SHARED_CREDENTIALS_FILE|CONFIG_FILE)$`)

// TestWindowsCIJobHasNoAWS: the job is unit tests only (#971). A test that
// needed AWS would pass here only if credentials were supplied, and supplying
// them to a job running every pull request's code is exactly what must not
// happen. Checked at workflow, job and step level, plus any credentials action.
func TestWindowsCIJobHasNoAWS(t *testing.T) {
	wf, jobs := windowsJobs(t)

	checkEnv := func(where string, env map[string]any) {
		for k, v := range env {
			if awsCredentialEnv.MatchString(k) {
				t.Errorf("%s sets %s; the Windows job must run without AWS credentials", where, k)
			}
			if s, ok := v.(string); ok && strings.Contains(s, "secrets.") {
				t.Errorf("%s passes a secret into %s; the Windows unit tests need none", where, k)
			}
		}
	}
	checkEnv("ci.yml's workflow-level env", wf.Env)
	for name, j := range jobs {
		checkEnv("job "+name, j.Env)
		for i, s := range j.Steps {
			where := "job " + name + " step " + stepLabel(s, i)
			if env, ok := s["env"].(map[string]any); ok {
				checkEnv(where, env)
			}
			if uses, _ := s["uses"].(string); strings.Contains(uses, "configure-aws-credentials") {
				t.Errorf("%s uses %s; the Windows job must not obtain AWS credentials", where, uses)
			}
		}
	}
}

func stepLabel(s map[string]any, i int) string {
	if n, ok := s["name"].(string); ok {
		return n
	}
	return "#" + strconv.Itoa(i+1)
}

// windowsSkips parses the skip list into package dir -> reason.
func windowsSkips(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(windowsSkipList)
	if err != nil {
		t.Fatalf("open %s: %v", windowsSkipList, err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pkg, reason := line, ""
		if i := strings.IndexAny(line, " \t"); i >= 0 {
			pkg, reason = line[:i], strings.TrimSpace(line[i:])
		}
		if _, dup := out[pkg]; dup {
			t.Errorf("%s:%d lists %s twice", windowsSkipList, n, pkg)
		}
		out[pkg] = reason
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", windowsSkipList, err)
	}
	return out
}

// TestWindowsSkipListIsJustified: every skip names a real package and cites an
// issue, so a skip is tracked debt rather than a forgotten decision, and a stale
// entry (package moved or deleted) fails instead of quietly matching nothing.
func TestWindowsSkipListIsJustified(t *testing.T) {
	issueRef := regexp.MustCompile(`#\d+`)
	for pkg, reason := range windowsSkips(t) {
		if !issueRef.MatchString(reason) {
			t.Errorf("%s skips %s without citing an issue (#NNN): %q", windowsSkipList, pkg, reason)
		}
		if !hasGoFiles(filepath.FromSlash(pkg)) {
			t.Errorf("%s skips %s, which is not a Go package in this module; remove the "+
				"stale entry", windowsSkipList, pkg)
		}
	}
}

func hasGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// TestWindowsJobRunsWindowsOnlyTests: the job cannot pass vacuously. Every
// root-module test file that builds only on Windows is discovered (by filename
// suffix or //go:build constraint, never by a list), there must be at least one,
// and none may sit in a skipped package — otherwise the job would be green
// while running none of the tests it exists to run. The issue's minimum,
// pkg/agent and cmd/spored, must not be skipped either.
func TestWindowsJobRunsWindowsOnlyTests(t *testing.T) {
	skips := windowsSkips(t)

	for _, must := range []string{"pkg/agent", "cmd/spored"} {
		if reason, ok := skips[must]; ok {
			t.Errorf("%s skips %s (%s); the Windows job exists to test the Windows "+
				"spored, which lives there (#971)", windowsSkipList, must, reason)
		}
	}

	files := windowsOnlyTestFiles(t)
	if len(files) == 0 {
		t.Fatal("found no Windows-only _test.go files in the root module — discovery has " +
			"broken, or the Windows job now runs no Windows-specific test and passes vacuously")
	}
	for _, f := range files {
		dir := filepath.ToSlash(filepath.Dir(f))
		if reason, ok := skips[dir]; ok {
			t.Errorf("%s is a Windows-only test, but its package %s is skipped on Windows "+
				"(%s), so it never runs anywhere", f, dir, reason)
		}
	}
}

// windowsOnlyTestFiles walks the root module (not nested modules, which the
// Windows job does not run) for _test.go files that build on windows and not on
// linux.
func windowsOnlyTestFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if treewalk.Vanished(".", path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != "." && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // nested module
				}
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		only, err := windowsOnly(path)
		if err != nil {
			return err
		}
		if only {
			out = append(out, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for Windows-only tests: %v", err)
	}
	sort.Strings(out)
	return out
}

var knownArch = map[string]bool{"386": true, "amd64": true, "arm": true, "arm64": true}

// windowsOnly reports whether a Go file builds for GOOS=windows but not for
// GOOS=linux: a _windows filename suffix, or a //go:build line that does.
// Custom tags evaluate false, so `windows && integration` does not count — the
// job runs untagged.
func windowsOnly(path string) (bool, error) {
	base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".go"), "_test")
	parts := strings.Split(base, "_")
	n := len(parts)
	if n >= 2 && parts[n-1] == "windows" {
		return true, nil
	}
	if n >= 3 && parts[n-2] == "windows" && knownArch[parts[n-1]] {
		return true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return false, err
		}
		win := expr.Eval(func(tag string) bool { return tag == "windows" })
		lin := expr.Eval(func(tag string) bool { return tag == "linux" || tag == "unix" })
		return win && !lin, nil
	}
	return false, sc.Err()
}
