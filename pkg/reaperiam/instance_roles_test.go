package reaperiam

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/instancerole"
)

// iamGlobMatches is IAM's Resource matching: '*' is any run of characters,
// '?' any one character, everything else literal.
func iamGlobMatches(pattern, arn string) bool {
	var re strings.Builder
	re.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			re.WriteString(".*")
		case '?':
			re.WriteString(".")
		default:
			re.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String()).MatchString(arn)
}

// isReadOnlyIAM: the IAM read verbs. Everything else an IAM action can do
// creates, changes or deletes an identity or what it may do.
func isReadOnlyIAM(action string) bool {
	verb := strings.TrimPrefix(action, "iam:")
	return strings.HasPrefix(verb, "Get") || strings.HasPrefix(verb, "List")
}

// TestIAMMutationsAreScopedToPerTaskRoles is #1300's IAM rail, over all three
// copies of the reaper's policy.
//
// The reaper may delete spawn-instance-<hash> roles and instance profiles and
// nothing else. So every mutating iam: action it holds must be on exactly the
// spawn-instance-* role or profile ARN, never '*', and conditioned on
// iam:ResourceTag/spawn:managed=true. The protected identities below must be
// matched by none of those resources, so IAM denies the reaper the delete
// even if its code chose wrongly. The mutating actions are discovered, not
// listed: a verb added later inherits the rule.
func TestIAMMutationsAreScopedToPerTaskRoles(t *testing.T) {
	roleARN := "arn:aws:iam::*:role/" + instancerole.NamePrefix + "*"
	profileARN := "arn:aws:iam::*:instance-profile/" + instancerole.NamePrefix + "*"
	if InstanceRoleARN != roleARN || InstanceProfileARN != profileARN {
		t.Fatalf("InstanceRoleARN/InstanceProfileARN (%s, %s) disagree with instancerole.NamePrefix %q",
			InstanceRoleARN, InstanceProfileARN, instancerole.NamePrefix)
	}
	wantCond := `{"StringEquals":{"iam:ResourceTag/spawn:managed":"true"}}`

	// Must be denied. The shared default identity, the reaper's own roles in
	// both deployment modes, the pool workers (pool drain owns those), and a
	// role a user made.
	const acct = "111122223333"
	protected := []string{
		"arn:aws:iam::" + acct + ":role/" + instancerole.SharedRoleName,
		"arn:aws:iam::" + acct + ":instance-profile/" + instancerole.SharedProfileName,
		"arn:aws:iam::" + acct + ":role/spawn-ttl-reaper-ec2",
		"arn:aws:iam::" + acct + ":role/spawn-ttl-reaper-selfscan-role",
		"arn:aws:iam::" + acct + ":role/spawn-pool-worker-0123456789abcdef",
		"arn:aws:iam::" + acct + ":role/my-research-role",
		"arn:aws:iam::" + acct + ":role/spawn-instance",
	}
	// Must be allowed, or the rail is a wall and the sweep never works.
	perTask := []string{
		"arn:aws:iam::" + acct + ":role/spawn-instance-af541989",
		"arn:aws:iam::" + acct + ":instance-profile/spawn-instance-af541989",
	}

	sources := map[string]map[grant]bool{
		"pkg/reaperiam":                 reaperIAMGrants(t),
		samTemplatePath + " (ScanSelf)": explode(t, samTemplatePath, samScanSelfStatements(t)),
		crossAccountRolePath:            explode(t, crossAccountRolePath, crossAccountRoleStatements(t)),
	}
	for where, grants := range sources {
		mutating := 0
		allowed := map[string]bool{}
		for g := range grants {
			if !strings.HasPrefix(g.Action, "iam:") || isReadOnlyIAM(g.Action) {
				continue
			}
			mutating++
			for _, res := range strings.Split(g.Resources, ",") {
				if res != roleARN && res != profileARN {
					t.Errorf("%s: %s is granted on %q; a mutating IAM action may only be granted on %s or %s",
						where, g.Action, res, roleARN, profileARN)
				}
				for _, p := range protected {
					if iamGlobMatches(res, p) {
						t.Errorf("%s: %s on %q matches %s, which the reaper must never change", where, g.Action, res, p)
					}
				}
				for _, a := range perTask {
					if iamGlobMatches(res, a) {
						allowed[a] = true
					}
				}
			}
			if g.Condition != wantCond {
				t.Errorf("%s: %s is conditioned on %q, want %s", where, g.Action, g.Condition, wantCond)
			}
		}
		if mutating == 0 {
			t.Errorf("%s grants no mutating iam: action; the role sweep could not delete anything", where)
		}
		for _, a := range perTask {
			if !allowed[a] {
				t.Errorf("%s: no mutating grant covers %s, so the sweep cannot delete it", where, a)
			}
		}
	}

	// The matcher is what the protected check rests on; prove it can say yes
	// and no, so a broken matcher cannot pass the check above vacuously.
	if !iamGlobMatches("arn:aws:iam::*:role/*", protected[0]) || iamGlobMatches(roleARN, protected[0]) {
		t.Fatal("iamGlobMatches is broken")
	}
}

// crossAccountRoleStatements returns the reaper role's statements from the
// cross-account template, as TestCrossAccountRolePolicyMatchesReaperIAM reads them.
func crossAccountRoleStatements(t *testing.T) []interface{} {
	t.Helper()
	tmpl := loadCFN(t, crossAccountRolePath)
	resources, _ := tmpl["Resources"].(map[string]interface{})
	res, _ := resources[crossAccountReaperRole].(map[string]interface{})
	props, _ := res["Properties"].(map[string]interface{})
	var stmts []interface{}
	for _, p := range asList(props["Policies"]) {
		pm, _ := p.(map[string]interface{})
		doc, _ := pm["PolicyDocument"].(map[string]interface{})
		stmts = append(stmts, asList(doc["Statement"])...)
	}
	if len(stmts) == 0 {
		t.Fatalf("no statements on %s in %s", crossAccountReaperRole, crossAccountRolePath)
	}
	return stmts
}

// The scan-self policy is one inline policy (`spawn reaper deploy`
// PutRolePolicy), and IAM caps a role's inline policies at 10,240 characters.
// The #1300 statements are the first IAM grants in it; keep it deployable.
func TestScanSelfPolicyFitsAnInlinePolicy(t *testing.T) {
	doc, err := ScanSelfPolicyDocument()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(doc); n > 10240 {
		t.Errorf("scan-self policy is %d characters; an inline role policy is limited to 10,240", n)
	} else {
		t.Logf("scan-self policy is %d characters of 10,240", n)
	}
}
