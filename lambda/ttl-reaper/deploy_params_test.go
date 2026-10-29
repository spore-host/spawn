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

// explicitFromMakefile runs the Makefile's own emission and returns what it would
// send. Driving `make print-explicit-params` rather than reconstructing the logic
// is the point: #869 existed because the gate in this file asserted on PARAM_MAP
// membership, which is a statement about what is *mappable*, not about what is
// actually emitted. Version was in PARAM_MAP throughout and never reached the
// stack.
func explicitFromMakefile(t *testing.T, args ...string) (map[string]string, string) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available")
	}
	cmd := exec.Command("make", append([]string{"print-explicit-params"}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("make print-explicit-params %v: %v\nstderr: %s", args, err, stderr.String())
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unparsable emission line %q", line)
		}
		got[k] = v
	}
	return got, string(out)
}

// The computed version must actually reach CloudFormation (#869).
//
// `VERSION?=$(shell git describe ...)` looked like automatic stamping, but a `?=`
// assignment has origin `file`, and EMIT_EXPLICIT only emits `command line` or
// `environment` origins — so the value was computed and silently dropped, and the
// stack fell back to the template default. The production stack carried NO Version
// parameter from 2026-06-08 to 2026-10-12 while this Makefile appeared to set it.
//
// Driven end to end: the Makefile's real emission, through the real merge script,
// against a live parameter set that has no Version — exactly the production shape.
func TestVersionReachesCloudFormation(t *testing.T) {
	explicit, raw := explicitFromMakefile(t)

	if _, ok := explicit["Version"]; !ok {
		t.Fatalf("`make deploy` emits no Version override, so CloudFormation falls back to the "+
			"template default and the deployed bytes cannot be traced to a commit (#768).\n"+
			"emitted:\n%s", raw)
	}

	merged, _ := runMerge(t, templateSummaryFromTemplate(t), liveParams, raw)

	switch merged["Version"] {
	case "":
		t.Error("Version merged to the empty string — an empty stamp is worse than the " +
			"template default it replaced")
	case "unknown":
		t.Error("Version merged to \"unknown\", the template default: the computed value is " +
			"still not reaching the stack")
	}
	if !strings.ContainsAny(merged["Version"], "0123456789") {
		t.Errorf("Version = %q, which carries no digits — `git describe` output should", merged["Version"])
	}

	// LogRetentionDays is the other always-asserted one: an unset retention means
	// logs are kept forever (#770).
	if merged["LogRetentionDays"] == "" {
		t.Error("LogRetentionDays did not reach the merged parameters")
	}

	// The assertion that must survive this change. Asserting Version on every
	// deploy must not have turned the other parameters into always-asserted ones.
	if merged["DryRun"] != "false" {
		t.Errorf("DryRun = %q, want the live %q — making Version always-assert must NOT have "+
			"made DryRun always-assert, which would DISARM the production reaper (spawn#650)",
			merged["DryRun"], "false")
	}
}

