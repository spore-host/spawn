package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// repoRoot is the spawn checkout these tests run in.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func lambda(t *testing.T, r *Report, id string) LambdaResult {
	t.Helper()
	for _, l := range r.Lambdas {
		if l.ID == id {
			return l
		}
	}
	t.Fatalf("no Lambda %s in the report; have %v", id, lambdaIDs(r))
	return LambdaResult{}
}

func lambdaIDs(r *Report) []string {
	var ids []string
	for _, l := range r.Lambdas {
		ids = append(ids, l.ID)
	}
	return ids
}

func input(r *Report, p string) *InputResult {
	for i := range r.Inputs {
		if r.Inputs[i].Path == p {
			return &r.Inputs[i]
		}
	}
	return nil
}

func wantLambda(t *testing.T, r *Report, id, status, kind string) LambdaResult {
	t.Helper()
	l := lambda(t, r, id)
	if l.Status != status || l.DeployKind != kind {
		t.Errorf("%s: got %s/%s, want %s/%s; reasons: %v", id, l.Status, l.DeployKind, status, kind, l.Reasons)
	}
	return l
}

func wantInput(t *testing.T, r *Report, p, status string) {
	t.Helper()
	in := input(r, p)
	if in == nil {
		t.Errorf("%s is not a discovered deploy input", p)
		return
	}
	wantKind := KindManual
	if status == StatusUnchanged {
		wantKind = KindNone
	}
	if in.Status != status || in.DeployKind != wantKind {
		t.Errorf("%s: got %s/%s, want %s/%s", p, in.Status, in.DeployKind, status, wantKind)
	}
}

func hasChange(l LambdaResult, p, class string) bool {
	for _, c := range l.Changes {
		if c.Path == p && c.Class == class {
			return true
		}
	}
	return false
}

// TestV0151IsTheReleaseThatMotivatedThis replays the release #1444 was written
// about. On v0.151.0 a `git diff -- lambda/` showed only #1434's files; both
// orchestrators also compile pkg/aws (and the sweep orchestrator pkg/launcher),
// which #1433 changed, and only `go list -deps` caught it. The deploy record on
// PR #1440 is the documented intent: the role script and the mapping script were
// manual steps, ws-processor and both orchestrators needed a code deploy, and the
// reaper did not compile anything that changed.
//
// Needs the real tags; CI's shallow checkout has none, so it skips there and
// TestSyntheticRelease below covers the same shapes everywhere.
func TestV0151IsTheReleaseThatMotivatedThis(t *testing.T) {
	root := repoRoot(t)
	for _, tag := range []string{"v0.150.0", "v0.151.0"} {
		if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "-q", tag+"^{commit}").Run(); err != nil {
			t.Skipf("tag %s not in this checkout (shallow clone?); TestSyntheticRelease covers the same cases", tag)
		}
	}
	r, err := checklist(root, "v0.150.0", "v0.151.0", goListCompile)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"lambda/sweep-orchestrator", "lambda/pipeline-orchestrator"} {
		l := wantLambda(t, r, id, StatusChanged, KindCodeOnly)
		if !hasChange(l, "pkg/aws/client.go", ClassCompiled) {
			t.Errorf("%s: the pkg/aws change (#1433) is not among its compiled changes: %v", id, l.Reasons)
		}
	}
	if l := lambda(t, r, "lambda/sweep-orchestrator"); !hasChange(l, "pkg/launcher/provision.go", ClassCompiled) {
		t.Errorf("sweep-orchestrator compiles pkg/launcher/provision.go, which #1433 changed: %v", l.Reasons)
	}
	wantLambda(t, r, "lambda/dashboard-websocket-processor", StatusChanged, KindCodeOnly)
	wantLambda(t, r, "lambda/dashboard-api", StatusChanged, KindCodeOnly)
	wantLambda(t, r, "lambda/ttl-reaper", StatusUnchanged, KindNone)
	wantInput(t, r, "lambda/dashboard-api/scripts/setup-dashboard-lambda-role.sh", StatusChanged)
	wantInput(t, r, "lambda/dashboard-websocket/scripts/setup-event-source-mappings.sh", StatusChanged)
	wantInput(t, r, "scripts/setup-sweep-orchestrator-lambda-role.sh", StatusUnchanged)
}

