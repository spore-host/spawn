package aws

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/testutil"
)

// #661: spored releases the launch Elastic IP spawn allocated for it, and no
// other address. These pin the exact statements in every spored policy, and then
// evaluate each whole policy against requests the instance could make, so a
// broader grant added anywhere else (an unconditioned ReleaseAddress, a CreateTags
// that can forge the ARN tag) fails here rather than reading as unrelated.

var wantOwnEIPStatements = []map[string]interface{}{
	{
		"Sid":      "DescribeElasticIPs",
		"Effect":   "Allow",
		"Action":   "ec2:DescribeAddresses",
		"Resource": "*",
	},
	{
		"Sid":      "ReleaseOnlyOwnElasticIP",
		"Effect":   "Allow",
		"Action":   "ec2:ReleaseAddress",
		"Resource": "*",
		"Condition": map[string]interface{}{
			"StringEquals": map[string]interface{}{
				"ec2:ResourceTag/spawn:managed":      "true",
				"ec2:ResourceTag/spawn:instance-arn": "${ec2:SourceInstanceARN}",
			},
		},
	},
}

type policyStatement = map[string]interface{}

func parsePolicy(t *testing.T, what, doc string) []policyStatement {
	t.Helper()
	var parsed struct{ Statement []policyStatement }
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", what, err, doc)
	}
	if len(parsed.Statement) == 0 {
		t.Fatalf("%s has no statements", what)
	}
	return parsed.Statement
}

// sporedPolicies is every policy document a spored instance role is given.
func sporedPolicies(t *testing.T) map[string]string {
	t.Helper()
	inline, err := json.Marshal((&Client{}).buildInlinePolicy([]string{"s3:ReadOnly"}))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"dynamic baseline (CreateOrGetInstanceProfile)": (&Client{}).sporedBaselinePolicyDoc(),
		"static shared/DCV role (SetupSporedIAMRole)":   sporedDCVRolePolicy,
		"inline --iam-policy role":                      string(inline),
	}
}

func ownEIPStatementsIn(t *testing.T, what, doc string) {
	t.Helper()
	bySid := map[string][]policyStatement{}
	for _, st := range parsePolicy(t, what, doc) {
		if sid, _ := st["Sid"].(string); sid != "" {
			bySid[sid] = append(bySid[sid], st)
		}
	}
	for _, want := range wantOwnEIPStatements {
		sid := want["Sid"].(string)
		got := bySid[sid]
		if len(got) != 1 {
			t.Errorf("%s carries %d %s statements, want exactly 1", what, len(got), sid)
			continue
		}
		// Round-trip the expectation so both sides are JSON-shaped.
		b, _ := json.Marshal(want)
		var w policyStatement
		_ = json.Unmarshal(b, &w)
		if !reflect.DeepEqual(got[0], w) {
			gb, _ := json.Marshal(got[0])
			t.Errorf("%s: %s is not the reviewed form:\n  got  %s\n  want %s", what, sid, gb, b)
		}
	}
}

func TestEverySporedPolicyCarriesTheOwnElasticIPStatements(t *testing.T) {
	for what, doc := range sporedPolicies(t) {
		ownEIPStatementsIn(t, what, doc)
	}
}

// And as stored: what each writer actually sends to IAM, read back.
func TestTheOwnElasticIPStatementsReachIAM(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	c := NewClientFromConfig(env.AWSConfig)
	ic := env.IAMClient()
	if _, err := c.SetupSporedIAMRole(ctx); err != nil {
		t.Fatalf("SetupSporedIAMRole: %v", err)
	}
	ownEIPStatementsIn(t, "stored spored-policy", storedPolicy(t, ic, SporedRoleName, "spored-policy"))
	if _, err := c.CreateOrGetInstanceProfile(ctx, IAMRoleConfig{RoleName: "spawn-test-661", Policies: []string{"s3:ReadOnly"}}); err != nil {
		t.Fatalf("CreateOrGetInstanceProfile: %v", err)
	}
	ownEIPStatementsIn(t, "stored spored-baseline-policy", storedPolicy(t, ic, "spawn-test-661", "spored-baseline-policy"))
}