// A plain `make deploy` must emit no mode-setting parameter at all (#650, #869).
//
// The complement of the test above, and the one that catches the tempting fix for
// #869 — "just emit every default" — which would push DRY_RUN's `?=true` onto a
// stack running `false`. This asserts on the Makefile's real emission, which had
// no coverage before: TestDeployWithNoOverridesChangesNothing drives the script
// with a hand-written explicit string, so it could not have caught a Makefile that
// emitted too much.
func TestPlainDeployEmitsNoModeSettingParameter(t *testing.T) {
	explicit, raw := explicitFromMakefile(t)

	// These are the parameters whose DEPLOYED value must win: every PARAM_MAP
	// entry outside ALWAYS_ASSERT, discovered from the Makefile so a parameter
	// added later (ElasticIps, #1232) is covered without editing this list. The
	// named ones are a floor, so a discovery that silently shrank still fails.
	// DryRun first because it is the one that silently stops the reaper
	// terminating anything.
	keys := []string{
		"DryRun", "NetResources", "SpotRequests", "ElasticIps", "EmptyBuckets", "InstanceEvents", "InstanceRoles", "PoolQueues", "ScheduleParams", "LaunchConfigs", "TtlGrace", "ScanSelf", "DnsSweep", "DnsExpire", "DnsLeaseExpire",
		"NotifyUrl", "AlarmsEnabled", "AlarmTopicArn", "DnsZoneId", "DnsDomain",
		"MaxAge", "Regions", "Schedule", "RoleArns", "AccountsTable", "Environment",
	}
	mk, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	always := map[string]bool{}
	if m := regexp.MustCompile(`(?m)^ALWAYS_ASSERT\s*=(.*)$`).FindSubmatch(mk); m != nil {
		for _, pair := range strings.Fields(string(m[1])) {
			k, _, _ := strings.Cut(pair, ":")
			always[k] = true
		}
	}
	discovered := regexp.MustCompile(`(?m)^\t([A-Z][A-Za-z0-9]*):[A-Z][A-Z0-9_]*\s*\\?$`).FindAllSubmatch(mk, -1)
	if len(discovered) < len(keys) {
		t.Fatalf("discovered only %d PARAM_MAP entries — the extraction has broken", len(discovered))
	}
	for _, m := range discovered {
		if k := string(m[1]); !always[k] {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if v, ok := explicit[k]; ok {
			t.Errorf("a plain `make deploy` emits %s=%q. It must not: an unspecified parameter "+
				"has to inherit the deployed value, and for DryRun asserting the Makefile's "+
				"default of `true` over a live `false` disarms the production reaper (spawn#650).\n"+
				"emitted:\n%s", k, v, raw)
		}
	}
	if len(explicit) == 0 {
		t.Fatal("the Makefile emitted nothing at all — the always-asserted parameters are " +
			"missing, so this test would pass vacuously")
	}
}

// ALWAYS_ASSERT membership is pinned, so widening it is a reviewed decision.
//
// A parameter in that list cannot be left alone by a deploy. That is right for
// Version (a stale stamp is a lie) and wrong for every mode-setting parameter.
// Pinning the exact set here means adding one requires editing this test, which is
// the review step the #650 regression went around.
func TestAlwaysAssertContainsOnlySafeParameters(t *testing.T) {
	b, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	var line string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "ALWAYS_ASSERT") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("no ALWAYS_ASSERT in the Makefile — either it was renamed (update this test) " +
			"or the always-assert mechanism was removed and Version is being dropped again")
	}
	_, rhs, _ := strings.Cut(line, "=")

	want := map[string]bool{"Version": true, "LogRetentionDays": true}
	got := map[string]bool{}
	for _, pair := range strings.Fields(rhs) {
		key, _, ok := strings.Cut(pair, ":")
		if !ok {
			t.Errorf("ALWAYS_ASSERT entry %q is not CfnKey:MAKE_VAR", pair)
			continue
		}
		got[key] = true
		if !want[key] {
			t.Errorf("ALWAYS_ASSERT contains %s. A parameter here is sent on EVERY deploy and "+
				"cannot inherit its deployed value — which disarms the reaper if it is DryRun, "+
				"silences alarms if it is AlarmTopicArn, and blanks DNS if it is DnsZoneId. If "+
				"this is genuinely intended, widen the allowlist in this test deliberately.", key)
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("ALWAYS_ASSERT no longer contains %s, so its computed value is silently "+
				"dropped again (#869)", k)
		}
	}
}

// The version fallback must actually fall back.
//
// The old form was `git describe ... | sed 's/^v//' || echo unknown`. A pipeline's
// exit status is the LAST command's, and sed succeeds on empty input, so the `||`
// never fired: outside a git repository VERSION evaluated to the EMPTY STRING, not
// "unknown". That was invisible while the value was being dropped; now that it is
// asserted on every deploy it would stamp an empty version.
func TestVersionFallsBackOutsideAGitRepo(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available")
	}
	dir := t.TempDir() // not a git repository
	b, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	probe := "include Makefile\nv:\n\t@printf '%s' '$(VERSION)'\n"
	if err := os.WriteFile(filepath.Join(dir, "probe.mk"), []byte(probe), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "-f", "probe.mk", "v")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe make: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "unknown" {
		t.Errorf("VERSION outside a git repo = %q, want \"unknown\". An empty stamp is worse "+
			"than the template default, and it is now asserted on every deploy.", got)
	}
}

