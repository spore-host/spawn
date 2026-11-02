package main

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type efaWFStep struct {
	Name            string            `yaml:"name"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	If              string            `yaml:"if"`
	ContinueOnError any               `yaml:"continue-on-error"`
	Env             map[string]string `yaml:"env"`
}

type efaWorkflowFile struct {
	On struct {
		Schedule []struct {
			Cron string `yaml:"cron"`
		} `yaml:"schedule"`
	} `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Permissions map[string]string `yaml:"permissions"`
		Steps       []efaWFStep          `yaml:"steps"`
	} `yaml:"jobs"`
}

func readEFAWorkflow(t *testing.T, path string) (efaWorkflowFile, []byte) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the repo's own workflow
	if err != nil {
		t.Fatal(err)
	}
	var wf efaWorkflowFile
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return wf, b
}

// stepScript is a step's run: with its comment lines dropped, so a comment
// explaining a command cannot satisfy a gate looking for it (rule 3).
func efaStepScript(s efaWFStep) string {
	var out strings.Builder
	for _, l := range strings.Split(s.Run, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out.WriteString(l + "\n")
		}
	}
	return out.String()
}

// The weekly EFA installer pin workflow (#1505) runs the bump on a schedule,
// with GITHUB_TOKEN limited to what opening a PR, or the fallback issue,
// needs, on the one job that needs it, and falls back to that issue only
// through scripts/efapin's pr-refused check.
func TestEFAPinWorkflowRunsTheBumpWithOnlyPRPermissions(t *testing.T) {
	const path = ".github/workflows/efa-pin.yml"
	wf, b := readEFAWorkflow(t, path)
	if len(wf.On.Schedule) == 0 {
		t.Errorf("%s has no schedule: nothing would ever move the pin", path)
	}
	if len(wf.Permissions) != 0 {
		t.Errorf("%s: workflow-level permissions = %v, want {} (grant per job)", path, wf.Permissions)
	}
	invoke := regexp.MustCompile(`(?m)^\s*scripts/efa-pin\.sh bump\s*$`)
	want := map[string]string{"contents": "write", "pull-requests": "write", "issues": "write"}
	fallback := regexp.MustCompile(`(?m)^\s*if ! printf '%s' "\$out" \| go -C scripts run \./efapin pr-refused; then\n\s*echo "::error::[^\n]*\n\s*exit 1\n\s*fi$`)
	issueCreate := regexp.MustCompile(`(?m)^\s*gh issue (create|edit)\b`)
	commits := regexp.MustCompile(`(?m)^\s*git add pkg/userdata/efa\.go CHANGELOG\.md$`)
	runs := 0
	for name, job := range wf.Jobs {
		var script strings.Builder
		for _, s := range job.Steps {
			script.WriteString(efaStepScript(s))
		}
		if issueCreate.MatchString(script.String()) && !fallback.MatchString(script.String()) {
			t.Errorf("job %s opens an issue without first checking `efapin pr-refused` and failing otherwise", name)
		}
		if invoke.MatchString(script.String()) {
			runs++
			if !fallback.MatchString(script.String()) || !issueCreate.MatchString(script.String()) {
				t.Errorf("job %s: no pr-refused-gated issue fallback (GitHub refuses GITHUB_TOKEN PRs here)", name)
			}
			if !commits.MatchString(script.String()) {
				t.Errorf("job %s does not commit pkg/userdata/efa.go and CHANGELOG.md, the two files the bump edits", name)
			}
			if !reflect.DeepEqual(job.Permissions, want) {
				t.Errorf("job %s permissions = %v, want exactly %v", name, job.Permissions, want)
			}
		}
	}
	if runs != 1 {
		t.Errorf("%s: %d jobs run `scripts/efa-pin.sh bump`, want 1", path, runs)
	}
	if strings.Contains(string(b), "secrets.") {
		t.Errorf("%s references a secret; it must use only GITHUB_TOKEN (github.token)", path)
	}
}

// The release refuses to build unless the pinned EFA installer is the one AWS
// signed (#1505): `scripts/efa-pin.sh verify` runs, unconditionally and not
// allowed to fail, in the release job before anything is published (before
// the first AWS credentials step) and before GoReleaser.
func TestReleaseVerifiesThePinnedEFAInstallerFirst(t *testing.T) {
	const path = ".github/workflows/release.yaml"
	wf, _ := readEFAWorkflow(t, path)
	verify := regexp.MustCompile(`(?m)^\s*scripts/efa-pin\.sh verify\s*$`)
	found := 0
	for name, job := range wf.Jobs {
		at, creds, gorel := -1, -1, -1
		for i, s := range job.Steps {
			switch {
			case verify.MatchString(efaStepScript(s)):
				found++
				at = i
				if s.If != "" || s.ContinueOnError != nil {
					t.Errorf("job %s: the EFA verify step has if=%q continue-on-error=%v; it must always run and fail the release", name, s.If, s.ContinueOnError)
				}
			case strings.HasPrefix(s.Uses, "aws-actions/configure-aws-credentials@") && creds < 0:
				creds = i
			case strings.HasPrefix(s.Uses, "goreleaser/goreleaser-action@") && gorel < 0:
				gorel = i
			}
		}
		if at < 0 {
			continue
		}
		if gorel < 0 || at > gorel {
			t.Errorf("job %s: EFA verify is step %d, GoReleaser %d; it must come first", name, at, gorel)
		}
		if creds >= 0 && at > creds {
			t.Errorf("job %s: EFA verify is step %d, after the AWS credentials step %d; it must run before anything is published", name, at, creds)
		}
	}
	if found != 1 {
		t.Errorf("%s: %d steps run `scripts/efa-pin.sh verify`, want 1", path, found)
	}
}
