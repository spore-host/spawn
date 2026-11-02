// Command awsclipin reads and moves the AWS CLI v2 release the Linux bootstrap
// installs on an image with no aws CLI (#1490): the three constants
// awsCLIVersion, awsCLISHA256x86_64 and awsCLISHA256aarch64 in
// pkg/launcher/bootstrap.go (#1495).
//
// It is the rewrite half of scripts/bump-awscli-pin.sh, which the weekly
// .github/workflows/awscli-pin.yml runs. That script finds the newest release,
// downloads both installer zips and their .sig files, verifies the signatures
// against the AWS CLI team key committed beside this file, and computes the
// sha256s. This tool then only edits Go source and the CHANGELOG, so the parts
// a test can check offline are in Go:
//
//	go -C scripts run ./awsclipin show   --bootstrap ../pkg/launcher/bootstrap.go
//	go -C scripts run ./awsclipin set    --bootstrap ../pkg/launcher/bootstrap.go \
//	    --changelog ../CHANGELOG.md --version 2.40.0 --sha256-x86_64 <hex> --sha256-aarch64 <hex>
//
// The constants are found with go/parser, not a regular expression, and each
// must be declared exactly once as a string literal. Renaming one fails here,
// and in this tool's test, instead of making the bump a silent no-op.
//
// Moving the pin changes the published bootstrap body, so the CHANGELOG entry
// goes under Changed and says to deploy the sweep orchestrator.
package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The constants the bump moves, in the order `show` prints them.
const (
	constVersion = "awsCLIVersion"
	constX8664   = "awsCLISHA256x86_64"
	constAarch64 = "awsCLISHA256aarch64"
)

var pinConsts = []string{constVersion, constX8664, constAarch64}

// KeyFingerprint is the AWS CLI team's PGP key, as the AWS CLI install guide
// publishes it (https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html).
// The key material is aws-cli-team.asc beside this file; the test checks its
// fingerprint is this one, and bump-awscli-pin.sh accepts a signature only
// from it.
const KeyFingerprint = "FB5DB77FD5C118B80511ADA8A6310ACC4672475C"

