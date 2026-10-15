package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The secret scan covers the full history of the commit under test, and nothing
// else (#1024).
//
// Both halves are the point. `fetch-depth: 0` is there so a secret that was
// committed and later removed is still caught; a shallow checkout would scan one
// commit. But that checkout also fetches every other branch, and gitleaks with no
// --log-opts runs `git log --all` — so one open PR's finding failed every other
// PR's Secret Scan until that branch was force-pushed away. The scan must be
// scoped to HEAD's ancestry, which on pull_request is the merge commit (main plus
// every PR commit) and on push/schedule is main.
//
// The workflow is parsed as YAML and the gitleaks invocation is read from the
// step's `run:` with comment lines dropped, so the comment explaining the scope
// cannot satisfy this gate (rule 3).
var (
	gitleaksHistoryScan = regexp.MustCompile(`(?m)^\s*gitleaks\s+(detect|git)\b(.*)$`)
	gitleaksLogOptsHEAD = regexp.MustCompile(`\s--log-opts[= ]("HEAD"|'HEAD'|HEAD)(\s|$)`)
	gitleaksAnyGitScan  = regexp.MustCompile(`(?m)^\s*gitleaks\s+(detect|git|dir|protect)\b.*$`)
)

type secretScanWorkflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			With map[string]string `yaml:"with"`
			Run  string            `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestSecretScanIsScopedToHEADWithFullHistory(t *testing.T) {
	const f = ".github/workflows/security.yml"
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	var wf secretScanWorkflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("%s: %v", f, err)
	}

	scans := 0
	for jobName, job := range wf.Jobs {
		var invocations []string
		for _, st := range job.Steps {
			var lines []string
			for _, l := range strings.Split(st.Run, "\n") {
				if !strings.HasPrefix(strings.TrimSpace(l), "#") {
					lines = append(lines, l)
				}
			}
			run := strings.ReplaceAll(strings.Join(lines, "\n"), "\\\n", " ")
			for _, m := range gitleaksAnyGitScan.FindAllString(run, -1) {
				invocations = append(invocations, strings.TrimSpace(m))
			}
		}
		if len(invocations) == 0 {
			continue
		}

		fullHistory := false
		for _, st := range job.Steps {
			if strings.HasPrefix(st.Uses, "actions/checkout@") && st.With["fetch-depth"] == "0" {
				fullHistory = true
			}
		}
		if !fullHistory {
			t.Errorf("%s: job %q runs gitleaks without an actions/checkout using fetch-depth: 0 — "+
				"a shallow clone scans only the tip, so a secret committed and then removed is missed", f, jobName)
		}

		for _, inv := range invocations {
			scans++
			m := gitleaksHistoryScan.FindStringSubmatch(inv)
			if m == nil || strings.Contains(inv, "--no-git") {
				t.Errorf("%s: job %q runs %q, which does not scan git history; use "+
					"`gitleaks git --log-opts=\"HEAD\"`", f, jobName, inv)
				continue
			}
			if !gitleaksLogOptsHEAD.MatchString(" " + m[2]) {
				t.Errorf("%s: job %q runs %q without --log-opts=\"HEAD\". With no --log-opts gitleaks "+
					"scans `git log --all` — every fetched branch — so another branch's finding fails "+
					"this one's scan (#1024)", f, jobName, inv)
			}
			for _, wide := range []string{"--all", "--branches", "--remotes", "--glob"} {
				if strings.Contains(inv, wide) {
					t.Errorf("%s: job %q runs %q, which widens the scan beyond HEAD with %s (#1024)",
						f, jobName, inv, wide)
				}
			}
		}
	}
	if scans == 0 {
		t.Fatalf("%s: found no gitleaks invocation — the secret scan was removed or this gate's "+
			"discovery has broken; either way it would pass vacuously", f)
	}
}
