package cmd

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/instancerole"
)

// shippedTags decodes the tags member i's row carries for the orchestrator.
func shippedTags(t *testing.T, pf *ParamFileFormat, i int) []ec2types.Tag {
	t.Helper()
	raw, ok := pf.Params[i][detachedTagsKey].(map[string]interface{})
	if !ok || len(raw) == 0 {
		t.Fatalf("member %d carries no %s: the orchestrator refuses it", i, detachedTagsKey)
	}
	// Through JSON, as S3 delivers it.
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("member %d's tags are not a JSON object of strings: %v", i, err)
	}
	var tags []ec2types.Tag
	for k, v := range back {
		tags = append(tags, ec2types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return tags
}

func tagsAsMap(tags []ec2types.Tag) map[string]string {
	m := map[string]string{}
	for _, tg := range tags {
		m[awssdk.ToString(tg.Key)] = awssdk.ToString(tg.Value)
	}
	return m
}

func anchoredAt(t *testing.T, tags []ec2types.Tag, at time.Time) map[string]string {
	t.Helper()
	out, err := aws.AnchorLaunchTags(tags, at)
	if err != nil {
		t.Fatal(err)
	}
	return tagsAsMap(out)
}

// TestDetachedMemberTagsAreTheForegroundMembers is #1330's CLI half.
//
// A detached member used to launch with five tags the orchestrator made up, so
// its command, parameters, idle timeout, cost limit and completion action did
// nothing, and a member with no ttl had no bound. Now the tags it ships are,
// decoded and anchored at the same launch instant, the tags a foreground launch
// of the same member writes: the member config built as launchParameterSweep's
// foreground loop builds it, through Launch's own tag builder. The Lambda half,
// TestMemberLaunchesWithTheCLIsTags, proves the orchestrator launches with them.
func TestDetachedMemberTagsAreTheForegroundMembers(t *testing.T) {
	// A converted Slurm script: over the 256-character tag cap and multi-line.
	longCmd := "#!/bin/bash\n#SBATCH --time=1:00:00\n" + strings.Repeat("# padding to pass the tag cap\n", 10) +
		"python train.py --alpha \"$PARAM_ALPHA\"\n"
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0", "region": "us-east-1"},
		Params: []map[string]interface{}{
			{"alpha": 0.1, "command": "python train.py", "cost_limit": 8.0, "on_complete": "terminate", "ttl": "2h"},
			{"alpha": 0.2, "command": longCmd, "idle_timeout": "30m", "completion_delay": "5m"},
			{"alpha": 0.3}, // no ttl, no idle_timeout: the 1h idle default
		},
	}
	before := copyParamFile(pf)
	base := aws.LaunchConfig{InstanceType: "t3.micro", Tags: map[string]string{"project": "p1"}}

	if err := seedDetachedBootstrap(pf, base, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
		t.Fatalf("seedDetachedBootstrap: %v", err)
	}

	launchAt := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	for i := range pf.Params {
		// The foreground loop: buildLaunchConfigFromParams, the per-member
		// defaults, the target OS; Launch then builds the tags.
		cfg, err := buildLaunchConfigFromParams(base, before.Defaults, before.Params[i], "sw-1", "sw", i, len(before.Params))
		if err != nil {
			t.Fatal(err)
		}
		defaultSweepMember(&cfg, base, "sw", i)
		cfg.TargetOS = "linux"
		fg, err := testMemberTagList(cfg)
		if err != nil {
			t.Fatal(err)
		}

		got := anchoredAt(t, shippedTags(t, pf, i), launchAt)
		want := anchoredAt(t, fg, launchAt)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("member %d: detached tags\n  %v\nwant the foreground member's\n  %v", i, got, want)
		}
	}

	// What the tags are for, member by member.
	m0 := tagsAsMap(shippedTags(t, pf, 0))
	for k, v := range map[string]string{
		"spawn:command": "python train.py", "spawn:param:alpha": "0.1", "spawn:cost-limit": "8.0000",
		"spawn:on-complete": "terminate", "spawn:ttl": "2h", "spawn:price-per-hour": "0.041600",
		"project": "p1", "Name": "sw-0", "spawn:sweep-index": "0", "spawn:managed": "true",
	} {
		if m0[k] != v {
			t.Errorf("member 0: %s = %q, want %q", k, m0[k], v)
		}
	}
	m1 := tagsAsMap(shippedTags(t, pf, 1))
	if _, ok := m1["spawn:command"]; ok {
		t.Error("member 1's oversized command rides a tag, which EC2 refuses; it belongs in the user data (#1329)")
	}
	if !strings.Contains(decodedScript(t, memberUserData(pf, 1)), "SBATCH --time=1:00:00") {
		t.Error("member 1's multi-line command is in neither its tags nor its user data: it never runs")
	}
	if m1["spawn:idle-timeout"] != "30m" || m1["spawn:completion-delay"] != "5m" {
		t.Errorf("member 1: idle-timeout %q, completion-delay %q, want 30m and 5m", m1["spawn:idle-timeout"], m1["spawn:completion-delay"])
	}
	if m2 := tagsAsMap(shippedTags(t, pf, 2)); m2["spawn:idle-timeout"] != "1h" {
		t.Errorf("member 2 has no ttl and idle-timeout %q, want the 1h default a foreground member gets", m2["spawn:idle-timeout"])
	}
}

