package treewalk

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVanished(t *testing.T) {
	gone := &fs.PathError{Op: "lstat", Path: "x", Err: fs.ErrNotExist}
	for _, c := range []struct {
		root, path string
		err        error
		want       bool
	}{
		{"../..", "../../dist", gone, true},
		{"../..", "../../dist/a.json", fmt.Errorf("wrapped: %w", gone), true},
		{"../..", "../..", gone, false}, // the root itself: the walk broke
		{"docs", "docs/", gone, false},  // same root, spelled differently
		{"../..", "../../x", fs.ErrPermission, false},
		{"../..", "../../x", nil, false},
	} {
		if got := Vanished(c.root, c.path, c.err); got != c.want {
			t.Errorf("Vanished(%q, %q, %v) = %v, want %v", c.root, c.path, c.err, got, c.want)
		}
	}
}

// TestVanishedEntriesSkipAWalk drives a real walk over a directory whose entry
// is deleted between the listing and the visit, the race of #1334.
func TestVanishedEntriesSkipAWalk(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(root, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if Vanished(root, path, err) {
				return nil
			}
			return err
		}
		seen = append(seen, filepath.Base(path))
		if path == filepath.Join(root, "a") {
			// Walk has already listed root; b disappears before it is lstat'd.
			_ = os.Remove(filepath.Join(root, "b"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed on an entry that vanished mid-walk: %v", err)
	}
	if strings.Join(seen, ",") != filepath.Base(root)+",a,c" {
		t.Errorf("visited %v", seen)
	}
}

// TestTreeWalksTolerateVanishedEntries is the gate for #1334. It finds every
// filepath.Walk, filepath.WalkDir and fs.WalkDir call in the repo's tests,
// nested modules included, and requires each callback that returns the error
// it is handed to consult Vanished on it first (or, where a nested module
// cannot import this package, errors.Is(err, fs.ErrNotExist)). Callbacks that never return their error (they skip on
// any error) already tolerate a vanished entry.
func TestTreeWalksTolerateVanishedEntries(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var walks, files int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if Vanished(root, path, err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if path != root {
				switch d.Name() {
				case ".git", "node_modules", "vendor", "dist":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			if Vanished(root, path, perr) {
				return nil
			}
			return perr
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isWalkCall(call) || len(call.Args) < 2 {
				return true
			}
			walks++
			cb := call.Args[len(call.Args)-1]
			lit, ok := cb.(*ast.FuncLit)
			if !ok {
				t.Errorf("%s: walk callback is not a function literal; this gate cannot read it", fset.Position(call.Pos()))
				return true
			}
			if msg := checkCallback(lit); msg != "" {
				t.Errorf("%s: %s (a file removed mid-walk by another package's test fails the walk, #1334; "+
					"use `if treewalk.Vanished(root, path, err) { return nil }` before returning it)",
					fset.Position(call.Pos()), msg)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 || walks < 20 {
		t.Fatalf("found %d walks in %d test files; the discovery has broken", walks, files)
	}
}

func isWalkCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch {
	case pkg.Name == "filepath" && (sel.Sel.Name == "Walk" || sel.Sel.Name == "WalkDir"):
		return true
	case pkg.Name == "fs" && sel.Sel.Name == "WalkDir":
		return true
	}
	return false
}

// checkCallback returns why lit does not tolerate a vanished entry, or "".
func checkCallback(lit *ast.FuncLit) string {
	params := lit.Type.Params.List
	if len(params) == 0 || len(params[len(params)-1].Names) == 0 {
		return "walk callback has no named error parameter"
	}
	errName := params[len(params)-1].Names[len(params[len(params)-1].Names)-1].Name
	if errName == "_" {
		return ""
	}
	returnsErr, consults := false, false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false // a nested callback has its own error
		case *ast.ReturnStmt:
			for _, r := range n.Results {
				if id, ok := r.(*ast.Ident); ok && id.Name == errName {
					returnsErr = true
				}
			}
		case *ast.CallExpr:
			// treewalk.Vanished, or, in a nested module that cannot import
			// it, errors.Is(err, fs.ErrNotExist) inline.
			if isVanishedCall(n) && len(n.Args) == 3 && isIdent(n.Args[2], errName) {
				consults = true
			}
			if isSel(n.Fun, "errors", "Is") && len(n.Args) == 2 && isIdent(n.Args[0], errName) && isSel(n.Args[1], "fs", "ErrNotExist") {
				consults = true
			}
		}
		return true
	})
	if returnsErr && !consults {
		return "walk callback returns its error " + errName + " without consulting treewalk.Vanished"
	}
	return ""
}

// isVanishedCall matches treewalk.Vanished, and Vanished unqualified inside
// this package.
func isVanishedCall(call *ast.CallExpr) bool {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := f.X.(*ast.Ident)
		return ok && pkg.Name == "treewalk" && f.Sel.Name == "Vanished"
	case *ast.Ident:
		return f.Name == "Vanished"
	}
	return false
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isSel(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name && isIdent(sel.X, pkg)
}
