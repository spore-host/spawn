package plugin_test

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
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/plugin"
)

// `spawn plugin validate` must accept a step only if the executor that runs its
// section will run it as written (#1130). It used to accept push in
// local.deprovision (RunDeprovision refuses it at teardown), capture: on a
// remote step and background:/as_user: on a local one (each silently ignored).
//
// So this gate derives, from the executors' source, which step types each
// section's executor runs and which Step fields it reads for each type, and
// requires Validate to accept exactly that: every other type, and every other
// field on an accepted type, must be rejected by name.
//
// Reads are found by type (a field selection on plugin.Step), following calls
// into the plugin and pluginruntime packages. Types come from the executor's
// own dispatch: the cases of a switch on a Step's Type, or the literal in an
// `if step.Type != "x" { return ... }` guard; a case's reads count only for its
// type. Renderers (functions returning a Step) are not followed: they copy every
// field into the rendered step, which is not the executor using it.

const (
	pluginPkg  = "github.com/spore-host/spawn/pkg/plugin"
	runtimePkg = "github.com/spore-host/spawn/pkg/pluginruntime"
)

// sectionExecutors names the function that runs each step section. Every
// []Step section in PluginSpec must appear here (checked below), so a new
// section cannot be added without saying who runs it.
var sectionExecutors = map[string]string{
	"local.provision":     "(*" + pluginPkg + ".LocalExecutor).RunProvision",
	"local.deprovision":   "(*" + pluginPkg + ".LocalExecutor).RunDeprovision", // cmd/plugin.go runLocalDeprovision
	"local.reconcile":     "(*" + pluginPkg + ".LocalExecutor).RunDeprovision", // cmd/plugin.go reconcileAllLocalPlugins (spawn start)
	"remote.install":      "(*" + runtimePkg + ".RemoteExecutor).RunSteps",
	"remote.configure":    "(*" + runtimePkg + ".RemoteExecutor).RunSteps",
	"remote.start":        "(*" + runtimePkg + ".RemoteExecutor).RunSteps",
	"remote.stop":         "(*" + runtimePkg + ".RemoteExecutor).RunSteps",
	"remote.health.steps": "(*" + runtimePkg + ".RemoteExecutor).RunSteps",
}

func TestValidatorAcceptsExactlyWhatTheExecutorsRun(t *testing.T) {
	sections := discoverStepSections(reflect.TypeOf(plugin.PluginSpec{}), "")
	if len(sections) == 0 {
		t.Fatal("discovered no []Step sections in PluginSpec; the discovery is broken")
	}
	for path := range sections {
		if _, ok := sectionExecutors[path]; !ok {
			t.Errorf("step section %s has no entry in sectionExecutors: name the function that runs it", path)
		}
	}
	for path := range sectionExecutors {
		if _, ok := sections[path]; !ok {
			t.Errorf("sectionExecutors names %s, which is not a []Step section of PluginSpec", path)
		}
	}

	a := loadExecutorSource(t)
	yamlKey := stepYAMLKeys()

	allTypes := map[string]bool{"no-such-step-type": true}
	reads := map[string]*executorReads{}
	for path, fn := range sectionExecutors {
		r := a.analyze(t, fn)
		reads[path] = r
		for typ := range r.types {
			allTypes[typ] = true
		}
	}

	for path, r := range reads {
		for typ := range allTypes {
			err := validateOneStep(t, sections[path], plugin.Step{Type: typ})
			switch {
			case r.types[typ] && err != nil:
				t.Errorf("%s: %s runs a bare %q step, but Validate rejects it: %v", path, sectionExecutors[path], typ, err)
			case !r.types[typ] && err == nil:
				t.Errorf("%s: Validate accepts a %q step, which %s does not run", path, typ, sectionExecutors[path])
			}
		}
		for typ := range r.types {
			read := r.fieldsFor(typ)
			for _, k := range yamlKey {
				goName, key := k.goName, k.yaml
				st := plugin.Step{Type: typ}
				setStepField(&st, k.index)
				err := validateOneStep(t, sections[path], st)
				switch {
				case read[goName] && err != nil:
					t.Errorf("%s: a %s step's %s is read by %s, but Validate rejects it: %v", path, typ, key, sectionExecutors[path], err)
				case !read[goName] && err == nil:
					t.Errorf("%s: Validate accepts %s on a %s step, which %s never reads", path, key, typ, sectionExecutors[path])
				case !read[goName] && !strings.Contains(err.Error(), key):
					t.Errorf("%s: %s on a %s step is rejected, but the error does not name it: %v", path, key, typ, err)
				}
			}
		}
	}
}