// TestEveryTemplateParameterIsReferenced: a parameter declared but never
// referenced does nothing, whatever value a deploy passes. A new mode parameter
// (SpotRequests, #950) whose REAPER_* environment line was forgotten would pass
// every test above: it is in the PARAM_MAP, it deploys, it is echoed, and the
// Lambda never sees it. Matched on the reference forms CloudFormation evaluates
// (!Ref X, ${X}, Ref: X), outside the Parameters block, so the parameter's own
// declaration and Description cannot satisfy it (rule 3).
func TestEveryTemplateParameterIsReferenced(t *testing.T) {
	b, err := os.ReadFile("template.yaml")
	if err != nil {
		t.Fatalf("read template.yaml: %v", err)
	}
	body := string(b)
	start := strings.Index(body, "\nParameters:\n")
	if start < 0 {
		t.Fatal("template.yaml has no Parameters block")
	}
	// The block ends at the next top-level key (Conditions, Resources, ...).
	next := regexp.MustCompile(`(?m)^[A-Z][A-Za-z]*:`).FindStringIndex(body[start+len("\nParameters:\n"):])
	if next == nil {
		t.Fatal("nothing follows the Parameters block")
	}
	end := start + len("\nParameters:\n") + next[0]
	paramsBlock := body[start:end]
	rest := body[:start] + body[end:]

	names := regexp.MustCompile(`(?m)^  ([A-Z][A-Za-z0-9]*):$`).FindAllStringSubmatch(paramsBlock, -1)
	if len(names) < 15 {
		t.Fatalf("parsed only %d parameters — the extraction has broken", len(names))
	}
	for _, m := range names {
		n := regexp.QuoteMeta(m[1])
		ref := regexp.MustCompile(`!Ref\s+'?` + n + `'?\b|\$\{` + n + `\}|\bRef:\s+` + n + `\b`)
		if !ref.MatchString(rest) {
			t.Errorf("template parameter %s is declared but never referenced, so no value of it "+
				"reaches the function (a REAPER_* environment line missing?)", m[1])
		}
	}
}

// TestTemplateEnvironmentIsReadByTheReaper: every REAPER_* variable the SAM
// template sets on the function must be one the reaper reads. A typo on either
// side (REAPER_ELASTIC_IP for REAPER_ELASTIC_IPS, #1232) passes every gate
// above: the parameter is declared, mapped, referenced and deployed, and the
// reaper silently runs on its fallback. The reads are discovered from the
// reaper's source, matched on the call (os.Getenv("...")), not the bare name.
func TestTemplateEnvironmentIsReadByTheReaper(t *testing.T) {
	read := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	getenv := regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("(REAPER_[A-Z0-9_]+)"\)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range getenv.FindAllStringSubmatch(string(b), -1) {
			read[m[1]] = true
		}
	}
	if len(read) < 5 {
		t.Fatalf("found only %d REAPER_* reads — the discovery is broken", len(read))
	}

	tmpl, err := os.ReadFile("template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	set := regexp.MustCompile(`(?m)^\s+(REAPER_[A-Z0-9_]+):\s+!`).FindAllStringSubmatch(string(tmpl), -1)
	if len(set) < 5 {
		t.Fatalf("found only %d REAPER_* variables in template.yaml — the extraction is broken", len(set))
	}
	for _, m := range set {
		if !read[m[1]] {
			t.Errorf("template.yaml sets %s on the function but the reaper never reads it", m[1])
		}
	}
}

