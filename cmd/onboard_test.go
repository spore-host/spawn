package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// cmd/onboard.go had NO test file at all until #817, which is most of why four
// IAM defects sat in ~25 lines of it: it creates the role the hosted portal
// assumes inside a customer's BYOA account, so every grant here is a grant
// against someone else's infrastructure.
//
// These tests read the policy documents out of the source and assert on the
// parsed JSON rather than on substrings. A `strings.Contains(policy,
// "spawn:managed")` would be satisfied by any one statement carrying the
// condition — and the defect here was precisely that SOME statements carried it
// while the ones that mattered did not. That is CLAUDE.md rule 3: match the
// structure, not the name.

// policyFromSource extracts a backtick-quoted JSON policy assigned to the named
// variable in cmd/onboard.go, and parses it.
func policyFromSource(t *testing.T, varName string) policyDoc {
	t.Helper()
	src := filepath.Join(repoRootFromCmd(t), "cmd", "onboard.go")
	b, err := os.ReadFile(src) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	re := regexp.MustCompile("(?s)" + regexp.QuoteMeta(varName) + ` :?= (?:fmt\.Sprintf\()?` + "`" + `(\{.*?\})` + "`")
	m := re.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("could not find policy %q in cmd/onboard.go — the gate is reading the wrong "+
			"variable or the assignment changed shape", varName)
	}
	// Sprintf placeholders are not valid JSON values; substitute something inert.
	raw := strings.ReplaceAll(m[1], "%s", "PLACEHOLDER")

	var doc policyDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("policy %q does not parse as JSON: %v", varName, err)
	}
	if len(doc.Statement) == 0 {
		t.Fatalf("policy %q parsed to zero statements — the extraction is broken, not the policy", varName)
	}
	return doc
}

type policyDoc struct {
	Statement []policyStmt `json:"Statement"`
}

type policyStmt struct {
	Effect    string          `json:"Effect"`
	Action    json.RawMessage `json:"Action"`
	Resource  json.RawMessage `json:"Resource"`
	Condition map[string]map[string]interface{}
}

// actions normalises Action, which IAM allows as a string or a list.
func (s policyStmt) actions(t *testing.T) []string {
	t.Helper()
	return oneOrMany(t, s.Action)
}

func (s policyStmt) resources(t *testing.T) []string {
	t.Helper()
	return oneOrMany(t, s.Resource)
}

func oneOrMany(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		t.Fatalf("value is neither a string nor a list: %s", raw)
	}
	return many
}

// conditionKeys returns every condition key on the statement, across operators.
func (s policyStmt) conditionKeys() []string {
	var out []string
	for _, byKey := range s.Condition {
		for k := range byKey {
			out = append(out, k)
		}
	}
	return out
}

func (s policyStmt) hasConditionKey(want string) bool {
	for _, k := range s.conditionKeys() {
		if k == want {
			return true
		}
	}
	return false
}

// Every action that can reach an EXISTING instance must be conditioned.
//
// This is the property, stated once: an unconditioned grant on "*" for an action
// that mutates an instance makes the carefully-conditioned destructive grants
// beside it pointless, because the holder can bring any instance into scope
// first. Listing the actions rather than the statements means a future statement
// gains the requirement automatically.
func TestOnboardPolicyConditionsEveryInstanceMutatingAction(t *testing.T) {
	doc := policyFromSource(t, "launchPolicy")

	// RunInstances is create-only: there is no existing resource to condition on,
	// and the tag it applies is covered by the ec2:CreateAction grant below.
	mustBeConditioned := map[string]bool{
		"ec2:CreateTags":         true,
		"ec2:DeleteTags":         true,
		"ec2:TerminateInstances": true,
		"ec2:StopInstances":      true,
		"ec2:StartInstances":     true,
		"ssm:StartSession":       true,
		// #922: cancelling a persistent Spot request strands its stopped
		// instance, so it is as instance-affecting as StopInstances.
		"ec2:CancelSpotInstanceRequests": true,
	}

	seen := map[string]bool{}
	for _, st := range doc.Statement {
		for _, a := range st.actions(t) {
			if !mustBeConditioned[a] {
				continue
			}
			seen[a] = true
			if len(st.conditionKeys()) == 0 {
				// The SSM document grant is the one legitimate exception: a
				// document carries no spawn:managed tag, and StartSession
				// authorizes against the instance AND the document separately.
				if a == "ssm:StartSession" && onlyDocumentResources(t, st) {
					continue
				}
				t.Errorf("%s is granted with NO condition on %v — an unconditioned grant for an "+
					"action that can reach an existing instance defeats the tag-scoped grants "+
					"beside it", a, st.resources(t))
			}
		}
	}
	for a := range mustBeConditioned {
		if !seen[a] {
			t.Errorf("%s is not granted at all; if it was removed, drop it from this list — "+
				"otherwise the gate is silently checking nothing", a)
		}
	}
}

