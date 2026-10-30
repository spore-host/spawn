package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Gates on the approval-gated Lambda deploy workflow (#1445). The role it
// assumes trusts only the OIDC subject of a job in the `production`
// environment, so the environment IS the approval: a job that assumed the role
// outside it would fail, but one that named a different, unprotected
// environment, or none, would be a deploy nobody approved if the trust were
// ever widened. The role ARN must also be the one the script creates, or the
// first deploy fails on an AssumeRoleWithWebIdentity nobody has exercised.

const (
	deployWorkflow   = ".github/workflows/deploy-lambdas.yaml"
	deployRoleScript = "scripts/setup-lambda-deploy-ci-role.sh"
	awsCredsAction   = "aws-actions/configure-aws-credentials"
)

type wfStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

type wfJob struct {
	Needs       any               `yaml:"needs"`
	Uses        string            `yaml:"uses"`
	With        map[string]any    `yaml:"with"`
	Environment any               `yaml:"environment"`
	Permissions any               `yaml:"permissions"`
	Concurrency any               `yaml:"concurrency"`
	Steps       []wfStep          `yaml:"steps"`
}

type wfInput struct {
	Type    string `yaml:"type"`
	Default any    `yaml:"default"`
}

type workflow struct {
	On struct {
		WorkflowCall *struct {
			Inputs map[string]wfInput `yaml:"inputs"`
		} `yaml:"workflow_call"`
		WorkflowDispatch *struct {
			Inputs map[string]wfInput `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
	} `yaml:"on"`
	Jobs map[string]wfJob `yaml:"jobs"`
}

func readWorkflow(t *testing.T, path string) workflow {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return wf
}

func (j wfJob) environmentName() string {
	switch e := j.Environment.(type) {
	case string:
		return e
	case map[string]any:
		s, _ := e["name"].(string)
		return s
	}
	return ""
}

// deployRoleARN is the ARN scripts/setup-lambda-deploy-ci-role.sh creates: its
// ROLE_NAME in its default EXPECTED_ACCOUNT.
func deployRoleARN(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(deployRoleScript)
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`(?m)^ROLE_NAME="([^"]+)"$`).FindSubmatch(raw)
	acct := regexp.MustCompile(`(?m)^EXPECTED_ACCOUNT="\$\{SPAWN_LAMBDA_ACCOUNT:-([0-9]{12})\}"$`).FindSubmatch(raw)
	if name == nil || acct == nil {
		t.Fatalf("%s: cannot read ROLE_NAME and EXPECTED_ACCOUNT", deployRoleScript)
	}
	return "arn:aws:iam::" + string(acct[1]) + ":role/" + string(name[1])
}

// Every job, in every workflow, that assumes the deploy role or runs the
// deployer names environment: production and holds id-token: write; the role
// it assumes is the script's. Discovered across all workflows, not read from
// one job by name.
func TestDeployRoleIsAssumedOnlyInTheProductionEnvironment(t *testing.T) {
	want := deployRoleARN(t)
	roleName := want[strings.LastIndex(want, "/")+1:]
	files, err := filepath.Glob(".github/workflows/*.y*ml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	deployJobs := 0
	for _, f := range files {
		for id, j := range readWorkflow(t, f).Jobs {
			assumes, runs := false, false
			for i, s := range j.Steps {
				role := s.With["role-to-assume"]
				if strings.HasPrefix(s.Uses, awsCredsAction+"@") && strings.HasSuffix(role, "/"+roleName) {
					assumes = true
					if role != want {
						t.Errorf("%s job %s assumes %s, but the script creates %s", f, id, role, want)
					}
				}
				if regexp.MustCompile(`(?m)go run \./deploylambdas\b`).MatchString(s.Run) {
					runs = true
					// The credentials must be configured before the deployer runs.
					configured := false
					for _, p := range j.Steps[:i] {
						configured = configured || (strings.HasPrefix(p.Uses, awsCredsAction+"@") && p.With["role-to-assume"] == want)
					}
					if !configured {
						t.Errorf("%s job %s runs the deployer without first assuming %s", f, id, want)
					}
				}
			}
			if !assumes && !runs {
				continue
			}
			deployJobs++
			if env := j.environmentName(); env != "production" {
				t.Errorf("%s job %s assumes the deploy role or runs the deployer in environment %q, not production", f, id, env)
			}
			perms, _ := j.Permissions.(map[string]any)
			if perms["id-token"] != "write" {
				t.Errorf("%s job %s: id-token is %v, the OIDC role needs write", f, id, perms["id-token"])
			}
			conc, _ := j.Concurrency.(map[string]any)
			if conc["group"] == nil || conc["cancel-in-progress"] != false {
				t.Errorf("%s job %s: concurrency %v; two deploys must never overlap and a running one must not be cancelled", f, id, j.Concurrency)
			}
		}
	}
	if deployJobs == 0 {
		t.Fatal("found no job that assumes the deploy role: the discovery is broken")
	}
}

// The deploy workflow's credentials action is pinned exactly as release.yaml's.
func TestDeployWorkflowPinsTheReleaseCredentialsAction(t *testing.T) {
	pins := func(path string) map[string]bool {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, m := range regexp.MustCompile(`uses:\s*(`+regexp.QuoteMeta(awsCredsAction)+`@\S+(?:\s+#\s*\S+)?)`).FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
		return out
	}
	rel, dep := pins(".github/workflows/release.yaml"), pins(deployWorkflow)
	if len(rel) != 1 || len(dep) != 1 {
		t.Fatalf("pins: release %v, deploy %v; want one each", rel, dep)
	}
	for p := range dep {
		if !rel[p] {
			t.Errorf("%s pins %s; release.yaml pins %v", deployWorkflow, p, rel)
		}
	}
}

// A manual run is a dry run unless asked otherwise, and so is a call that
// does not say; release.yaml calls it after the release job, and no release job
// waits on it.
func TestDeployWorkflowDefaultsToDryRunAndBlocksNoReleaseJob(t *testing.T) {
	wf := readWorkflow(t, deployWorkflow)
	if wf.On.WorkflowDispatch == nil || wf.On.WorkflowCall == nil {
		t.Fatal("deploy-lambdas.yaml must have both workflow_dispatch and workflow_call")
	}
	for name, in := range map[string]map[string]wfInput{"workflow_dispatch": wf.On.WorkflowDispatch.Inputs, "workflow_call": wf.On.WorkflowCall.Inputs} {
		if d := in["dry_run"]; d.Type != "boolean" || d.Default != true {
			t.Errorf("%s dry_run = %+v, want a boolean defaulting to true", name, d)
		}
	}

	rel := readWorkflow(t, ".github/workflows/release.yaml")
	callers := 0
	for id, j := range rel.Jobs {
		if j.Uses == "./"+deployWorkflow {
			callers++
			if n, _ := j.Needs.(string); n != "goreleaser" {
				t.Errorf("release.yaml job %s needs %v; it must run after the release job (goreleaser)", id, j.Needs)
			}
			continue
		}
		needs := []string{}
		switch n := j.Needs.(type) {
		case string:
			needs = append(needs, n)
		case []any:
			for _, x := range n {
				s, _ := x.(string)
				needs = append(needs, s)
			}
		}
		for _, n := range needs {
			if j2, ok := rel.Jobs[n]; ok && j2.Uses == "./"+deployWorkflow {
				t.Errorf("release.yaml job %s needs the deploy job %s; a pending approval would block it", id, n)
			}
		}
	}
	if callers != 1 {
		t.Errorf("release.yaml calls %s %d times, want 1", deployWorkflow, callers)
	}
}