// TestElasticIpsSurvivesAMakeDeploy is #1232's preservation property, end to
// end through the Makefile's real emission and the real merge script: a stack
// set to ElasticIps=report keeps it across a `make deploy` that does not
// mention it (the #650 class), a stack that predates the parameter gets the
// template default (the empty string: what it did before the parameter existed),
// and `make deploy ELASTIC_IPS=off` sets it.
func TestElasticIpsSurvivesAMakeDeploy(t *testing.T) {
	summary := templateSummaryFromTemplate(t)
	armed := strings.Replace(liveParams, "\n]",
		",\n  {\"ParameterKey\":\"ElasticIps\",\"ParameterValue\":\"report\"}\n]", 1)
	if armed == liveParams {
		t.Fatal("could not add ElasticIps to the live fixture")
	}
	toExplicit := func(m map[string]string) string {
		var b strings.Builder
		for k, v := range m {
			b.WriteString(k + "=" + v + "\n")
		}
		return b.String()
	}

	plain, _ := explicitFromMakefile(t)
	got, _ := runMerge(t, summary, armed, toExplicit(plain))
	if got["ElasticIps"] != "report" {
		t.Errorf("plain make deploy over a stack with ElasticIps=report sent %q: a deploy "+
			"that did not mention it changed it", got["ElasticIps"])
	}

	got, _ = runMerge(t, summary, liveParams, toExplicit(plain))
	if got["ElasticIps"] != "" {
		t.Errorf("a stack predating ElasticIps got %q, want the template default '' (reap, or "+
			"off under NetResources=off: what it did before the parameter existed)", got["ElasticIps"])
	}

	set, _ := explicitFromMakefile(t, "ELASTIC_IPS=off")
	got, _ = runMerge(t, summary, armed, toExplicit(set))
	if got["ElasticIps"] != "off" {
		t.Errorf("make deploy ELASTIC_IPS=off sent %q", got["ElasticIps"])
	}
}

// TestTtlGraceSurvivesAMakeDeploy is #1186's preservation property, end to end
// through the Makefile's real emission and the real merge script, the same
// shape as TestElasticIpsSurvivesAMakeDeploy: a stack set to TtlGrace=0 keeps
// it across a `make deploy` that does not mention it, a stack that predates the
// parameter gets the template default (”, the reaper's 10m), and `make deploy
// TTL_GRACE=5m` sets it.
func TestTtlGraceSurvivesAMakeDeploy(t *testing.T) {
	summary := templateSummaryFromTemplate(t)
	set0 := strings.Replace(liveParams, "\n]",
		",\n  {\"ParameterKey\":\"TtlGrace\",\"ParameterValue\":\"0\"}\n]", 1)
	if set0 == liveParams {
		t.Fatal("could not add TtlGrace to the live fixture")
	}
	toExplicit := func(m map[string]string) string {
		var b strings.Builder
		for k, v := range m {
			b.WriteString(k + "=" + v + "\n")
		}
		return b.String()
	}

	plain, _ := explicitFromMakefile(t)
	got, _ := runMerge(t, summary, set0, toExplicit(plain))
	if got["TtlGrace"] != "0" {
		t.Errorf("plain make deploy over a stack with TtlGrace=0 sent %q: a deploy that did "+
			"not mention it changed it", got["TtlGrace"])
	}

	got, _ = runMerge(t, summary, liveParams, toExplicit(plain))
	if got["TtlGrace"] != "" {
		t.Errorf("a stack predating TtlGrace got %q, want the template default ''", got["TtlGrace"])
	}

	set, _ := explicitFromMakefile(t, "TTL_GRACE=5m")
	got, _ = runMerge(t, summary, set0, toExplicit(set))
	if got["TtlGrace"] != "5m" {
		t.Errorf("make deploy TTL_GRACE=5m sent %q", got["TtlGrace"])
	}
}

// TestInstanceEventsSurvivesAMakeDeploy is #1287's preservation property, the
// same shape as TestTtlGraceSurvivesAMakeDeploy: a stack set to
// InstanceEvents=on keeps it across a `make deploy` that does not mention it
// (otherwise a code-only redeploy would delete the bus and its policy), a
// stack that predates the parameter gets the template default ('off', so an
// upgrade creates no cross-account grant by itself), and
// `make deploy INSTANCE_EVENTS=on` sets it.
func TestInstanceEventsSurvivesAMakeDeploy(t *testing.T) {
	summary := templateSummaryFromTemplate(t)
	setOn := strings.Replace(liveParams, "\n]",
		",\n  {\"ParameterKey\":\"InstanceEvents\",\"ParameterValue\":\"on\"}\n]", 1)
	if setOn == liveParams {
		t.Fatal("could not add InstanceEvents to the live fixture")
	}
	toExplicit := func(m map[string]string) string {
		var b strings.Builder
		for k, v := range m {
			b.WriteString(k + "=" + v + "\n")
		}
		return b.String()
	}

	plain, _ := explicitFromMakefile(t)
	got, _ := runMerge(t, summary, setOn, toExplicit(plain))
	if got["InstanceEvents"] != "on" {
		t.Errorf("plain make deploy over a stack with InstanceEvents=on sent %q: a deploy that did "+
			"not mention it would delete the event bus", got["InstanceEvents"])
	}

	got, _ = runMerge(t, summary, liveParams, toExplicit(plain))
	if got["InstanceEvents"] != "off" {
		t.Errorf("a stack predating InstanceEvents got %q, want the template default 'off'", got["InstanceEvents"])
	}

	set, _ := explicitFromMakefile(t, "INSTANCE_EVENTS=on")
	got, _ = runMerge(t, summary, liveParams, toExplicit(set))
	if got["InstanceEvents"] != "on" {
		t.Errorf("make deploy INSTANCE_EVENTS=on sent %q", got["InstanceEvents"])
	}
}