func onlyDocumentResources(t *testing.T, st policyStmt) bool {
	t.Helper()
	for _, r := range st.resources(t) {
		if !strings.Contains(r, ":document/") {
			return false
		}
	}
	return true
}

// ssm:StartSession on an instance must use the ssm: condition key, never ec2:.
//
// A condition key belongs to the service of the ACTION. ec2:ResourceTag is absent
// from the request context of an ssm:* call, so a StringEquals on it can never
// match and the statement becomes a silent implicit DENY — a policy that reads
// as working and grants nothing. That is #665, and it has shipped in this repo
// before.
func TestOnboardStartSessionUsesTheSSMConditionKey(t *testing.T) {
	doc := policyFromSource(t, "launchPolicy")

	checked := 0
	for _, st := range doc.Statement {
		isStart := false
		for _, a := range st.actions(t) {
			if a == "ssm:StartSession" {
				isStart = true
			}
		}
		if !isStart || onlyDocumentResources(t, st) {
			continue
		}
		checked++
		if st.hasConditionKey("ec2:ResourceTag/spawn:managed") {
			t.Error("ssm:StartSession is conditioned on ec2:ResourceTag, which is absent from an " +
				"ssm: request context — the statement can never match and is a silent implicit DENY (#665)")
		}
		if !st.hasConditionKey("ssm:resourceTag/spawn:managed") {
			t.Errorf("ssm:StartSession on %v is not conditioned on ssm:resourceTag/spawn:managed — "+
				"a portal compromise would yield an interactive shell on arbitrary non-spawn "+
				"instances in every onboarded account", st.resources(t))
		}
	}
	if checked == 0 {
		t.Fatal("found no instance-scoped ssm:StartSession statement — the gate is checking nothing")
	}
}

// iam:PassRole must name a ROLE, and must stay service-scoped.
//
// The resource was built from a variable defaulting to "spored-instance-profile"
// — an instance PROFILE name in a role ARN slot — so the grant matched nothing
// and the portal's RunInstances was denied. An IAM grant that cannot match is not
// a safe failure: it reads as working, and the feature it enables is simply
// broken (which is why this went unnoticed).
func TestOnboardPassRoleNamesARoleNotAProfile(t *testing.T) {
	root := repoRootFromCmd(t)
	b, err := os.ReadFile(filepath.Join(root, "cmd", "onboard.go")) //nolint:gosec // fixed path
	if err != nil {
		t.Fatalf("read onboard.go: %v", err)
	}
	src := string(b)

	if strings.Contains(src, `sporedRole = "spored-instance-profile"`) ||
		strings.Contains(src, `sporedProfile = "spored-instance-profile"`) {
		t.Error("the PassRole target defaults to an instance PROFILE name; iam:PassRole names a role")
	}
	if !strings.Contains(src, "sporedRole = spawnaws.SporedRoleName") {
		t.Errorf("the PassRole target does not default to spawnaws.SporedRoleName (%q) — "+
			"deriving it from the shared constant is what keeps it from drifting again",
			spawnaws.SporedRoleName)
	}
	if strings.Contains(spawnaws.SporedRoleName, "profile") {
		t.Errorf("spawnaws.SporedRoleName is %q, which names a profile — the constant itself is wrong",
			spawnaws.SporedRoleName)
	}

	doc := policyFromSource(t, "passPolicy")
	found := false
	for _, st := range doc.Statement {
		for _, a := range st.actions(t) {
			if a != "iam:PassRole" {
				continue
			}
			found = true
			for _, r := range st.resources(t) {
				if !strings.Contains(r, ":role/") {
					t.Errorf("iam:PassRole resource %q is not a role ARN", r)
				}
				if r == "*" || strings.HasSuffix(r, ":role/*") {
					t.Errorf("iam:PassRole resource %q is a wildcard — with ec2:RunInstances in the "+
						"same role this is escalation to any EC2-assumable role in the account", r)
				}
			}
			if !st.hasConditionKey("iam:PassedToService") {
				t.Error("iam:PassRole is not conditioned on iam:PassedToService")
			}
		}
	}
	if !found {
		t.Fatal("no iam:PassRole statement found — the gate is checking nothing")
	}
}

// The deprecated --spored-profile alias must keep working, because reinterpreting
// its value as a role name is the fix rather than a breaking change.
func TestOnboardSporedProfileFlagRemainsADeprecatedAlias(t *testing.T) {
	f := onboardCmd.Flags().Lookup("spored-profile")
	if f == nil {
		t.Fatal("--spored-profile was removed; it should remain as a deprecated alias so existing " +
			"invocations keep working — and they now work, since the value is finally used as a role")
	}
	if f.Deprecated == "" {
		t.Error("--spored-profile is not marked deprecated")
	}
	if onboardCmd.Flags().Lookup("spored-role") == nil {
		t.Error("--spored-role is not registered")
	}
}
