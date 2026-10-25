package reaperiam

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The event-driven launch Elastic IP release (#1287) adds the only
// cross-account grant in the reaper that points the other way: member accounts
// put events on a bus in the reaper's account. These gates hold it to the
// account set the reaper is already configured for.

const instanceEventsRulePath = "../../deployment/cloudformation/ttl-reaper-instance-events.yaml"

// cfnResources returns a template's resources of one type, by logical ID.
func cfnResources(t *testing.T, tmpl map[string]interface{}, typ string) map[string]map[string]interface{} {
	t.Helper()
	out := map[string]map[string]interface{}{}
	resources, _ := tmpl["Resources"].(map[string]interface{})
	for name, raw := range resources {
		res, _ := raw.(map[string]interface{})
		if res["Type"] == typ {
			out[name] = res
		}
	}
	return out
}

func props(res map[string]interface{}) map[string]interface{} {
	p, _ := res["Properties"].(map[string]interface{})
	return p
}

func paramDefault(t *testing.T, tmpl map[string]interface{}, name string) string {
	t.Helper()
	params, _ := tmpl["Parameters"].(map[string]interface{})
	p, _ := params[name].(map[string]interface{})
	d, ok := p["Default"].(string)
	if !ok {
		t.Fatalf("parameter %s has no string Default", name)
	}
	return d
}

// evalCFN evaluates the intrinsics the bus policy's principal list may use,
// with parameters bound to the given values. Anything else fails the test: a
// principal this gate cannot evaluate is a principal it cannot vouch for.
func evalCFN(t *testing.T, v interface{}, params map[string][]string) []string {
	t.Helper()
	switch x := v.(type) {
	case string:
		return []string{x}
	case []interface{}:
		var out []string
		for _, e := range x {
			out = append(out, evalCFN(t, e, params)...)
		}
		return out
	case map[string]interface{}:
		if len(x) != 1 {
			break
		}
		if ref, ok := x["Ref"].(string); ok {
			vals, ok := params[ref]
			if !ok {
				t.Fatalf("principal references %s, which is not RoleArns", ref)
			}
			return vals
		}
		if args, ok := x["Fn::Join"].([]interface{}); ok && len(args) == 2 {
			delim, _ := args[0].(string)
			return []string{strings.Join(evalCFN(t, args[1], params), delim)}
		}
		if args, ok := x["Fn::Split"].([]interface{}); ok && len(args) == 2 {
			delim, _ := args[0].(string)
			joined := evalCFN(t, args[1], params)
			if len(joined) != 1 {
				t.Fatalf("Fn::Split of %d values", len(joined))
			}
			return strings.Split(joined[0], delim)
		}
	}
	t.Fatalf("the bus policy principal uses %v, which this gate cannot evaluate", v)
	return nil
}

// TestInstanceEventBusPolicyAdmitsExactlyTheConfiguredAccounts: the bus
// policy's principals are the forwarder roles of exactly the accounts in
// RoleArns, derived from RoleArns itself, for events:PutEvents on that bus and
// nothing else. Evaluated, not pattern-matched: RoleArns is bound to a sample
// and the derived list must be exactly those roles with "-events" appended.
func TestInstanceEventBusPolicyAdmitsExactlyTheConfiguredAccounts(t *testing.T) {
	tmpl := loadCFN(t, samTemplatePath)
	policies := cfnResources(t, tmpl, "AWS::Events::EventBusPolicy")
	if len(policies) != 1 {
		t.Fatalf("found %d AWS::Events::EventBusPolicy resources in %s, want 1", len(policies), samTemplatePath)
	}
	for name, res := range policies {
		if res["Condition"] != "InstanceEventsEnabled" {
			t.Errorf("%s is not conditioned on InstanceEventsEnabled: InstanceEvents=off must create no grant", name)
		}
		p := props(res)
		st, ok := p["Statement"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: Statement is not a single statement mapping: %v", name, p["Statement"])
		}
		for k := range st {
			switch k {
			case "Effect", "Principal", "Action", "Resource":
			default:
				// A Condition could widen the principal (aws:PrincipalOrgID with
				// a broader Principal); nothing here needs one.
				t.Errorf("%s: unexpected statement key %s", name, k)
			}
		}
		if st["Effect"] != "Allow" {
			t.Errorf("%s: Effect %v", name, st["Effect"])
		}
		if a := asList(st["Action"]); len(a) != 1 || a[0] != "events:PutEvents" {
			t.Errorf("%s: Action %v, want only events:PutEvents", name, st["Action"])
		}
		if !reflect.DeepEqual(st["Resource"], map[string]interface{}{"Fn::GetAtt": "InstanceEventBus.Arn"}) {
			t.Errorf("%s: Resource %v, want the bus's own ARN", name, st["Resource"])
		}
		principal, _ := st["Principal"].(map[string]interface{})
		if len(principal) != 1 || principal["AWS"] == nil {
			t.Fatalf("%s: Principal %v, want only AWS", name, st["Principal"])
		}
		sample := []string{
			"arn:aws:iam::435415984226:role/spawn-ttl-reaper-ec2",
			"arn:aws:iam::812107987990:role/custom-reaper-role",
		}
		for _, roles := range [][]string{sample, sample[:1]} {
			got := evalCFN(t, principal["AWS"], map[string][]string{"RoleArns": roles})
			var want []string
			for _, r := range roles {
				want = append(want, r+forwarderSuffix(t))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: RoleArns %v derives principals %v, want %v", name, roles, got, want)
			}
			for _, g := range got {
				if strings.Contains(g, "*") || !strings.HasPrefix(g, "arn:aws:iam::") {
					t.Errorf("%s: derived principal %q", name, g)
				}
			}
		}
	}
}

// forwarderSuffix is what ttl-reaper-cross-account-role.yaml appends to its
// RoleName to name the forwarder role. The bus policy's derivation must append
// the same, or it admits a role that does not exist and refuses the real one.
func forwarderSuffix(t *testing.T) string {
	t.Helper()
	tmpl := loadCFN(t, crossAccountRolePath)
	res, ok := cfnResources(t, tmpl, "AWS::IAM::Role")["InstanceEventsForwarderRole"]
	if !ok {
		t.Fatalf("%s has no InstanceEventsForwarderRole", crossAccountRolePath)
	}
	sub, _ := props(res)["RoleName"].(map[string]interface{})
	name, _ := sub["Fn::Sub"].(string)
	suffix, ok := strings.CutPrefix(name, "${RoleName}")
	if !ok || suffix == "" {
		t.Fatalf("forwarder RoleName %v is not ${RoleName}<suffix>", props(res)["RoleName"])
	}
	return suffix
}

// TestNoEventBusPolicyAnywhereAdmitsAWildcard discovers every bus policy in
// every template this repository deploys and fails on a wildcard or
// organization-wide principal.
func TestNoEventBusPolicyAnywhereAdmitsAWildcard(t *testing.T) {
	paths, _ := filepath.Glob("../../lambda/*/template.yaml")
	more, _ := filepath.Glob("../../deployment/cloudformation/*.yaml")
	paths = append(paths, more...)
	if len(paths) < 5 {
		t.Fatalf("discovered only %d templates; the discovery has broken", len(paths))
	}
	found := 0
	for _, p := range paths {
		tmpl := loadCFN(t, p)
		for name, res := range cfnResources(t, tmpl, "AWS::Events::EventBusPolicy") {
			found++
			b, _ := json.Marshal(props(res))
			s := string(b)
			for _, bad := range []string{`"*"`, `"Principal":"*"`, "PrincipalOrgID", "PrincipalOrgPaths", ":root"} {
				if strings.Contains(s, bad) {
					t.Errorf("%s %s: bus policy contains %s: %s", p, name, bad, s)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("found no AWS::Events::EventBusPolicy anywhere; the discovery has broken (the reaper template has one)")
	}
}

// TestInstanceEventsForwarderRole: the member-account forwarder may only put
// events on the reaper's one bus, and only EventBridge may assume it, on behalf
// of this account's own rule of the fixed name.
func TestInstanceEventsForwarderRole(t *testing.T) {
	tmpl := loadCFN(t, crossAccountRolePath)
	res := cfnResources(t, tmpl, "AWS::IAM::Role")["InstanceEventsForwarderRole"]
	if res == nil {
		t.Fatal("no InstanceEventsForwarderRole")
	}
	p := props(res)
	trust, _ := p["AssumeRolePolicyDocument"].(map[string]interface{})
	stmts := asList(trust["Statement"])
	if len(stmts) != 1 {
		t.Fatalf("forwarder trust has %d statements, want 1", len(stmts))
	}
	st, _ := stmts[0].(map[string]interface{})
	if !reflect.DeepEqual(st["Principal"], map[string]interface{}{"Service": "events.amazonaws.com"}) {
		t.Errorf("forwarder trust principal %v, want only events.amazonaws.com", st["Principal"])
	}
	cond, _ := json.Marshal(st["Condition"])
	for _, want := range []string{`"aws:SourceAccount":{"Ref":"AWS::AccountId"}`, `rule/spawn-ttl-reaper-instance-terminated"`} {
		if !strings.Contains(string(cond), want) {
			t.Errorf("forwarder trust condition %s lacks %s (confused-deputy guard)", cond, want)
		}
	}

	var actions []string
	for _, pol := range asList(p["Policies"]) {
		pm, _ := pol.(map[string]interface{})
		doc, _ := pm["PolicyDocument"].(map[string]interface{})
		for _, s := range asList(doc["Statement"]) {
			sm, _ := s.(map[string]interface{})
			for _, a := range asList(sm["Action"]) {
				actions = append(actions, a.(string))
			}
			want := map[string]interface{}{"Fn::Sub": "arn:aws:events:${ReaperRegion}:${ReaperAccountId}:event-bus/${ReaperEventBus}"}
			if !reflect.DeepEqual(sm["Resource"], want) {
				t.Errorf("forwarder grants on %v, want only the reaper's bus", sm["Resource"])
			}
		}
	}
	if !reflect.DeepEqual(actions, []string{"events:PutEvents"}) {
		t.Errorf("forwarder actions %v, want only events:PutEvents", actions)
	}

	// The bus it names by default is the one the reaper template creates by
	// default; the account is the one the reaper role already trusts.
	sam := loadCFN(t, samTemplatePath)
	bus := cfnResources(t, sam, "AWS::Events::EventBus")["InstanceEventBus"]
	if bus == nil {
		t.Fatal("reaper template has no InstanceEventBus")
	}
	nameSub, _ := props(bus)["Name"].(map[string]interface{})
	samName := strings.ReplaceAll(nameSub["Fn::Sub"].(string), "${Environment}", paramDefault(t, sam, "Environment"))
	for _, path := range []string{crossAccountRolePath, instanceEventsRulePath} {
		tm := loadCFN(t, path)
		if got := paramDefault(t, tm, "ReaperEventBus"); got != samName {
			t.Errorf("%s ReaperEventBus default %q, the reaper creates %q", path, got, samName)
		}
		if got, want := paramDefault(t, tm, "ReaperAccountId"), paramDefault(t, tmpl, "ReaperAccountId"); got != want {
			t.Errorf("%s ReaperAccountId default %q, the reaper role trusts %q", path, got, want)
		}
	}
	rule := loadCFN(t, instanceEventsRulePath)
	if got, want := paramDefault(t, rule, "ForwarderRoleName"), paramDefault(t, tmpl, "RoleName")+forwarderSuffix(t); got != want {
		t.Errorf("rule template forwards through %q, the role stack creates %q", got, want)
	}
}

// TestInstanceEventRulesMatchOnlyTerminatedStateChanges: every EventBridge rule
// in the reaper's templates that listens to EC2 matches exactly the terminated
// state change, nothing broader, and the member rule's name is the one the
// forwarder trusts.
func TestInstanceEventRulesMatchOnlyTerminatedStateChanges(t *testing.T) {
	want := map[string]interface{}{
		"source":      []interface{}{"aws.ec2"},
		"detail-type": []interface{}{"EC2 Instance State-change Notification"},
		"detail":      map[string]interface{}{"state": []interface{}{"terminated"}},
	}
	found := 0
	for _, path := range []string{samTemplatePath, instanceEventsRulePath} {
		for name, res := range cfnResources(t, loadCFN(t, path), "AWS::Events::Rule") {
			found++
			if got := props(res)["EventPattern"]; !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s: EventPattern %v, want %v", path, name, got, want)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d instance-event rules, want 2 (the reaper's bus rule and the member rule)", found)
	}
	for _, res := range cfnResources(t, loadCFN(t, instanceEventsRulePath), "AWS::Events::Rule") {
		if got := props(res)["Name"]; got != "spawn-ttl-reaper-instance-terminated" {
			t.Errorf("member rule Name %v; the forwarder role trusts only spawn-ttl-reaper-instance-terminated", got)
		}
	}
}
