package tasklaunch

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// recordingProvision is a fake launcher.Provision: it records the config and
// Options of every call and fails the Spot attempts for capacity.
type recordingProvision struct {
	configs []aws.LaunchConfig
	opts    []launcher.Options
}

func (r *recordingProvision) provision(c aws.LaunchConfig, o launcher.Options) (*aws.LaunchResult, error) {
	r.configs = append(r.configs, c)
	r.opts = append(r.opts, o)
	if c.Spot {
		return nil, &aws.LaunchError{Code: "InsufficientInstanceCapacity"}
	}
	return &aws.LaunchResult{InstanceID: "i-fallback"}, nil
}

// #1144: the Spot→on-demand retry must launch with the same Options as the Spot
// attempt — the storage mounts and the terminal-flush hook. It used to pass an
// empty launcher.Options{}, so a fallback instance lost its EFS/FSx/volume
// mounts and the hook that writes a completion record on a TTL kill.
func TestProvisionTask_FallbackKeepsOptions(t *testing.T) {
	opts := launcher.Options{
		StorageScript:   "mount -t efs fs-123 /mnt/data",
		TaskFlushScript: "#!/bin/bash\necho flush",
	}
	rec := &recordingProvision{}
	cfg := aws.LaunchConfig{Spot: true}
	var out bytes.Buffer

	res, err := provisionTask(&out, rec.provision, &cfg, opts, taskproto.PurchaseOnDemand)
	if err != nil {
		t.Fatalf("provisionTask: %v", err)
	}
	if res == nil || res.InstanceID != "i-fallback" {
		t.Fatalf("result = %+v, want the on-demand fallback instance", res)
	}
	if len(rec.opts) != 2 {
		t.Fatalf("Provision called %d times, want 2 (spot, then on-demand fallback)", len(rec.opts))
	}
	if !rec.configs[0].Spot || rec.configs[1].Spot {
		t.Fatalf("attempts Spot = %v,%v; want true,false", rec.configs[0].Spot, rec.configs[1].Spot)
	}
	if cfg.Spot {
		t.Error("cfg.Spot still true after fallback; the launch summary would mislabel the instance")
	}
	for i, o := range rec.opts {
		if !reflect.DeepEqual(o, opts) {
			t.Errorf("Provision call %d got Options %+v, want %+v", i, o, opts)
		}
	}
}

// No fallback configured: one attempt, the capacity error surfaces.
func TestProvisionTask_NoFallbackNoRetry(t *testing.T) {
	rec := &recordingProvision{}
	cfg := aws.LaunchConfig{Spot: true}
	if _, err := provisionTask(&bytes.Buffer{}, rec.provision, &cfg, launcher.Options{}, ""); err == nil {
		t.Fatal("want the capacity error when no fallback is configured")
	}
	if len(rec.opts) != 1 {
		t.Fatalf("Provision called %d times, want 1", len(rec.opts))
	}
}

// The fake above proves provisionTask forwards one Options to both attempts;
// this proves the package's real launcher.Provision call forwards the Options
// it is handed. Every launcher.Provision invocation in this package must be
// inside a func literal and pass that literal's launcher.Options parameter — a
// direct launcher.Provision(..., launcher.Options{}) beside it (the #1144
// shape) fails this. (It used to scan cmd/task.go; the launch moved here.)
func TestTaskLaunch_ProvisionForwardsSeamOptions(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found += checkProvisionCalls(t, fset, f)
	}
	if found == 0 {
		t.Fatal("found no launcher.Provision call in pkg/tasklaunch; the discovery is broken")
	}
}

func checkProvisionCalls(t *testing.T, fset *token.FileSet, f *ast.File) int {
	t.Helper()
	found := 0
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Provision" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "launcher" {
			return true
		}
		found++
		pos := fset.Position(call.Pos())
		var lit *ast.FuncLit
		for i := len(stack) - 1; i >= 0; i-- {
			if fl, ok := stack[i].(*ast.FuncLit); ok {
				lit = fl
				break
			}
		}
		if lit == nil || len(call.Args) != 4 {
			t.Errorf("%s: launcher.Provision must be called only from a provisioner closure", pos)
			return true
		}
		arg, ok := call.Args[3].(*ast.Ident)
		if !ok {
			t.Errorf("%s: launcher.Provision Options arg is %T, want the closure's Options parameter", pos, call.Args[3])
			return true
		}
		params := lit.Type.Params.List
		last := params[len(params)-1]
		if len(last.Names) == 0 || last.Names[len(last.Names)-1].Name != arg.Name {
			t.Errorf("%s: launcher.Provision passes %q, not the closure's Options parameter", pos, arg.Name)
		}
		return true
	})
	return found
}