// TestEveryDetachedMemberIsBounded: whatever a row leaves out, a member ships
// with a spawn:ttl (which the orchestrator turns into spawn:ttl-deadline at the
// real launch) or a spawn:idle-timeout, as a foreground member does. Only
// --no-timeout, the explicit opt-out, leaves one unbounded, as it does in the
// foreground.
func TestEveryDetachedMemberIsBounded(t *testing.T) {
	rows := []map[string]interface{}{{}, {"ttl": "3h"}, {"idle_timeout": "10m"}, {"on_complete": "terminate"}, {"cost_limit": 2.0}}
	seed := func() *ParamFileFormat {
		pf := &ParamFileFormat{
			Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-1", "region": "us-east-1"},
			Params:   copyParamFile(&ParamFileFormat{Params: rows}).Params,
		}
		if err := seedDetachedBootstrap(pf, aws.LaunchConfig{}, "sw-1", "sw", testDetachedBootstrap(t)); err != nil {
			t.Fatal(err)
		}
		return pf
	}
	pf := seed()
	for i := range rows {
		m := tagsAsMap(shippedTags(t, pf, i))
		_, ttl := m["spawn:ttl"]
		_, idle := m["spawn:idle-timeout"]
		if !ttl && !idle {
			t.Errorf("member %d (%v) ships with neither spawn:ttl nor spawn:idle-timeout: nothing in-instance ends it", i, rows[i])
		}
		if ttl {
			if _, ok := anchoredAt(t, shippedTags(t, pf, i), time.Now())["spawn:ttl-deadline"]; !ok {
				t.Errorf("member %d has a spawn:ttl and no deadline once launched", i)
			}
		}
	}

	prev := noTimeout
	noTimeout = true
	t.Cleanup(func() { noTimeout = prev })
	if _, idle := tagsAsMap(shippedTags(t, seed(), 0))["spawn:idle-timeout"]; idle {
		t.Error("--no-timeout still gave a member the idle default; it is the foreground's explicit opt-out")
	}
}