// discoverStepSections returns the yaml path of every []Step field reachable
// from typ, mapped to its index path.
func discoverStepSections(typ reflect.Type, prefix string) map[string][]int {
	out := map[string][]int{}
	stepSlice := reflect.TypeOf([]plugin.Step{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		key := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		switch {
		case f.Type == stepSlice:
			out[path] = []int{i}
		case f.Type.Kind() == reflect.Struct:
			for p, idx := range discoverStepSections(f.Type, path) {
				out[p] = append([]int{i}, idx...)
			}
		}
	}
	return out
}

// validateOneStep validates an otherwise-valid spec holding only st, in the
// section at index path idx.
func validateOneStep(t *testing.T, idx []int, st plugin.Step) error {
	t.Helper()
	spec := plugin.PluginSpec{Name: "gate", Version: "v1.0.0", Description: "d"}
	reflect.ValueOf(&spec).Elem().FieldByIndex(idx).Set(reflect.ValueOf([]plugin.Step{st}))
	return spec.Validate("")
}

type stepFieldKey struct {
	index        int
	goName, yaml string
}

// stepYAMLKeys lists every Step field except Type with its yaml key.
func stepYAMLKeys() []stepFieldKey {
	var out []stepFieldKey
	typ := reflect.TypeOf(plugin.Step{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Name == "Type" {
			continue
		}
		out = append(out, stepFieldKey{i, f.Name, strings.Split(f.Tag.Get("yaml"), ",")[0]})
	}
	return out
}

// setStepField gives a Step field a value that is well-formed for every check
// Validate makes on it (64 lowercase hex chars passes sha256's format check),
// so a rejection can only be about the field being there.
func setStepField(st *plugin.Step, index int) {
	f := reflect.ValueOf(st).Elem().Field(index)
	switch f.Kind() {
	case reflect.String:
		f.SetString(strings.Repeat("a", 64))
	case reflect.Bool:
		f.SetBool(true)
	case reflect.Map:
		f.Set(reflect.ValueOf(map[string]string{"A": ".a"}))
	default:
		panic("setStepField: unhandled kind " + f.Kind().String() + " for Step field " + strconv.Itoa(index) + "; extend the gate")
	}
}

type executorSource struct {
	fset  *token.FileSet
	decls map[string]*ast.FuncDecl // by types.Func FullName
	info  map[string]*types.Info   // the package Info for each decl
}

type executorReads struct {
	types   map[string]bool
	common  map[string]bool            // Step fields read whatever the type
	perType map[string]map[string]bool // Step fields read under one type's dispatch
}

func (r *executorReads) fieldsFor(typ string) map[string]bool {
	out := map[string]bool{}
	for f := range r.common {
		out[f] = true
	}
	for f := range r.perType[typ] {
		out[f] = true
	}
	return out
}

type listedPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
}

// loadExecutorSource type-checks the plugin and pluginruntime packages.
func loadExecutorSource(t *testing.T) *executorSource {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-export", "-json", runtimePkg)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	exports := map[string]string{}
	own := map[string]listedPkg{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p listedPkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if p.ImportPath == pluginPkg || p.ImportPath == runtimePkg {
			own[p.ImportPath] = p
		}
	}
	if len(own) != 2 {
		t.Fatalf("go list found %d of the 2 executor packages", len(own))
	}

	s := &executorSource{fset: token.NewFileSet(), decls: map[string]*ast.FuncDecl{}, info: map[string]*types.Info{}}
	imp := importer.ForCompiler(s.fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(f)
	})
	for _, p := range own {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(s.fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{
			Uses:       map[*ast.Ident]types.Object{},
			Defs:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
			Types:      map[ast.Expr]types.TypeAndValue{},
		}
		if _, err := (&types.Config{Importer: imp}).Check(p.ImportPath, s.fset, files, info); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for _, f := range files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
					if fn, ok := info.Defs[fd.Name].(*types.Func); ok {
						s.decls[fn.FullName()] = fd
						s.info[fn.FullName()] = info
					}
				}
			}
		}
	}
	return s
}

