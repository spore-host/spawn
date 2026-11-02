package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/scripts/internal/pinkit"
)

// AWS's EFA installer key, as the issue (#1505) and the EC2 User Guide
// (https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-start.html) give
// it. Written out here, not read from KeyFingerprint, so changing the constant
// cannot make the test agree with it.
const wantFingerprint = "4E90 91BC BB97 A96B 26B1 5E59 A054 80B1 DD2D 3CCC"

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join("..", "..", filepath.FromSlash(rel))
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return p
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // the repo's own files
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func efaGoPath(t *testing.T) string { return repoFile(t, "pkg/userdata/efa.go") }

// The committed key is the one AWS documents, by its computed fingerprint.
func TestCommittedKeyIsAWSsEFAInstallerKey(t *testing.T) {
	want := strings.ReplaceAll(wantFingerprint, " ", "")
	got, err := pinkit.KeyFingerprint(string(readFile(t, "aws-efa-installer.key")))
	if err != nil {
		t.Fatalf("aws-efa-installer.key: %v", err)
	}
	if got != want {
		t.Errorf("aws-efa-installer.key fingerprint = %s, want %s", got, want)
	}
	if KeyFingerprint != want {
		t.Errorf("KeyFingerprint = %s, want %s", KeyFingerprint, want)
	}
}

// efa-pin.sh, which runs gpg, must pin the same fingerprint: matched on its
// assignment, not the bare value, so a comment quoting it cannot pass.
func TestPinScriptPinsTheSameFingerprint(t *testing.T) {
	src := string(readFile(t, repoFile(t, "scripts/efa-pin.sh")))
	m := regexp.MustCompile(`(?m)^readonly EFA_KEY_FPR=([0-9A-F]{40})$`).FindAllStringSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("efa-pin.sh: want exactly one `readonly EFA_KEY_FPR=<40 hex>` line, found %d", len(m))
	}
	if m[0][1] != strings.ReplaceAll(wantFingerprint, " ", "") {
		t.Errorf("efa-pin.sh pins %s, want %s", m[0][1], wantFingerprint)
	}
	k := regexp.MustCompile(`(?m)^readonly KEY_FILE=(\S+)$`).FindStringSubmatch(src)
	if k == nil || k[1] != "scripts/efapin/aws-efa-installer.key" {
		t.Errorf("efa-pin.sh KEY_FILE = %v, want scripts/efapin/aws-efa-installer.key", k)
	}
}

// The rewrite targets exactly the constants efa.go has: renaming one must fail
// here, not leave the weekly bump moving nothing.
func TestPinConstantsExistInEFAGo(t *testing.T) {
	pins, err := readPins(readFile(t, efaGoPath(t)))
	if err != nil {
		t.Fatalf("pkg/userdata/efa.go: %v", err)
	}
	if err := validate(pins[constVersion].Value, pins[constSHA256].Value); err != nil {
		t.Errorf("the committed pin: %v", err)
	}
}

// Discover the set rather than list it: every string const in pkg/userdata
// whose name says EFA and whose value is a version or a sha256 must be one the
// bump moves (a second, per-distribution pin added by hand would otherwise
// never be bumped or verified).
func TestBumpCoversEveryEFAPin(t *testing.T) {
	dir := filepath.Dir(efaGoPath(t))
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files in %s (%v): discovery has broken", dir, err)
	}
	moved := map[string]bool{}
	for _, n := range pinConsts {
		moved[n] = true
	}
	anyVersion := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	found := 0
	fset := token.NewFileSet()
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i >= len(vs.Values) || !strings.Contains(strings.ToLower(id.Name), "efa") {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, _ := strconv.Unquote(lit.Value)
					if !anyVersion.MatchString(v) && !sha256Re.MatchString(v) {
						continue
					}
					found++
					if !moved[id.Name] {
						t.Errorf("%s: const %s = %q looks like an EFA installer pin the bump does not move; add it to pinConsts",
							filepath.Base(p), id.Name, v)
					}
				}
			}
		}
	}
	if found != len(pinConsts) {
		t.Errorf("found %d EFA installer pin constants in pkg/userdata, want %d", found, len(pinConsts))
	}
}

var newSum = strings.Repeat("ab", 32)

