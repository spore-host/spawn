package aws

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// #39: the --nested-spawn role must cover what a spawn CLI on the instance
// actually calls for the child-instance commands. The set is DISCOVERED from
// source, not listed: starting from every function in the cmd files for those
// commands, follow references through the spawn module, and collect every AWS
// SDK request built (`&<service>.<Op>Input{`). Each discovered action must be
// granted by the nested role, or be in nestedSpawnNotGranted with the reason a
// nested spore deliberately goes without it.
//
// The resolution is by name, so it over-approximates (a method name shared by
// two types follows both). That only ever adds actions to account for; it
// cannot hide one.

// nestedSpawnCommandFiles are the commands nested spawn is for: the cmd files
// defining launch, list, status, extend, connect and terminate.
var nestedSpawnCommandFiles = []string{
	"launch_single.go", "list.go", "status.go", "extend.go", "connect.go", "terminate.go",
}

// nestedSpawnNotGranted: discovered calls the nested role does not grant, and
// why. Each one fails at the call (AccessDenied) rather than silently.
var nestedSpawnNotGranted = map[string]string{
	// IAM writes: a spore that can write IAM can grant itself anything. A nested
	// spore uses existing profiles instead (cmd nestedChildProfile).
	"iam:CreateRole":               "IAM write; nested spores never write IAM",
	"iam:PutRolePolicy":            "IAM write; nested spores never write IAM",
	"iam:AttachRolePolicy":         "IAM write; nested spores never write IAM",
	"iam:CreateInstanceProfile":    "IAM write; nested spores never write IAM",
	"iam:AddRoleToInstanceProfile": "IAM write; nested spores never write IAM",
	"iam:GetRole":                  "only reached by the role-setup paths a nested spore skips",
	"iam:GetInstanceProfile":       "only reached by the role-setup paths a nested spore skips",
	// Not part of a plain child launch.
	"fsx:CreateFileSystem":               "--fsx-create: ~$174/month minimum; create filesystems from a workstation",
	"fsx:DeleteFileSystem":               "--fsx-create teardown; see fsx:CreateFileSystem",
	"ec2:AllocateAddress":                "multi-card EFA public IP; its association cannot be tag-scoped (the ENI is untagged)",
	"ec2:AssociateAddress":               "multi-card EFA public IP; see ec2:AllocateAddress",
	"ec2:DisassociateAddress":            "multi-card EFA public IP; see ec2:AllocateAddress",
	"s3:CreateBucket":                    "staging/results bucket creation belongs to the workstation",
	"s3:HeadBucket":                      "staging buckets; not a plain child launch",
	"s3:ListBuckets":                     "--fsx-s3-bucket config lookup; FSx is not granted",
	"s3:PutBucketLifecycleConfiguration": "staging bucket setup; see s3:CreateBucket",
	"s3:GetBucketLifecycleConfiguration": "schedules bucket lifecycle (batch queue upload, #1382); see s3:CreateBucket",
	"s3:DeleteBucketLifecycle":           "schedules bucket lifecycle migration (#1382); see s3:CreateBucket",
	"s3:PutPublicAccessBlock":            "staging bucket setup; see s3:CreateBucket",
	"s3:GetBucketTagging":                "results-bucket bookkeeping; not a plain child launch",
	"s3:PutBucketTagging":                "results-bucket bookkeeping; not a plain child launch",
	"s3:DeleteBucketTagging":             "results-bucket bookkeeping; not a plain child launch",
	"s3:PutObject":                       "sweep/schedule parameter upload; sweeps are refused with --nested-spawn",
	"dynamodb:GetItem":                   "sweep records; sweeps are not a child launch",
	"dynamodb:PutItem":                   "sweep records; sweeps are not a child launch",
	"lambda:Invoke":                      "detached sweep orchestrator; sweeps are not a child launch",
	"sts:AssumeRole":                     "cross-account sweep validation; nested spawn stays in its own account",
	"account:GetAccountInformation":      "best effort: DNS falls back to the base36 account label",
}

// nestedSpawnNeedsNoGrant are calls IAM does not authorize.
var nestedSpawnNeedsNoGrant = map[string]bool{
	"sts:GetCallerIdentity": true,
}

func TestNestedSpawnPolicyCoversTheChildLaunchCalls(t *testing.T) {
	discovered := discoverSDKActions(t, filepath.Join("..", ".."), nestedSpawnCommandFiles)
	if len(discovered) < 20 || discovered["ec2:RunInstances"] == "" {
		t.Fatalf("discovery found %d actions (RunInstances: %q); a discovery that finds this little has broken, not proven anything", len(discovered), discovered["ec2:RunInstances"])
	}
	// The nested role carries the spored baseline too, so a call the baseline
	// grants is covered. "Both granted and excluded" is judged against the
	// nested grant document alone: the baseline grants some actions on
	// resources a child launch does not use (dynamodb:PutItem on the hybrid
	// registry table only, for one).
	nestedGrant := allowedActions(t, nestedSpawnPolicyDoc())
	granted := map[string]bool{}
	for a := range nestedGrant {
		granted[a] = true
	}
	for a := range allowedActions(t, (&Client{}).nestedSpawnBaselinePolicyDoc()) {
		granted[a] = true
	}
	var names []string
	for a := range discovered {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		_, excluded := nestedSpawnNotGranted[a]
		switch {
		case nestedSpawnNeedsNoGrant[a]:
		case nestedGrant[a] && excluded:
			t.Errorf("%s is both granted to the nested role and listed as not granted", a)
		case !granted[a] && !excluded:
			t.Errorf("the child-launch commands call %s (via %s) and the nested-spawn role does not grant it; grant it in nestedSpawnStatements or list it in nestedSpawnNotGranted with the reason", a, discovered[a])
		}
	}
	for a := range nestedSpawnNotGranted {
		if discovered[a] == "" {
			t.Errorf("nestedSpawnNotGranted lists %s, which no child-launch command calls any more; remove it", a)
		}
	}
}

