package cmd

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/spore-host/spawn/pkg/launcher"
)

// The release workflow publishes what `spawn bootstrap-body` prints, and a
// release build's stub refuses any body but launcher.BootstrapBody() (#1274):
// the two must be the same bytes, with nothing added (no newline, no banner).
func TestBootstrapBodyCommandPrintsThePublishedBody(t *testing.T) {
	var out bytes.Buffer
	bootstrapBodyCmd.SetOut(&out)
	t.Cleanup(func() { bootstrapBodyCmd.SetOut(nil) })
	if err := bootstrapBodyCmd.RunE(bootstrapBodyCmd, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != launcher.BootstrapBody() {
		t.Errorf("spawn bootstrap-body printed %d bytes that are not launcher.BootstrapBody() (%d bytes); every release launch would refuse the published body",
			out.Len(), len(launcher.BootstrapBody()))
	}
	if !bootstrapBodyCmd.Hidden {
		t.Error("bootstrap-body is release tooling and should stay hidden")
	}
}

// A release CLI must tell pkg/launcher which release it is, or its launches
// fetch no body (or the wrong one). Execute does it, next to aws.CallerVersion;
// this finds the assignment itself, not a mention of it.
func TestExecuteTellsTheLauncherItsVersion(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "root.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Execute" {
			continue
		}
		for _, st := range fn.Body.List {
			as, ok := st.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				continue
			}
			sel, ok := as.Lhs[0].(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "CallerVersion" {
				continue
			}
			pkg, ok := sel.X.(*ast.Ident)
			call, isCall := as.Rhs[0].(*ast.CallExpr)
			if ok && pkg.Name == "launcher" && isCall {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "version" && len(call.Args) == 0 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("Execute does not set launcher.CallerVersion = version(); a release build would never use the published bootstrap body (#1274)")
	}
}
