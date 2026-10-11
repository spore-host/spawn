package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// templateSummaryFromTemplate builds what `aws cloudformation
// get-template-summary` would report, FROM template.yaml.
//
// This was a hardcoded const listing "the 16 parameters the reaper template
// declares". The template declares nineteen. NetResources, Version and
// LogRetentionDays had been added and the fixture never updated, so the
// merge-params machinery — whose entire job is to stop a deploy resetting a
// parameter — was never exercised for three of them, and NetResources is the
// staged DESTRUCTIVE sweep (#685). A fixture that lists the world by hand
// reports a clean bill of health for everything it forgot (CLAUDE.md rule 4).
//
// Deriving it means the fixture cannot drift. DryRun's default being "true" is
// the thing these tests care about — a fallback to the template default disarms
// the reaper — and that now comes from the template rather than from a copy of
// it.
func templateSummaryFromTemplate(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("template.yaml")
	if err != nil {
		t.Fatalf("read template.yaml: %v", err)
	}
	body := string(b)

	start := strings.Index(body, "\nParameters:\n")
	if start < 0 {
		t.Fatal("template.yaml has no Parameters block")
	}
	end := strings.Index(body[start+1:], "\nResources:\n")
	if end < 0 {
		t.Fatal("template.yaml has no Resources block")
	}
	block := body[start : start+1+end]

	type param struct {
		ParameterKey string `json:"ParameterKey"`
		DefaultValue string `json:"DefaultValue"`
	}
	var params []param

	nameRe := regexp.MustCompile(`(?m)^  ([A-Z][A-Za-z0-9]*):$`)
	defRe := regexp.MustCompile(`(?m)^    Default:\s*(.*)$`)

	idx := nameRe.FindAllStringSubmatchIndex(block, -1)
	for i, loc := range idx {
		name := block[loc[2]:loc[3]]
		segEnd := len(block)
		if i+1 < len(idx) {
			segEnd = idx[i+1][0]
		}
		def := ""
		if m := defRe.FindStringSubmatch(block[loc[1]:segEnd]); m != nil {
			def = strings.Trim(strings.TrimSpace(m[1]), `'"`)
		}
		params = append(params, param{ParameterKey: name, DefaultValue: def})
	}

	if len(params) < 15 {
		t.Fatalf("parsed only %d parameters out of template.yaml; the extraction has broken rather "+
			"than the template having shrunk", len(params))
	}
	out, err := json.Marshal(map[string][]param{"Parameters": params})
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	return string(out)
}

// templateParameterCount is how many parameters template.yaml declares, read from
// the template.
//
// The two assertions below used to say "want all 16 declared", which is the same
// hand-maintained count as the fixture and went stale the same way — three
// parameters were added and both numbers stayed at 16, so the tests asserted
// against a world three parameters smaller than the real one and still passed.
func templateParameterCount(t *testing.T) int {
	t.Helper()
	var doc struct {
		Parameters []struct{} `json:"Parameters"`
	}
	if err := json.Unmarshal([]byte(templateSummaryFromTemplate(t)), &doc); err != nil {
		t.Fatalf("parse derived summary: %v", err)
	}
	return len(doc.Parameters)
}

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
	got, stderr := runMerge(t, templateSummaryFromTemplate(t), liveParams, "")

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
	got, stderr := runMerge(t, templateSummaryFromTemplate(t), liveParams, "DryRun=true\nDnsSweep=false\n")

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
	got, _ := runMerge(t, templateSummaryFromTemplate(t), "[]", "")

	if got["DryRun"] != "true" {
		t.Errorf("DryRun = %q, want \"true\" on a fresh stack — a new reaper starts unarmed",
			got["DryRun"])
	}
	if got["MaxAge"] != "168h" {
		t.Errorf("MaxAge = %q, want the template default", got["MaxAge"])
	}
	if want := templateParameterCount(t); len(got) != want {
		t.Errorf("got %d parameters, want all %d the template declares", len(got), want)
	}
}

