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
// The grant must name spored's role (read from pkg/aws, not written here), stay
// exact (no wildcard: a PassRole on role/* lets the orchestrator hand any role
// in the account to an instance), and stay limited to EC2.
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
		hasSpored := false
		for _, r := range res {
			if strings.Contains(r, "*") {
				t.Errorf("PassRole resource %q is a wildcard", r)
			}
			if r == sporedRole {
				hasSpored = true
			}
		}
		if !hasSpored {
			t.Errorf("PassRole resources %v do not include %s: every detached member's RunInstances is refused", res, sporedRole)
		}
		if got := st.Condition["StringEquals"]["iam:PassedToService"]; got != "ec2.amazonaws.com" {
			t.Errorf("PassRole is not limited to EC2 (iam:PassedToService = %q)", got)
		}
	}
	if passStatements == 0 {
		t.Error("the cross-account role grants no iam:PassRole at all")
	}
}