// TestEmptyBucketsSurvivesAMakeDeploy is #1301's preservation property, the
// same shape as TestTtlGraceSurvivesAMakeDeploy: a stack set to
// EmptyBuckets=reap keeps it across a `make deploy` that does not mention it, a
// stack that predates the parameter gets the template default ('report', which
// deletes nothing and writes nothing), and `make deploy EMPTY_BUCKETS=off` sets
// it.
func TestEmptyBucketsSurvivesAMakeDeploy(t *testing.T) {
	summary := templateSummaryFromTemplate(t)
	setReap := strings.Replace(liveParams, "\n]",
		",\n  {\"ParameterKey\":\"EmptyBuckets\",\"ParameterValue\":\"reap\"}\n]", 1)
	if setReap == liveParams {
		t.Fatal("could not add EmptyBuckets to the live fixture")
	}
	toExplicit := func(m map[string]string) string {
		var b strings.Builder
		for k, v := range m {
			b.WriteString(k + "=" + v + "\n")
		}
		return b.String()
	}

	plain, _ := explicitFromMakefile(t)
	got, _ := runMerge(t, summary, setReap, toExplicit(plain))
	if got["EmptyBuckets"] != "reap" {
		t.Errorf("plain make deploy over a stack with EmptyBuckets=reap sent %q: a deploy that did "+
			"not mention it changed it", got["EmptyBuckets"])
	}

	got, _ = runMerge(t, summary, liveParams, toExplicit(plain))
	if got["EmptyBuckets"] != "report" {
		t.Errorf("a stack predating EmptyBuckets got %q, want the template default 'report'", got["EmptyBuckets"])
	}

	set, _ := explicitFromMakefile(t, "EMPTY_BUCKETS=off")
	got, _ = runMerge(t, summary, setReap, toExplicit(set))
	if got["EmptyBuckets"] != "off" {
		t.Errorf("make deploy EMPTY_BUCKETS=off sent %q", got["EmptyBuckets"])
	}
}

// TestInstanceRolesSurvivesAMakeDeploy is #1300's preservation property, the
// same shape as TestInstanceEventsSurvivesAMakeDeploy: a stack set to
// InstanceRoles=reap keeps it across a `make deploy` that does not mention it
// (a code-only redeploy must not disarm, or arm, the role sweep), a stack that
// predates the parameter gets the template default 'report' (an upgrade
// deletes nothing by itself), and `make deploy INSTANCE_ROLES=off` sets it.
func TestInstanceRolesSurvivesAMakeDeploy(t *testing.T) {
	summary := templateSummaryFromTemplate(t)
	setReap := strings.Replace(liveParams, "\n]",
		",\n  {\"ParameterKey\":\"InstanceRoles\",\"ParameterValue\":\"reap\"}\n]", 1)
	if setReap == liveParams {
		t.Fatal("could not add InstanceRoles to the live fixture")
	}
	toExplicit := func(m map[string]string) string {
		var b strings.Builder
		for k, v := range m {
			b.WriteString(k + "=" + v + "\n")
		}
		return b.String()
	}

	plain, _ := explicitFromMakefile(t)
	got, _ := runMerge(t, summary, setReap, toExplicit(plain))
	if got["InstanceRoles"] != "reap" {
		t.Errorf("plain make deploy over a stack with InstanceRoles=reap sent %q: a deploy that did "+
			"not mention it changed it", got["InstanceRoles"])
	}

	got, _ = runMerge(t, summary, liveParams, toExplicit(plain))
	if got["InstanceRoles"] != "report" {
		t.Errorf("a stack predating InstanceRoles got %q, want the template default 'report': "+
			"new deletion behaviour must start report-only", got["InstanceRoles"])
	}

	set, _ := explicitFromMakefile(t, "INSTANCE_ROLES=off")
	got, _ = runMerge(t, summary, setReap, toExplicit(set))
	if got["InstanceRoles"] != "off" {
		t.Errorf("make deploy INSTANCE_ROLES=off sent %q", got["InstanceRoles"])
	}
}

