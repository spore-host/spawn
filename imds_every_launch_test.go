package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spore-host/spawn/internal/treewalk"
)

// Every RunInstances spawn makes requires IMDSv2 (#1588). Before, only a
// compliance-mode launch set MetadataOptions; the sweep and pipeline Lambdas
// and the default launch path left IMDS to the AMI, so a custom, Ubuntu or
// older Windows image accepted IMDSv1.
//
// This gate discovers every ec2.RunInstancesInput composite literal in the
// repo's non-test Go code and holds the function building it to one of: the
// literal sets MetadataOptions, the function assigns .MetadataOptions, or it
// calls applyLaunchSettings (pkg/aws), which sets it unconditionally
// (TestApplyLaunchSettingsAlwaysRequiresIMDSv2 holds that half). A DryRun
// literal launches nothing and is exempt.
func TestEveryRunInstancesRequiresIMDSv2(t *testing.T) {
	fset := token.NewFileSet()
	found := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if treewalk.Vanished(".", path, err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "node_modules", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var lits []*ast.CompositeLit
			setsInBody := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "RunInstancesInput" {
						lits = append(lits, x)
					}
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "MetadataOptions" {
							setsInBody = true
						}
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "applyLaunchSettings" {
						setsInBody = true
					}
				}
				return true
			})
			for _, lit := range lits {
				found++
				if setsInBody || litHasKey(lit, "MetadataOptions") || litHasKey(lit, "DryRun") {
					continue
				}
				t.Errorf("%s: %s builds an ec2.RunInstancesInput without MetadataOptions: every launch must "+
					"require IMDSv2 (spawnaws.IMDSMetadataOptions, #1588)", fset.Position(lit.Pos()), fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("found no ec2.RunInstancesInput literal: the discovery is broken, not the repo")
	}
}

func litHasKey(lit *ast.CompositeLit, key string) bool {
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				return true
			}
		}
	}
	return false
}
