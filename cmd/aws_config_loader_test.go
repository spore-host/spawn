package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sdkConfigImport is the AWS SDK package whose LoadDefaultConfig reads only the
// ambient chain.
const sdkConfigImport = "github.com/aws/aws-sdk-go-v2/config"

// TestCommandsLoadAWSConfigThroughTheSharedLoader is #939's gate.
//
// The root command records --region, --profile and --account through
// spawnconfig.SetSharedFlags, and they are read only by the shared loaders:
// loadAWSConfig / spawnaws.NewClientWithRegion, and pkg/config's
// LoadInfraAWSConfig / LoadComputeAWSConfig. A command that calls the SDK's
// LoadDefaultConfig itself reads none of the root flags or SPORE_REGION /
// SPORE_PROFILE, so the flags its --help advertises do nothing. That was
// `spawn burst` (#939), `spawn launch` (#276) and `spawn autoscale` (#774), each
// found one command at a time. Passing WithRegion does not make a call safe:
// it still ignores --profile.
//
// Every non-test file of package cmd is parsed, so a new command is covered
// without being listed. A call is matched by its resolved import path, not by
// its text, so an import alias does not hide it and a comment naming it does
// not trip it.
func TestCommandsLoadAWSConfigThroughTheSharedLoader(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd dir: %v", err)
	}

	fset := token.NewFileSet()
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, pos := range bareSDKConfigLoads(f) {
			t.Errorf("%s: calls %s.LoadDefaultConfig directly, which ignores the root --region and "+
				"--profile flags and SPORE_REGION/SPORE_PROFILE (#939). Use loadAWSConfig(ctx, region) "+
				"(cmd/awsconfig.go), spawnaws.NewClientWithRegion, or pkg/config's "+
				"LoadInfraAWSConfig/LoadComputeAWSConfig.", fset.Position(pos), sdkConfigImport)
		}
	}
	if parsed == 0 {
		t.Fatal("found no Go files in package cmd: this gate is checking nothing")
	}
}

// bareSDKConfigLoads returns the position of every call to the SDK config
// package's LoadDefaultConfig in f, under whatever name f imports it as.
func bareSDKConfigLoads(f *ast.File) []token.Pos {
	var local string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != sdkConfigImport {
			continue
		}
		local = "config"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if local == "" || local == "_" {
		return nil
	}

	var found []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "LoadDefaultConfig" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == local {
			found = append(found, call.Pos())
		}
		return true
	})
	return found
}

// TestBareSDKConfigLoadsMatchesTheCallUnderAnyName checks the matcher itself,
// on source where the import is aliased, where the call takes options, and
// where only a comment or a different package's function names it.
func TestBareSDKConfigLoadsMatchesTheCallUnderAnyName(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"default name", `package p
import "github.com/aws/aws-sdk-go-v2/config"
func f() { config.LoadDefaultConfig(nil) }`, 1},
		{"aliased, with options", `package p
import awsconfig "github.com/aws/aws-sdk-go-v2/config"
func f() { awsconfig.LoadDefaultConfig(nil, awsconfig.WithRegion("x")) }`, 1},
		{"comment only", `package p
import "github.com/aws/aws-sdk-go-v2/config"
// config.LoadDefaultConfig(ctx) is the bug.
var _ = config.WithRegion`, 0},
		{"another package's function", `package p
import config "example.com/notsdk"
func f() { config.LoadDefaultConfig(nil) }`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(bareSDKConfigLoads(f)); got != tc.want {
				t.Fatalf("found %d calls, want %d", got, tc.want)
			}
		})
	}
}

// sanctionedPkgLoaders are the pkg/ functions that call LoadDefaultConfig and
// apply the shared config (--profile/--region/SPORE_*) themselves, through an
// options slice the scan below cannot see into. Everything else is discovered.
var sanctionedPkgLoaders = map[string]bool{
	"github.com/spore-host/spawn/pkg/aws.NewClientWithRegion":     true,
	"github.com/spore-host/spawn/pkg/config.LoadInfraAWSConfig":   true,
	"github.com/spore-host/spawn/pkg/config.LoadComputeAWSConfig": true,
}

// TestCommandsCallNoAmbientPkgLoader extends #939's gate one level into pkg/
// (#967): `spawn dns` signed its requests through dns.NewClient, which loads the
// ambient chain itself, so --profile reached the instance lookup but not the
// signature. Every exported top-level pkg/ function whose body calls
// LoadDefaultConfig — without explicit credentials, and not one of the shared
// loaders above — is discovered, and package cmd may call none of them.
//
// One level only: an unexported method that loads its own config behind an
// exported one (pkg/cost's pricer was one) is not found here; the behaviour
// tests in dns_profile_test.go cover those two commands end to end.
func TestCommandsCallNoAmbientPkgLoader(t *testing.T) {
	const module = "github.com/spore-host/spawn/"
	ambient := map[string]bool{} // import path + "." + func
	err := filepath.WalkDir("../pkg", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		loads := bareSDKConfigLoadCalls(f)
		pkgPath := module + filepath.ToSlash(filepath.Dir(strings.TrimPrefix(p, "../")))
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv != nil || !fd.Name.IsExported() {
				continue
			}
			for _, call := range loads {
				if call.Pos() >= fd.Body.Pos() && call.Pos() < fd.Body.End() && !passesCredentials(call) {
					if name := pkgPath + "." + fd.Name.Name; !sanctionedPkgLoaders[name] {
						ambient[name] = true
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ambient) == 0 {
		t.Fatal("discovered no ambient loaders in pkg/ (dns.NewClient is one, for spored): the scan is broken")
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		local := map[string]string{} // import name -> path
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			n := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				n = imp.Name.Name
			}
			local[n] = path
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && ambient[local[id.Name]+"."+sel.Sel.Name] {
				t.Errorf("%s: uses %s.%s, which loads the ambient AWS config and so ignores --profile/SPORE_PROFILE (#967); "+
					"pass the config the command loaded instead", fset.Position(sel.Pos()), id.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

// bareSDKConfigLoadCalls is bareSDKConfigLoads returning the calls themselves.
func bareSDKConfigLoadCalls(f *ast.File) []*ast.CallExpr {
	positions := map[token.Pos]bool{}
	for _, p := range bareSDKConfigLoads(f) {
		positions[p] = true
	}
	var calls []*ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && positions[c.Pos()] {
			calls = append(calls, c)
		}
		return true
	})
	return calls
}

// passesCredentials reports whether a LoadDefaultConfig call names its
// credentials (WithCredentialsProvider): then the ambient chain is not used for
// them, whatever profile is configured.
func passesCredentials(call *ast.CallExpr) bool {
	for _, a := range call.Args {
		if c, ok := a.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "WithCredentialsProvider" {
				return true
			}
		}
	}
	return false
}