// fixtureRepo is a small repo with the same shapes as spawn: a nested Lambda
// module with `replace ../..`, Lambdas in the root module behind SAM templates,
// a shell deploy with no `cd`, role and mapping scripts, and a data backfill.
var fixtureV1 = map[string]string{
	"go.mod":                                 "module example.com/fix\n\ngo 1.24\n",
	"pkg/aws/aws.go":                         "package aws\n\nfunc F() int { return 1 }\n",
	"pkg/other/other.go":                     "package other\n\nfunc G() int { return 1 }\n",
	"pkg/other/other_test.go":                "package other\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) {}\n",
	"pkg/unused/unused.go":                   "package unused\n\nfunc H() int { return 1 }\n",
	"lambda/sweep/go.mod":                    "module example.com/fix/lambda/sweep\n\ngo 1.24\n\nrequire example.com/fix v0.0.0-00010101000000-000000000000\n\nreplace example.com/fix => ../..\n",
	"lambda/sweep/main.go":                   "package main\n\nimport \"example.com/fix/pkg/aws\"\n\nfunc main() { _ = aws.F() }\n",
	"lambda/pipeline/main.go":                "package main\n\nimport \"example.com/fix/pkg/aws\"\n\nfunc main() { _ = aws.F() }\n",
	"lambda/reaper/main.go":                  "package main\n\nimport \"example.com/fix/pkg/other\"\n\nfunc main() { _ = other.G() }\n",
	"lambda/reaper/README.md":                "# reaper\n",
	"lambda/dash/main.go":                    "package main\n\nfunc main() {}\n",
	"lambda/dash/deploy.sh":                  "#!/bin/bash\n# Usage: ./deploy.sh <aws-profile>\nFUNCTION_NAME=\"fix-dash\"\nGOOS=linux GOARCH=amd64 go build -o bootstrap .\naws lambda update-function-code --function-name \"$FUNCTION_NAME\" --zip-file fileb://function.zip\n",
	"scripts/deploy-sweep.sh":                "#!/bin/bash\nFUNCTION_NAME=\"${FIX_NAME:-fix-sweep}\"\ncd \"$(dirname \"$0\")/../lambda/sweep\"\nGOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap .\naws lambda update-function-code --function-name \"$FUNCTION_NAME\" --zip-file fileb://function.zip\n",
	"lambda/dash/scripts/setup-dash-role.sh": "#!/bin/bash\naws iam put-role-policy --role-name R --policy-name P --policy-document file://policy.json\n",
	"lambda/dash/scripts/policy.json":        "{\"Version\": \"2012-10-17\"}\n",
	"lambda/dash/scripts/setup-mappings.sh":  "#!/bin/bash\naws lambda create-event-source-mapping --function-name fix-dash\n",
	"lambda/dash/scripts/backfill.sh":        "#!/bin/bash\naws dynamodb update-item --table-name T --key '{}'\n",
	"lambda/dash/scripts/hints.sh":           "#!/bin/bash\necho \"next: aws iam put-role-policy ...\"\n",
}

