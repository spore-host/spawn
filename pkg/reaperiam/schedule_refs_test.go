package reaperiam

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/scheduler"
)

func scheduleGrants(t *testing.T) map[grant]bool {
	t.Helper()
	out := map[grant]bool{}
	for _, st := range ScheduleReferenceStatements() {
		cond := ""
		if st.Condition != nil {
			cond = canonicalJSON(t, st.Condition)
		}
		for _, a := range st.Action {
			out[grant{Effect: st.Effect, Action: a, Resources: st.Resource, Condition: cond}] = true
		}
	}
	return out
}

// looseGrants explodes statements like explode, but renders a Resource it
// cannot resolve (a !Ref to a parameter, a !Sub of something other than the
// account) as text instead of failing: the gates here only need to find the
// grants that touch schedule data among statements that are about other
// things. A schedule grant written that way will not equal the source and
// fails the comparison.
func looseGrants(t *testing.T, stmts []interface{}) map[grant]bool {
	t.Helper()
	out := map[grant]bool{}
	for _, raw := range stmts {
		st, _ := raw.(map[string]interface{})
		effect, _ := st["Effect"].(string)
		var res []string
		for _, r := range asList(st["Resource"]) {
			switch v := r.(type) {
			case string:
				res = append(res, v)
			case map[string]interface{}:
				if sub, ok := v["Fn::Sub"].(string); ok && len(v) == 1 {
					res = append(res, strings.ReplaceAll(sub, "${AWS::AccountId}", "*"))
					continue
				}
				res = append(res, fmt.Sprint(v))
			default:
				res = append(res, fmt.Sprint(v))
			}
		}
		sort.Strings(res)
		cond := ""
		if c, ok := st["Condition"]; ok && c != nil {
			cond = canonicalJSON(t, c)
		}
		for _, a := range asList(st["Action"]) {
			name, _ := a.(string)
			out[grant{Effect: effect, Action: name, Resources: strings.Join(res, ","), Condition: cond}] = true
		}
	}
	return out
}

// isScheduleGrant says whether a grant reads schedule data: any s3:GetObject,
// or anything on a spawn-schedules resource. Discovered by what it touches,
// not by statement position, so a second, broader copy cannot hide.
func isScheduleGrant(g grant) bool {
	return g.Action == "s3:GetObject" || strings.Contains(g.Resources, "spawn-schedules")
}

func sortedGrants(m map[grant]bool) []string {
	var out []string
	for g := range m {
		out = append(out, g.String())
	}
	sort.Strings(out)
	return out
}

// Gate (#1378): the schedule read is exactly a Scan of the schedules table and
// a GetObject under the params prefix, on the resources pkg/scheduler says the
// CLI writes, and nothing that writes.
func TestScheduleReadIsScopedToTheSchedules(t *testing.T) {
	wantTable := fmt.Sprintf("arn:aws:dynamodb:%s:*:table/%s", scheduler.TableRegion, scheduler.TableName)
	wantParams := "arn:aws:s3:::" + strings.ReplaceAll(scheduler.ParamsBucketTemplate, "%s", "*") + "/" + scheduler.ParamsKeyPrefix + "*"
	if SchedulesTableARN != wantTable {
		t.Errorf("SchedulesTableARN = %q, but the CLI writes schedules to %q", SchedulesTableARN, wantTable)
	}
	if ScheduleParamsARN != wantParams {
		t.Errorf("ScheduleParamsARN = %q, but the CLI writes params under %q", ScheduleParamsARN, wantParams)
	}
	want := map[grant]bool{
		{Effect: "Allow", Action: "dynamodb:Scan", Resources: wantTable}: true,
		{Effect: "Allow", Action: "s3:GetObject", Resources: wantParams}: true,
	}
	if got := scheduleGrants(t); strings.Join(sortedGrants(got), "|") != strings.Join(sortedGrants(want), "|") {
		t.Errorf("ScheduleReferenceStatements grants\n  %s\nwant exactly\n  %s",
			strings.Join(sortedGrants(got), "\n  "), strings.Join(sortedGrants(want), "\n  "))
	}
}

