package scripts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestSweepCrossAccountRoleMayPassSporedsRole is #1323's IAM gate.
//
// The sweep-orchestrator Lambda launches every detached sweep member with an
// instance profile, because spored needs its credentials, and the CLI seeds
// spored-instance-profile for every member without an iam_role. RunInstances
// with a profile needs iam:PassRole on the profile's role, granted to the role
// the Lambda assumes in the member account, SpawnSweepCrossAccountRole. It used
// to name only spawnd-role, so every member would have been refused.
//
// The grant must name spored's role (read from pkg/aws, not written here) and
// the spawn-instance-* roles spawn creates for --iam-policy and friends (#1331,
// the prefix read from pkg/instancerole), stay otherwise exact (no other
// wildcard: a PassRole on role/* lets the orchestrator hand any role in the
// account to an instance), and stay limited to EC2.
func TestSweepCrossAccountRoleMayPassSporedsRole(t *testing.T) {
	src, err := os.ReadFile("setup-sweep-cross-account-role.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)POLICY_DOCUMENT=\$\(cat <<EOF\n(.*?)\nEOF\n\)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("found no POLICY_DOCUMENT heredoc; the matcher is stale")
	}
	doc := strings.ReplaceAll(string(m[1]), "${CURRENT_ACCOUNT_ID}", "111122223333")

	var policy struct {
		Statement []struct {
			Effect    string
			Action    interface{}
			Resource  interface{}
			Condition map[string]map[string]string
		}
	}
	if err := json.Unmarshal([]byte(doc), &policy); err != nil {
		t.Fatalf("the policy document is not JSON: %v\n%s", err, doc)
	}

	iamGo, err := os.ReadFile(filepath.Join(repoRoot(t), "pkg", "aws", "iam.go"))
	if err != nil {
		t.Fatal(err)
	}
	rm := regexp.MustCompile(`(?m)^\s*SporedRoleName\s*=\s*"([^"]+)"`).FindSubmatch(iamGo)
	if rm == nil {
		t.Fatal("cannot read SporedRoleName from pkg/aws/iam.go")
	}
	sporedRole := "arn:aws:iam::111122223333:role/" + string(rm[1])
	irGo, err := os.ReadFile(filepath.Join(repoRoot(t), "pkg", "instancerole", "instancerole.go"))
	if err != nil {
		t.Fatal(err)
	}
	pm := regexp.MustCompile(`(?m)^\s*NamePrefix\s*=\s*"([^"]+)"`).FindSubmatch(irGo)
	if pm == nil {
		t.Fatal("cannot read NamePrefix from pkg/instancerole/instancerole.go")
	}
	perPolicy := "arn:aws:iam::111122223333:role/" + string(pm[1]) + "*"

	list := func(v interface{}) []string {
		switch x := v.(type) {
		case string:
			return []string{x}
		case []interface{}:
			var out []string
			for _, e := range x {
				s, _ := e.(string)
				out = append(out, s)
			}
			return out
		}
		return nil
	}

	var passStatements int
	for _, st := range policy.Statement {
		passes := false
		for _, a := range list(st.Action) {
			if a == "iam:PassRole" || a == "iam:*" || a == "*" {
				passes = true
			}
		}
		if !passes || st.Effect != "Allow" {
			continue
		}
		passStatements++
		res := list(st.Resource)
		sort.Strings(res)
		hasSpored, hasPerPolicy := false, false
		for _, r := range res {
			if r == perPolicy {
				hasPerPolicy = true
				continue
			}
			if strings.Contains(r, "*") {
				t.Errorf("PassRole resource %q is a wildcard other than spawn's %s", r, perPolicy)
			}
			if r == sporedRole {
				hasSpored = true
			}
		}
		if !hasSpored {
			t.Errorf("PassRole resources %v do not include %s: every detached member's RunInstances is refused", res, sporedRole)
		}
		if !hasPerPolicy {
			t.Errorf("PassRole resources %v do not include %s: a detached sweep with --iam-policy has every member refused (#1331)", res, perPolicy)
		}
		if got := st.Condition["StringEquals"]["iam:PassedToService"]; got != "ec2.amazonaws.com" {
			t.Errorf("PassRole is not limited to EC2 (iam:PassedToService = %q)", got)
		}
	}
	if passStatements == 0 {
		t.Error("the cross-account role grants no iam:PassRole at all")
	}
}