func samFixture(name, goarch string) map[string]string {
	return map[string]string{
		"lambda/" + name + "/template.yaml": "AWSTemplateFormatVersion: '2010-09-09'\nTransform: AWS::Serverless-2016-10-31\nResources:\n  Fn:\n    Type: AWS::Serverless::Function\n    Properties:\n      FunctionName: fix-" + name + "\n      CodeUri: build/\n",
		"lambda/" + name + "/Makefile":      "build:\n\t@# a comment naming go build main.go, which must not be read as the build\n\tGOOS=linux GOARCH=" + goarch + " go build -tags lambda.norpc -o build/bootstrap .\n\ndeploy: build\n\tsam deploy --template-file template.yaml\n",
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	// Hermetic: no global or system config (signing hooks, templates).
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newFixture commits v1, applies the change set, and commits it as v2.
func newFixture(t *testing.T, change map[string]string, remove ...string) string {
	t.Helper()
	dir := t.TempDir()
	v1 := map[string]string{}
	for k, v := range fixtureV1 {
		v1[k] = v
	}
	for k, v := range samFixture("pipeline", "arm64") {
		v1[k] = v
	}
	for k, v := range samFixture("reaper", "arm64") {
		v1[k] = v
	}
	writeFiles(t, dir, v1)
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "v1")
	gitIn(t, dir, "tag", "v1")
	writeFiles(t, dir, change)
	for _, p := range remove {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "v2")
	gitIn(t, dir, "tag", "v2")
	return dir
}

// TestSyntheticRelease covers the v0.151.0 shapes without needing spawn's
// history: a shared package compiled into two Lambdas (one in a nested module
// through `replace ../..`, one in the root module), role and mapping scripts,
// and test-only and doc-only edits that must NOT mark a Lambda changed.
func TestSyntheticRelease(t *testing.T) {
	dir := newFixture(t, map[string]string{
		"pkg/aws/aws.go": "package aws\n\nfunc F() int { return 2 }\n",
		"pkg/aws/new.go": "package aws\n\nfunc N() int { return 3 }\n",
		// Never compiled into anything: not a reason to deploy.
		"pkg/unused/unused.go": "package unused\n\nfunc H() int { return 2 }\n",
		// Test-only and doc-only edits to what the reaper compiles.
		"pkg/other/other_test.go":                "package other\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) { t.Log(1) }\n",
		"lambda/reaper/main_test.go":             "package main\n\nimport \"testing\"\n\nfunc TestMain2(t *testing.T) {}\n",
		"lambda/reaper/README.md":                "# reaper, documented\n",
		"docs/reaper.md":                         "# the reaper\n",
		"lambda/dash/scripts/setup-dash-role.sh": "#!/bin/bash\naws iam put-role-policy --role-name R --policy-name P2 --policy-document file://policy.json\n",
		"lambda/dash/scripts/policy.json":        "{\"Version\": \"2012-10-17\", \"Statement\": []}\n",
		"lambda/dash/scripts/setup-mappings.sh":  "#!/bin/bash\naws lambda delete-event-source-mapping --uuid x\n",
		"lambda/dash/scripts/backfill.sh":        "#!/bin/bash\naws dynamodb update-item --table-name T2 --key '{}'\n",
		"lambda/dash/deploy.sh":                  fixtureV1["lambda/dash/deploy.sh"] + "aws lambda update-function-configuration --function-name \"$FUNCTION_NAME\" --timeout 30\n",
	})
	r, err := checklist(dir, "v1", "v2", goListCompile)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"lambda/sweep", "lambda/pipeline"} {
		l := wantLambda(t, r, id, StatusChanged, KindCodeOnly)
		if !hasChange(l, "pkg/aws/aws.go", ClassCompiled) || !hasChange(l, "pkg/aws/new.go", ClassCompiled) {
			t.Errorf("%s: want pkg/aws/aws.go (modified) and pkg/aws/new.go (added) among compiled changes, got %+v", id, l.Changes)
		}
		if hasChange(l, "pkg/unused/unused.go", ClassCompiled) {
			t.Errorf("%s: a package it never compiles counted as an input", id)
		}
	}
	wantLambda(t, r, "lambda/reaper", StatusUnchanged, KindNone)
	// A changed deploy script is a manual deploy even when the code changed too.
	wantLambda(t, r, "lambda/dash", StatusChanged, KindManual)
	wantInput(t, r, "lambda/dash/scripts/setup-dash-role.sh", StatusChanged)
	wantInput(t, r, "lambda/dash/scripts/setup-mappings.sh", StatusChanged)
	wantInput(t, r, "lambda/dash/scripts/policy.json", StatusChanged)
	for _, p := range []string{"lambda/dash/scripts/backfill.sh", "lambda/dash/scripts/hints.sh", "lambda/reaper/README.md", "docs/reaper.md"} {
		if in := input(r, p); in != nil {
			t.Errorf("%s is not a deploy input (a data write, an echoed hint, or a doc) but was reported: %+v", p, in)
		}
	}

	// The commands come from the mechanisms: a script that never cds runs from
	// its own directory with its Usage arguments; one that locates itself runs by
	// path; a SAM template deploys through its Makefile.
	cmds := map[string]string{}
	for _, l := range r.Lambdas {
		for _, m := range l.Mechanisms {
			cmds[l.ID] = m.Command
		}
	}
	for id, want := range map[string]string{
		"lambda/dash":     "(cd lambda/dash && ./deploy.sh <aws-profile>)",
		"lambda/sweep":    "scripts/deploy-sweep.sh",
		"lambda/pipeline": "make -C lambda/pipeline deploy",
	} {
		if cmds[id] != want {
			t.Errorf("%s: deploy command %q, want %q", id, cmds[id], want)
		}
	}
	if l := lambda(t, r, "lambda/sweep"); strings.Join(l.Functions, ",") != "fix-sweep" || l.Build.GOARCH != "amd64" || strings.Join(l.Build.Tags, ",") != "lambda.norpc" {
		t.Errorf("sweep: functions %v build %+v; want fix-sweep, amd64, lambda.norpc from its deploy script", l.Functions, l.Build)
	}
}