// ---- a small evaluator, for the "can it release what it does not own" half ----
//
// Not IAM: just enough of identity-policy evaluation to ask the question
// faithfully for these policies. Explicit Deny wins; otherwise any matching
// Allow allows. A condition operator it does not know fails the test rather
// than being skipped, so a policy cannot pass by using an operator this ignores.

type evalRequest struct {
	action   string
	resource string
	single   map[string]string   // single-valued context keys (lower-cased)
	multi    map[string][]string // multi-valued context keys (lower-cased)
}

func globMatch(pattern, s string) bool {
	re := "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern)) + "$"
	return regexp.MustCompile("(?i)" + re).MatchString(s)
}

func asStrings(v interface{}) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, e.(string))
		}
		return out
	}
	return nil
}

var policyVar = regexp.MustCompile(`\$\{([^}]+)\}`)

// substitute resolves ${key} from the request; ok is false when a variable has
// no value, in which case IAM's StringEquals/StringLike/Arn* do not match.
func substitute(v string, r evalRequest) (string, bool) {
	ok := true
	out := policyVar.ReplaceAllStringFunc(v, func(m string) string {
		key := strings.ToLower(policyVar.FindStringSubmatch(m)[1])
		val, present := r.single[key]
		if !present {
			ok = false
		}
		return val
	})
	return out, ok
}

func conditionHolds(t *testing.T, cond map[string]interface{}, r evalRequest) bool {
	t.Helper()
	for op, body := range cond {
		for key, raw := range body.(map[string]interface{}) {
			lk := strings.ToLower(key)
			want := asStrings(raw)
			switch op {
			case "StringEquals", "StringLike", "ArnEquals", "ArnLike":
				got, present := r.single[lk]
				if !present {
					return false
				}
				matched := false
				for _, w := range want {
					v, ok := substitute(w, r)
					if !ok {
						continue
					}
					if op == "StringEquals" && got == v || op == "ArnEquals" && got == v ||
						(op == "StringLike" || op == "ArnLike") && globMatch(v, got) {
						matched = true
					}
				}
				if !matched {
					return false
				}
			case "ForAnyValue:StringEquals":
				any := false
				for _, g := range r.multi[lk] {
					for _, w := range want {
						if v, ok := substitute(w, r); ok && g == v {
							any = true
						}
					}
				}
				if !any {
					return false
				}
			default:
				t.Fatalf("condition operator %q is not modelled by this evaluator; extend it rather than skip it", op)
			}
		}
	}
	return true
}

func statementMatches(t *testing.T, st policyStatement, r evalRequest) bool {
	t.Helper()
	if _, ok := st["NotAction"]; ok {
		t.Fatalf("NotAction is not modelled by this evaluator: %v", st)
	}
	if _, ok := st["NotResource"]; ok {
		t.Fatalf("NotResource is not modelled by this evaluator: %v", st)
	}
	actionOK := false
	for _, a := range asStrings(st["Action"]) {
		if globMatch(a, r.action) {
			actionOK = true
		}
	}
	if !actionOK {
		return false
	}
	resourceOK := false
	for _, res := range asStrings(st["Resource"]) {
		if globMatch(res, r.resource) {
			resourceOK = true
		}
	}
	if !resourceOK {
		return false
	}
	if c, ok := st["Condition"].(map[string]interface{}); ok {
		return conditionHolds(t, c, r)
	}
	return true
}

func policyAllows(t *testing.T, stmts []policyStatement, r evalRequest) bool {
	t.Helper()
	allowed := false
	for _, st := range stmts {
		if !statementMatches(t, st, r) {
			continue
		}
		if st["Effect"] == "Deny" {
			return false
		}
		allowed = true
	}
	return allowed
}

const (
	selfARN  = "arn:aws:ec2:us-west-2:111122223333:instance/i-self"
	otherARN = "arn:aws:ec2:us-west-2:111122223333:instance/i-other"
	eipARN   = "arn:aws:ec2:us-west-2:111122223333:elastic-ip/eipalloc-1"
)

