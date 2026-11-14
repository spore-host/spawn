package main

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type ipStep struct {
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

type ipWorkflow struct {
	On          yaml.Node         `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		RunsOn      string            `yaml:"runs-on"`
		Needs       []string          `yaml:"needs"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []ipStep          `yaml:"steps"`
	} `yaml:"jobs"`
}

// The weekly installer pins workflow (#1620): scheduled; the two jobs that
// download and verify hold only contents: read, the Windows one runs on
// Windows (Authenticode is checked by Get-AuthenticodeSignature, as at boot);
// only the job that opens the PR writes, it needs both verify jobs, and it
// falls back to an issue only through installerpin pr-refused.
func TestInstallerPinsWorkflowVerifiesBeforeItWrites(t *testing.T) {
	const path = ".github/workflows/installer-pins.yml"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wf ipWorkflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	triggers := map[string]bool{}
	for i := 0; i+1 < len(wf.On.Content); i += 2 {
		triggers[wf.On.Content[i].Value] = true
	}
	if !triggers["schedule"] || !triggers["workflow_dispatch"] || len(triggers) != 2 {
		t.Errorf("%s triggers %v, want exactly schedule and workflow_dispatch (it downloads from AWS origins, so never on a PR)", path, triggers)
	}
	if len(wf.Permissions) != 0 {
		t.Errorf("%s: workflow-level permissions %v, want {}", path, wf.Permissions)
	}
	script := func(steps []ipStep) string {
		var s strings.Builder
		for _, st := range steps {
			for _, l := range strings.Split(st.Run, "\n") {
				if !strings.HasPrefix(strings.TrimSpace(l), "#") {
					s.WriteString(l + "\n")
				}
			}
		}
		return s.String()
	}
	readOnly := map[string]string{"contents": "read"}
	writes := map[string]string{"contents": "write", "pull-requests": "write", "issues": "write"}
	verifiers := map[string]*regexp.Regexp{
		"windows": regexp.MustCompile(`\./scripts/installer-pins-windows\.ps1\b`),
		"dcv":     regexp.MustCompile(`scripts/installer-pins-dcv\.sh\b`),
	}
	for id, re := range verifiers {
		j, ok := wf.Jobs[id]
		if !ok {
			t.Errorf("%s has no %s job", path, id)
			continue
		}
		if !re.MatchString(script(j.Steps)) {
			t.Errorf("job %s does not run %s", id, re)
		}
		if !reflect.DeepEqual(j.Permissions, readOnly) {
			t.Errorf("job %s permissions %v, want exactly %v: it runs what it downloads' verifiers, not a writer", id, j.Permissions, readOnly)
		}
	}
	if !strings.HasPrefix(wf.Jobs["windows"].RunsOn, "windows-") {
		t.Errorf("the windows job runs on %q: Get-AuthenticodeSignature needs Windows", wf.Jobs["windows"].RunsOn)
	}
	writers := 0
	for id, j := range wf.Jobs {
		s := script(j.Steps)
		if len(j.Permissions) != 0 && !reflect.DeepEqual(j.Permissions, readOnly) {
			writers++
			if !reflect.DeepEqual(j.Permissions, writes) {
				t.Errorf("job %s permissions %v, want exactly %v", id, j.Permissions, writes)
			}
			needs := map[string]bool{}
			for _, n := range j.Needs {
				needs[n] = true
			}
			if !needs["windows"] || !needs["dcv"] {
				t.Errorf("job %s writes but needs %v: it must wait for both verify jobs", id, j.Needs)
			}
			if !regexp.MustCompile(`go -C scripts run \./installerpin set\b`).MatchString(s) {
				t.Errorf("job %s writes without installerpin set", id)
			}
			if !regexp.MustCompile(`(?m)^\s*git add pkg/sporedinstall/installer_pins\.go cmd/app\.go scripts/install-spored\.ps1 CHANGELOG\.md$`).MatchString(s) {
				t.Errorf("job %s does not commit the four files a refresh changes", id)
			}
			if !regexp.MustCompile(`UPDATE_GOLDEN=1 go test \./pkg/sporedinstall -run TestInstallSporedPs1IsGenerated`).MatchString(s) {
				t.Errorf("job %s does not regenerate scripts/install-spored.ps1, which embeds the MSI pin", id)
			}
		}
		issue := regexp.MustCompile(`(?m)^\s*gh issue (create|edit)\b`)
		gate := regexp.MustCompile(`(?m)^\s*if ! printf '%s' "\$out" \| go -C scripts run \./installerpin pr-refused; then\n\s*echo "::error::[^\n]*\n\s*exit 1\n\s*fi$`)
		if issue.MatchString(s) && !gate.MatchString(s) {
			t.Errorf("job %s opens an issue without first checking installerpin pr-refused and failing otherwise", id)
		}
	}
	if writers != 1 {
		t.Errorf("%s: %d jobs write, want 1", path, writers)
	}
	if strings.Contains(string(b), "secrets.") {
		t.Errorf("%s references a secret; it must use only GITHUB_TOKEN", path)
	}
}
