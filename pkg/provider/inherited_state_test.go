package provider

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// spored discards state inherited from an image before it loads any (#1241):
// NewEC2Provider calls discardInheritedState with the instance ID before
// loadConfigFromEC2Tags, the only reader of plugins.json.
func TestNewEC2ProviderDiscardsInheritedStateBeforeLoadingIt(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "ec2.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	callsIn := func(fn string) map[string][]token.Pos {
		out := map[string][]token.Pos{}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != fn || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok {
						out[id.Name] = append(out[id.Name], c.Pos())
					}
				}
				return true
			})
		}
		return out
	}
	np := callsIn("NewEC2Provider")
	if len(np["discardInheritedState"]) == 0 || len(np["loadConfigFromEC2Tags"]) == 0 {
		t.Fatalf("NewEC2Provider calls discardInheritedState %d time(s) and loadConfigFromEC2Tags %d; want both",
			len(np["discardInheritedState"]), len(np["loadConfigFromEC2Tags"]))
	}
	if np["discardInheritedState"][0] > np["loadConfigFromEC2Tags"][0] {
		t.Error("NewEC2Provider loads plugins.json (loadConfigFromEC2Tags) before discarding inherited state")
	}
	// The plugin declarations are read nowhere else at startup.
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name == "loadConfigFromEC2Tags" {
			continue
		}
		if len(callsIn(fd.Name.Name)["loadPluginDeclarations"]) > 0 {
			t.Errorf("%s reads plugins.json; it must run after discardInheritedState too", fd.Name.Name)
		}
	}
}

func TestDiscardInheritedStateReconcilesThisInstance(t *testing.T) {
	saved, savedLinux, savedRoot, savedStrip := reconcileInstanceState, stateOnLinux, stateAsRoot, stripClusterKeys
	t.Cleanup(func() {
		reconcileInstanceState, stateOnLinux, stateAsRoot, stripClusterKeys = saved, savedLinux, savedRoot, savedStrip
	})
	var got, stripped []string
	// #1343: the inherited MPI cluster key is stripped here too, for the
	// same instance, so a launch from the image outside spawn is covered.
	stripClusterKeys = func(id string) (int, error) {
		stripped = append(stripped, id)
		return 1, nil
	}
	reconcileInstanceState = func(id string) ([]string, string, error) {
		got = append(got, id)
		return []string{"/etc/spawn/plugins.json"}, "i-0parent", errors.New("partial")
	}

	stateOnLinux, stateAsRoot = true, func() bool { return true }
	discardInheritedState("i-0child")
	if len(got) != 1 || got[0] != "i-0child" {
		t.Fatalf("reconciled %v, want [i-0child]", got)
	}
	if len(stripped) != 1 || stripped[0] != "i-0child" {
		t.Fatalf("stripped cluster keys for %v, want [i-0child]", stripped)
	}

	// Not root (a `spored` subcommand run by a user), or not Linux: nothing.
	stateAsRoot = func() bool { return false }
	discardInheritedState("i-0child")
	stateOnLinux, stateAsRoot = false, func() bool { return true }
	discardInheritedState("i-0child")
	if len(got) != 1 || len(stripped) != 1 {
		t.Errorf("reconciled %d and stripped %d times, want 1 each", len(got), len(stripped))
	}
}
