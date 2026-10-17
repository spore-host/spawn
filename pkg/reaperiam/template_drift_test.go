package reaperiam

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// samTemplatePath is the SAM template that deploys the operated reaper. With
// ScanSelf=true it attaches its own inline copy of the scan-self permissions, which
// is the copy #985 found had drifted from ScanSelfStatements: no #685
// network-resource grants, and ssm:SendCommand on Resource '*' with no condition.
// Nothing read it, so nothing noticed.
const samTemplatePath = "../../lambda/ttl-reaper/template.yaml"

// grant is one (effect, action, resources, condition) tuple. Statements are
// exploded per action so that grouping — two actions in one statement or in two —
// does not count as a difference, while every semantic property does.
type grant struct {
	Effect    string
	Action    string
	Resources string // sorted, comma-joined
	Condition string // canonical JSON, "" when unconditioned
}

func (g grant) String() string {
	s := fmt.Sprintf("%s %s on [%s]", g.Effect, g.Action, g.Resources)
	if g.Condition != "" {
		s += " if " + g.Condition
	}
	return s
}

// reaperIAMGrants is the source of truth, exploded.
func reaperIAMGrants(t *testing.T) map[grant]bool {
	t.Helper()
	out := map[grant]bool{}
	for _, st := range ScanSelfStatements() {
		cond := ""
		if st.Condition != nil {
			cond = canonicalJSON(t, st.Condition)
		}
		for _, a := range st.Action {
			out[grant{Effect: st.Effect, Action: a, Resources: st.Resource, Condition: cond}] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("ScanSelfStatements produced no grants — the comparison would be vacuous")
	}
	return out
}

// canonicalJSON re-marshals v through interface{}, so a map[string]map[string]string
// from Go and a map[string]interface{} decoded from YAML render identically.
func canonicalJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	var generic interface{} // nosemgrep: go.lang.security.deserialization.unsafe-deserialization-interface.go-unsafe-deserialization-interface -- test-only canonicalisation of JSON this test itself marshalled from the policy structs; no untrusted input
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, err = json.Marshal(generic)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// cfnValue converts a yaml.v3 node into plain Go values, rendering CloudFormation
// short-form intrinsics (!If, !Sub, !Ref, …) as their long-form single-key maps.
// A plain yaml.Unmarshal into interface{} would refuse nothing but silently drop
// the tag, so `!Sub 'arn:…${AWS::AccountId}…'` and the literal string would
// compare equal — which is exactly the kind of difference this gate must see.
func cfnValue(n *yaml.Node) interface{} {
	switch n.Kind {
	case yaml.DocumentNode:
		return cfnValue(n.Content[0])
	case yaml.AliasNode:
		return cfnValue(n.Alias)
	}
	var v interface{}
	switch n.Kind {
	case yaml.MappingNode:
		m := map[string]interface{}{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			m[n.Content[i].Value] = cfnValue(n.Content[i+1])
		}
		v = m
	case yaml.SequenceNode:
		s := []interface{}{}
		for _, c := range n.Content {
			s = append(s, cfnValue(c))
		}
		v = s
	default:
		v = n.Value
	}
	if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
		name := strings.TrimPrefix(n.Tag, "!")
		if name != "Ref" && name != "Condition" {
			name = "Fn::" + name
		}
		return map[string]interface{}{name: v}
	}
	return v
}

func loadCFN(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	m, ok := cfnValue(&doc).(map[string]interface{})
	if !ok {
		t.Fatalf("%s is not a YAML mapping", path)
	}
	return m
}

// asList accepts IAM's string-or-list shorthand.
func asList(v interface{}) []interface{} {
	if l, ok := v.([]interface{}); ok {
		return l
	}
	if v == nil {
		return nil
	}
	return []interface{}{v}
}

// resolveResource turns a template Resource entry into the string reaperiam would
// write. Only one substitution is accepted, ${AWS::AccountId} → '*': reaperiam's
// policy is rendered without knowing the account, and an ARN pinned to the policy's
// own account grants nothing more than one with '*' there, because credentials
// cannot act on another account's resources without that account's own policy.
// Any other intrinsic is a value this gate cannot compare, so it fails.
func resolveResource(t *testing.T, where string, v interface{}) string {
	t.Helper()
	switch r := v.(type) {
	case string:
		return r
	case map[string]interface{}:
		if sub, ok := r["Fn::Sub"].(string); ok && len(r) == 1 {
			out := strings.ReplaceAll(sub, "${AWS::AccountId}", "*")
			if strings.Contains(out, "${") {
				t.Errorf("%s: Resource %q substitutes something other than ${AWS::AccountId}; "+
					"the gate cannot compare it with reaperiam", where, sub)
			}
			return out
		}
	}
	t.Errorf("%s: Resource %v is not a string or a !Sub — the gate cannot compare it", where, v)
	return fmt.Sprint(v)
}

