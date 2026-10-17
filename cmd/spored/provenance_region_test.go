package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// On an instance nothing but IMDS knows the region: the wrapper exports no
// AWS_REGION and there is no ~/.aws/config. v0.132.0's provenance S3 client had
// no region there, so every output upload failed ("Invalid region") while the
// same code passed against real S3 from a workstation that had one configured.
func TestProvAWSConfigTakesTheRegionFromIMDS(t *testing.T) {
	imdsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/api/token":
			_, _ = w.Write([]byte("tok"))
		case "/latest/dynamic/instance-identity/document": // what the SDK's GetRegion reads
			_, _ = w.Write([]byte(`{"region":"eu-west-2","instanceId":"i-0123456789abcdef0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(imdsSrv.Close)

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "")
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", imdsSrv.URL)

	cfg, err := provAWSConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "eu-west-2" {
		t.Fatalf("region = %q with only IMDS to go on; want the instance's region, eu-west-2", cfg.Region)
	}
}

// Every provenance subcommand loads its config through provAWSConfig. A direct
// config.LoadDefaultConfig call is how the region was lost.
func TestProvenanceSubcommandsUseProvAWSConfig(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "provenance.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	uses := 0
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && fn.Name.Name == "provAWSConfig" {
			return false // the one place allowed to call LoadDefaultConfig
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fun.Sel.Name == "LoadDefaultConfig" {
				t.Errorf("provenance.go calls %s.LoadDefaultConfig directly; use provAWSConfig, which takes the region from IMDS", fun.X)
			}
		case *ast.Ident:
			if fun.Name == "provAWSConfig" {
				uses++
			}
		}
		return true
	})
	if uses != 3 {
		t.Errorf("provAWSConfig is called %d times; want 3 (stage-in, cost, upload)", uses)
	}
}
