package pipeline

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeIMDS serves the IMDSv2 token and the identity document the SDK's region
// lookup reads.
func fakeIMDS(t *testing.T, region string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
			_, _ = w.Write([]byte("token"))
		case r.Method == http.MethodGet && r.URL.Path == "/latest/dynamic/instance-identity/document":
			_, _ = w.Write([]byte(`{"region":"` + region + `","instanceId":"i-0123456789abcdef0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", srv.URL)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "")
}

// onAnInstance strips every ambient source of a region, leaving the environment
// `spored run-pipeline-stage` runs in: no AWS_REGION, no AWS_DEFAULT_REGION and
// no shared config naming one.
func onAnInstance(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "absent")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", missing)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", missing)
}

// On an instance nothing ambient names a region, and the SDK does not read it
// from IMDS unless asked, so every pkg/pipeline client failed with "Missing
// Region" (#906). The shared config must take the instance's region from IMDS.
func TestLoadAWSConfig_TakesTheInstanceRegionFromIMDS(t *testing.T) {
	onAnInstance(t)
	fakeIMDS(t, "us-west-2")
	cfg, err := loadAWSConfig(context.Background())
	if err != nil {
		t.Fatalf("loadAWSConfig: %v", err)
	}
	if cfg.Region != "us-west-2" {
		t.Fatalf("pipeline AWS config region = %q, want the instance's us-west-2 from IMDS", cfg.Region)
	}
}

// An explicit region still wins over IMDS.
func TestLoadAWSConfig_AmbientRegionBeatsIMDS(t *testing.T) {
	onAnInstance(t)
	fakeIMDS(t, "us-west-2")
	t.Setenv("AWS_REGION", "eu-west-1")
	cfg, err := loadAWSConfig(context.Background())
	if err != nil {
		t.Fatalf("loadAWSConfig: %v", err)
	}
	if cfg.Region != "eu-west-1" {
		t.Fatalf("pipeline AWS config region = %q, want the explicit eu-west-1", cfg.Region)
	}
}

// The stage's S3 data handler is one of the clients the issue names; check the
// client it actually builds, not only the helper.
func TestNewStageDataHandler_TakesTheInstanceRegionFromIMDS(t *testing.T) {
	onAnInstance(t)
	fakeIMDS(t, "us-west-2")
	h, err := NewStageDataHandler(context.Background(), "bucket", "prefix", "pipe", "stage")
	if err != nil {
		t.Fatalf("NewStageDataHandler: %v", err)
	}
	if got := h.s3Client.Options().Region; got != "us-west-2" {
		t.Fatalf("data handler S3 region = %q, want the instance's us-west-2 from IMDS", got)
	}
}

// Every AWS config in pkg/pipeline must come from loadAWSConfig. Six call sites
// used to build their own with a bare LoadDefaultConfig (#906); fixing five and
// missing one, or a new client added later the same way, would put the bug back
// for that client alone. Matched structurally: a call to LoadDefaultConfig on
// whatever name the SDK config package is imported under, so a comment or a
// string mentioning it does not count.
func TestEveryPipelineAWSConfigGoesThroughLoadAWSConfig(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var found, inHelper int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		configNames := map[string]bool{}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path != "github.com/aws/aws-sdk-go-v2/config" {
				continue
			}
			if imp.Name != nil {
				configNames[imp.Name.Name] = true
			} else {
				configNames["config"] = true
			}
		}
		if len(configNames) == 0 {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "LoadDefaultConfig" {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || !configNames[pkg.Name] {
					return true
				}
				found++
				if fn.Recv == nil && fn.Name.Name == "loadAWSConfig" {
					inHelper++
					return true
				}
				t.Errorf("%s: %s builds its own AWS config; use loadAWSConfig so it gets the instance's region (#906)",
					fset.Position(call.Pos()), fn.Name.Name)
				return true
			})
		}
	}
	// A discovery that found nothing has broken, not proven the package clean.
	if found == 0 || inHelper == 0 {
		t.Fatalf("found %d LoadDefaultConfig calls (%d in loadAWSConfig); the scan is broken", found, inHelper)
	}
}
