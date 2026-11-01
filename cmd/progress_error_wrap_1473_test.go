package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/progress"
)

const progressPkg = "github.com/spore-host/spawn/pkg/progress"

// wrapSite is a step that shows an error with (*progress.Progress).Error and
// then returns it wrapped by fmt.Errorf.
type wrapSite struct {
	pos    string
	fn     string
	format string
	args   []bool // per fmt.Errorf argument: is it the shown error?
}

// #1473: a step that shows an error (prog.Error(step, err)) and returns it
// wrapped ("failed to create FSx filesystem: %w") printed the error's text
// twice, once in the progress line and once inside the wrapper the root
// command printed. #1470 fixed only the unwrapped case.
//
// Every such step in package cmd is discovered from the type-checked source
// (any call of (*progress.Progress).Error whose error the next return wraps
// in fmt.Errorf), and each is driven through the real output path with its
// own format string: progress.Error prints the shown error, the root printer
// prints the returned one, and finishLaunch, as a launch that failed after
// RunInstances does, wraps it once more. The shown error's text must appear
// exactly once and every literal part of the step's context at least once.
func TestWrappedStepErrorIsPrintedOnce(t *testing.T) {
	sites := discoverWrapSites(t)

	// The steps #1473 names: if discovery stops seeing them, it is blind.
	seen := map[string]bool{}
	for _, s := range sites {
		seen[s.fn] = true
	}
	for _, fn := range []string{"launchWithProgress", "ensureIAMProfile", "ensureSecurityGroup", "ensureEFAGroup", "runArrayCohort", "launchParameterSweep", "buildWarmAMI"} {
		if !seen[fn] {
			t.Errorf("no wrapping step discovered in %s; #1473 found them there, so the discovery is blind or the list is stale", fn)
		}
	}

	for i, s := range sites {
		t.Run(s.pos, func(t *testing.T) {
			for _, quiet := range []bool{false, true} {
				// A fresh error per run: progress matches what it printed by
				// identity, and the runs share one process.
				shown := fmt.Errorf("InvalidParameterValue: shown-by-step-%d-quiet-%v", i, quiet)
				args := make([]any, len(s.args))
				for j, isShown := range s.args {
					if isShown {
						args[j] = shown
					} else {
						args[j] = "ARG"
					}
				}
				returned := fmt.Errorf(s.format, args...)
				if !errors.Is(returned, shown) {
					// A %v of the shown error: it does not wrap it, so the
					// root printer cannot know its text was shown.
					t.Fatalf("%s returns fmt.Errorf(%q) around the error its step showed without %%w: wrap it with %%w, or the root printer prints its text again", s.fn, s.format)
				}
				afterRunInstances := finishLaunch(t.Context(), io.Discard, nil, "us-east-1", "i-0123456789abcdef0", "", true, returned)

				prog := progress.NewProgress()
				if quiet {
					prog = progress.NewQuietProgress()
				}
				var stderr strings.Builder
				prog.SetErrorOutput(&stderr)
				for _, ret := range []error{returned, afterRunInstances} {
					stderr.Reset()
					prog.Start("step")
					prog.Error("step", shown)
					printCommandError(&stderr, ret)
					out := stderr.String()
					if n := strings.Count(out, shown.Error()); n != 1 {
						t.Errorf("quiet=%v: the step's error is printed %d times, want once:\n%s", quiet, n, out)
					}
					for _, lit := range formatLiterals(s.format) {
						if !strings.Contains(out, lit) {
							t.Errorf("quiet=%v: the step's context %q is not printed:\n%s", quiet, lit, out)
						}
					}
					if ret == afterRunInstances && !strings.Contains(out, "kept it (--keep-on-error)") {
						t.Errorf("quiet=%v: finishLaunch's context is not printed:\n%s", quiet, out)
					}
				}
			}
		})
	}
}

