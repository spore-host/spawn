package cmd

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/queue"
)

type countingProfiler struct {
	calls   int
	configs []aws.IAMRoleConfig
}

func (c *countingProfiler) CreateOrGetInstanceProfile(_ context.Context, cfg aws.IAMRoleConfig) (string, error) {
	c.calls++
	c.configs = append(c.configs, cfg)
	return "spawn-instance-test", nil
}

type stmt struct {
	Effect   string
	Action   []string
	Resource []string
}

func parseStatements(t *testing.T, doc string) []stmt {
	t.Helper()
	var d struct{ Statement []stmt }
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		t.Fatalf("policy is not JSON: %v\n%s", err, doc)
	}
	return d.Statement
}

func grants(stmts []stmt, action, resource string) bool {
	for _, s := range stmts {
		if s.Effect != "Allow" {
			continue
		}
		for _, a := range s.Action {
			if a != action {
				continue
			}
			for _, r := range s.Resource {
				if r == resource {
					return true
				}
			}
		}
	}
	return false
}

// TestBatchQueueGetsAScopedRole is #1344 item 2: a batch queue launched with
// no --iam-role/--instance-profile gets a spawn-instance-* role that can write
// its results under exactly its prefix and read its queue file, and nothing
// else; an explicit profile or role is used verbatim with no IAM call.
func TestBatchQueueGetsAScopedRole(t *testing.T) {
	qc := &queue.QueueConfig{ResultS3Bucket: "my-results", ResultS3Prefix: "runs/q1"}

	p := &countingProfiler{}
	got, err := batchQueueInstanceProfile(context.Background(), p, "", "", "spawn-schedules-us-east-1", qc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "spawn-instance-test" || p.calls != 1 {
		t.Fatalf("want one CreateOrGetInstanceProfile call, got %d (profile %q)", p.calls, got)
	}
	cfg := p.configs[0]
	if cfg.RoleName != "" || len(cfg.Policies) > 0 || len(cfg.ManagedPolicies) > 0 {
		t.Fatalf("the role must be named by its policy hash (spawn-instance-*, reaped by #1300) and carry only the scoped policy: %+v", cfg)
	}
	stmts := parseStatements(t, cfg.InlinePolicyJSON)
	for _, a := range []string{"s3:PutObject", "s3:AbortMultipartUpload"} {
		if !grants(stmts, a, "arn:aws:s3:::my-results/runs/q1/*") {
			t.Errorf("no %s on the results prefix:\n%s", a, cfg.InlinePolicyJSON)
		}
	}
	if !grants(stmts, "s3:GetObject", "arn:aws:s3:::spawn-schedules-us-east-1/schedules/*/params.yaml") {
		t.Errorf("no s3:GetObject on the queue file:\n%s", cfg.InlinePolicyJSON)
	}
	for _, s := range stmts {
		for _, r := range s.Resource {
			if r == "*" || strings.HasSuffix(r, ":::my-results/*") || strings.HasSuffix(r, ":::my-results") {
				t.Errorf("grant wider than the results prefix: %s", r)
			}
		}
	}

	for _, tc := range []struct{ profile, role, want string }{
		{"my-profile", "my-role", "my-profile"},
		{"", "my-role", "my-role"},
	} {
		p := &countingProfiler{}
		got, err := batchQueueInstanceProfile(context.Background(), p, tc.profile, tc.role, "spawn-schedules-us-east-1", qc)
		if err != nil || got != tc.want || p.calls != 0 {
			t.Errorf("profile=%q role=%q: got %q, err %v, %d IAM calls; want %q verbatim and none", tc.profile, tc.role, got, err, p.calls, tc.want)
		}
	}
}

// TestBatchQueueRejectsUnscopableResults: a results location that would widen
// the grant or break the policy is refused before any IAM call.
func TestBatchQueueRejectsUnscopableResults(t *testing.T) {
	for _, qc := range []queue.QueueConfig{
		{ResultS3Bucket: "s3://my-results", ResultS3Prefix: "x"},
		{ResultS3Bucket: "my-*", ResultS3Prefix: "x"},
		{ResultS3Bucket: "my-results", ResultS3Prefix: "runs/*"},
		{ResultS3Bucket: "my-results", ResultS3Prefix: "${aws:username}"},
	} {
		p := &countingProfiler{}
		if _, err := batchQueueInstanceProfile(context.Background(), p, "", "", "spawn-schedules-us-east-1", &qc); err == nil || p.calls != 0 {
			t.Errorf("%+v: want refusal before any IAM call, got err %v and %d calls", qc, err, p.calls)
		}
	}
	// No results bucket: the role still reads its queue file, and writes nowhere.
	p := &countingProfiler{}
	if _, err := batchQueueInstanceProfile(context.Background(), p, "", "", "spawn-schedules-us-east-1", &queue.QueueConfig{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range parseStatements(t, p.configs[0].InlinePolicyJSON) {
		for _, a := range s.Action {
			if a == "s3:PutObject" {
				t.Errorf("write granted with no results bucket: %+v", s)
			}
		}
	}
}

// TestBatchQueueLaunchUsesTheScopedRole holds launchWithBatchQueue to the
// mechanism: it resolves its profile through batchQueueInstanceProfile and
// never calls SetupSporedIAMRole, the shared role that cannot write results.
func TestBatchQueueLaunchUsesTheScopedRole(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "launch_batchqueue.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "launchWithBatchQueue" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("launchWithBatchQueue not found")
	}
	var scoped, shared int
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == "batchQueueInstanceProfile" {
				scoped++
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == "SetupSporedIAMRole" {
				shared++
			}
		}
		return true
	})
	if scoped != 1 || shared != 0 {
		t.Fatalf("launchWithBatchQueue: %d calls to batchQueueInstanceProfile (want 1), %d to SetupSporedIAMRole (want 0)", scoped, shared)
	}
}
