package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// updateAPISnapshot regenerates api/public-surface.txt instead of asserting
// against it: `make api-snapshot`, or `go test -run TestPublicAPISurface -update .`
var updateAPISnapshot = flag.Bool("update", false, "rewrite api/public-surface.txt from the current source")

const apiSnapshotPath = "api/public-surface.txt"

// publicPackages names every package in this module that is imported by ANOTHER
// repository, mapped to the repos that import it. Changing an exported symbol in
// one of these is a cross-repo wire-contract change, not a refactor.
//
// This is spawn#679. `pkg/taskproto.GenerateWrapper` gained a parameter in
// v0.111.1 — a PATCH release:
//
//	v0.111.0: func GenerateWrapper(spec *TaskSpec, resultsBucket, region string) string
//	v0.111.1: func GenerateWrapper(spec *TaskSpec, resultsBucket, region string, gpu bool) string
//
// spore-host-mcp called it with three arguments, so its Dependabot bump from
// v0.111.0 to v0.111.1 turned into a red build in a DIFFERENT repository, and sat
// red for six days before anyone connected it to the cause. A consumer following
// SemVer is entitled to treat a patch bump as safe, and Dependabot is configured
// across these repos on exactly that assumption.
//
// The deeper problem was that nothing here knew these packages had outside
// callers at all, so re-shaping an exported symbol looked like ordinary cleanup.
//
// The consumer lists were MEASURED, not assumed — read from each repo's imports
// on 2026-10-08. Worth stating because #679 itself named only taskproto,
// launcher and aws; five of the eight were missing, and a gate built from the
// issue's list would have left them unguarded.
var publicPackages = map[string][]string{
	"pkg/aws":         {"spore-host-mcp", "lagotto", "calque"},
	"pkg/launcher":    {"spore-host-mcp", "lagotto", "calque"},
	"pkg/taskproto":   {"spore-host-mcp"},
	"pkg/ecrref":      {"spore-host-mcp"},
	"pkg/launchererr": {"lagotto"},
	"pkg/taskcohort":  {"calque"},
	"pkg/taskpool":    {"calque"},
	"pkg/storage":     {"calque"},
}