// A detached Spot member whose lifecycle stops the instance would need a
// persistent request, which the orchestrator does not make (it could not cancel
// one before terminating its instance): refused, with what to do instead. With
// a ttl and nothing that stops, it is a one-time request, as in the foreground.
func TestDetachedSpotMemberThatWouldStopIsRefused(t *testing.T) {
	for _, tc := range []struct {
		row     map[string]interface{}
		refused bool
	}{
		{map[string]interface{}{"spot": true}, true}, // the 1h idle default stops
		{map[string]interface{}{"spot": true, "ttl": "2h", "on_complete": "stop"}, true},
		{map[string]interface{}{"spot": true, "ttl": "2h"}, false},
		{map[string]interface{}{"spot": true, "ttl": "2h", "on_complete": "terminate"}, false},
	} {
		pf := &ParamFileFormat{
			Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-1", "region": "us-east-1"},
			Params:   []map[string]interface{}{tc.row},
		}
		err := seedDetachedBootstrap(pf, aws.LaunchConfig{}, "sw-1", "sw", testDetachedBootstrap(t))
		if tc.refused {
			if err == nil || !strings.Contains(err.Error(), "--no-detach") {
				t.Errorf("%v: err = %v, want a refusal pointing at --no-detach", tc.row, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", tc.row, err)
			continue
		}
		if got := tagsAsMap(shippedTags(t, pf, 0))["spawn:spot-request-type"]; got != "one-time" {
			t.Errorf("%v: spawn:spot-request-type = %q, want one-time", tc.row, got)
		}
	}
}

// A schedule's members carry their tags too, named after the schedule's sweep.
func TestSeedScheduleParams_CarriesTheTags(t *testing.T) {
	p := &parsedParams{
		defaults: map[string]interface{}{"instance_type": "t3.micro", "sweep_name": "nightly", "ttl": "4h"},
		params:   []map[string]interface{}{{"trial": "a", "ami": "ami-row", "command": "make run"}},
	}
	data, err := seedScheduleParams(context.Background(), nil, p, "eu-west-1", &fakeProfileEnsurer{}, testDetachedBootstrap(t))
	if err != nil {
		t.Fatal(err)
	}
	var got ParamFileFormat
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	m := tagsAsMap(shippedTags(t, &got, 0))
	if m["spawn:command"] != "make run" || m["spawn:ttl"] != "4h" || m["Name"] != "nightly-0" {
		t.Errorf("scheduled member tags: command %q, ttl %q, Name %q", m["spawn:command"], m["spawn:ttl"], m["Name"])
	}
}

// A file cannot supply the tags itself: they would replace spawn's.
func TestDetachedTagsNeverFromTheFile(t *testing.T) {
	file := &ParamFileFormat{Params: []map[string]interface{}{{detachedTagsKey: map[string]interface{}{"spawn:managed": "false"}}}}
	if err := validateSweepParamKeys(file); err == nil {
		t.Errorf("a param file may set %s", detachedTagsKey)
	}
}

// TestSweepRowUserDataRuns is #1327: a row's user_data: was parsed into
// LaunchConfig.UserData and then overwritten by the foreground loop, and never
// read by the detached path. It now runs, after spored's bootstrap and after
// --user-data, on both paths.
func TestSweepRowUserDataRuns(t *testing.T) {
	prev := userData
	userData = "echo FLAG-UD-MARKER"
	t.Cleanup(func() { userData = prev })

	row := map[string]interface{}{"user_data": "echo ROW-UD-MARKER"}
	boot := testDetachedBootstrap(t)

	// Foreground: the config the loop builds, then buildUserData.
	cfg, err := buildLaunchConfigFromParams(aws.LaunchConfig{}, nil, row, "sw-1", "sw", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TargetOS = "linux"
	fg, err := buildUserData(boot.plat, &cfg, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Detached: what the orchestrator launches.
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-1", "region": "us-east-1"},
		Params:   []map[string]interface{}{row, {}},
	}
	if err := seedDetachedBootstrap(pf, aws.LaunchConfig{}, "sw-1", "sw", boot); err != nil {
		t.Fatal(err)
	}
	for path, script := range map[string]string{"foreground": fg, "detached": decodedScript(t, memberUserData(pf, 0))} {
		f, r := strings.Index(script, "FLAG-UD-MARKER"), strings.Index(script, "ROW-UD-MARKER")
		if r < 0 {
			t.Errorf("%s: the row's user_data: is not in the member's user data", path)
		}
		if f < 0 || r < f {
			t.Errorf("%s: --user-data at %d, the row's at %d; want both, the flag's first", path, f, r)
		}
	}
	if strings.Contains(decodedScript(t, memberUserData(pf, 1)), "ROW-UD-MARKER") {
		t.Error("one row's user_data: reached another member")
	}

	// Windows' bootstrap would drop it: refused, not dropped.
	cfg.TargetOS = "windows"
	userData = ""
	if err := userDataOSGuard(&cfg); err == nil {
		t.Error("a Windows member with a row user_data: was accepted; its bootstrap would drop it")
	}
}

// TestDetachedPassableProfilesMatchTheGrant is #1331's CLI half: the profiles a
// detached sweep may use are exactly those whose roles the orchestrator's
// cross-account role may pass (scripts/setup-sweep-cross-account-role.sh, read
// here, not restated), and anything else is refused before upload.
func TestDetachedPassableProfilesMatchTheGrant(t *testing.T) {
	grant := passRoleResources(t, "Allow")
	if len(grant) == 0 {
		t.Fatal("found no iam:PassRole resources in the script; the matcher is stale")
	}
	denied := passRoleResources(t, "Deny")
	match := func(g, role string) bool {
		return g == role || (strings.HasSuffix(g, "*") && strings.HasPrefix(role, strings.TrimSuffix(g, "*")))
	}
	// spawn names a profile after its role, except spored's.
	roleOf := func(profile string) string {
		if profile == aws.SporedInstanceProfileName {
			return aws.SporedRoleName
		}
		return profile
	}
	granted := func(role string) bool {
		for _, d := range denied {
			if match(d, role) {
				return false
			}
		}
		for _, g := range grant {
			if match(g, role) {
				return true
			}
		}
		return false
	}
	allowed := append([]string{instancerole.NamePrefix + "0123abcd"}, orchestratorPassableProfiles...)
	for _, p := range allowed {
		if !orchestratorPassableProfile(p) {
			t.Errorf("%s is not passable", p)
		}
		if !granted(roleOf(p)) {
			t.Errorf("the CLI lets a detached member use %s, whose role %s the cross-account role cannot pass: "+
				"every such member is refused at RunInstances", p, roleOf(p))
		}
	}
	for _, g := range grant {
		probe := strings.TrimSuffix(g, "*") + map[bool]string{true: "x", false: ""}[strings.HasSuffix(g, "*")]
		ok := false
		for _, p := range append([]string{probe}, orchestratorPassableProfiles...) {
			if orchestratorPassableProfile(p) && roleOf(p) == probe {
				ok = true
			}
		}
		if !ok {
			t.Errorf("the grant passes %s but the CLI refuses every profile holding it", g)
		}
	}
	for _, p := range []string{"my-admin-profile", "spawn-instance", "spored-instance-role"} {
		if orchestratorPassableProfile(p) {
			t.Errorf("%s is passable, but the grant cannot pass its role", p)
		}
	}
	// #1372: whatever the script denies, the CLI refuses, and the nested-spawn
	// roles are among them.
	for _, d := range denied {
		probe := strings.TrimSuffix(d, "*") + map[bool]string{true: "0123abcd", false: ""}[strings.HasSuffix(d, "*")]
		if orchestratorPassableProfile(probe) {
			t.Errorf("the script denies PassRole on %s, but the CLI lets a detached member use %s", d, probe)
		}
	}
	if granted(aws.NestedSpawnRoleName()) {
		t.Errorf("the cross-account role may pass the nested-spawn role %s (#1372)", aws.NestedSpawnRoleName())
	}
}

// passRoleResources returns the role names (after role/) in the cross-account
// role's iam:PassRole statements with the given Effect.
func passRoleResources(t *testing.T, effect string) []string {
	t.Helper()
	src, err := os.ReadFile("../scripts/setup-sweep-cross-account-role.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)POLICY_DOCUMENT=\$\(cat <<EOF\n(.*?)\nEOF\n\)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("found no POLICY_DOCUMENT heredoc")
	}
	var policy struct {
		Statement []struct {
			Effect   string
			Action   interface{}
			Resource interface{}
		}
	}
	if err := json.Unmarshal([]byte(strings.ReplaceAll(string(m[1]), "${CURRENT_ACCOUNT_ID}", "111122223333")), &policy); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, st := range policy.Statement {
		if st.Effect != effect {
			continue
		}
		if !reflect.DeepEqual(st.Action, []interface{}{"iam:PassRole"}) && st.Action != "iam:PassRole" {
			continue
		}
		res, _ := st.Resource.([]interface{})
		for _, r := range res {
			s, _ := r.(string)
			out = append(out, strings.TrimPrefix(s, "arn:aws:iam::111122223333:role/"))
		}
	}
	return out
}

// Each way a detached sweep can name an unpassable profile is refused, and the
// launch path refuses before applyCLIIAMToSweep creates a role.
func TestUnpassableDetachedProfileIsRefusedUpFront(t *testing.T) {
	nested := aws.NestedSpawnRoleName()
	reset := func() { instanceProfile, iamRole, iamPolicy = "", "", nil }
	t.Cleanup(reset)
	pf := func(def, row string) *ParamFileFormat {
		f := &ParamFileFormat{Defaults: map[string]interface{}{}, Params: []map[string]interface{}{{}}}
		if def != "" {
			f.Defaults["iam_role"] = def
		}
		if row != "" {
			f.Params[0]["iam_role"] = row
		}
		return f
	}
	for _, tc := range []struct {
		name    string
		set     func()
		file    *ParamFileFormat
		refused bool
	}{
		{"--instance-profile mine", func() { instanceProfile = "mine" }, pf("", ""), true},
		{"--iam-role mine --iam-policy s3:ReadOnly", func() { iamRole, iamPolicy = "mine", []string{"s3:ReadOnly"} }, pf("", ""), true},
		{"defaults iam_role", func() {}, pf("mine", ""), true},
		{"row iam_role", func() {}, pf("", "mine"), true},
		{"--iam-policy (spawn-instance-*)", func() { iamPolicy = []string{"s3:ReadOnly"} }, pf("mine", ""), false},
		{"spawn's own", func() {}, pf(aws.SporedInstanceProfileName, instancerole.NamePrefix+"ab12"), false},
		// #1372: the nested-spawn roles are spawn-instance-* but never passable.
		{"nested --instance-profile", func() { instanceProfile = nested }, pf("", ""), true},
		{"nested --iam-role --iam-policy", func() { iamRole, iamPolicy = nested, []string{"s3:ReadOnly"} }, pf("", ""), true},
		{"nested defaults iam_role", func() {}, pf(nested, ""), true},
		{"nested row iam_role", func() {}, pf("", nested), true},
		{"nested-prefix row iam_role", func() {}, pf("", aws.NestedSpawnRolePrefix+"0123abcd"), true},
	} {
		reset()
		tc.set()
		err := refuseUnpassableDetachedProfiles(tc.file)
		if (err != nil) != tc.refused {
			t.Errorf("%s: err = %v, want refused=%v", tc.name, err, tc.refused)
		}
		if err == nil {
			continue
		}
		if strings.HasPrefix(tc.name, "nested") {
			if !strings.Contains(err.Error(), "nested spawn isn't available for detached sweeps") ||
				!strings.Contains(err.Error(), "--nested-spawn on a single launch, which requires --ttl") {
				t.Errorf("%s: the refusal does not say nested spawn is single-launch only, with --ttl: %v", tc.name, err)
			}
			continue
		}
		if !strings.Contains(err.Error(), "--no-detach") {
			t.Errorf("%s: the refusal does not say how to run the sweep: %v", tc.name, err)
		}
	}

	src, err := os.ReadFile("launch_sweep.go")
	if err != nil {
		t.Fatal(err)
	}
	refuse := regexp.MustCompile(`(?m)^\s*if detach && maxConcurrent > 0 \{\n\s*if err := refuseUnpassableDetachedProfiles\(paramFormat\); err != nil \{\n\s*return err\n`).FindIndex(src)
	apply := regexp.MustCompile(`(?m)^\s*iamRecord, err := applyCLIIAMToSweep\(ctx, awsClient, paramFormat, auditLog\)$`).FindIndex(src)
	if apply == nil {
		t.Fatal("cannot find applyCLIIAMToSweep's call; the matcher is stale")
	}
	if refuse == nil || refuse[0] > apply[0] {
		t.Error("launchParameterSweep does not refuse an unpassable profile before applyCLIIAMToSweep creates a role (#1331)")
	}
	sched, err := os.ReadFile("schedule.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s*if err := refuseUnpassableDetachedProfiles\(pf\); err != nil \{\n\s*return nil, err\n`).Match(sched) {
		t.Error("spawn schedule create does not refuse an unpassable profile (#1331)")
	}
}

// The foreground loop builds its members through defaultSweepMember, the
// function the detached path renders tags after; a foreground loop with its own
// copy is how the two drift apart.
func TestForegroundSweepUsesTheSharedMemberDefaults(t *testing.T) {
	src, err := os.ReadFile("launch_sweep.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s*if defaultSweepMember\(&config, base, name, i\) \{`).Match(src) {
		t.Error("foregroundSweepMembers does not apply defaultSweepMember")
	}
	if !regexp.MustCompile(`(?m)^\s*launchConfigs, defaultedVerbs, err := foregroundSweepMembers\(\*baseConfig, paramFormat, sweepID, name\)`).Match(src) {
		t.Error("launchParameterSweep does not build its foreground members with foregroundSweepMembers")
	}
	if regexp.MustCompile(`(?m)^\s*if applyIdleTimeoutDefault\(`).Match(src) {
		t.Error("launch_sweep.go applies the idle default itself, beside defaultSweepMember")
	}
}