// allowedActions is every action an Allow statement names in doc.
func allowedActions(t *testing.T, doc string) map[string]bool {
	t.Helper()
	var pol struct {
		Statement []struct {
			Effect string
			Action interface{}
		}
	}
	if err := json.Unmarshal([]byte(doc), &pol); err != nil {
		t.Fatalf("policy is not JSON: %v", err)
	}
	out := map[string]bool{}
	for _, st := range pol.Statement {
		if st.Effect != "Allow" {
			continue
		}
		for _, a := range stringList(st.Action) {
			out[a] = true
		}
	}
	return out
}

func stringList(v interface{}) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, _ := e.(string)
			out = append(out, s)
		}
		return out
	}
	return nil
}

const spawnModule = "github.com/spore-host/spawn/"

// sdkIAMPrefix maps an SDK import path to its IAM action prefix; "" means the
// package makes no IAM-authorized call (IMDS).
func sdkIAMPrefix(path string) (string, bool) {
	switch {
	case strings.HasPrefix(path, "github.com/aws/aws-sdk-go-v2/service/"):
		svc := strings.TrimPrefix(path, "github.com/aws/aws-sdk-go-v2/service/")
		if svc == "efs" {
			return "elasticfilesystem", true
		}
		return svc, true
	case path == "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager":
		return "s3", true
	case path == "github.com/aws/aws-sdk-go-v2/feature/ec2/imds":
		return "", true
	}
	return "", false
}

// transferManagerOps maps the transfer manager's operations to the S3 action.
var transferManagerOps = map[string]string{"UploadObject": "PutObject", "DownloadObject": "GetObject"}

type srcFunc struct {
	body    *ast.BlockStmt
	pkg     string
	imports map[string]string // local name -> import path
}

type sdkDiscovery struct {
	root    string
	fset    *token.FileSet
	pkgs    map[string]map[string][]srcFunc // pkg dir -> func name -> bodies
	seen    map[string]bool
	actions map[string]string // action -> the function that builds it
	t       *testing.T
}

func (d *sdkDiscovery) load(dir string) map[string][]srcFunc {
	if p, ok := d.pkgs[dir]; ok {
		return p
	}
	out := map[string][]srcFunc{}
	d.pkgs[dir] = out
	files, _ := filepath.Glob(filepath.Join(d.root, dir, "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(d.fset, f, nil, 0)
		if err != nil {
			d.t.Fatalf("parse %s: %v", f, err)
		}
		imps := map[string]string{}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			name := filepath.Base(p)
			if im.Name != nil {
				name = im.Name.Name
			}
			imps[name] = p
		}
		for _, decl := range af.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				out[fd.Name.Name] = append(out[fd.Name.Name], srcFunc{fd.Body, dir, imps})
			}
		}
	}
	return out
}

func (d *sdkDiscovery) resolve(dir, name string) {
	key := dir + "." + name
	if d.seen[key] {
		return
	}
	fns := d.load(dir)[name]
	if len(fns) == 0 {
		return
	}
	d.seen[key] = true
	for _, f := range fns {
		d.visit(key, f)
	}
}

func (d *sdkDiscovery) visit(where string, f srcFunc) {
	ast.Inspect(f.body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			se, ok := x.Type.(*ast.SelectorExpr)
			if !ok || !strings.HasSuffix(se.Sel.Name, "Input") {
				return true
			}
			id, ok := se.X.(*ast.Ident)
			if !ok {
				return true
			}
			prefix, isSDK := sdkIAMPrefix(f.imports[id.Name])
			if !isSDK || prefix == "" {
				return true
			}
			op := strings.TrimSuffix(se.Sel.Name, "Input")
			if m, ok := transferManagerOps[op]; ok && prefix == "s3" {
				op = m
			}
			if _, ok := d.actions[prefix+":"+op]; !ok {
				d.actions[prefix+":"+op] = where
			}
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				if p, ok := f.imports[id.Name]; ok {
					if strings.HasPrefix(p, spawnModule) {
						d.resolve(strings.TrimPrefix(p, spawnModule), x.Sel.Name)
					}
					return false
				}
			}
			// A method or field: resolve in this package and every spawn
			// package this file imports.
			d.resolve(f.pkg, x.Sel.Name)
			for _, p := range f.imports {
				if strings.HasPrefix(p, spawnModule) {
					d.resolve(strings.TrimPrefix(p, spawnModule), x.Sel.Name)
				}
			}
		case *ast.Ident:
			d.resolve(f.pkg, x.Name)
		}
		return true
	})
}

// discoverSDKActions returns every IAM action reachable from the functions
// defined in cmdFiles, mapped to the function that builds the request.
func discoverSDKActions(t *testing.T, root string, cmdFiles []string) map[string]string {
	t.Helper()
	d := &sdkDiscovery{root: root, fset: token.NewFileSet(), pkgs: map[string]map[string][]srcFunc{},
		seen: map[string]bool{}, actions: map[string]string{}, t: t}
	cmdFuncs := d.load("cmd")
	for _, file := range cmdFiles {
		af, err := parser.ParseFile(d.fset, filepath.Join(root, "cmd", file), nil, 0)
		if err != nil {
			t.Fatalf("parse cmd/%s: %v", file, err)
		}
		found := 0
		for _, decl := range af.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil && len(cmdFuncs[fd.Name.Name]) > 0 {
				found++
				d.resolve("cmd", fd.Name.Name)
			}
		}
		if found == 0 {
			t.Fatalf("cmd/%s defines no functions; the command list is stale", file)
		}
	}
	return d.actions
}