// Gate (#1378): the SAM template gives the function exactly that read, on its
// own credentials, unconditionally (the sweep reads the schedules in every
// mode it runs in), and no other grant anywhere in it touches schedule data.
func TestSAMTemplateGrantsTheScheduleRead(t *testing.T) {
	tmpl := loadCFN(t, samTemplatePath)
	resources, _ := tmpl["Resources"].(map[string]interface{})
	got := map[grant]bool{}
	conditional := map[grant]bool{}
	var fns int
	for _, raw := range resources {
		res, _ := raw.(map[string]interface{})
		if res["Type"] != "AWS::Serverless::Function" {
			continue
		}
		fns++
		props, _ := res["Properties"].(map[string]interface{})
		for _, p := range asList(props["Policies"]) {
			pm, _ := p.(map[string]interface{})
			if ifv, ok := pm["Fn::If"].([]interface{}); ok && len(ifv) == 3 {
				for _, arm := range ifv[1:] {
					body, _ := arm.(map[string]interface{})
					if stmts := asList(body["Statement"]); len(stmts) > 0 {
						for g := range looseGrants(t, stmts) {
							if isScheduleGrant(g) {
								conditional[g] = true
							}
						}
					}
				}
				continue
			}
			for g := range looseGrants(t, asList(pm["Statement"])) {
				if isScheduleGrant(g) {
					got[g] = true
				}
			}
		}
	}
	if fns == 0 {
		t.Fatalf("no function in %s", samTemplatePath)
	}
	for g := range conditional {
		t.Errorf("%s grants %s under an !If: the role sweep reads the schedules whenever it runs, so a "+
			"condition that drops the grant makes it delete nothing", samTemplatePath, g)
	}
	want := scheduleGrants(t)
	if strings.Join(sortedGrants(got), "|") != strings.Join(sortedGrants(want), "|") {
		t.Errorf("%s's schedule grants are\n  %s\nwant exactly ScheduleReferenceStatements:\n  %s",
			samTemplatePath, strings.Join(sortedGrants(got), "\n  "), strings.Join(sortedGrants(want), "\n  "))
	}
}

// Gate (#1378): the member accounts' role reads no schedule data (the
// schedules are in the reaper's own account, read on its base credentials),
// and the one role `spawn reaper deploy` creates carries the read.
func TestScheduleReadIsOnTheReapersOwnRoleOnly(t *testing.T) {
	tmpl := loadCFN(t, crossAccountRolePath)
	resources, _ := tmpl["Resources"].(map[string]interface{})
	for name, raw := range resources {
		res, _ := raw.(map[string]interface{})
		if res["Type"] != "AWS::IAM::Role" {
			continue
		}
		props, _ := res["Properties"].(map[string]interface{})
		for _, p := range asList(props["Policies"]) {
			pm, _ := p.(map[string]interface{})
			doc, _ := pm["PolicyDocument"].(map[string]interface{})
			for g := range looseGrants(t, asList(doc["Statement"])) {
				if isScheduleGrant(g) {
					t.Errorf("%s %s grants %s; schedule data is read on the reaper's own credentials", crossAccountRolePath, name, g)
				}
			}
		}
	}
	for g := range reaperIAMGrants(t) {
		if isScheduleGrant(g) {
			t.Errorf("ScanSelfStatements (also the member role's source) grants %s", g)
		}
	}

	doc, err := ScanSelfPolicyDocument()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct{ Statement []interface{} }
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatal(err)
	}
	got := map[grant]bool{}
	for g := range explode(t, "ScanSelfPolicyDocument", parsed.Statement) {
		if isScheduleGrant(g) {
			got[g] = true
		}
	}
	if strings.Join(sortedGrants(got), "|") != strings.Join(sortedGrants(scheduleGrants(t)), "|") {
		t.Errorf("ScanSelfPolicyDocument (spawn reaper deploy) schedule grants %v, want ScheduleReferenceStatements", sortedGrants(got))
	}
}
