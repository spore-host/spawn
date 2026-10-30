package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const ciRoleScript = "scripts/setup-lambda-deploy-ci-role.sh"

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// sourceSubcommands discovers every `aws lambda` subcommand this package can
// invoke: the string literal at each deployer.callLambda call site in the
// non-test source. A call site with a non-literal subcommand fails, because it
// would hide a call from this gate.
func sourceSubcommands(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	awsExecs := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "callLambda":
				if len(c.Args) < 2 {
					t.Errorf("%s: callLambda with no subcommand", fset.Position(c.Pos()))
					return true
				}
				lit, ok := c.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s: callLambda's subcommand is not a string literal, so this gate cannot see it", fset.Position(c.Pos()))
					return true
				}
				s, _ := strconv.Unquote(lit.Value)
				seen[s] = true
			case "Command", "CommandContext":
				// exec.Command[Context](…, "aws", …): the CLI may be run only by awsCLI.
				for _, a := range c.Args {
					if lit, ok := a.(*ast.BasicLit); ok && lit.Value == `"aws"` {
						awsExecs++
					}
				}
			}
			return true
		})
	}
	if awsExecs != 1 {
		t.Errorf("found %d exec of the aws CLI; it must be exactly one (awsCLI.Lambda), so every call goes through callLambda", awsExecs)
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("discovered no callLambda subcommands: the discovery is broken")
	}
	return out
}

