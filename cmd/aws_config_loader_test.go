package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