// addressRequest is an instance-role request on an address carrying tags.
func addressRequest(action, sourceARN string, tags map[string]string, tagKeys ...string) evalRequest {
	r := evalRequest{action: action, resource: eipARN, single: map[string]string{}, multi: map[string][]string{}}
	if sourceARN != "" {
		r.single["ec2:sourceinstancearn"] = sourceARN
	}
	for k, v := range tags {
		r.single[strings.ToLower("ec2:ResourceTag/"+k)] = v
		r.single[strings.ToLower("aws:ResourceTag/"+k)] = v
	}
	if len(tagKeys) > 0 {
		r.multi["aws:tagkeys"] = tagKeys
	}
	return r
}

func TestSporedCanReleaseOnlyItsOwnElasticIP(t *testing.T) {
	own := map[string]string{"spawn:managed": "true", "spawn:instance-id": "i-self", "spawn:instance-arn": selfARN, "spawn:purpose": "efa-public-ip"}
	others := map[string]string{"spawn:managed": "true", "spawn:instance-id": "i-other", "spawn:instance-arn": otherARN, "spawn:purpose": "efa-public-ip"}
	cases := []struct {
		name   string
		req    evalRequest
		allow  bool
		reason string
	}{
		{"its own address", addressRequest("ec2:ReleaseAddress", selfARN, own), true,
			"spored could not release its own address, so every non-`spawn terminate` ending leaks it"},
		{"another instance's address", addressRequest("ec2:ReleaseAddress", selfARN, others), false,
			"one spore could release every other spore's public IP"},
		{"a user's address (no spawn tags)", addressRequest("ec2:ReleaseAddress", selfARN, map[string]string{"Name": "prod-nat"}), false,
			"a spore could release a user's Elastic IP (#262)"},
		{"a spawn address with no ARN tag", addressRequest("ec2:ReleaseAddress", selfARN, map[string]string{"spawn:managed": "true", "spawn:instance-id": "i-self"}), false,
			"the instance-id tag alone cannot be compared with the caller, so it must not suffice"},
		{"its ARN but not spawn-managed", addressRequest("ec2:ReleaseAddress", selfARN, map[string]string{"spawn:instance-arn": selfARN}), false,
			"the grant must stay on the spawn:managed rail"},
		{"credentials used off the instance (no SourceInstanceARN)", addressRequest("ec2:ReleaseAddress", "", own), false,
			"an unresolved policy variable must not match"},
		{"forging its ARN onto another's address", addressRequest("ec2:CreateTags", selfARN, others, "spawn:instance-arn"), false,
			"CreateTags reaches spawn:managed addresses, so the ARN tag is forgeable and the release scope is void"},
		{"repointing another's address by instance id", addressRequest("ec2:CreateTags", selfARN, others, "Name", "spawn:instance-id"), false,
			"the reaper and `spawn terminate` select addresses by spawn:instance-id"},
		{"deleting the ARN tag from another's address", addressRequest("ec2:DeleteTags", selfARN, others, "spawn:instance-arn"), false,
			"the instance must not edit which instance an address belongs to"},
	}
	for what, doc := range sporedPolicies(t) {
		stmts := parsePolicy(t, what, doc)
		for _, c := range cases {
			if got := policyAllows(t, stmts, c.req); got != c.allow {
				t.Errorf("%s: %s: allowed=%v, want %v — %s", what, c.name, got, c.allow, c.reason)
			}
		}
	}
}

// The evaluator itself must be able to say yes and no, or the cases above prove
// nothing: an always-deny evaluator passes every "false" case.
func TestTheOwnElasticIPEvaluatorDiscriminates(t *testing.T) {
	stmts := []policyStatement{{"Effect": "Allow", "Action": "ec2:ReleaseAddress", "Resource": "*"}}
	if !policyAllows(t, stmts, addressRequest("ec2:ReleaseAddress", selfARN, nil)) {
		t.Error("an unconditioned Allow was evaluated as a deny")
	}
	stmts = append(stmts, policyStatement{"Effect": "Deny", "Action": "ec2:*", "Resource": "*"})
	if policyAllows(t, stmts, addressRequest("ec2:ReleaseAddress", selfARN, nil)) {
		t.Error("an explicit Deny did not win")
	}
}
