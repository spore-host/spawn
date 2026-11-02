package main

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The weekly AWS CLI pin workflow (#1495) runs the bump script on a schedule,
// with GITHUB_TOKEN limited to what opening a PR, or the fallback issue, needs,
// on the one job that needs it; and it falls back to that issue only through
// scripts/awsclipin's pr-refused check, so a push or gh failure still fails. Read as YAML, with comment lines of each `run:` dropped, so the
// workflow's header explaining the script cannot satisfy this (rule 3).
func TestAWSCLIPinWorkflowRunsTheBumpWithOnlyPRPermissions(t *testing.T) {
	const path = ".github/workflows/awscli-pin.yml"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On struct {
			Schedule []struct {
				Cron string `yaml:"cron"`
			} `yaml:"schedule"`
		} `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(wf.On.Schedule) == 0 {
		t.Errorf("%s has no schedule: nothing would ever move the pin", path)
	}
	if len(wf.Permissions) != 0 {
		t.Errorf("%s: workflow-level permissions = %v, want {} (grant per job)", path, wf.Permissions)
	}
	invoke := regexp.MustCompile(`(?m)^\s*scripts/bump-awscli-pin\.sh(\s|$)`)
	want := map[string]string{"contents": "write", "pull-requests": "write", "issues": "write"}
	fallback := regexp.MustCompile(`(?m)^\s*if ! printf '%s' "\$out" \| go -C scripts run \./awsclipin pr-refused; then\n\s*echo "::error::[^\n]*\n\s*exit 1\n\s*fi$`)
	issueCreate := regexp.MustCompile(`(?m)^\s*gh issue (create|edit)\b`)
	runs := 0
	for name, job := range wf.Jobs {
		var script strings.Builder
		for _, s := range job.Steps {
			for _, l := range strings.Split(s.Run, "\n") {
				if !strings.HasPrefix(strings.TrimSpace(l), "#") {
					script.WriteString(l + "\n")
				}
			}
		}
		if issueCreate.MatchString(script.String()) && !fallback.MatchString(script.String()) {
			t.Errorf("job %s opens an issue without first checking `awsclipin pr-refused` and failing otherwise", name)
		}
		if invoke.MatchString(script.String()) {
			runs++
			if !fallback.MatchString(script.String()) || !issueCreate.MatchString(script.String()) {
				t.Errorf("job %s: no pr-refused-gated issue fallback (GitHub refuses GITHUB_TOKEN PRs here)", name)
			}
			if !reflect.DeepEqual(job.Permissions, want) {
				t.Errorf("job %s permissions = %v, want exactly %v", name, job.Permissions, want)
			}
		}
	}
	if runs != 1 {
		t.Errorf("%s: %d jobs run scripts/bump-awscli-pin.sh, want 1", path, runs)
	}
	if strings.Contains(string(b), "secrets.") {
		t.Errorf("%s references a secret; it must use only GITHUB_TOKEN (github.token)", path)
	}
}
