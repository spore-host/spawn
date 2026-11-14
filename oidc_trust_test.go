package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The workflows half of the OIDC trust inventory (#1601). The inventory,
// infra/iam/github-oidc/trusts.json, says what each AWS role's trust must be;
// scripts/oidctrust checks its rules and compares live IAM against it. This
// test checks that the workflows agree with it: a job can only assume a role
// whose trust's `sub` is the one GitHub will issue to that job, and only the
// signers in signing-key.json run `aws kms sign`.

const oidcInventoryDir = "infra/iam/github-oidc"

type oidcRole struct {
	Role      string          `json:"role"`
	RoleARN   string          `json:"role_arn"`
	Variables []string        `json:"variables"`
	UsedBy    []string        `json:"used_by"`
	Trust     json.RawMessage `json:"trust"`
}

type oidcStatement struct {
	Condition map[string]map[string]any `json:"Condition"`
}

// sub is the trust's single sub condition and whether it is a pattern.
func (r oidcRole) sub(t *testing.T) (string, bool) {
	t.Helper()
	var doc struct{ Statement []oidcStatement }
	if err := json.Unmarshal(r.Trust, &doc); err != nil || len(doc.Statement) != 1 {
		t.Fatalf("%s: trust is not one statement: %v", r.Role, err)
	}
	c := doc.Statement[0].Condition
	if s, ok := c["StringEquals"]["token.actions.githubusercontent.com:sub"].(string); ok {
		return s, false
	}
	if s, ok := c["StringLike"]["token.actions.githubusercontent.com:sub"].(string); ok {
		return s, true
	}
	t.Fatalf("%s: trust has no single string sub", r.Role)
	return "", false
}

func readOIDCInventory(t *testing.T) ([]oidcRole, []string, []string) {
	t.Helper()
	var inv struct{ Roles []oidcRole }
	b, err := os.ReadFile(filepath.Join(oidcInventoryDir, "trusts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatal(err)
	}
	var sk struct {
		Signers   []string
		Variables []string
	}
	b, err = os.ReadFile(filepath.Join(oidcInventoryDir, "signing-key.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &sk); err != nil {
		t.Fatal(err)
	}
	if len(inv.Roles) == 0 || len(sk.Signers) == 0 {
		t.Fatal("the inventory lists no roles or no signers")
	}
	return inv.Roles, sk.Signers, sk.Variables
}

type oidcStep struct {
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type oidcJob struct {
	Environment yaml.Node  `yaml:"environment"`
	Steps       []oidcStep `yaml:"steps"`
}

func (j oidcJob) environment() string {
	switch j.Environment.Kind {
	case yaml.ScalarNode:
		return j.Environment.Value
	case yaml.MappingNode:
		for i := 0; i+1 < len(j.Environment.Content); i += 2 {
			if j.Environment.Content[i].Value == "name" {
				return j.Environment.Content[i+1].Value
			}
		}
	}
	return ""
}

type oidcWorkflow struct {
	On   yaml.Node          `yaml:"on"`
	Jobs map[string]oidcJob `yaml:"jobs"`
}

// pushTagsOnly returns the tag filters when the workflow's only trigger is a
// push of tags (no branches, no paths), and ok=false otherwise.
func (w oidcWorkflow) pushTagsOnly() ([]string, bool) {
	if w.On.Kind != yaml.MappingNode || len(w.On.Content) != 2 || w.On.Content[0].Value != "push" {
		return nil, false
	}
	push := w.On.Content[1]
	if push.Kind != yaml.MappingNode || len(push.Content) != 2 || push.Content[0].Value != "tags" {
		return nil, false
	}
	var tags []string
	for _, n := range push.Content[1].Content {
		tags = append(tags, n.Value)
	}
	return tags, len(tags) > 0
}

func (w oidcWorkflow) triggers() []string {
	var out []string
	switch w.On.Kind {
	case yaml.ScalarNode:
		out = append(out, w.On.Value)
	case yaml.SequenceNode:
		for _, n := range w.On.Content {
			out = append(out, n.Value)
		}
	case yaml.MappingNode:
		for i := 0; i < len(w.On.Content); i += 2 {
			out = append(out, w.On.Content[i].Value)
		}
	}
	return out
}

var (
	awsCredsUses = regexp.MustCompile(`^aws-actions/configure-aws-credentials@`)
	exprRef      = regexp.MustCompile(`^\$\{\{\s*((?:vars|secrets)\.[A-Za-z0-9_]+)\s*\}\}$`)
	// The invocation, not the name in a comment or an echo (CLAUDE.md, rule 3).
	kmsSignCall = regexp.MustCompile(`(?m)^\s*aws kms sign\b`)
)

func oidcWorkflowFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(".github/workflows/*.y*ml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	sort.Strings(files)
	return files
}

func readOIDCWorkflow(t *testing.T, path string) oidcWorkflow {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the repo's own workflows
	if err != nil {
		t.Fatal(err)
	}
	var w oidcWorkflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return w
}

func noComments(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestWorkflowsAssumeOnlyInventoriedRolesWithTheirTrustsSub(t *testing.T) {
	roles, signers, keyVars := readOIDCInventory(t)
	signer := map[string]bool{}
	for _, s := range signers {
		signer[s] = true
	}
	byRef := map[string]oidcRole{}
	for _, r := range roles {
		byRef[r.RoleARN] = r
		for _, v := range r.Variables {
			byRef[v] = r
		}
	}
	assumedIn := map[string]map[string]bool{} // role -> workflow paths
	assumptions, signs := 0, 0
	for _, f := range oidcWorkflowFiles(t) {
		w := readOIDCWorkflow(t, f)
		for id, j := range w.Jobs {
			var assumed []oidcRole
			for _, s := range j.Steps {
				if awsCredsUses.MatchString(s.Uses) && s.With["role-to-assume"] != "" {
					assumptions++
					ref := s.With["role-to-assume"]
					if m := exprRef.FindStringSubmatch(ref); m != nil {
						ref = m[1]
					}
					r, ok := byRef[ref]
					if !ok {
						t.Errorf("%s job %s assumes %s, which is not in %s/trusts.json: add its expected trust there (#1601)", f, id, s.With["role-to-assume"], oidcInventoryDir)
						continue
					}
					assumed = append(assumed, r)
					if assumedIn[r.Role] == nil {
						assumedIn[r.Role] = map[string]bool{}
					}
					assumedIn[r.Role][f] = true
					checkSubFitsJob(t, f, id, w, j, r)
				}
				if kmsSignCall.MatchString(noComments(s.Run)) {
					signs++
					ok := false
					for _, r := range assumed {
						ok = ok || signer[r.RoleARN]
					}
					if !ok {
						t.Errorf("%s job %s runs aws kms sign without first assuming a signer in signing-key.json (%v)", f, id, signers)
					}
					usesKeyVar := false
					for _, v := range s.Env {
						if m := exprRef.FindStringSubmatch(v); m != nil {
							for _, kv := range keyVars {
								usesKeyVar = usesKeyVar || m[1] == kv
							}
						}
					}
					if !usesKeyVar {
						t.Errorf("%s job %s runs aws kms sign, but its step env names none of signing-key.json's variables %v", f, id, keyVars)
					}
				}
			}
		}
	}
	if assumptions == 0 || signs == 0 {
		t.Fatalf("found %d role assumptions and %d kms sign calls: the discovery is broken (release.yaml has both)", assumptions, signs)
	}
	// And the other way: every spawn workflow the inventory names does assume
	// the role, so a role a workflow stopped using does not stay listed.
	for _, r := range roles {
		for _, u := range r.UsedBy {
			path, ok := strings.CutPrefix(u, "spore-host/spawn:")
			if !ok {
				t.Errorf("%s: used_by %q is not a spore-host/spawn workflow", r.Role, u)
				continue
			}
			if !assumedIn[r.Role][path] {
				t.Errorf("%s: trusts.json says %s assumes it, but no job there does", r.Role, path)
			}
		}
	}
}

// checkSubFitsJob: the sub GitHub issues the job must be the trust's. An
// environment sub needs the job in that environment; a tag-pattern sub needs a
// workflow run only by pushing tags the pattern covers, and no environment
// (which would change the sub).
func checkSubFitsJob(t *testing.T, f, id string, w oidcWorkflow, j oidcJob, r oidcRole) {
	t.Helper()
	sub, pattern := r.sub(t)
	_, rest, _ := strings.Cut(strings.TrimPrefix(sub, "repo:"), ":")
	for _, trig := range w.triggers() {
		if strings.HasPrefix(trig, "pull_request") {
			t.Errorf("%s runs on %s and job %s assumes %s: no OIDC role may be reachable from a pull request", f, trig, id, r.Role)
		}
	}
	switch {
	case strings.HasPrefix(rest, "environment:"):
		if env := strings.TrimPrefix(rest, "environment:"); j.environment() != env {
			t.Errorf("%s job %s assumes %s, whose trust's sub is environment %q, but runs in environment %q", f, id, r.Role, env, j.environment())
		}
	case strings.HasPrefix(rest, "ref:refs/tags/"):
		if j.environment() != "" {
			t.Errorf("%s job %s assumes %s (sub %s) but sets environment %q, which changes the sub GitHub issues", f, id, r.Role, sub, j.environment())
		}
		tags, ok := w.pushTagsOnly()
		if !ok {
			t.Errorf("%s job %s assumes %s (sub %s), so the workflow must run only on a push of tags; it runs on %v", f, id, r.Role, sub, w.triggers())
		}
		want := strings.TrimPrefix(rest, "ref:refs/tags/")
		for _, tag := range tags {
			if tag != want || !pattern {
				t.Errorf("%s triggers on tag %q, the trust of %s covers %q", f, tag, r.Role, want)
			}
		}
	default:
		t.Errorf("%s: sub %q is neither an environment nor a tag ref; extend this test before using such a role", r.Role, sub)
	}
}