// formatLiterals is the literal text of a format string between its verbs,
// each part trimmed of separators, that is long enough to identify.
func formatLiterals(format string) []string {
	var lits []string
	for _, part := range regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z%]`).Split(format, -1) {
		part = strings.Trim(part, " :;,()\n\t")
		if len(part) >= 4 {
			lits = append(lits, part)
		}
	}
	return lits
}

type cmdListedPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	CgoFiles   []string
	Export     string
}

// discoverWrapSites type-checks package cmd and returns each return that
// wraps an error a step showed with (*progress.Progress).Error: any return
// after the call in the same block, up to the block's own return. A return
// that carries the shown error in any other expression fails the test: the
// gate cannot drive it, so it cannot vouch for it.
func discoverWrapSites(t *testing.T) []wrapSite {
	t.Helper()
	list := exec.Command("go", "list", "-deps", "-export", "-json", ".")
	list.Stderr = os.Stderr
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	exports := map[string]string{}
	var self cmdListedPkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p cmdListedPkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		self = p // -deps lists the named package last
	}
	if !strings.HasSuffix(self.ImportPath, "/spawn/cmd") || len(self.CgoFiles) > 0 {
		t.Fatalf("go list: last package is %s (cgo files %v), want package cmd without cgo", self.ImportPath, self.CgoFiles)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range self.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(self.Dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(f)
	})
	info := &types.Info{
		Uses:       map[*ast.Ident]types.Object{},
		Defs:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	if _, err := (&types.Config{Importer: imp}).Check(self.ImportPath, fset, files, info); err != nil {
		t.Fatalf("type-check %s: %v", self.ImportPath, err)
	}

	var sites []wrapSite
	errorCalls := 0
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				var list []ast.Stmt
				switch b := n.(type) {
				case *ast.BlockStmt:
					list = b.List
				case *ast.CaseClause:
					list = b.Body
				case *ast.CommClause:
					list = b.Body
				default:
					return true
				}
				for i, st := range list {
					shown := progressErrorArg(info, st)
					if shown == nil {
						continue
					}
					errorCalls++
					for _, ret := range returnsIn(list[i+1:]) {
						pos := fset.Position(ret.Pos())
						site := wrapSite{pos: filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line), fn: fd.Name.Name}
						for _, res := range ret.Results {
							if id, ok := res.(*ast.Ident); ok && info.Uses[id] == shown {
								continue // returned as shown (#1470)
							}
							if !mentions(info, res, shown) {
								continue
							}
							call, ok := res.(*ast.CallExpr)
							if !ok || !isFmtErrorf(info, call) {
								t.Errorf("%s: %s returns the error its step showed in %s, which this gate cannot drive; wrap it with fmt.Errorf(\"...: %%w\", err) or extend the gate", site.pos, site.fn, types.ExprString(res))
								continue
							}
							lit, ok := call.Args[0].(*ast.BasicLit)
							if !ok || lit.Kind != token.STRING {
								t.Errorf("%s: %s wraps the shown error with a non-literal format", site.pos, site.fn)
								continue
							}
							site.format, _ = strconv.Unquote(lit.Value)
							for _, a := range call.Args[1:] {
								id, ok := a.(*ast.Ident)
								site.args = append(site.args, ok && info.Uses[id] == shown)
							}
							sites = append(sites, site)
						}
					}
				}
				return true
			})
		}
	}
	// A discovery that found nothing has broken, not proven the code clean.
	if errorCalls == 0 || len(sites) == 0 {
		t.Fatalf("discovered %d (*progress.Progress).Error calls and %d wrapping steps; the discovery is broken", errorCalls, len(sites))
	}
	t.Logf("%d (*progress.Progress).Error calls, %d return the shown error wrapped", errorCalls, len(sites))
	for _, s := range sites {
		t.Logf("  %s %s %q", s.pos, s.fn, s.format)
	}
	return sites
}

// progressErrorArg is the variable st passes as the error to a
// (*progress.Progress).Error call, when st is such a call.
func progressErrorArg(info *types.Info, st ast.Stmt) types.Object {
	es, ok := st.(*ast.ExprStmt)
	if !ok {
		return nil
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return nil
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Error" {
		return nil
	}
	s := info.Selections[sel]
	if s == nil || s.Kind() != types.MethodVal {
		return nil
	}
	recv := s.Recv()
	if p, ok := recv.(*types.Pointer); ok {
		recv = p.Elem()
	}
	named, ok := recv.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != progressPkg || named.Obj().Name() != "Progress" {
		return nil
	}
	id, ok := call.Args[1].(*ast.Ident)
	if !ok {
		return nil // an error built in place (fmt.Errorf("status %s", ...)) is not returned
	}
	return info.Uses[id]
}

// returnsIn is every return statement in list, up to and including the first
// one directly in it (after which the rest is unreachable), outside function
// literals.
func returnsIn(list []ast.Stmt) []*ast.ReturnStmt {
	var rets []*ast.ReturnStmt
	for _, st := range list {
		ast.Inspect(st, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.ReturnStmt:
				rets = append(rets, x)
			}
			return true
		})
		if _, ok := st.(*ast.ReturnStmt); ok {
			break
		}
	}
	return rets
}

func mentions(info *types.Info, e ast.Expr, obj types.Object) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && info.Uses[id] == obj {
			found = true
		}
		return !found
	})
	return found
}

func isFmtErrorf(info *types.Info, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == "fmt" && fn.Name() == "Errorf"
}