var (
	versionRe = regexp.MustCompile(`^2\.\d+\.\d+$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if !errors.Is(err, errNotRefused) {
			fmt.Fprintln(os.Stderr, "awsclipin:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: awsclipin show|set|pr-refused|issue-title|issue-body [flags]")
	}
	switch args[0] {
	case "pr-refused", "issue-title", "issue-body":
		return runFallback(args, stdin, stdout)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	bootstrap := fs.String("bootstrap", "pkg/launcher/bootstrap.go", "path to pkg/launcher/bootstrap.go")
	changelog := fs.String("changelog", "CHANGELOG.md", "path to CHANGELOG.md (set only)")
	version := fs.String("version", "", "new AWS CLI v2 version (set only)")
	x8664 := fs.String("sha256-x86_64", "", "sha256 of awscli-exe-linux-x86_64-<version>.zip (set only)")
	aarch64 := fs.String("sha256-aarch64", "", "sha256 of awscli-exe-linux-aarch64-<version>.zip (set only)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	src, err := os.ReadFile(*bootstrap)
	if err != nil {
		return err
	}
	pins, err := readPins(src)
	if err != nil {
		return fmt.Errorf("%s: %w", *bootstrap, err)
	}
	switch args[0] {
	case "show":
		for _, name := range pinConsts {
			fmt.Fprintf(stdout, "%s=%s\n", name, pins[name].value)
		}
		return nil
	case "set":
		want := map[string]string{constVersion: *version, constX8664: *x8664, constAarch64: *aarch64}
		out, err := rewritePins(src, want)
		if err != nil {
			return err
		}
		cl, err := os.ReadFile(*changelog)
		if err != nil {
			return err
		}
		cl, err = addChangelogEntry(cl, pins[constVersion].value, want)
		if err != nil {
			return fmt.Errorf("%s: %w", *changelog, err)
		}
		if err := os.WriteFile(*bootstrap, out, 0o644); err != nil { //nolint:gosec // a tracked source file
			return err
		}
		return os.WriteFile(*changelog, cl, 0o644) //nolint:gosec // a tracked source file
	default:
		return fmt.Errorf("unknown subcommand %q (want show, set, pr-refused, issue-title or issue-body)", args[0])
	}
}

// pin is one constant: its value and the byte span of its string literal.
type pin struct {
	value      string
	start, end int
}

// readPins finds each of pinConsts in src. Each must be declared exactly once,
// as a const, with a plain string literal value.
func readPins(src []byte) (map[string]pin, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bootstrap.go", src, 0)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range pinConsts {
		want[n] = true
	}
	pins := map[string]pin{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if !want[id.Name] {
					continue
				}
				if _, dup := pins[id.Name]; dup {
					return nil, fmt.Errorf("%s is declared twice", id.Name)
				}
				if i >= len(vs.Values) {
					return nil, fmt.Errorf("%s has no value", id.Name)
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return nil, fmt.Errorf("%s is not a string literal", id.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					return nil, err
				}
				pins[id.Name] = pin{value: v, start: fset.Position(lit.Pos()).Offset, end: fset.Position(lit.End()).Offset}
			}
		}
	}
	for _, n := range pinConsts {
		if _, ok := pins[n]; !ok {
			return nil, fmt.Errorf("no const %s: the bump would move nothing (renamed? update scripts/awsclipin)", n)
		}
	}
	return pins, nil
}

// validate checks the new values before anything is written.
func validate(want map[string]string) error {
	if !versionRe.MatchString(want[constVersion]) {
		return fmt.Errorf("--version %q is not an AWS CLI v2 release (2.N.N)", want[constVersion])
	}
	for _, n := range []string{constX8664, constAarch64} {
		if !sha256Re.MatchString(want[n]) {
			return fmt.Errorf("%s %q is not a lowercase hex sha256", n, want[n])
		}
	}
	if want[constX8664] == want[constAarch64] {
		return errors.New("the x86_64 and aarch64 sha256s are the same: one zip was hashed twice")
	}
	return nil
}

// rewritePins replaces the three literals in src and nothing else.
func rewritePins(src []byte, want map[string]string) ([]byte, error) {
	if err := validate(want); err != nil {
		return nil, err
	}
	pins, err := readPins(src)
	if err != nil {
		return nil, err
	}
	// Splice from the last literal back, so earlier offsets stay valid.
	order := append([]string(nil), pinConsts...)
	sort.Slice(order, func(i, j int) bool { return pins[order[i]].start > pins[order[j]].start })
	out := append([]byte(nil), src...)
	for _, n := range order {
		p := pins[n]
		out = append(out[:p.start:p.start], append([]byte(strconv.Quote(want[n])), out[p.end:]...)...)
	}
	return out, nil
}

// afterChanged: a new "### Changed" group goes before the first of these.
var afterChanged = map[string]bool{"Deprecated": true, "Removed": true, "Fixed": true, "Security": true, "Documentation": true}

// addChangelogEntry adds the bump's entry to the `## [Unreleased]` section,
// under its `### Changed` group (made if absent).
func addChangelogEntry(cl []byte, oldVersion string, want map[string]string) ([]byte, error) {
	entry := fmt.Sprintf("- **The Linux bootstrap installs AWS CLI %[2]s instead of %[1]s on an image with no aws CLI** (#1495). "+
		"This changes the published bootstrap body, so deploy the sweep orchestrator from the release that ships it, as the deploy checklist says. "+
		"Both installer zips' signatures verified against the AWS CLI team's PGP key (`%[3]s`) before their sha256s were pinned: "+
		"x86_64 `%[4]s`, aarch64 `%[5]s`. Opened by the weekly AWS CLI pin workflow.",
		oldVersion, want[constVersion], KeyFingerprint, want[constX8664], want[constAarch64])

	lines := strings.Split(string(cl), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "## [Unreleased]" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, errors.New("no `## [Unreleased]` section")
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	changed, later := -1, -1
	for i := start + 1; i < end; i++ {
		g, ok := strings.CutPrefix(lines[i], "### ")
		if !ok {
			continue
		}
		switch g = strings.TrimSpace(g); {
		case g == "Changed" && changed < 0:
			changed = i
		case afterChanged[g] && later < 0:
			later = i
		}
	}
	var at int
	var add []string
	switch {
	case changed >= 0: // first in the existing group, after its blank line
		at = changed + 1
		for at < end && strings.TrimSpace(lines[at]) == "" {
			at++
		}
		add = []string{entry, ""}
	case later >= 0: // a new group, before the first group that follows Changed
		at = later
		add = []string{"### Changed", "", entry, ""}
	default: // a new group, after the section's last non-blank line
		at = end
		for at > start+1 && strings.TrimSpace(lines[at-1]) == "" {
			at--
		}
		add = []string{"", "### Changed", "", entry}
	}
	out := append([]string(nil), lines[:at]...)
	out = append(out, add...)
	out = append(out, lines[at:]...)
	return []byte(strings.Join(out, "\n")), nil
}
