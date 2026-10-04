package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The 16 parameters the reaper template declares, as get-template-summary reports
// them. DryRun's default is deliberately "true" — that is what made the old
// Makefile dangerous, since a fallback to the template default disarms the reaper.
const templateSummary = `{"Parameters":[
  {"ParameterKey":"Environment","DefaultValue":"production"},
  {"ParameterKey":"DryRun","DefaultValue":"true"},
  {"ParameterKey":"NotifyUrl","DefaultValue":""},
  {"ParameterKey":"ScanSelf","DefaultValue":"false"},
  {"ParameterKey":"RoleArns","DefaultValue":"arn:aws:iam::435415984226:role/spawn-ttl-reaper-ec2"},
  {"ParameterKey":"DnsZoneId","DefaultValue":""},
  {"ParameterKey":"DnsDomain","DefaultValue":""},
  {"ParameterKey":"DnsSweep","DefaultValue":"false"},
  {"ParameterKey":"DnsExpire","DefaultValue":"false"},
  {"ParameterKey":"AccountsTable","DefaultValue":"spore-portal-accounts"},
  {"ParameterKey":"AccountsTableKeyArn","DefaultValue":""},
  {"ParameterKey":"AlarmsEnabled","DefaultValue":"true"},
  {"ParameterKey":"AlarmTopicArn","DefaultValue":""},
  {"ParameterKey":"MaxAge","DefaultValue":"168h"},
  {"ParameterKey":"Regions","DefaultValue":"us-east-1"},
  {"ParameterKey":"Schedule","DefaultValue":"rate(10 minutes)"}
]}`

// The live production stack as it actually was on 2026-10-04: ARMED (DryRun
// false), DNS sweep on, alarms wired to a real topic, 11 regions.
const liveParams = `[
  {"ParameterKey":"Environment","ParameterValue":"production"},
  {"ParameterKey":"DryRun","ParameterValue":"false"},
  {"ParameterKey":"NotifyUrl","ParameterValue":""},
  {"ParameterKey":"ScanSelf","ParameterValue":"false"},
  {"ParameterKey":"RoleArns","ParameterValue":"arn:aws:iam::435415984226:role/spawn-ttl-reaper-ec2"},
  {"ParameterKey":"DnsZoneId","ParameterValue":"Z0341053304H0DQXF6U4X"},
  {"ParameterKey":"DnsDomain","ParameterValue":"spore.host"},
  {"ParameterKey":"DnsSweep","ParameterValue":"true"},
  {"ParameterKey":"DnsExpire","ParameterValue":"false"},
  {"ParameterKey":"AccountsTable","ParameterValue":"spore-portal-accounts"},
  {"ParameterKey":"AccountsTableKeyArn","ParameterValue":"arn:aws:kms:us-east-1:966362334030:key/48eefc31"},
  {"ParameterKey":"AlarmsEnabled","ParameterValue":"true"},
  {"ParameterKey":"AlarmTopicArn","ParameterValue":"arn:aws:sns:us-east-1:966362334030:spawn-sweep-alerts"},
  {"ParameterKey":"MaxAge","ParameterValue":"168h"},
  {"ParameterKey":"Regions","ParameterValue":"us-east-1,us-west-2,eu-west-1"},
  {"ParameterKey":"Schedule","ParameterValue":"rate(10 minutes)"}
]`

// runMerge drives the real script with the given inputs and returns the merged
// params file plus whatever it reported on stderr.
func runMerge(t *testing.T, summary, live, explicit string) (map[string]string, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	cmd := exec.Command("python3", "scripts/merge_deploy_params.py",
		write("summary.json", summary), write("live.json", live),
		write("explicit.txt", explicit))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("merge failed: %v\nstderr: %s", err, stderr.String())
	}

	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("unparsable output line %q", line)
		}
		got[k] = strings.Trim(v, `"`)
	}
	return got, stderr.String()
}

// TestDeployWithNoOverridesChangesNothing is spawn#650, and the assertion that
// matters most in this file.
//
// `make deploy` used to assert all 13 of its own `?=` defaults on every run, so
// the documented command would have set DryRun true on a stack running
// DryRun=false — DISARMING the live production reaper. It keeps running and
// reporting; it just stops terminating anything, which is invisible until
// something outlives its TTL. It would also have turned DnsSweep off, blanked the
// DNS zone/domain, and detached AlarmTopicArn so every failure alarm in the stack
// fired into nothing, including the not-invoked alarm that exists to catch a
// stopped reaper.
func TestDeployWithNoOverridesChangesNothing(t *testing.T) {
	got, stderr := runMerge(t, templateSummary, liveParams, "")

	var live []struct {
		ParameterKey   string
		ParameterValue string
	}
	if err := json.Unmarshal([]byte(liveParams), &live); err != nil {
		t.Fatal(err)
	}
	for _, p := range live {
		if got[p.ParameterKey] != p.ParameterValue {
			t.Errorf("%s = %q, want the deployed value %q — an unspecified parameter must "+
				"keep what is live", p.ParameterKey, got[p.ParameterKey], p.ParameterValue)
		}
	}

	// The specific one that would have broken production.
	if got["DryRun"] != "false" {
		t.Errorf("DryRun = %q, want \"false\": a plain `make deploy` must NOT disarm an "+
			"armed reaper", got["DryRun"])
	}
	if !strings.Contains(stderr, "changes no parameters") {
		t.Errorf("a no-override deploy should say it changes nothing, got: %s", stderr)
	}
}

