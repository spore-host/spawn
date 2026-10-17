package main

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A hibernated instance resumes the same spored process, and without an EIP it
// resumes with a new public IP. provider.Identity.PublicIP is the boot-time
// one, and identity is never mutated (it is shared unguarded with goroutines
// off the monitor loop), so spored re-reads the IP after a resume into a
// guarded value instead (#987). Every reader of the boot-time field was then a
// place the old IP leaked: the ready-url, {{ instance.ip }}, the hybrid
// registry, and the DNS client's existence (#1019).
//
// So this gate: in every package linked into spored, the only reads of
// Identity.PublicIP are the getters that fall back to it when there is no
// current value. Everything else must call one of them.
//
// The set of packages is discovered from spored's import graph, and the reads
// are found by type, not by name, so a struct of another type with a PublicIP
// field (pipeline's, config's) does not count, and a new package spored starts
// importing is covered without anyone adding it.
const sporedPkg = "github.com/spore-host/spawn/cmd/spored"

// bootIPFallbacks are the getters whose job is to read the boot-time IP when
// no current value is set. Each is "pkg.Recv.Method".
var bootIPFallbacks = map[string]bool{
	"github.com/spore-host/spawn/pkg/agent.Agent.publicIP":             true, // the guarded current value itself
	"github.com/spore-host/spawn/pkg/pluginruntime.Runtime.InstanceIP": true, // {{ instance.ip }} without a source (tests, `spawn` side)
	"github.com/spore-host/spawn/pkg/registry.PeerRegistry.ip":         true, // a registry without a source (its own tests)
}

// The provider package produces the identity; its writes are not reads.
const identityPkg = "github.com/spore-host/spawn/pkg/provider"

type listedPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	CgoFiles   []string
	Export     string
	Module     *struct{ Path string }
	Standard   bool
}

func TestSporedReadsThePublicIPOnlyThroughTheCurrentValue(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-export", "-json", sporedPkg)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	exports := map[string]string{}
	var own []listedPkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p listedPkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if p.Module != nil && p.Module.Path == "github.com/spore-host/spawn" && p.ImportPath != identityPkg {
			own = append(own, p)
		}
	}
	if len(own) == 0 {
		t.Fatal("discovered no spawn packages linked into spored; the discovery is broken")
	}

	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(f)
	})

	sawFallback := map[string]bool{}
	for _, p := range own {
		if len(p.CgoFiles) > 0 {
			t.Fatalf("%s has cgo files; this gate does not type-check them", p.ImportPath)
		}
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		conf := types.Config{Importer: imp}
		if _, err := conf.Check(p.ImportPath, fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for _, f := range files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				where := p.ImportPath + "." + funcName(fd)
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok || !isIdentityPublicIP(info.Selections[sel]) {
						return true
					}
					if bootIPFallbacks[where] {
						sawFallback[where] = true
						return true
					}
					t.Errorf("%s: %s reads the boot-time Identity.PublicIP; use the current value (Agent.publicIP, or a source set with SetPublicIPSource), which a resume from hibernation updates (#1019)",
						fset.Position(sel.Pos()), where)
					return true
				})
			}
		}
	}
	// A fallback the scan never saw means the scan is not seeing reads, or the
	// fallback is gone and this list is stale; either way, say so.
	for fn := range bootIPFallbacks {
		if !sawFallback[fn] {
			t.Errorf("%s was not found reading Identity.PublicIP: the gate is blind, or the list is stale", fn)
		}
	}
}

// isIdentityPublicIP reports whether sel selects the PublicIP field of
// provider.Identity.
func isIdentityPublicIP(sel *types.Selection) bool {
	if sel == nil || sel.Kind() != types.FieldVal || sel.Obj().Name() != "PublicIP" {
		return false
	}
	recv := sel.Recv()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}
	named, ok := recv.(*types.Named)
	return ok && named.Obj().Name() == "Identity" && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == identityPkg
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}