// TestJSONIsTheContractFor1445 pins the fields #1445 will read to pick what it
// may auto-deploy: only status "changed" with deploy_kind "code-only", by
// function name and build.
func TestJSONIsTheContractFor1445(t *testing.T) {
	dir := newFixture(t, map[string]string{"pkg/aws/aws.go": "package aws\n\nfunc F() int { return 2 }\n"})
	var buf bytes.Buffer
	if err := run([]string{"--repo", dir, "--from", "v1", "--to", "v2", "--json"}, &buf); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schema  int `json:"schema"`
		Lambdas []struct {
			ID         string   `json:"id"`
			Status     string   `json:"status"`
			DeployKind string   `json:"deploy_kind"`
			Functions  []string `json:"functions"`
			Build      struct {
				GOOS, GOARCH string
			} `json:"build"`
		} `json:"lambdas"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != SchemaVersion {
		t.Errorf("schema %d, want %d", doc.Schema, SchemaVersion)
	}
	var deployable []string
	for _, l := range doc.Lambdas {
		if l.Status == StatusChanged && l.DeployKind == KindCodeOnly {
			deployable = append(deployable, l.ID+"="+strings.Join(l.Functions, ",")+"@"+l.Build.GOOS+"/"+l.Build.GOARCH)
		}
	}
	sort.Strings(deployable)
	if got, want := strings.Join(deployable, " "), "lambda/pipeline=fix-pipeline@linux/arm64 lambda/sweep=fix-sweep@linux/amd64"; got != want {
		t.Errorf("code-only deployables %q, want %q", got, want)
	}
	var md bytes.Buffer
	if err := run([]string{"--repo", dir, "--from", "v1", "--to", "v2"}, &md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "- [ ] **lambda/sweep** (`fix-sweep`): changed, **code-only**") {
		t.Errorf("markdown checklist lacks the sweep item:\n%s", md.String())
	}
}

// TestDiscoveryAgreesWithTheCensus is the discovery gate. The census
// (scripts/lambda-deploy-census.sh) is the inventory of operated deploy
// mechanisms; this tool must see exactly what it sees. A new mechanism the
// census learns and this tool does not, or a new FORM of mechanism, fails here.
func TestDiscoveryAgreesWithTheCensus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the census is a bash script")
	}
	root := repoRoot(t)
	cmd := exec.Command("bash", "scripts/lambda-deploy-census.sh")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("census failed, so it has no inventory to agree with: %v\n%s", err, out)
	}
	line := regexp.MustCompile(`^\s*✅ (\S+) \((.+)\)$`)
	census := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		m := line.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		switch m[2] {
		case "SAM template":
			census["sam:lambda/"+m[1]+"/template.yaml"] = true
		case "SAM template, no function":
		case "shell deploy":
			census["shell:"+m[1]] = true
		case "Go deploy":
			census["go:"+m[1]] = true
		default:
			t.Errorf("the census reports a mechanism form this tool does not know: %q. Teach discover() to read it, then add it here", l)
		}
	}
	if len(census) == 0 {
		t.Fatalf("parsed no mechanisms from the census output; its format changed:\n%s", out)
	}
	tree, err := newTree(root)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := discover(tree)
	if err != nil {
		t.Fatal(err)
	}
	tool := map[string]bool{}
	for _, c := range inv.Lambdas {
		for _, m := range c.Mechanisms {
			tool[m.Kind+":"+m.Path] = true
		}
	}
	for _, g := range inv.CLIDeploys {
		tool["go:"+g.Dir] = true
	}
	for k := range census {
		if !tool[k] {
			t.Errorf("the census sees deploy mechanism %s; this tool does not, so no release checklist would mention it", k)
		}
	}
	for k := range tool {
		if !census[k] {
			t.Errorf("this tool sees deploy mechanism %s; the census does not, so it is not checked for a version stamp or log retention", k)
		}
	}
}

// TestANewDeployMechanismIsSeenOrRefused: a deploy script added anywhere is
// discovered; one the tool cannot map to a package is an error, never dropped.
func TestANewDeployMechanismIsSeenOrRefused(t *testing.T) {
	dir := newFixture(t, map[string]string{
		"lambda/newfn/main.go":      "package main\n\nfunc main() {}\n",
		"ops/release/ship-newfn.sh": "#!/bin/bash\nFUNCTION_NAME=fix-newfn\ncd lambda/newfn\nGOOS=linux GOARCH=arm64 go build -o bootstrap .\naws lambda create-function --function-name \"$FUNCTION_NAME\"\n",
	})
	r, err := checklist(dir, "v1", "v2", goListCompile)
	if err != nil {
		t.Fatal(err)
	}
	l := wantLambda(t, r, "lambda/newfn", StatusAdded, KindManual)
	if len(l.Mechanisms) != 1 || l.Mechanisms[0].Path != "ops/release/ship-newfn.sh" {
		t.Errorf("newfn mechanisms %+v, want ops/release/ship-newfn.sh", l.Mechanisms)
	}

	bad := newFixture(t, map[string]string{
		"ops/mystery.sh": "#!/bin/bash\nGOOS=linux GOARCH=arm64 go build -o bootstrap .\naws lambda update-function-code --function-name somewhere\n",
	})
	if _, err := checklist(bad, "v1", "v2", goListCompile); err == nil || !strings.Contains(err.Error(), "ops/mystery.sh") {
		t.Errorf("a deploy script naming no Lambda directory must fail discovery, got %v", err)
	}
}

// TestAPackageErrorIsNotUnchanged: `go list -e` reports a broken import as a
// package error and carries on; a partial file set would read as "unchanged".
func TestAPackageErrorIsNotUnchanged(t *testing.T) {
	dir := newFixture(t, map[string]string{
		"lambda/reaper/main.go": "package main\n\nimport (\n\t\"example.com/fix/pkg/missing\"\n\t\"example.com/fix/pkg/other\"\n)\n\nfunc main() { _ = other.G(); _ = missing.X }\n",
	})
	_, err := checklist(dir, "v1", "v2", goListCompile)
	if err == nil || !strings.Contains(err.Error(), "pkg/missing") {
		t.Fatalf("want a refusal naming the broken import, got %v", err)
	}
}

// TestRemovedInputsAndLambdasAreReported: deleting a role script or a Lambda's
// only mechanism is a manual step (tear down what it made), not silence.
func TestRemovedInputsAndLambdasAreReported(t *testing.T) {
	dir := newFixture(t, nil, "lambda/dash/scripts/setup-mappings.sh", "scripts/deploy-sweep.sh")
	r, err := checklist(dir, "v1", "v2", goListCompile)
	if err != nil {
		t.Fatal(err)
	}
	wantInput(t, r, "lambda/dash/scripts/setup-mappings.sh", StatusRemoved)
	wantLambda(t, r, "lambda/sweep", StatusRemoved, KindManual)
	if got := path.Base(lambda(t, r, "lambda/sweep").Mechanisms[0].Path); got != "deploy-sweep.sh" {
		t.Errorf("a removed Lambda should name the mechanism that used to deploy it, got %s", got)
	}
}