// TestPoolQueuesSurvivesAMakeDeploy is #1351's preservation property, the
// same shape as TestInstanceRolesSurvivesAMakeDeploy: a stack set to
// PoolQueues=reap keeps it across a `make deploy` that does not mention it, a
// stack that predates the parameter gets the template default 'report' (an
// upgrade deletes nothing by itself), and `make deploy POOL_QUEUES=off` sets it.
func TestPoolQueuesSurvivesAMakeDeploy(t *testing.T) {
	summary := templateSummaryFromTemplate(t)
	setReap := strings.Replace(liveParams, "\n]",
		",\n  {\"ParameterKey\":\"PoolQueues\",\"ParameterValue\":\"reap\"}\n]", 1)
	if setReap == liveParams {
		t.Fatal("could not add PoolQueues to the live fixture")
	}
	toExplicit := func(m map[string]string) string {
		var b strings.Builder
		for k, v := range m {
			b.WriteString(k + "=" + v + "\n")
		}
		return b.String()
	}

	plain, _ := explicitFromMakefile(t)
	got, _ := runMerge(t, summary, setReap, toExplicit(plain))
	if got["PoolQueues"] != "reap" {
		t.Errorf("plain make deploy over a stack with PoolQueues=reap sent %q: a deploy that did "+
			"not mention it changed it", got["PoolQueues"])
	}

	got, _ = runMerge(t, summary, liveParams, toExplicit(plain))
	if got["PoolQueues"] != "report" {
		t.Errorf("a stack predating PoolQueues got %q, want the template default 'report': "+
			"new deletion behaviour must start report-only", got["PoolQueues"])
	}

	set, _ := explicitFromMakefile(t, "POOL_QUEUES=off")
	got, _ = runMerge(t, summary, setReap, toExplicit(set))
	if got["PoolQueues"] != "off" {
		t.Errorf("make deploy POOL_QUEUES=off sent %q", got["PoolQueues"])
	}
}

// TestScheduleParamsSurvivesAMakeDeploy is #1398's preservation property, the
// same shape as TestPoolQueuesSurvivesAMakeDeploy: a stack set to
// ScheduleParams=reap keeps it across a `make deploy` that does not mention
// it, a stack that predates the parameter gets the template default 'report'
// (an upgrade deletes nothing by itself), and `make deploy
// SCHEDULE_PARAMS=off` sets it.
func TestScheduleParamsSurvivesAMakeDeploy(t *testing.T) {
	summary := templateSummaryFromTemplate(t)
	setReap := strings.Replace(liveParams, "\n]",
		",\n  {\"ParameterKey\":\"ScheduleParams\",\"ParameterValue\":\"reap\"}\n]", 1)
	if setReap == liveParams {
		t.Fatal("could not add ScheduleParams to the live fixture")
	}
	toExplicit := func(m map[string]string) string {
		var b strings.Builder
		for k, v := range m {
			b.WriteString(k + "=" + v + "\n")
		}
		return b.String()
	}

	plain, _ := explicitFromMakefile(t)
	got, _ := runMerge(t, summary, setReap, toExplicit(plain))
	if got["ScheduleParams"] != "reap" {
		t.Errorf("plain make deploy over a stack with ScheduleParams=reap sent %q: a deploy that did "+
			"not mention it changed it", got["ScheduleParams"])
	}

	got, _ = runMerge(t, summary, liveParams, toExplicit(plain))
	if got["ScheduleParams"] != "report" {
		t.Errorf("a stack predating ScheduleParams got %q, want the template default 'report': "+
			"new deletion behaviour must start report-only", got["ScheduleParams"])
	}

	set, _ := explicitFromMakefile(t, "SCHEDULE_PARAMS=off")
	got, _ = runMerge(t, summary, setReap, toExplicit(set))
	if got["ScheduleParams"] != "off" {
		t.Errorf("make deploy SCHEDULE_PARAMS=off sent %q", got["ScheduleParams"])
	}
}