// explode turns a list of parsed IAM statements into grants, failing on anything
// the comparison cannot represent rather than skipping it.
func explode(t *testing.T, where string, stmts []interface{}) map[grant]bool {
	t.Helper()
	out := map[grant]bool{}
	for i, raw := range stmts {
		st, ok := raw.(map[string]interface{})
		if !ok {
			t.Errorf("%s statement %d is not a mapping: %v", where, i, raw)
			continue
		}
		for k := range st {
			switch k {
			case "Effect", "Action", "Resource", "Condition", "Sid":
			default:
				t.Errorf("%s statement %d has %q, which this gate does not compare — extend it "+
					"rather than let the element pass unexamined", where, i, k)
			}
		}
		effect, _ := st["Effect"].(string)
		var res []string
		for _, r := range asList(st["Resource"]) {
			res = append(res, resolveResource(t, fmt.Sprintf("%s statement %d", where, i), r))
		}
		sort.Strings(res)
		cond := ""
		if c, ok := st["Condition"]; ok && c != nil {
			cond = canonicalJSON(t, c)
		}
		actions := asList(st["Action"])
		if len(actions) == 0 {
			t.Errorf("%s statement %d grants no Action", where, i)
		}
		for _, a := range actions {
			name, ok := a.(string)
			if !ok {
				t.Errorf("%s statement %d has a non-string action %v", where, i, a)
				continue
			}
			out[grant{Effect: effect, Action: name, Resources: strings.Join(res, ","), Condition: cond}] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: parsed no grants — the comparison would pass vacuously", where)
	}
	return out
}

// diffGrants reports both directions. exceptions maps a grant reaperiam has to the
// grant the other policy holds instead, each with its reason.
func diffGrants(t *testing.T, where string, got map[grant]bool, exceptions map[grant]grant) {
	t.Helper()
	want := reaperIAMGrants(t)
	for from, to := range exceptions {
		if !want[from] {
			t.Errorf("exception for %s names a grant reaperiam no longer has — remove it", from)
			continue
		}
		if !got[to] {
			t.Errorf("exception for %s expects %s in %s, which is not there — remove the exception "+
				"if the two now agree", from, to, where)
		}
		delete(want, from)
		want[to] = true
	}

	var missing, extra []string
	for g := range want {
		if !got[g] {
			missing = append(missing, g.String())
		}
	}
	for g := range got {
		if !want[g] {
			extra = append(extra, g.String())
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s lacks grants pkg/reaperiam.ScanSelfStatements has:\n  %s\n"+
			"A missing grant fails silently: the reaper skips whatever that call was for.",
			where, strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("%s has grants pkg/reaperiam.ScanSelfStatements does not:\n  %s\n"+
			"Either reaperiam is missing them or this copy is broader than the reaper needs. A "+
			"different resource or condition counts — #985 was ssm:SendCommand on '*' with no "+
			"condition, which is root shell on every SSM-managed instance in the account.",
			where, strings.Join(extra, "\n  "))
	}
}

// samScanSelfStatements finds the template's ScanSelf branch by discovery rather
// than by name: the Condition whose definition is `!Equals [!Ref ScanSelf, 'true']`,
// then every Policies entry that is an !If on it.
func samScanSelfStatements(t *testing.T) []interface{} {
	t.Helper()
	tmpl := loadCFN(t, samTemplatePath)

	conds, _ := tmpl["Conditions"].(map[string]interface{})
	var condName string
	for name, def := range conds {
		eq, ok := def.(map[string]interface{})["Fn::Equals"].([]interface{})
		if !ok || len(eq) != 2 {
			continue
		}
		ref, _ := eq[0].(map[string]interface{})
		if ref["Ref"] == "ScanSelf" && eq[1] == "true" {
			if condName != "" {
				t.Fatalf("two Conditions test ScanSelf=true (%s, %s)", condName, name)
			}
			condName = name
		}
	}
	if condName == "" {
		t.Fatalf("%s has no Condition testing ScanSelf=true — has the template's shape changed?", samTemplatePath)
	}

	resources, _ := tmpl["Resources"].(map[string]interface{})
	var stmts []interface{}
	var branches int
	for rname, raw := range resources {
		res, _ := raw.(map[string]interface{})
		if res["Type"] != "AWS::Serverless::Function" {
			continue
		}
		props, _ := res["Properties"].(map[string]interface{})
		for _, p := range asList(props["Policies"]) {
			pm, _ := p.(map[string]interface{})
			ifv, ok := pm["Fn::If"].([]interface{})
			if !ok || len(ifv) != 3 || ifv[0] != condName {
				// Not the ScanSelf branch. It must not grant the scan-self services
				// either, or a statement moved out of the branch would escape this gate.
				checkNoScanSelfActionsOutsideBranch(t, rname, p)
				continue
			}
			branches++
			body, _ := ifv[1].(map[string]interface{})
			stmts = append(stmts, asList(body["Statement"])...)
			checkNoScanSelfActionsOutsideBranch(t, rname, ifv[2])
		}
	}
	if branches != 1 {
		t.Fatalf("found %d Policies entries conditioned on %s, want exactly 1", branches, condName)
	}
	if len(stmts) == 0 {
		t.Fatalf("the %s branch of %s parsed to zero statements — the gate would pass vacuously",
			condName, samTemplatePath)
	}
	return stmts
}

// checkNoScanSelfActionsOutsideBranch fails on an ec2:/fsx:/ssm: action anywhere in
// v. Those services belong to the ScanSelf branch only; the rest of the function's
// policies are sts, route53, dynamodb and kms on the base credentials.
func checkNoScanSelfActionsOutsideBranch(t *testing.T, resource string, v interface{}) {
	t.Helper()
	b, _ := json.Marshal(v)
	var walk func(interface{})
	walk = func(x interface{}) {
		switch y := x.(type) {
		case map[string]interface{}:
			for k, c := range y {
				if k == "Action" {
					for _, a := range asList(c) {
						s, _ := a.(string)
						for _, svc := range []string{"ec2:", "fsx:", "ssm:"} {
							if strings.HasPrefix(s, svc) {
								t.Errorf("%s grants %s outside the ScanSelf branch, where the "+
									"reaperiam comparison does not see it", resource, s)
							}
						}
					}
				}
				walk(c)
			}
		case []interface{}:
			for _, c := range y {
				walk(c)
			}
		}
	}
	var generic interface{} // nosemgrep: go.lang.security.deserialization.unsafe-deserialization-interface.go-unsafe-deserialization-interface -- test-only canonicalisation of JSON this test itself marshalled from the policy structs; no untrusted input
	_ = json.Unmarshal(b, &generic)
	walk(generic)
}

// TestSAMTemplateScanSelfPolicyMatchesReaperIAM is #985.
//
// The SAM template's ScanSelf policy must equal ScanSelfStatements, statement for
// statement after exploding by action: the same actions, on the same resources,
// under the same conditions. Action names alone were not enough — the template
// granted ssm:SendCommand by name all along, on '*' with no condition.
func TestSAMTemplateScanSelfPolicyMatchesReaperIAM(t *testing.T) {
	got := explode(t, samTemplatePath+" (ScanSelf branch)", samScanSelfStatements(t))
	diffGrants(t, samTemplatePath+" (ScanSelf branch)", got, nil)
	t.Logf("compared %d grants", len(got))
}

// crossAccountExceptions are the deliberate differences between the cross-account
// role and ScanSelfStatements. Each is a place where the cross-account role is
// NARROWER, never broader.
var crossAccountExceptions = map[grant]grant{
	// fsx:DeleteFileSystem is conditioned on aws:ResourceTag in the cross-account
	// role and unconditioned in scan-self. ScanSelfStatements explains why scan-self
	// does not adopt it: IAM evaluates the global key, but FSx populating it at
	// request time is unproven, and an unpopulated key fails closed (deletes denied,
	// filesystems leak). Cross-account already carries that dependency; #613 resolves
	// it with a real ephemeral-FSx reap, after which the two should agree.
	{Effect: "Allow", Action: "fsx:DeleteFileSystem", Resources: "*"}: {
		Effect: "Allow", Action: "fsx:DeleteFileSystem", Resources: "*",
		Condition: `{"StringEquals":{"aws:ResourceTag/spawn:managed":"true"}}`,
	},
}

// TestCrossAccountRolePolicyMatchesReaperIAM holds the production role (ScanSelf
// is false in production, so this is the policy the operated reaper actually runs
// under) to the same full comparison.
func TestCrossAccountRolePolicyMatchesReaperIAM(t *testing.T) {
	tmpl := loadCFN(t, crossAccountRolePath)
	resources, _ := tmpl["Resources"].(map[string]interface{})
	var stmts []interface{}
	var roles int
	for _, raw := range resources {
		res, _ := raw.(map[string]interface{})
		if res["Type"] != "AWS::IAM::Role" {
			continue
		}
		roles++
		props, _ := res["Properties"].(map[string]interface{})
		for _, p := range asList(props["Policies"]) {
			pm, _ := p.(map[string]interface{})
			doc, _ := pm["PolicyDocument"].(map[string]interface{})
			stmts = append(stmts, asList(doc["Statement"])...)
		}
	}
	if roles != 1 {
		t.Fatalf("found %d AWS::IAM::Role resources in %s, want 1", roles, crossAccountRolePath)
	}
	got := explode(t, crossAccountRolePath, stmts)
	diffGrants(t, crossAccountRolePath, got, crossAccountExceptions)
	t.Logf("compared %d grants", len(got))
}