func isStep(typ types.Type) bool {
	if p, ok := typ.(*types.Pointer); ok {
		typ = p.Elem()
	}
	n, ok := typ.(*types.Named)
	return ok && n.Obj().Name() == "Step" && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pluginPkg
}

// stepField returns the Step field e selects, or "".
func stepField(info *types.Info, e ast.Expr) string {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	s := info.Selections[sel]
	if s == nil || s.Kind() != types.FieldVal || !isStep(s.Recv()) {
		return ""
	}
	return s.Obj().Name()
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := ast.Unparen(e).(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

func (s *executorSource) analyze(t *testing.T, entry string) *executorReads {
	t.Helper()
	if s.decls[entry] == nil {
		t.Fatalf("executor %s not found in the type-checked source", entry)
	}
	r := &executorReads{types: map[string]bool{}, common: map[string]bool{}, perType: map[string]map[string]bool{}}
	record := func(typ, field string) {
		if typ == "" {
			r.common[field] = true
			return
		}
		if r.perType[typ] == nil {
			r.perType[typ] = map[string]bool{}
		}
		r.perType[typ][field] = true
	}

	// walk inspects node in function fn, attributing reads to step type cur
	// ("" until a dispatch on Step.Type has chosen one).
	visited := map[string]bool{}
	var walk func(fn string, node ast.Node, cur string)
	walk = func(fn string, node ast.Node, cur string) {
		info := s.info[fn]
		ast.Inspect(node, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SwitchStmt:
				if n.Tag == nil || stepField(info, n.Tag) != "Type" {
					return true
				}
				for _, c := range n.Body.List {
					cc := c.(*ast.CaseClause)
					// default: is the unsupported-type error; it runs nothing.
					for _, e := range cc.List {
						typ, ok := stringLit(e)
						if !ok || (cur != "" && cur != typ) {
							continue
						}
						r.types[typ] = true
						for _, st := range cc.Body {
							walk(fn, st, typ)
						}
					}
				}
				return false
			case *ast.IfStmt:
				// if step.Type != "run" { return ... }: only "run" is run.
				if be, ok := n.Cond.(*ast.BinaryExpr); ok && be.Op == token.NEQ && stepField(info, be.X) == "Type" {
					if typ, ok := stringLit(be.Y); ok && endsInReturn(n.Body) {
						r.types[typ] = true
					}
				}
			case *ast.SelectorExpr:
				if f := stepField(info, n); f != "" {
					record(cur, f)
				}
			case *ast.CallExpr:
				if callee := s.callee(info, n); callee != "" && !visited[callee+"\x00"+cur] {
					visited[callee+"\x00"+cur] = true
					walk(callee, s.decls[callee].Body, cur)
				}
			}
			return true
		})
	}
	visited[entry+"\x00"] = true
	walk(entry, s.decls[entry].Body, "")

	if len(r.types) == 0 {
		t.Fatalf("%s: found no step-type dispatch (a switch on Step.Type or a != guard); the discovery is broken", entry)
	}
	for typ := range r.types {
		if len(r.fieldsFor(typ)) == 0 {
			t.Fatalf("%s: found no Step field read for a %q step; the discovery is broken", entry, typ)
		}
	}
	return r
}

func endsInReturn(b *ast.BlockStmt) bool {
	if len(b.List) == 0 {
		return false
	}
	_, ok := b.List[len(b.List)-1].(*ast.ReturnStmt)
	return ok
}

// callee returns the FullName of the function call invokes, if it is declared
// in the executor packages and is not a renderer (a function returning a Step).
func (s *executorSource) callee(info *types.Info, call *ast.CallExpr) string {
	var obj types.Object
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		obj = info.Uses[fun]
	case *ast.SelectorExpr:
		if sel := info.Selections[fun]; sel != nil {
			obj = sel.Obj()
		} else {
			obj = info.Uses[fun.Sel]
		}
	}
	fn, ok := obj.(*types.Func)
	if !ok || s.decls[fn.FullName()] == nil {
		return ""
	}
	res := fn.Type().(*types.Signature).Results()
	for i := 0; i < res.Len(); i++ {
		if isStep(res.At(i).Type()) {
			return ""
		}
	}
	return fn.FullName()
}
