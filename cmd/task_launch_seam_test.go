package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// `spawn task run` plans and launches only through pkg/tasklaunch, the code
// spore-host-mcp calls too (spore-host-mcp#60/#61). The MCP server had drifted
// by re-implementing this path: it called launcher.Provision with an empty
// Options{} and no On-Demand fallback, and sized with spec.Resources instead
// of spec.SizingRequest(). If cmd/task.go grows its own copy again, the two
// front ends can drift again, so task.go must not call the launch's building
// blocks itself, and must reach tasklaunch.Plan and tasklaunch.Launch.
func TestTaskGo_LaunchesOnlyThroughTasklaunch(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "task.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Local import names, so an aliased import is matched too.
	names := map[string]string{}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		name := lastSegment(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = path
	}

	// The launch's building blocks: calling any of these from task.go is a
	// second launch path beside pkg/tasklaunch.
	forbidden := map[string]map[string]bool{
		"github.com/spore-host/spawn/pkg/launcher":  {"Provision": true, "BuildUserData": true},
		"github.com/spore-host/spawn/pkg/taskproto": {"Size": true, "GenerateWrapper": true, "GenerateFlushScript": true},
	}
	reached := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		path := names[pkg.Name]
		if forbidden[path][sel.Sel.Name] {
			t.Errorf("%s: task.go calls %s.%s; plan and launch tasks through pkg/tasklaunch instead", fset.Position(call.Pos()), pkg.Name, sel.Sel.Name)
		}
		if path == "github.com/spore-host/spawn/pkg/tasklaunch" {
			reached[sel.Sel.Name] = true
		}
		return true
	})
	for _, fn := range []string{"Plan", "Launch"} {
		if !reached[fn] {
			t.Errorf("task.go never calls tasklaunch.%s; the task run path no longer goes through the shared library", fn)
		}
	}
}

func lastSegment(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