// TestPublicAPISurface fails when the exported surface of a cross-repo package
// changes without the snapshot being updated in the same commit.
//
// It is deliberately NOT a judgement about whether the change is allowed —
// adding to the surface is routine and the right answer is usually "yes, update
// the snapshot." What it buys is that the change becomes VISIBLE in review, on
// the diff, next to the CHANGELOG entry and the version decision, rather than
// surfacing days later as someone else's red CI.
//
// A syntactic snapshot rather than a type-checked one (go/parser, no x/tools, no
// network, no git tags) so it runs in the same conditions as every other gate
// here and cannot fail for an unrelated reason.
func TestPublicAPISurface(t *testing.T) {
	var got bytes.Buffer
	pkgs := make([]string, 0, len(publicPackages))
	for p := range publicPackages {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	got.WriteString("# Exported API surface of spawn's cross-repo packages (spawn#679).\n")
	got.WriteString("# GENERATED — regenerate with `make api-snapshot`. Do not hand-edit.\n")
	got.WriteString("#\n")
	got.WriteString("# A change here is a change to a contract another repo compiles against.\n")
	got.WriteString("# Adding a symbol is backward-compatible; removing one, renaming one, or\n")
	got.WriteString("# changing a signature or an exported struct field is NOT, and pre-1.0 that\n")
	got.WriteString("# means the release bumps MINOR, never PATCH.\n")

	for _, pkg := range pkgs {
		decls, err := exportedDecls(pkg)
		if err != nil {
			t.Fatalf("%s: %v", pkg, err)
		}
		if len(decls) == 0 {
			t.Errorf("%s is listed as a public package but exports nothing — "+
				"either the path is wrong or it should be removed from publicPackages", pkg)
			continue
		}
		fmt.Fprintf(&got, "\n## %s (imported by: %s)\n", pkg, strings.Join(publicPackages[pkg], ", "))
		for _, d := range decls {
			got.WriteString(d + "\n")
		}
	}

	if *updateAPISnapshot {
		if err := os.MkdirAll(filepath.Dir(apiSnapshotPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(apiSnapshotPath, got.Bytes(), 0o644); err != nil {
			t.Fatalf("write snapshot: %v", err)
		}
		t.Logf("wrote %s", apiSnapshotPath)
		return
	}

	want, err := os.ReadFile(apiSnapshotPath)
	if err != nil {
		t.Fatalf("read %s: %v\nRun `make api-snapshot` to create it.", apiSnapshotPath, err)
	}
	if bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got.Bytes())) {
		return
	}

	added, removed := diffLines(string(want), got.String())
	var b strings.Builder
	b.WriteString("the exported API of a cross-repo package changed (spawn#679).\n\n")
	if len(removed) > 0 {
		b.WriteString("REMOVED or CHANGED — breaks a consumer, so the release bumps MINOR, not PATCH:\n")
		for _, l := range removed {
			b.WriteString("  - " + l + "\n")
		}
		b.WriteString("\n")
	}
	if len(added) > 0 {
		b.WriteString("ADDED — backward-compatible:\n")
		for _, l := range added {
			b.WriteString("  + " + l + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("If this is intended, run `make api-snapshot` and commit the result in\n")
	b.WriteString("THIS change, with a CHANGELOG entry. If anything is listed as removed or\n")
	b.WriteString("changed, say so in the entry — a consumer pinned to a patch bump will\n")
	b.WriteString("fail to compile, in their repo, with no link back to here.\n")
	b.WriteString("Consumers today: spore-host-mcp, lagotto, calque.")
	t.Error(b.String())
}

// TestPublicPackagesExist keeps publicPackages honest in the other direction: a
// listed package that no longer exists would silently stop being guarded, and
// the snapshot would still look complete.
func TestPublicPackagesExist(t *testing.T) {
	for pkg := range publicPackages {
		fi, err := os.Stat(pkg)
		if err != nil || !fi.IsDir() {
			t.Errorf("publicPackages lists %s, which is not a directory — "+
				"if it moved, update the entry; if it is gone, that was a breaking "+
				"change for %s", pkg, strings.Join(publicPackages[pkg], ", "))
		}
	}
}

// TestPublicPackagesDocumented keeps CLAUDE.md's public-surface table and
// publicPackages from drifting apart.
//
// CLAUDE.md is what a human (or an agent) reads before deciding whether a change
// is breaking; publicPackages is what CI reads. If they disagree, the one that
// gets believed is the prose, and it would be wrong — so a package added to the
// map without being documented, or documented without being guarded, fails here.
//
// Written because the alternative was a comment in CLAUDE.md asking a reader to
// keep two lists in step by hand, which is the same category of promise that
// produced #679 in the first place.
func TestPublicPackagesDocumented(t *testing.T) {
	b, err := os.ReadFile("CLAUDE.md")
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	doc := string(b)
	for pkg, consumers := range publicPackages {
		if !strings.Contains(doc, "`"+pkg+"`") {
			t.Errorf("CLAUDE.md does not list %s in the public-surface table, but "+
				"publicPackages guards it (imported by %s)", pkg, strings.Join(consumers, ", "))
		}
	}
	// And the reverse: a row in the table naming a package the map does not
	// guard would read as enforced while being invisible to CI.
	for _, row := range regexp.MustCompile("(?m)^\\| `(pkg/[a-z0-9]+)` \\|").FindAllStringSubmatch(doc, -1) {
		if _, ok := publicPackages[row[1]]; !ok {
			t.Errorf("CLAUDE.md's public-surface table lists %s, but publicPackages "+
				"does not — it is documented as enforced and is not", row[1])
		}
	}
}

// exportedDecls returns a sorted, stable rendering of every exported
// declaration in a package directory: functions, methods on exported types,
// types (with their exported struct fields and interface methods), constants
// and variables.
//
// Exported struct FIELDS are included on purpose. Removing or retyping one
// breaks a consumer exactly as surely as changing a function signature, and it
// is the easier of the two to do by accident.
func exportedDecls(dir string) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if s := renderFunc(fset, d); s != "" {
						out = append(out, s)
					}
				case *ast.GenDecl:
					out = append(out, renderGenDecl(fset, d)...)
				}
			}
		}
	}
	sort.Strings(out)
	return dedupe(out), nil
}

// renderFunc renders an exported function or a method on an exported type.
// A method on an unexported receiver is unreachable from another module, so it
// is not part of the contract.
func renderFunc(fset *token.FileSet, d *ast.FuncDecl) string {
	if !d.Name.IsExported() {
		return ""
	}
	sig := strings.TrimPrefix(oneLine(fset, d.Type), "func")
	if d.Recv == nil {
		return "func " + d.Name.Name + sig
	}
	if len(d.Recv.List) == 0 {
		return ""
	}
	recv := oneLine(fset, d.Recv.List[0].Type)
	if !isExportedTypeExpr(d.Recv.List[0].Type) {
		return ""
	}
	return "func (" + recv + ") " + d.Name.Name + sig
}

