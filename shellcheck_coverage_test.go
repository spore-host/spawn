package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryShellGeneratorIsShellchecked is the discovery half of the
// generated-script shellcheck gate (#689 part C).
//
// The checking half is internal/shellcheckgate.Check, called from each
// generator's own tests with the generator's id. A gate over a hand-written
// list of generators would pass for every one the list forgot (CLAUDE.md,
// "Writing a gate", rule 4), so this test finds the generators itself: any
// function (or package-level var/const) in non-test Go source holding a string
// literal with a shell shebang at the start of a line is a script generator,
// and some test must call shellcheckgate.Check naming it.
//
// Two further checks keep the coverage honest: a Check id must name a
// declaration that exists (so a renamed generator cannot leave a stale id
// behind looking like coverage), and discovery must find something on both
// sides, because a walk that found nothing has broken, not proven the repo
// clean.
//
// Generators without a shebang (fragments such as pkg/userdata's storage and
// MPI scripts, pkg/sporedinstall's installer) cannot be discovered this way;
// they are checked as part of the scripts they are spliced into, and named
// directly where they have their own Check call.
func TestEveryShellGeneratorIsShellchecked(t *testing.T) {
	emitters, decls, covered := scanShellGenerators(t, ".")

	if len(emitters) == 0 {
		t.Fatal("discovered no shell-script generators; the source walk has broken")
	}
	if len(covered) == 0 {
		t.Fatal("found no shellcheckgate.Check calls; the test walk has broken")
	}

	var missing []string
	for id, where := range emitters {
		if _, ok := covered[id]; !ok {
			missing = append(missing, id+"  ("+where+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("shell-script generator %s is not shellchecked: render it in a test and pass it to "+
			"shellcheckgate.Check(t, %q, script)", m, strings.SplitN(m, "  ", 2)[0])
	}

	var stale []string
	for id, where := range covered {
		if !decls[id] {
			stale = append(stale, id+"  ("+where+")")
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("shellcheckgate.Check names %s, which is not a declaration in this repo; "+
			"the id must be \"<package dir>.<func>\" of the generator it checks", s)
	}
}

// shebangLine matches a shell shebang at the start of a line inside a literal.
var shebangLine = regexp.MustCompile(`(?m)^#!\s*/(usr/)?bin/(env\s+)?(ba)?sh\b`)

// scanShellGenerators walks every .go file under root. It returns the
// discovered shebang emitters (id → file:line), every declaration id in
// non-test source, and the ids named by shellcheckgate.Check calls in tests
// (id → file:line).
func scanShellGenerators(t *testing.T, root string) (emitters map[string]string, decls map[string]bool, covered map[string]string) {
	t.Helper()
	emitters, decls, covered = map[string]string{}, map[string]bool{}, map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // the repo's own sources
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		isTest := strings.HasSuffix(path, "_test.go")

		if isTest {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 3 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Check" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "shellcheckgate" {
					return true
				}
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s: shellcheckgate.Check's id must be a string literal", fset.Position(call.Pos()))
					return true
				}
				id, _ := strconv.Unquote(lit.Value)
				covered[id] = fset.Position(call.Pos()).String()
				return true
			})
			return nil
		}

		for _, decl := range f.Decls {
			switch dd := decl.(type) {
			case *ast.FuncDecl:
				id := dir + "." + shellGenFuncName(dd)
				decls[id] = true
				if pos, ok := shebangIn(dd); ok {
					emitters[id] = fset.Position(pos).String()
				}
			case *ast.GenDecl:
				for _, spec := range dd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						id := dir + "." + name.Name
						decls[id] = true
						if pos, ok := shebangIn(vs); ok {
							emitters[id] = fset.Position(pos).String()
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return emitters, decls, covered
}

func shellGenFuncName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func shebangIn(n ast.Node) (token.Pos, bool) {
	var at token.Pos
	ast.Inspect(n, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || at.IsValid() {
			return at == token.NoPos
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if shebangLine.MatchString(s) {
			at = lit.Pos()
		}
		return true
	})
	return at, at.IsValid()
}