// Rewriting the real file changes those two literals and no other byte.
func TestRewriteChangesOnlyThePins(t *testing.T) {
	src := readFile(t, efaGoPath(t))
	old, err := readPins(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := rewritePins(src, "1.99.0", newSum)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readPins(out)
	if err != nil {
		t.Fatalf("rewritten file: %v", err)
	}
	if got[constVersion].Value != "1.99.0" || got[constSHA256].Value != newSum {
		t.Errorf("after rewrite: %+v", got)
	}
	back := bytes.Replace(out, []byte(strconv.Quote("1.99.0")), []byte(strconv.Quote(old[constVersion].Value)), 1)
	back = bytes.Replace(back, []byte(strconv.Quote(newSum)), []byte(strconv.Quote(old[constSHA256].Value)), 1)
	if !bytes.Equal(back, src) {
		t.Error("the rewrite changed bytes other than the two pin literals")
	}
}

func TestRewriteRefusesBadValues(t *testing.T) {
	src := readFile(t, efaGoPath(t))
	for _, c := range [][2]string{{"2.40.0", newSum}, {"latest", newSum}, {"1.52.0", "ABC"}, {"1.52.0", strings.ToUpper(newSum)}} {
		if _, err := rewritePins(src, c[0], c[1]); err == nil {
			t.Errorf("rewritePins accepted %v", c)
		}
	}
	renamed := bytes.Replace(src, []byte(constSHA256+" "), []byte("efaInstallerSum "), 1)
	if _, err := rewritePins(renamed, "1.52.0", newSum); err == nil {
		t.Error("rewritePins accepted an efa.go whose sha256 constant was renamed")
	}
}

// The set subcommand edits efa.go and puts the entry under Unreleased's Changed.
func TestSetEditsTheSourceAndTheChangelog(t *testing.T) {
	dir := t.TempDir()
	src, cl := filepath.Join(dir, "efa.go"), filepath.Join(dir, "CHANGELOG.md")
	if err := os.WriteFile(src, readFile(t, efaGoPath(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cl, []byte("# Changelog\n\n## [Unreleased]\n\n### Fixed\n\n- f\n\n## [0.1.0] - 2026-01-01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, _ := readPins(readFile(t, src))
	if err := run([]string{"set", "--source", src, "--changelog", cl, "--version", "1.99.0", "--sha256", newSum}, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"show", "--source", src}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if want := "efaInstallerVersion=1.99.0\nefaInstallerSHA256=" + newSum + "\n"; out.String() != want {
		t.Errorf("show after set = %q, want %q", out.String(), want)
	}
	got := string(readFile(t, cl))
	entry := "- **`--efa` installs EFA installer 1.99.0 instead of " + old[constVersion].Value + "**"
	ci, ei, fi := strings.Index(got, "### Changed"), strings.Index(got, entry), strings.Index(got, "### Fixed")
	if ci < 0 || ei < ci || fi < ei || !strings.Contains(got, newSum) || !strings.Contains(got, KeyFingerprint) {
		t.Errorf("CHANGELOG after set:\n%s", got)
	}
}

const ghRefusal = "pull request create failed: GraphQL: GitHub Actions is not permitted to create or approve pull requests (createPullRequest)"

func TestPRRefusedSubcommand(t *testing.T) {
	if err := run([]string{"pr-refused"}, strings.NewReader(ghRefusal), &bytes.Buffer{}); err != nil {
		t.Errorf("pr-refused on GitHub's refusal: %v, want nil", err)
	}
	for _, out := range []string{"", "HTTP 401: Bad credentials", "GitHub Actions is not permitted to push to this branch"} {
		if err := run([]string{"pr-refused"}, strings.NewReader(out), &bytes.Buffer{}); !errors.Is(err, errNotRefused) {
			t.Errorf("pr-refused on %q: %v, want errNotRefused", out, err)
		}
	}
}

// The fallback issue holds what the owner needs to open the PR.
func TestFallbackIssueHoldsTheVerifiedPin(t *testing.T) {
	compare := "https://github.com/spore-host/spawn/compare/main...bot/efa-installer-1.52.0?expand=1"
	var title, body bytes.Buffer
	if err := run([]string{"issue-title", "--from", "1.51.0", "--version", "1.52.0"}, nil, &title); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(title.String()); got != "EFA installer pin bump available: 1.51.0 → 1.52.0" {
		t.Errorf("title = %q", got)
	}
	if err := run([]string{"issue-body", "--from", "1.51.0", "--version", "1.52.0", "--sha256", newSum, "--compare", compare}, nil, &body); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{compare, "`1.52.0`", newSum, KeyFingerprint, "Open the PR from this link"} {
		if !strings.Contains(body.String(), want) {
			t.Errorf("issue body lacks %q:\n%s", want, body.String())
		}
	}
	for _, bad := range [][]string{
		{"issue-body", "--from", "1.51.0", "--version", "1.52.0", "--sha256", "", "--compare", compare},
		{"issue-body", "--from", "1.51.0", "--version", "1.52.0", "--sha256", newSum, "--compare", ""},
	} {
		if err := run(bad, nil, &bytes.Buffer{}); err == nil {
			t.Errorf("issue-body accepted %v", bad)
		}
	}
}