// renderGenDecl renders exported types, constants and variables.
func renderGenDecl(fset *token.FileSet, d *ast.GenDecl) []string {
	var out []string
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if !s.Name.IsExported() {
				continue
			}
			out = append(out, renderTypeSpec(fset, s)...)
		case *ast.ValueSpec:
			kw := "var"
			if d.Tok == token.CONST {
				kw = "const"
			}
			for _, n := range s.Names {
				if !n.IsExported() {
					continue
				}
				line := kw + " " + n.Name
				if s.Type != nil {
					line += " " + oneLine(fset, s.Type)
				}
				out = append(out, line)
			}
		}
	}
	return out
}

// renderTypeSpec renders an exported type as ONE LINE PER MEMBER rather than one
// line per type.
//
// That shape is the whole point. Rendered as a single blob, adding a struct
// field changes the one line that represents the type, so a set difference sees
// the old line vanish and a new one appear — and the gate reports a
// backward-compatible addition under "breaks a consumer". The first version of
// this test did exactly that, and a gate that cries wolf on a safe change is a
// gate people learn to regenerate without reading, which is worse than no gate.
//
// Per-member lines make additions pure additions and removals pure removals, so
// the two buckets mean what they say.
//
// Unexported fields and methods are omitted throughout: they are invisible to
// another module, so including them would churn the snapshot on purely internal
// edits — the same noise problem from the other direction.
func renderTypeSpec(fset *token.FileSet, s *ast.TypeSpec) []string {
	name := s.Name.Name
	switch t := s.Type.(type) {
	case *ast.StructType:
		out := []string{"type " + name + " struct"}
		return append(out, renderMembers(fset, name, t.Fields, "field")...)
	case *ast.InterfaceType:
		out := []string{"type " + name + " interface"}
		return append(out, renderMembers(fset, name, t.Methods, "method")...)
	default:
		// Alias, named basic type, func type, generic instantiation: no members to
		// decompose, and the definition IS the contract.
		return []string{"type " + name + " " + oneLine(fset, s.Type)}
	}
}

// renderMembers renders the exported fields of a struct or methods of an
// interface, one per line, qualified by the owning type so the lines are
// globally unique within a package.
func renderMembers(fset *token.FileSet, owner string, fl *ast.FieldList, kind string) []string {
	if fl == nil {
		return nil
	}
	var out []string
	for _, f := range fl.List {
		if len(f.Names) == 0 {
			// Embedded field or embedded interface. Part of the surface only when
			// the embedded type is exported, since it promotes that type's own
			// exported members into this one.
			if isExportedTypeExpr(f.Type) {
				out = append(out, kind+" "+owner+".embeds "+oneLine(fset, f.Type))
			}
			continue
		}
		for _, n := range f.Names {
			if !n.IsExported() {
				continue
			}
			// Struct tags are dropped on purpose. A tag change alters JSON/DynamoDB
			// encoding, which matters a great deal, but it is not part of the
			// COMPILE contract this gate guards — and taskproto's wire format has
			// its own round-trip tests for exactly that.
			sig := oneLine(fset, f.Type)
			if kind == "method" {
				sig = strings.TrimPrefix(sig, "func")
			}
			out = append(out, kind+" "+owner+"."+n.Name+" "+sig)
		}
	}
	return out
}

// isExportedTypeExpr reports whether a type expression names an exported type,
// looking through pointers, slices and generic instantiations.
func isExportedTypeExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.IsExported()
	case *ast.StarExpr:
		return isExportedTypeExpr(v.X)
	case *ast.ArrayType:
		return isExportedTypeExpr(v.Elt)
	case *ast.IndexExpr:
		return isExportedTypeExpr(v.X)
	case *ast.IndexListExpr:
		return isExportedTypeExpr(v.X)
	case *ast.SelectorExpr:
		// A qualified type from another package, e.g. ec2.Client — exported by
		// definition, since an unexported one could not be referenced here.
		return true
	}
	return false
}

// oneLine prints an AST node and collapses it to a single line, so a struct that
// is reformatted or has a comment added does not register as an API change.
func oneLine(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return "<unprintable>"
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}

func dedupe(in []string) []string {
	out := in[:0:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// diffLines reports which lines are present in only one of two snapshots.
func diffLines(want, got string) (added, removed []string) {
	w := lineSet(want)
	g := lineSet(got)
	for l := range g {
		if !w[l] {
			added = append(added, l)
		}
	}
	for l := range w {
		if !g[l] {
			removed = append(removed, l)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func lineSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out[l] = true
	}
	return out
}
