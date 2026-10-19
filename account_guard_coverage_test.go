package main

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryCLIConfigLoadIsAccountGuarded is #1118's gate.
//
// `--account`, SPORE_ACCOUNT and `account =` in the spore config promise that
// spawn "refuses to act if your credentials resolve to a different account".
// The check, pkg/config.CheckAccount, runs where credentials are resolved: an
// AWS config built from a credential chain is checked before it is handed out,
// so a command cannot make a call, mutating or not, without passing it. That
// holds only if EVERY place the spawn binary builds a config from a credential
// chain calls CheckAccount. Before #1118 none did.
//
// So, in every package linked into the spawn binary (discovered from its import
// graph for each release GOOS, so a package spawn starts importing is covered
// without being listed), every call to the SDK's config.LoadDefaultConfig must
// either:
//   - sit in a function that also calls CheckAccount, or
//   - pass config.WithCredentialsProvider, i.e. use credentials it was handed
//     explicitly (the sweep code's assumed-role credentials, minted from a
//     config that was itself guarded) rather than resolving a chain.
//
// Calls are matched by their resolved import path, not their text, so an
// import alias does not hide one and a comment naming one does not count.
func TestEveryCLIConfigLoadIsAccountGuarded(t *testing.T) {
	files := spawnBinaryFiles(t)

	loads := 0
	for _, path := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		sdkConfig := importName(f, "github.com/aws/aws-sdk-go-v2/config", "config")
		if sdkConfig == "" {
			continue
		}
		guard := importName(f, "github.com/spore-host/spawn/pkg/config", "config")
		inConfigPkg := f.Name.Name == "config" && strings.HasSuffix(filepath.ToSlash(filepath.Dir(path)), "/pkg/config")

		forEachFunc(f, func(body *ast.BlockStmt) {
			for _, call := range directCalls(body) {
				if !isPkgCall(call, sdkConfig, "LoadDefaultConfig") {
					continue
				}
				loads++
				if passesExplicitCredentials(call, sdkConfig) {
					continue
				}
				guarded := false
				for _, c := range directCalls(body) {
					if (guard != "" && isPkgCall(c, guard, "CheckAccount")) || (inConfigPkg && isBareCall(c, "CheckAccount")) {
						guarded = true
					}
				}
				if !guarded {
					rel, _ := filepath.Rel(mustGetwd(t), path)
					t.Errorf("%s: LoadDefaultConfig builds AWS credentials without the --account / SPORE_ACCOUNT guard "+
						"(#1118). Call spawnconfig.CheckAccount(ctx, cfg) on the result in the same function, or load "+
						"through spawnaws.NewClientWithRegion / pkg/config's LoadComputeAWSConfig.", rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
				}
			}
		})
	}
	if loads == 0 {
		t.Fatal("found no LoadDefaultConfig call in any package linked into spawn: discovery is broken, not the binary clean")
	}
}

// spawnBinaryFiles returns the non-test Go files of every spawn-module package
// linked into the spawn binary, for each GOOS the release builds.
func spawnBinaryFiles(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		// -e and GOPROXY=off: the hermetic test wrapper blocks the network, and a
		// third-party module needed only for another GOOS (cobra's mousetrap on
		// windows) may not be in the module cache. Such a package is reported
		// with an Error instead of failing the whole listing; only an error on a
		// spawn package fails the test, since that would hide its files.
		cmd := exec.Command("go", "list", "-e", "-deps", "-json", ".")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "CGO_ENABLED=0", "GOPROXY=off", "GOFLAGS=-mod=mod")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list (GOOS=%s): %v", goos, err)
		}
		dec := json.NewDecoder(strings.NewReader(string(out)))
		for {
			var p struct {
				ImportPath string
				Dir        string
				GoFiles    []string
				Module     *struct{ Path string }
				Error      *struct{ Err string }
			}
			if err := dec.Decode(&p); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("decode go list: %v", err)
			}
			if p.Module == nil || p.Module.Path != "github.com/spore-host/spawn" {
				continue
			}
			if p.Error != nil {
				t.Fatalf("go list (GOOS=%s): spawn package %s: %s", goos, p.ImportPath, p.Error.Err)
			}
			for _, f := range p.GoFiles {
				seen[filepath.Join(p.Dir, f)] = true
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("go list found no spawn-module files linked into spawn")
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// importName is the name f refers to path by, or "" if f does not import it.
func importName(f *ast.File, path, defaultName string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return ""
			}
			return imp.Name.Name
		}
		return defaultName
	}
	return ""
}

// forEachFunc calls fn with the body of every function declaration and literal.
func forEachFunc(f *ast.File, fn func(*ast.BlockStmt)) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				fn(d.Body)
			}
		case *ast.FuncLit:
			fn(d.Body)
		}
		return true
	})
}

// directCalls returns the calls in body that are not inside a nested function
// literal, so a call is attributed to the innermost function containing it.
func directCalls(body *ast.BlockStmt) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			calls = append(calls, x)
		}
		return true
	})
	return calls
}

func isPkgCall(call *ast.CallExpr, pkg, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func isBareCall(call *ast.CallExpr, name string) bool {
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == name
}

func passesExplicitCredentials(call *ast.CallExpr, sdkConfig string) bool {
	for _, arg := range call.Args {
		if c, ok := arg.(*ast.CallExpr); ok && isPkgCall(c, sdkConfig, "WithCredentialsProvider") {
			return true
		}
	}
	return false
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