func TestExplicitOverrideIsAppliedAndReported(t *testing.T) {
	got, stderr := runMerge(t, templateSummary, liveParams, "DryRun=true\nDnsSweep=false\n")

	if got["DryRun"] != "true" || got["DnsSweep"] != "false" {
		t.Errorf("explicit values must win, got DryRun=%q DnsSweep=%q", got["DryRun"], got["DnsSweep"])
	}
	// Everything else still inherited.
	if got["DnsDomain"] != "spore.host" {
		t.Errorf("DnsDomain = %q, want it inherited", got["DnsDomain"])
	}
	// And the change is announced, because disarming on purpose should still be
	// visible in the deploy log.
	if !strings.Contains(stderr, "DryRun") || !strings.Contains(stderr, "->") {
		t.Errorf("an explicit change must be reported, got: %s", stderr)
	}
}

// TestFreshStackUsesTemplateDefaults: with no stack to read, the template's own
// defaults apply — which is why DryRun defaults to "true" there. A new deployment
// starts unarmed.
func TestFreshStackUsesTemplateDefaults(t *testing.T) {
	got, _ := runMerge(t, templateSummary, "[]", "")

	if got["DryRun"] != "true" {
		t.Errorf("DryRun = %q, want \"true\" on a fresh stack — a new reaper starts unarmed",
			got["DryRun"])
	}
	if got["MaxAge"] != "168h" {
		t.Errorf("MaxAge = %q, want the template default", got["MaxAge"])
	}
	if len(got) != 16 {
		t.Errorf("got %d parameters, want all 16 declared", len(got))
	}
}

// TestEmptyValuesSurvive. An empty NotifyUrl is the case that broke sam's inline
// Key=Value form in the first place (see the Makefile's comment), so it has to
// round-trip as "" rather than being dropped.
func TestEmptyValuesSurvive(t *testing.T) {
	got, _ := runMerge(t, templateSummary, liveParams, "NotifyUrl=\n")
	if v, ok := got["NotifyUrl"]; !ok || v != "" {
		t.Errorf("NotifyUrl = %q (present=%v), want an empty string", v, ok)
	}
}

// TestValuesWithSpacesAndCommasSurvive: Schedule is `rate(10 minutes)` and Regions
// is comma-separated, which is why the explicit file is Key=value lines rather
// than JSON assembled in Make.
func TestValuesWithSpacesAndCommasSurvive(t *testing.T) {
	got, _ := runMerge(t, templateSummary, liveParams,
		"Schedule=rate(5 minutes)\nRegions=us-east-1,eu-central-1,ap-northeast-1\n")

	if got["Schedule"] != "rate(5 minutes)" {
		t.Errorf("Schedule = %q, want the space preserved", got["Schedule"])
	}
	if got["Regions"] != "us-east-1,eu-central-1,ap-northeast-1" {
		t.Errorf("Regions = %q, want the commas preserved", got["Regions"])
	}
}

// TestUndeclaredExplicitParameterIsRejected: silently dropping a typo'd override
// would mean a deploy that looks like it did what you asked. `make deploy
// DRY_RUNN=false` must not quietly leave the reaper armed-or-not at random.
func TestUndeclaredExplicitParameterIsRejected(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	write := func(n, b string) string {
		p := filepath.Join(dir, n)
		_ = os.WriteFile(p, []byte(b), 0o600)
		return p
	}
	cmd := exec.Command("python3", "scripts/merge_deploy_params.py",
		write("s.json", templateSummary), write("l.json", liveParams),
		write("e.txt", "NotAParameter=x\n"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an undeclared parameter must fail the deploy, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "not a parameter") {
		t.Errorf("the error should name the problem, got: %s", out)
	}
}

// TestMakefilePassesOnlyExplicitParameters is the other half of the fix: the
// script's correctness is moot if the Makefile hands it every default anyway.
// $(origin) is what distinguishes a caller's value from a `?=` default.
func TestMakefilePassesOnlyExplicitParameters(t *testing.T) {
	b, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	mk := string(b)

	if !strings.Contains(mk, "$(origin ") {
		t.Error("the Makefile must use $(origin ...) to tell a caller-supplied value from a " +
			"?= default; without it every default is asserted on every deploy and a plain " +
			"`make deploy` disarms an armed reaper (spawn#650)")
	}
	if !strings.Contains(mk, "command line environment") {
		t.Error("expected $(origin) to be filtered on `command line environment` — those are " +
			"the two origins that mean the caller actually set it")
	}
	// The old generator wrote every value unconditionally via one big printf.
	if strings.Contains(mk, `printf 'Environment: "%s"`) {
		t.Error("the Makefile still contains the unconditional params-file printf, which " +
			"asserts all defaults regardless of what the caller set")
	}
	// It must read the live stack before writing.
	if !strings.Contains(mk, "describe-stacks") {
		t.Error("the deploy target must read the live stack's parameters before building the " +
			"override file, or an unspecified value cannot be inherited")
	}
}
