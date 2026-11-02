// Package pinkit is what the pin rewriters (scripts/awsclipin for the AWS CLI,
// #1495; scripts/efapin for the EFA installer, #1505) share: finding and
// rewriting named string constants in Go source with go/parser, adding a
// CHANGELOG entry under Unreleased's Changed group, and computing a PGP key's
// fingerprint without gpg. None of it calls the network or AWS.
package pinkit

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// Const is one string constant: its value and the byte span of its literal.
type Const struct {
	Value      string
	Start, End int
}

// ReadConsts finds each of names in src. Each must be declared exactly once,
// as a const, with a plain string literal value; otherwise the rewrite would
// move nothing, so it is an error naming tool.
func ReadConsts(src []byte, file, tool string, names []string) (map[string]Const, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := map[string]Const{}
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
				if _, dup := out[id.Name]; dup {
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
				out[id.Name] = Const{Value: v, Start: fset.Position(lit.Pos()).Offset, End: fset.Position(lit.End()).Offset}
			}
		}
	}
	for _, n := range names {
		if _, ok := out[n]; !ok {
			return nil, fmt.Errorf("no const %s: the bump would move nothing (renamed? update %s)", n, tool)
		}
	}
	return out, nil
}

// RewriteConsts replaces the literals of want's constants in src and nothing
// else. The caller validates the values first.
func RewriteConsts(src []byte, file, tool string, want map[string]string) ([]byte, error) {
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	consts, err := ReadConsts(src, file, tool, names)
	if err != nil {
		return nil, err
	}
	// Splice from the last literal back, so earlier offsets stay valid.
	sort.Slice(names, func(i, j int) bool { return consts[names[i]].Start > consts[names[j]].Start })
	out := append([]byte(nil), src...)
	for _, n := range names {
		c := consts[n]
		out = append(out[:c.Start:c.Start], append([]byte(strconv.Quote(want[n])), out[c.End:]...)...)
	}
	return out, nil
}

// afterChanged: a new "### Changed" group goes before the first of these.
var afterChanged = map[string]bool{"Deprecated": true, "Removed": true, "Fixed": true, "Security": true, "Documentation": true}

// AddUnreleasedChanged adds entry (one "- ..." line) to the `## [Unreleased]`
// section of a Keep a Changelog file, first under its `### Changed` group,
// which it makes, in the right place, if absent.
func AddUnreleasedChanged(cl []byte, entry string) ([]byte, error) {
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