// TestLaunchConfigsSurvivesAMakeDeploy is #1359's preservation property, the
// same shape as TestPoolQueuesSurvivesAMakeDeploy: a stack set to
// LaunchConfigs=reap keeps it across a `make deploy` that does not mention
// it, a stack that predates the parameter gets the template default 'report'
// (an upgrade deletes nothing by itself), and `make deploy
// LAUNCH_CONFIGS=off` sets it.
func TestLaunchConfigsSurvivesAMakeDeploy(t *testing.T) {
	summary := templateSummaryFromTemplate(t)
	setReap := strings.Replace(liveParams, "\n]",
		",\n  {\"ParameterKey\":\"LaunchConfigs\",\"ParameterValue\":\"reap\"}\n]", 1)
	if setReap == liveParams {
		t.Fatal("could not add LaunchConfigs to the live fixture")
	}
	toExplicit := func(m map[string]string) string {
		var b strings.Builder
		for k, v := range m {
			b.WriteString(k + "=" + v + "\n")
		}
		return b.String()
	}

	plain, _ := explicitFromMakefile(t)
	got, _ := runMerge(t, summary, setReap, toExplicit(plain))
	if got["LaunchConfigs"] != "reap" {
		t.Errorf("plain make deploy over a stack with LaunchConfigs=reap sent %q: a deploy that did "+
			"not mention it changed it", got["LaunchConfigs"])
	}

	got, _ = runMerge(t, summary, liveParams, toExplicit(plain))
	if got["LaunchConfigs"] != "report" {
		t.Errorf("a stack predating LaunchConfigs got %q, want the template default 'report': "+
			"new deletion behaviour must start report-only", got["LaunchConfigs"])
	}

	set, _ := explicitFromMakefile(t, "LAUNCH_CONFIGS=off")
	got, _ = runMerge(t, summary, setReap, toExplicit(set))
	if got["LaunchConfigs"] != "off" {
		t.Errorf("make deploy LAUNCH_CONFIGS=off sent %q", got["LaunchConfigs"])
	}
}

// TestMemberRegionsDefaultIsTheReapersRegions: `make member-events-deploy`
// puts the forwarding rule in MEMBER_REGIONS, and the reaper only acts on an
// event from a region in its own Regions. The two defaults must be the same
// list, or a default deployment has regions whose terminations are forwarded
// and ignored, or scanned and never forwarded (#1287).
func TestMemberRegionsDefaultIsTheReapersRegions(t *testing.T) {
	mk, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^MEMBER_REGIONS\?=(\S+)$`).FindSubmatch(mk)
	if m == nil {
		t.Fatal("Makefile has no MEMBER_REGIONS?= default")
	}
	tmpl, err := os.ReadFile("template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := regexp.MustCompile(`(?m)^  Regions:\n(?:    .*\n)*?    Default: '([^']+)'`).FindSubmatch(tmpl)
	if d == nil {
		t.Fatal("template.yaml has no Regions Default")
	}
	if string(m[1]) != string(d[1]) {
		t.Errorf("MEMBER_REGIONS defaults to %s, the reaper's Regions to %s", m[1], d[1])
	}
	// Every region in the loop gets an explicit --region, and teardown covers
	// the same list as deploy.
	for _, target := range []string{"member-events-deploy", "member-events-teardown"} {
		body := regexp.MustCompile(`(?ms)^` + target + `:\n(.*?)\n\n`).FindSubmatch(mk)
		if body == nil {
			t.Fatalf("no %s target", target)
		}
		if !strings.Contains(string(body[1]), "$(subst $(comma), ,$(MEMBER_REGIONS))") {
			t.Errorf("%s does not loop over MEMBER_REGIONS", target)
		}
		for _, line := range regexp.MustCompile(`(?m)^\s*aws cloudformation .*$`).FindAllString(string(body[1]), -1) {
			if !strings.Contains(line, "--region $$r") {
				t.Errorf("%s: %q has no --region $$r", target, strings.TrimSpace(line))
			}
		}
	}
}