// crossAccountPolicy is SpawnSweepCrossAccountRole's policy document as the
// script writes it, for an example account.
func crossAccountPolicy(t *testing.T) (string, []map[string]interface{}) {
	t.Helper()
	src, err := os.ReadFile("setup-sweep-cross-account-role.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)POLICY_DOCUMENT=\$\(cat <<EOF\n(.*?)\nEOF\n\)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("found no POLICY_DOCUMENT heredoc; the matcher is stale")
	}
	doc := strings.ReplaceAll(string(m[1]), "${CURRENT_ACCOUNT_ID}", "111122223333")
	var policy struct{ Statement []map[string]interface{} }
	if err := json.Unmarshal([]byte(doc), &policy); err != nil {
		t.Fatalf("the policy document is not JSON: %v\n%s", err, doc)
	}
	return doc, policy.Statement
}

// TestSweepCrossAccountRoleGrantsTheEgressProxy is #1515 PR B's golden: the
// statements that let the sweep orchestrator re-create a detached sweep's
// egress proxy in the member account, exactly. Each is scoped as the
// nested-spawn policy scopes the same actions: the groups by spawn:managed
// (a request tag at creation, ec2:ResourceTag after), the probe to
// spawn:managed proxies (ssm:resourceTag, the key of the action's service)
// and the one shell document. RunInstances, CreateTags, TerminateInstances,
// the Describes and the PassRole of spored-instance-role were granted before
// and are not repeated. That every call the orchestrator makes is granted is
// internal/iamcoverage's TestEveryPrincipalCallIsGranted.
func TestSweepCrossAccountRoleGrantsTheEgressProxy(t *testing.T) {
	_, statements := crossAccountPolicy(t)
	golden := `[
 {"Sid":"EgressProxyCreateManagedGroups","Effect":"Allow","Action":"ec2:CreateSecurityGroup",
  "Resource":"arn:aws:ec2:*:111122223333:security-group/*",
  "Condition":{"StringEquals":{"aws:RequestTag/spawn:managed":"true"}}},
 {"Sid":"EgressProxyCreateGroupsInVPC","Effect":"Allow","Action":"ec2:CreateSecurityGroup",
  "Resource":"arn:aws:ec2:*:111122223333:vpc/*"},
 {"Sid":"EgressProxyManageManagedGroupRules","Effect":"Allow",
  "Action":["ec2:AuthorizeSecurityGroupIngress","ec2:AuthorizeSecurityGroupEgress","ec2:RevokeSecurityGroupIngress","ec2:RevokeSecurityGroupEgress"],
  "Resource":"arn:aws:ec2:*:111122223333:security-group/*",
  "Condition":{"StringEquals":{"ec2:ResourceTag/spawn:managed":"true"}}},
 {"Sid":"EgressProxyProbeProxies","Effect":"Allow","Action":"ssm:SendCommand",
  "Resource":"arn:aws:ec2:*:111122223333:instance/*",
  "Condition":{"StringEquals":{"ssm:resourceTag/spawn:managed":"true"},"StringLike":{"ssm:resourceTag/spawn:egress-proxy":"vpc-*"}}},
 {"Sid":"EgressProxyProbeDocument","Effect":"Allow","Action":"ssm:SendCommand",
  "Resource":"arn:aws:ssm:*::document/AWS-RunShellScript"},
 {"Sid":"EgressProxyProbeResult","Effect":"Allow","Action":"ssm:GetCommandInvocation","Resource":"*"}
]`
	var want []map[string]interface{}
	if err := json.Unmarshal([]byte(golden), &want); err != nil {
		t.Fatal(err)
	}
	var got []map[string]interface{}
	for _, st := range statements {
		if sid, _ := st["Sid"].(string); strings.HasPrefix(sid, "EgressProxy") {
			got = append(got, st)
		}
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("the egress proxy's statements:\n%s\nwant exactly:\n%s", g, w)
	}

	// The traps this grant must not fall into: a policy variable in a
	// Resource fails PutRolePolicy (#1069), and a condition key of another
	// service than the action's never matches.
	for _, st := range statements {
		res, _ := json.Marshal(st["Resource"])
		if strings.Contains(string(res), "${") {
			t.Errorf("statement %v: a policy variable in Resource fails PutRolePolicy (#1069)", st["Sid"])
		}
		cond, _ := st["Condition"].(map[string]interface{})
		actions := map[string]bool{}
		switch a := st["Action"].(type) {
		case string:
			actions[strings.SplitN(a, ":", 2)[0]] = true
		case []interface{}:
			for _, x := range a {
				s, _ := x.(string)
				actions[strings.SplitN(s, ":", 2)[0]] = true
			}
		}
		for _, op := range cond {
			keys, _ := op.(map[string]interface{})
			for k := range keys {
				svc := strings.SplitN(k, ":", 2)[0]
				if svc != "aws" && !actions[svc] {
					t.Errorf("statement %v: condition key %s is not of its actions' service %v: it never matches", st["Sid"], k, actions)
				}
			}
		}
	}
}