// TestEmptyValuesSurvive. An empty NotifyUrl is the case that broke sam's inline
// Key=Value form in the first place (see the Makefile's comment), so it has to
// round-trip as "" rather than being dropped.
func TestEmptyValuesSurvive(t *testing.T) {
	got, _ := runMerge(t, templateSummaryFromTemplate(t), liveParams, "NotifyUrl=\n")
	if v, ok := got["NotifyUrl"]; !ok || v != "" {
		t.Errorf("NotifyUrl = %q (present=%v), want an empty string", v, ok)
	}
}

// TestValuesWithSpacesAndCommasSurvive: Schedule is `rate(10 minutes)` and Regions
// is comma-separated, which is why the explicit file is Key=value lines rather
// than JSON assembled in Make.
func TestValuesWithSpacesAndCommasSurvive(t *testing.T) {
	got, _ := runMerge(t, templateSummaryFromTemplate(t), liveParams,
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
		write("s.json", templateSummaryFromTemplate(t)), write("l.json", liveParams),
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

// TestEmptyLiveSetIsReportedLoudly is the follow-up to #650, written after the
// fix for #650 DISARMED the live production reaper.
//
// The Makefile's describe-stacks call had no --region, so it resolved against the
// profile default, the stack came back "not found", the `|| echo '[]'` branch
// wrote an empty live set, and every parameter fell back to its TEMPLATE default
// — DryRun among them, whose default is "true". The reaper went to dry-run: still
// running, still reporting, terminating nothing.
//
// What let it past me was the REPORTING. The merge printed "deploy changes no
// parameters (code only)" because its change notes only fired when a live value
// existed to differ from. With no live values there were no notes, so asserting
// all 16 defaults looked identical to inheriting all 16.
//
// So an empty live set must be the loudest case, not the quietest.
func TestEmptyLiveSetIsReportedLoudly(t *testing.T) {
	merged, stderr := runMerge(t, templateSummaryFromTemplate(t), "[]", "")

	if want := templateParameterCount(t); len(merged) != want {
		t.Fatalf("expected all %d template defaults, got %d", want, len(merged))
	}
	if !strings.Contains(stderr, "WARNING") {
		t.Errorf("an empty live set must WARN — it means every parameter is being asserted "+
			"from a template default, which is how the production reaper got disarmed.\n"+
			"stderr: %s", stderr)
	}
	if !strings.Contains(stderr, "TEMPLATE DEFAULTS") {
		t.Errorf("the warning must say the values come from template defaults, got: %s", stderr)
	}
	// And every parameter must be listed as a change, not silently applied.
	if !strings.Contains(stderr, "deploy will CHANGE") {
		t.Errorf("asserting defaults is a CHANGE and must be listed; got: %s", stderr)
	}
	if !strings.Contains(stderr, "DryRun") {
		t.Errorf("DryRun is the dangerous one and must appear by name; got: %s", stderr)
	}
	// The old message must NOT appear — that is the one that read as safe.
	if strings.Contains(stderr, "changes no parameters") {
		t.Error("an empty live set must not report \"changes no parameters\" — that message is " +
			"what made a full reset look like a no-op")
	}
}

// TestDeployReadsTheStackInTheRightRegion. The region bug is not visible in the
// merge logic at all — it is in the call that FEEDS it, so the gate has to read
// the Makefile.
func TestDeployReadsTheStackInTheRightRegion(t *testing.T) {
	b, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	mk := string(b)

	// Every aws cloudformation call in the deploy path must be region-explicit.
	for _, line := range strings.Split(mk, "\n") {
		if !strings.Contains(line, "aws cloudformation") {
			continue
		}
		if !strings.Contains(line, "--region") {
			t.Errorf("an aws cloudformation call without --region:\n    %s\n\n"+
				"Without it the call resolves against the profile's default region, the stack "+
				"reads as absent, and the deploy falls back to template defaults — which "+
				"disarmed the production reaper (DryRun defaults to true).",
				strings.TrimSpace(line))
		}
	}

	// A failed read must abort, not degrade to an empty live set.
	if !strings.Contains(mk, "refusing to deploy with template defaults") {
		t.Error("a describe-stacks failure other than a genuinely absent stack must ABORT the " +
			"deploy; collapsing every failure into '[]' is what silently reset the stack")
	}
	if !strings.Contains(mk, "does not exist") {
		t.Error("the deploy must distinguish a genuinely absent stack (fine, first deploy) from " +
			"any other read failure (abort)")
	}
}

// TestEveryTemplateParameterIsInThePARAMMAP: a CloudFormation parameter the
// Makefile cannot set is only reachable by hand-editing the stack, which is the
// state the merge-params machinery exists to prevent. Missing NetResources from
// PARAM_MAP would have made #685's staged rollout unsettable by `make deploy`.
func TestEveryTemplateParameterIsInThePARAMMAP(t *testing.T) {
	tmpl, err := os.ReadFile("template.yaml")
	if err != nil {
		t.Fatalf("read template.yaml: %v", err)
	}
	mk, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	// Parameter names are the 2-space-indented keys inside the Parameters block.
	params := regexp.MustCompile(`(?m)^  ([A-Z][A-Za-z0-9]*):$`)
	body := string(tmpl)
	start := strings.Index(body, "\nParameters:\n")
	if start < 0 {
		t.Fatal("template.yaml has no Parameters block")
	}
	end := strings.Index(body[start+1:], "\nResources:\n")
	if end < 0 {
		t.Fatal("template.yaml has no Resources block")
	}
	found := params.FindAllStringSubmatch(body[start:start+1+end], -1)
	if len(found) == 0 {
		t.Fatal("parsed zero parameters — the regex no longer matches the template")
	}

	// Parse PARAM_MAP into real CfnKey:MAKE_VAR pairs.
	//
	// This used to be strings.Contains(mk, name+":"), which checks only the CFN
	// key half. `DryRun:DRYRUN` satisfied it while `make deploy DRY_RUN=true` was
	// silently ignored — the exact failure the PARAM_MAP exists to prevent, left
	// undetected by the gate guarding it. Contains was also unanchored, so any
	// mention of `DryRun:` anywhere in the Makefile counted.
	pairRe := regexp.MustCompile(`(?m)^\t([A-Z][A-Za-z0-9]*):([A-Z][A-Z0-9_]*)\s*\\?$`)
	pairs := map[string]string{}
	for _, m := range pairRe.FindAllStringSubmatch(string(mk), -1) {
		pairs[m[1]] = m[2]
	}
	if len(pairs) == 0 {
		t.Fatal("parsed zero PARAM_MAP entries — the pair regex no longer matches the Makefile, " +
			"which is not the same as every parameter being mapped")
	}

	// The make variable is conventionally the SCREAMING_SNAKE of the CFN key.
	// Environment:ENV is the one deliberate departure.
	exceptions := map[string]string{"Environment": "ENV"}

	for _, m := range found {
		name := m[1]
		mkVar, ok := pairs[name]
		if !ok {
			t.Errorf("template parameter %s is not in the Makefile's PARAM_MAP — "+
				"`make deploy %s=…` would be silently ignored", name, name)
			continue
		}
		want := exceptions[name]
		if want == "" {
			want = screamingSnake(name)
		}
		if mkVar != want {
			t.Errorf("PARAM_MAP maps %s to the make variable %s, but the Makefile's own default and "+
				"every documented invocation use %s. A mismatch here is silent: $(origin %s) is "+
				"undefined, so the parameter is never sent and the deploy inherits whatever is live.",
				name, mkVar, want, mkVar)
		}
	}
}

// screamingSnake converts a CloudFormation parameter name to the Makefile
// variable convention: NetResources -> NET_RESOURCES, DnsZoneId -> DNS_ZONE_ID.
func screamingSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := rune(s[i-1])
			next := rune(0)
			if i+1 < len(s) {
				next = rune(s[i+1])
			}
			prevLower := prev >= 'a' && prev <= 'z'
			nextLower := next >= 'a' && next <= 'z'
			if prevLower || nextLower {
				b.WriteByte('_')
			}
		}
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r - 32)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
