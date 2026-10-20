package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCmdReadsCompletionRecordsOnlyThroughTasklaunch is the spawn#1260 gate:
// cmd/ reads task completion records only through tasklaunch.ReadCompletion /
// ReadCompletionAt, the reader the MCP server calls too, so the CLI and the
// other front ends cannot drift apart on where a record is or how it parses.
// That drift has happened before: in spawn#715 the reader built a different key
// from the writer, and nothing read a completion record for four releases.
//
// It walks every non-test Go file under cmd/ (discovered, not listed) and fails
// on either half of a hand-rolled reader:
//
//   - a call to ParseCompletionRecord, which only the shared reader needs; or
//   - an S3 object read (GetS3Object, GetObject, or a GetObjectInput literal)
//     whose arguments name the completion record: a CompletionKey /
//     completionKey call, a variable assigned from one in the same function,
//     or a string literal containing "completion.json".
//
// It also requires that cmd/ does call the shared reader, so that deleting the
// reads entirely (or renaming the reader) cannot leave the gate vacuously
// green.
func TestCmdReadsCompletionRecordsOnlyThroughTasklaunch(t *testing.T) {
	fset := token.NewFileSet()
	var files int
	sharedCalls := map[string]int{}

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		files++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			checkCompletionReads(t, fset, fn, sharedCalls)
		}
		// Package-level var initialisers (closures assigned to vars, such as
		// cobra RunE fields) are not FuncDecls; inspect them too.
		for _, decl := range f.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok {
				checkCompletionReads(t, fset, gd, sharedCalls)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/: %v", err)
	}
	if files == 0 {
		t.Fatal("discovered no Go files under cmd/: the walk is broken, not the package clean")
	}
	for _, name := range []string{"ReadCompletion", "ReadCompletionAt"} {
		if sharedCalls[name] == 0 {
			t.Errorf("cmd/ never calls tasklaunch.%s: `spawn task status` (ReadCompletion) and "+
				"`spawn task run --wait` (ReadCompletionAt) must read records through it (spawn#1260)", name)
		}
	}
}

// checkCompletionReads reports hand-rolled completion-record reads inside
// root, and counts calls to the shared reader into shared.
func checkCompletionReads(t *testing.T, fset *token.FileSet, root ast.Node, shared map[string]int) {
	t.Helper()

	// Names assigned (anywhere in root) from an expression naming the
	// completion key, so `key := completionKey(p); GetS3Object(.., key)` is
	// caught as well as the inline form.
	keyVars := map[string]bool{}
	ast.Inspect(root, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range s.Rhs {
				if i < len(s.Lhs) && namesCompletion(rhs, nil) {
					if id, ok := s.Lhs[i].(*ast.Ident); ok {
						keyVars[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for i, v := range s.Values {
				if i < len(s.Names) && namesCompletion(v, nil) {
					keyVars[s.Names[i].Name] = true
				}
			}
		}
		return true
	})

	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			name := calleeName(x.Fun)
			switch name {
			case "ReadCompletion", "ReadCompletionAt":
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "tasklaunch" {
						shared[name]++
					}
				}
			case "ParseCompletionRecord":
				t.Errorf("%s: cmd/ calls ParseCompletionRecord directly; read the record with "+
					"tasklaunch.ReadCompletion / ReadCompletionAt instead (spawn#1260)", fset.Position(x.Pos()))
			case "GetS3Object", "GetObject":
				for _, a := range x.Args {
					if namesCompletion(a, keyVars) {
						t.Errorf("%s: cmd/ reads a completion record with %s directly; use "+
							"tasklaunch.ReadCompletion / ReadCompletionAt instead (spawn#1260)", fset.Position(x.Pos()), name)
						break
					}
				}
			}
		case *ast.CompositeLit:
			if typeName(x.Type) == "GetObjectInput" && namesCompletion(x, keyVars) {
				t.Errorf("%s: cmd/ builds an s3 GetObjectInput for a completion record; use "+
					"tasklaunch.ReadCompletion / ReadCompletionAt instead (spawn#1260)", fset.Position(x.Pos()))
			}
		}
		return true
	})
}

// namesCompletion reports whether e contains a CompletionKey/completionKey
// call, an identifier in keyVars, or a string literal containing
// "completion.json".
func namesCompletion(e ast.Node, keyVars map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			if c := calleeName(x.Fun); c == "CompletionKey" || c == "completionKey" {
				found = true
			}
		case *ast.Ident:
			if keyVars[x.Name] {
				found = true
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil && strings.Contains(s, "completion.json") {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.StarExpr:
		return typeName(x.X)
	}
	return ""
}