// roleGrants reads the actions and the TagResource tag keys the CI role's
// inline policy grants, from the script that puts it.
func roleGrants(t *testing.T) (actions map[string]bool, tagKeys map[string]bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ciRoleScript))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)POLICY_DOCUMENT=\$\(cat <<EOF\n(.*?)\nEOF\n\)`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s: no POLICY_DOCUMENT heredoc", ciRoleScript)
	}
	// The resource list is a shell expansion; replace it so the JSON parses.
	doc := strings.ReplaceAll(string(m[1]), "[${ARNS}]", `["arn"]`)
	var p struct {
		Statement []struct {
			Effect    string
			Action    any
			Condition map[string]map[string]any
		}
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("%s policy: %v\n%s", ciRoleScript, err, doc)
	}
	actions, tagKeys = map[string]bool{}, map[string]bool{}
	for _, s := range p.Statement {
		if s.Effect != "Allow" {
			continue
		}
		var acts []string
		switch a := s.Action.(type) {
		case string:
			acts = []string{a}
		case []any:
			for _, x := range a {
				acts = append(acts, x.(string))
			}
		}
		for _, a := range acts {
			actions[a] = true
			if a == "lambda:TagResource" {
				for _, cond := range s.Condition {
					if keys, ok := cond["aws:TagKeys"].([]any); ok {
						for _, k := range keys {
							tagKeys[k.(string)] = true
						}
					}
				}
			}
		}
	}
	if len(actions) == 0 {
		t.Fatalf("%s: parsed no actions", ciRoleScript)
	}
	return actions, tagKeys
}

// iamAction is the IAM action an `aws lambda` subcommand calls: the CLI
// spells the API operation in kebab case.
func iamAction(sub string) string {
	var b strings.Builder
	b.WriteString("lambda:")
	for _, part := range strings.Split(sub, "-") {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// Gate: every aws subcommand the deployer can invoke is granted by the CI role,
// and every tag it writes is one the role may write. The role cannot be widened
// from CI; a call it does not grant would fail only on the first live deploy.
func TestEveryLambdaCallIsGrantedByTheCIRole(t *testing.T) {
	subs := sourceSubcommands(t)
	actions, tagKeys := roleGrants(t)
	for _, s := range subs {
		if !actions[iamAction(s)] {
			t.Errorf("deploylambdas calls `aws lambda %s` (%s), which %s does not grant", s, iamAction(s), ciRoleScript)
		}
	}
	if !tagKeys[versionTag] {
		t.Errorf("deploylambdas stamps %s, which the role's TagResource condition %v does not allow", versionTag, tagKeys)
	}

	// The static set covers what actually runs: a deploy that fails, rolls back
	// and restores its tag exercises every path, and each subcommand it made
	// must be one the source scan found.
	f := newFixture(t)
	f.aws.onUpdate = func(fn string, fx *fakeFn, n int) {
		if fn == "fn-b" && n == 1 {
			fx.cfg["MemorySize"] = float64(1)
		}
	}
	if _, err := f.run(false, nil); err == nil {
		t.Fatal("expected the injected failure")
	}
	static := map[string]bool{}
	for _, s := range subs {
		static[s] = true
	}
	ran := map[string]bool{}
	for _, c := range f.aws.calls {
		ran[c.sub] = true
		if !static[c.sub] {
			t.Errorf("the deploy ran `aws lambda %s`, which the source scan did not find", c.sub)
		}
	}
	for _, s := range []string{"update-function-code", "tag-resource", "get-function", "get-function-configuration"} {
		if !ran[s] {
			t.Errorf("the exercised deploy never ran %s, so it does not cover the deployer", s)
		}
	}
}

func TestIAMActionMapping(t *testing.T) {
	if got := iamAction("get-function-configuration"); got != "lambda:GetFunctionConfiguration" {
		t.Errorf("got %s", got)
	}
}

// realInventory is the checklist of the current tree against itself: every
// operated Lambda, with its build and mechanisms, as discovered by
// scripts/deploychecklist.
func realInventory(t *testing.T) *checklist {
	t.Helper()
	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), "checklist.json")
	cmd := exec.Command("go", "run", "./deploychecklist", "--from", "HEAD", "--to", "HEAD", "--json-out", out)
	cmd.Dir = filepath.Join(root, "scripts")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deploychecklist: %v\n%s", err, b)
	}
	c, err := readChecklist(out)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Every operated Lambda's build line and mechanisms are ones this deployer can
// reproduce and check. A new mechanism it cannot read fails here, not on the
// first release that changes it.
func TestEveryOperatedLambdaIsDeployable(t *testing.T) {
	root := repoRoot(t)
	c := realInventory(t)
	bodyChecks := map[string]bool{}
	for _, l := range c.Lambdas {
		if _, err := parseBuild(l.Build, "0.0.0"); err != nil {
			t.Errorf("%s: %v", l.ID, err)
		}
		e, err := expect(root, l)
		if err != nil {
			t.Errorf("%s: %v", l.ID, err)
			continue
		}
		if e.BodyCheck {
			bodyChecks[l.ID] = true
		}
		sam := false
		for _, m := range l.Mechanisms {
			sam = sam || m.Kind == "sam"
		}
		if e.SAM != sam {
			t.Errorf("%s: SAM %v, but its mechanisms say %v", l.ID, e.SAM, sam)
		}
	}
	// deploy-sweep-orchestrator.sh verifies the published bootstrap body before
	// it deploys (#1368); the port must find that check.
	if !bodyChecks["lambda/sweep-orchestrator"] {
		t.Errorf("the bootstrap-body pre-flight was not found for lambda/sweep-orchestrator (found for %v)", bodyChecks)
	}
}

// The deployer renders the bootstrap body the way the release published it.
func TestBodyRenderMatchesTheRelease(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github/workflows/release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*(?:[A-Z_]+=\S+\s+)*go run -ldflags "([^"]*)" \. bootstrap-body\b`)
	m := re.FindAllStringSubmatch(string(raw), -1)
	if len(m) != 1 {
		t.Fatalf("release.yaml: found %d `go run … bootstrap-body` renders, want 1", len(m))
	}
	if m[0][1] != bodyVersionLDFlag+"${VERSION}" {
		t.Errorf("release.yaml renders the body with -ldflags %q, deploylambdas with %q", m[0][1], bodyVersionLDFlag+"${VERSION}")
	}
}

func TestRealBodyRenderRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("builds spawn")
	}
	b, err := goRunBootstrapBody(context.Background(), repoRoot(t), "0.0.0")
	if err != nil || len(b) == 0 {
		t.Fatalf("%v", err)
	}
}

// The real build: the zip holds exactly `bootstrap` at its root, executable,
// and the same build gives the same CodeSha256 (so a re-run sees "already
// current" instead of pushing identical code again).
func TestRealBuildZipLayout(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a Lambda")
	}
	root := repoRoot(t)
	var l checklistLambda
	for _, x := range realInventory(t).Lambdas {
		if x.ID == "lambda/sweep-orchestrator" {
			l = x
		}
	}
	spec, err := parseBuild(l.Build, "0.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var shas []string
	for i := 0; i < 2; i++ {
		b, err := goBuild(context.Background(), root, l.ID, spec, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		if len(zr.File) != 1 || zr.File[0].Name != "bootstrap" || zr.File[0].Mode().Perm()&0o111 == 0 {
			t.Fatalf("zip entries: %+v", zr.File)
		}
		shas = append(shas, codeSha256(b))
	}
	if shas[0] != shas[1] {
		t.Errorf("two builds of one tree gave %v", shas)
	}
}
