package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The AWS CLI team's key, as the issue (#1495) and the AWS CLI install guide
// give it: "FB5D B77F D5C1 18B8 0511 ADA8 A631 0ACC 4672 475C". Written out
// here, not read from KeyFingerprint, so changing the constant cannot make the
// test agree with it.
const wantFingerprint = "FB5D B77F D5C1 18B8 0511 ADA8 A631 0ACC 4672 475C"

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

// The committed key is the one the pin procedure trusts.
func TestCommittedKeyIsTheAWSCLITeamKey(t *testing.T) {
	want := strings.ReplaceAll(wantFingerprint, " ", "")
	got, err := keyFingerprint(string(readFile(t, "aws-cli-team.asc")))
	if err != nil {
		t.Fatalf("aws-cli-team.asc: %v", err)
	}
	if got != want {
		t.Errorf("aws-cli-team.asc fingerprint = %s, want %s", got, want)
	}
	if KeyFingerprint != want {
		t.Errorf("KeyFingerprint = %s, want %s", KeyFingerprint, want)
	}
}

// bump-awscli-pin.sh, which runs gpg, must pin the same fingerprint: matched on
// its assignment, not the bare value, so a comment quoting it cannot pass.
func TestBumpScriptPinsTheSameFingerprint(t *testing.T) {
	src := string(readFile(t, repoFile(t, "scripts/bump-awscli-pin.sh")))
	m := regexp.MustCompile(`(?m)^readonly AWSCLI_KEY_FPR=([0-9A-F]{40})$`).FindAllStringSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("bump-awscli-pin.sh: want exactly one `readonly AWSCLI_KEY_FPR=<40 hex>` line, found %d", len(m))
	}
	if m[0][1] != strings.ReplaceAll(wantFingerprint, " ", "") {
		t.Errorf("bump-awscli-pin.sh pins %s, want %s", m[0][1], wantFingerprint)
	}
}

// The fingerprint is computed from the key material, so text that is not a key
// is refused and an altered key does not keep the pinned fingerprint.
func TestKeyFingerprintRejectsOtherInput(t *testing.T) {
	if _, err := keyFingerprint("not a key"); err == nil {
		t.Error("keyFingerprint accepted text with no armor")
	}
	key := string(readFile(t, "aws-cli-team.asc"))
	// One altered base64 character inside the key material.
	i := strings.Index(key, "mQINBF2Cr7UBEADJ")
	if i < 0 {
		t.Fatal("aws-cli-team.asc does not start with the expected key packet")
	}
	altered := key[:i+20] + string(rune(key[i+20]^1)) + key[i+21:]
	if got, err := keyFingerprint(altered); err == nil && got == KeyFingerprint {
		t.Error("an altered key still has the pinned fingerprint")
	}
}

func bootstrapPath(t *testing.T) string { return repoFile(t, "pkg/launcher/bootstrap.go") }

// #1495's gate: the rewrite targets exactly the constants bootstrap.go has.
// Renaming one of them must fail here, not leave the weekly bump moving
// nothing.
func TestPinConstantsExistInBootstrap(t *testing.T) {
	pins, err := readPins(readFile(t, bootstrapPath(t)))
	if err != nil {
		t.Fatalf("pkg/launcher/bootstrap.go: %v", err)
	}
	if !versionRe.MatchString(pins[constVersion].value) {
		t.Errorf("%s = %q, not a 2.N.N release", constVersion, pins[constVersion].value)
	}
	for _, n := range []string{constX8664, constAarch64} {
		if !sha256Re.MatchString(pins[n].value) {
			t.Errorf("%s = %q, not a sha256", n, pins[n].value)
		}
	}
}

// Discover the set rather than list it: every string const in pkg/launcher
// whose value is an AWS CLI pin (a 2.N.N version or a sha256) and whose name
// says AWS CLI must be one the bump moves. A fourth architecture added by hand
// would otherwise never be bumped.
func TestBumpCoversEveryAWSCLIPin(t *testing.T) {
	dir := filepath.Dir(bootstrapPath(t))
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files in %s (%v): discovery has broken", dir, err)
	}
	moved := map[string]bool{}
	for _, n := range pinConsts {
		moved[n] = true
	}
	found := 0
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
					if i >= len(vs.Values) || !strings.HasPrefix(strings.ToLower(id.Name), "awscli") {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, _ := strconv.Unquote(lit.Value)
					if !versionRe.MatchString(v) && !sha256Re.MatchString(v) {
						continue
					}
					found++
					if !moved[id.Name] {
						t.Errorf("%s: const %s = %q looks like an AWS CLI pin the bump does not move; add it to pinConsts",
							filepath.Base(p), id.Name, v)
					}
				}
			}
		}
	}
	if found != len(pinConsts) {
		t.Errorf("found %d AWS CLI pin constants in pkg/launcher, want %d", found, len(pinConsts))
	}
}

var newPins = map[string]string{
	constVersion: "2.40.1",
	constX8664:   strings.Repeat("ab", 32),
	constAarch64: strings.Repeat("cd", 32),
}

// Rewriting the real file changes those three literals and no other byte.
func TestRewriteChangesOnlyThePins(t *testing.T) {
	src := readFile(t, bootstrapPath(t))
	old, err := readPins(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := rewritePins(src, newPins)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readPins(out)
	if err != nil {
		t.Fatalf("rewritten file: %v", err)
	}
	for n, v := range newPins {
		if got[n].value != v {
			t.Errorf("after rewrite %s = %q, want %q", n, got[n].value, v)
		}
	}
	// Undo the three edits; what is left must be the original exactly.
	back := out
	for _, n := range pinConsts {
		back = bytes.Replace(back, []byte(strconv.Quote(newPins[n])), []byte(strconv.Quote(old[n].value)), 1)
	}
	if !bytes.Equal(back, src) {
		t.Error("the rewrite changed bytes other than the three pin literals")
	}
}

func TestRewriteRefusesBadValues(t *testing.T) {
	src := readFile(t, bootstrapPath(t))
	bad := []map[string]string{
		{constVersion: "1.33.0", constX8664: newPins[constX8664], constAarch64: newPins[constAarch64]},
		{constVersion: "2.40.1", constX8664: "ABC", constAarch64: newPins[constAarch64]},
		{constVersion: "2.40.1", constX8664: newPins[constX8664], constAarch64: newPins[constX8664]},
	}
	for _, w := range bad {
		if _, err := rewritePins(src, w); err == nil {
			t.Errorf("rewritePins accepted %v", w)
		}
	}
	renamed := bytes.Replace(src, []byte(constAarch64+" "), []byte("awsCLISHA256arm64 "), 1)
	if _, err := rewritePins(renamed, newPins); err == nil {
		t.Error("rewritePins accepted a bootstrap.go whose aarch64 constant was renamed")
	}
}

// The entry lands in Unreleased under exactly one Changed group, whatever
// groups the section already has.
func TestChangelogEntryGoesUnderUnreleasedChanged(t *testing.T) {
	cases := map[string]string{
		"empty":       "# Changelog\n\n## [Unreleased]\n\n## [0.1.0] - 2026-01-01\n\n### Fixed\n\n- old\n",
		"has changed": "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- a\n\n### Changed\n\n- c\n\n### Fixed\n\n- f\n\n## [0.1.0] - 2026-01-01\n",
		"fixed only":  "# Changelog\n\n## [Unreleased]\n\n### Fixed\n\n- f\n\n## [0.1.0] - 2026-01-01\n",
		"added only":  "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- a\n\n## [0.1.0] - 2026-01-01\n",
		"real (repo)": string(readFile(t, repoFile(t, "CHANGELOG.md"))),
	}
	for name, cl := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := addChangelogEntry([]byte(cl), "2.39.3", newPins)
			if err != nil {
				t.Fatal(err)
			}
			s := string(out)
			unrel := s[strings.Index(s, "## [Unreleased]"):]
			if k := strings.Index(unrel[3:], "\n## ["); k >= 0 {
				unrel = unrel[:k+3]
			}
			if n := strings.Count(unrel, "\n### Changed\n"); n != 1 {
				t.Errorf("Unreleased has %d Changed groups, want 1:\n%s", n, unrel)
			}
			ci := strings.Index(unrel, "### Changed")
			ei := strings.Index(unrel, "AWS CLI 2.40.1 instead of 2.39.3")
			if ei < ci {
				t.Errorf("entry is not under Unreleased's Changed group:\n%s", unrel)
			}
			if f := strings.Index(unrel, "### Fixed"); f >= 0 && (f < ci || ei > f) {
				t.Errorf("Changed must come before Fixed and hold the entry:\n%s", unrel)
			}
			if strings.Contains(unrel, "\n\n\n") {
				t.Errorf("a triple newline in Unreleased:\n%s", unrel)
			}
			// Nothing changed but one inserted block holding the entry.
			block, ok := insertion(strings.Split(cl, "\n"), strings.Split(s, "\n"))
			if !ok || !strings.Contains(strings.Join(block, "\n"), "AWS CLI 2.40.1 instead of 2.39.3") {
				t.Errorf("the CHANGELOG changed other than by one inserted block with the entry (block %q)", block)
			}
		})
	}
}

// insertion reports whether out is in with one contiguous block of lines
// inserted, and returns that block.
func insertion(in, out []string) ([]string, bool) {
	m := len(out) - len(in)
	if m <= 0 {
		return nil, false
	}
	k := 0
	for k < len(in) && in[k] == out[k] {
		k++
	}
	for i := k; i < len(in); i++ {
		if in[i] != out[i+m] {
			return nil, false
		}
	}
	return out[k : k+m], true
}
