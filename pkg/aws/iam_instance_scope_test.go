package aws

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// A small IAM evaluator for the instance-scope gates: Allow/Deny with
// StringEquals, StringNotEquals, StringNotLike, ForAnyValue:StringEquals and Null, policy
// variables resolved from the request context, and "*" globs in Action and
// Resource. Explicit Deny beats Allow; no Allow is an implicit deny. It covers
// exactly the forms these documents use, so the gates below fail when a
// statement stops scoping what it claims to. It is not a substitute for the
// live checks (PutRolePolicy and a dry-run from a real instance), which are
// what prove IAM itself evaluates aws:ARN and ${ec2:SourceInstanceARN} this
// way for each action.

type scopeRequest struct {
	action   string
	resource string
	// ctx is the request context. aws:ARN is set from resource.
	ctx map[string][]string
}

type scopeDecision int

const (
	implicitDeny scopeDecision = iota
	allowed
	explicitDeny
)

func (d scopeDecision) String() string {
	return [...]string{"implicit deny", "allow", "explicit deny"}[d]
}

func scopeGlob(pattern, s string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	return regexp.MustCompile(re).MatchString(s)
}

func scopeStrings(v interface{}) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

var scopeVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// resolve substitutes policy variables; ok is false when one is not in ctx.
func scopeResolve(v string, ctx map[string][]string) (string, bool) {
	ok := true
	out := scopeVarRe.ReplaceAllStringFunc(v, func(m string) string {
		vals := ctx[m[2:len(m)-1]]
		if len(vals) == 0 {
			ok = false
			return ""
		}
		return vals[0]
	})
	return out, ok
}

func scopeCondHolds(cond map[string]interface{}, ctx map[string][]string, t *testing.T) bool {
	for op, keys := range cond {
		km, _ := keys.(map[string]interface{})
		for key, want := range km {
			have, present := ctx[key]
			var wants []string
			for _, w := range scopeStrings(want) {
				r, ok := scopeResolve(w, ctx)
				if !ok {
					return false
				}
				wants = append(wants, r)
			}
			in := func(v string) bool {
				for _, w := range wants {
					if v == w {
						return true
					}
				}
				return false
			}
			switch op {
			case "StringEquals":
				if !present || !in(have[0]) {
					return false
				}
			case "StringNotEquals":
				if present && in(have[0]) {
					return false
				}
			case "StringNotLike":
				if present {
					for _, w := range wants {
						if scopeGlob(w, have[0]) {
							return false
						}
					}
				}
			case "ForAnyValue:StringEquals":
				any := false
				for _, h := range have {
					any = any || in(h)
				}
				if !any {
					return false
				}
			case "Null":
				if (wants[0] == "true") == present {
					return false
				}
			default:
				t.Fatalf("evaluator does not model condition operator %s", op)
			}
		}
	}
	return true
}

func scopeEval(t *testing.T, docs []string, req scopeRequest) scopeDecision {
	t.Helper()
	ctx := map[string][]string{"aws:ARN": {req.resource}}
	for k, v := range req.ctx {
		ctx[k] = v
	}
	decision := implicitDeny
	for _, doc := range docs {
		var pol struct{ Statement []map[string]interface{} }
		if err := json.Unmarshal([]byte(doc), &pol); err != nil {
			t.Fatalf("policy is not JSON: %v\n%s", err, doc)
		}
		for _, st := range pol.Statement {
			matched := false
			for _, a := range scopeStrings(st["Action"]) {
				matched = matched || scopeGlob(a, req.action)
			}
			if !matched {
				continue
			}
			matched = false
			for _, r := range scopeStrings(st["Resource"]) {
				matched = matched || scopeGlob(r, req.resource)
			}
			if !matched {
				continue
			}
			if c, ok := st["Condition"].(map[string]interface{}); ok && !scopeCondHolds(c, ctx, t) {
				continue
			}
			if st["Effect"] == "Deny" {
				return explicitDeny
			}
			decision = allowed
		}
	}
	return decision
}

const (
	scopeAcct     = testAccount
	scopeSelfARN  = "arn:aws:ec2:us-east-1:" + scopeAcct + ":instance/i-0self"
	scopeOtherARN = "arn:aws:ec2:us-east-1:" + scopeAcct + ":instance/i-0other"
	scopeChildARN = "arn:aws:ec2:us-west-2:" + scopeAcct + ":instance/i-0child"
	selfUserID    = "AROASELF:i-0self"
	otherUserID   = "AROAOTHER:i-0head2"
)

// legacySpawnManagedGrants is what a CLI up to schema 1 wrote, and what one up
// to v0.161.0 still rewrites on every launch (spored-policy /
// spored-baseline-policy): stop, terminate and tag any spawn:managed instance.
const legacySpawnManagedGrants = `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["ec2:CreateTags","ec2:DeleteTags"],"Resource":"*","Condition":{"StringEquals":{"ec2:ResourceTag/spawn:managed":"true"}}},
 {"Effect":"Allow","Action":["ec2:TerminateInstances","ec2:StopInstances","ec2:CancelSpotInstanceRequests"],"Resource":"*","Condition":{"StringEquals":{"ec2:ResourceTag/spawn:managed":"true"}}}]}`

func instanceReq(action, resource string, tags map[string]string, extra map[string][]string) scopeRequest {
	ctx := map[string][]string{
		"ec2:SourceInstanceARN": {scopeSelfARN},
		"aws:userid":            {selfUserID},
	}
	for k, v := range tags {
		ctx["ec2:ResourceTag/"+k] = []string{v}
	}
	for k, v := range extra {
		ctx[k] = v
	}
	return scopeRequest{action: action, resource: resource, ctx: ctx}
}

func tagKeys(keys ...string) map[string][]string { return map[string][]string{"aws:TagKeys": keys} }

var managedTags = map[string]string{"spawn:managed": "true"}

// The default spored roles may stop, terminate and tag their own instance and
// no other, including with an older CLI's account-wide grants beside them
// (instance-4). Asserted from the intent in iam_instance_scope.go, for both
// documents that carry the grants.
func TestSporedRolesMutateOnlyTheirOwnInstance(t *testing.T) {
	c := &Client{}
	docs := map[string]string{
		"spored-policy (DCV)":    bound(t, sporedDCVRolePolicy),
		"spored-baseline-policy": bound(t, c.sporedBaselinePolicyDoc()),
	}
	cases := []struct {
		name string
		req  scopeRequest
		want scopeDecision
	}{
		{"terminate self", instanceReq("ec2:TerminateInstances", scopeSelfARN, managedTags, nil), allowed},
		{"stop self", instanceReq("ec2:StopInstances", scopeSelfARN, managedTags, nil), allowed},
		{"tag self", instanceReq("ec2:CreateTags", scopeSelfARN, managedTags, tagKeys("spawn:last-heartbeat")), allowed},
		{"untag self", instanceReq("ec2:DeleteTags", scopeSelfARN, managedTags, tagKeys("spawn:dns-error")), allowed},
		{"cancel own spot request", instanceReq("ec2:CancelSpotInstanceRequests",
			"arn:aws:ec2:us-east-1:"+scopeAcct+":spot-instances-request/sir-1", managedTags, nil), allowed},
		{"terminate another spawn instance", instanceReq("ec2:TerminateInstances", scopeOtherARN, managedTags, nil), explicitDeny},
		{"stop another spawn instance", instanceReq("ec2:StopInstances", scopeOtherARN, managedTags, nil), explicitDeny},
		{"retag another spawn instance", instanceReq("ec2:CreateTags", scopeOtherARN, managedTags, tagKeys("spawn:ttl")), explicitDeny},
		{"untag another spawn instance", instanceReq("ec2:DeleteTags", scopeOtherARN, managedTags, tagKeys("spawn:ttl")), explicitDeny},
		{"terminate another head's child", instanceReq("ec2:TerminateInstances", scopeOtherARN,
			map[string]string{"spawn:managed": "true", NestedParentTagKey: otherUserID}, nil), explicitDeny},
		{"repoint own spawn:config", instanceReq("ec2:CreateTags", scopeSelfARN, managedTags, tagKeys("spawn:last-heartbeat", "spawn:config")), explicitDeny},
		{"reassign own spawn:iam-user", instanceReq("ec2:CreateTags", scopeSelfARN, managedTags, tagKeys("spawn:iam-user")), explicitDeny},
		{"write own nested-parent", instanceReq("ec2:CreateTags", scopeSelfARN, managedTags, tagKeys(NestedParentTagKey)), explicitDeny},
	}
	for what, doc := range docs {
		for _, withLegacy := range []bool{false, true} {
			set := []string{doc}
			label := what
			if withLegacy {
				set = append(set, legacySpawnManagedGrants)
				label += " + a pre-#1558 CLI's grants"
			}
			for _, tc := range cases {
				// Every refusal must be an explicit Deny: an implicit one is
				// what the old CLI's Allow beside it overrides.
				if got := scopeEval(t, set, tc.req); got != tc.want {
					t.Errorf("%s: %s: %v, want %v", label, tc.name, got, tc.want)
				}
			}
		}
	}
}

// The Denies retire the old grants only if they are explicit: an implicit
// deny is overridden by the old CLI's Allow. Pinned separately so a refactor
// that drops a Deny but keeps the self-scoped Allow fails here by name.
func TestSporedRolesCarryTheRetiringDenies(t *testing.T) {
	c := &Client{}
	for what, doc := range map[string]string{
		"spored-policy (DCV)":    bound(t, sporedDCVRolePolicy),
		"spored-baseline-policy": bound(t, c.sporedBaselinePolicyDoc()),
		"nested baseline":        bound(t, c.nestedSpawnBaselinePolicyDoc()),
	} {
		var pol struct{ Statement []map[string]interface{} }
		if err := json.Unmarshal([]byte(doc), &pol); err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, st := range pol.Statement {
			if sid, _ := st["Sid"].(string); st["Effect"] == "Deny" {
				found[sid] = true
			}
		}
		for _, sid := range []string{"DenyTagOthers", "DenyStopTerminateOthers"} {
			if !found[sid] {
				t.Errorf("%s has no %s Deny", what, sid)
			}
		}
	}
}

// A nested head manages the children it launched and nothing else: not
// another head's children, not an unrelated spawn instance, and it may pass
// only spored-instance-role and its own role (instance-3, iam-6).
func TestNestedSpawnRoleReachesOnlyItsOwnChildren(t *testing.T) {
	c := &Client{}
	docs := []string{bound(t, c.nestedSpawnBaselinePolicyDoc()), bound(t, nestedSpawnPolicyDoc())}
	child := map[string]string{"spawn:managed": "true", NestedParentTagKey: selfUserID}
	foreignChild := map[string]string{"spawn:managed": "true", NestedParentTagKey: otherUserID}
	ssmTag := func(tags map[string]string) map[string][]string {
		out := map[string][]string{}
		for k, v := range tags {
			out["ssm:resourceTag/"+k] = []string{v}
		}
		return out
	}
	runReq := func(parent string) scopeRequest {
		return instanceReq("ec2:RunInstances", scopeChildARN, nil, map[string][]string{
			"aws:RequestTag/spawn:managed":         {"true"},
			"aws:RequestTag/" + NestedParentTagKey: {parent},
		})
	}
	passReq := func(role string) scopeRequest {
		return instanceReq("iam:PassRole", "arn:aws:iam::"+scopeAcct+":role/"+role, nil,
			map[string][]string{"iam:PassedToService": {"ec2.amazonaws.com"}})
	}
	cases := []struct {
		name string
		req  scopeRequest
		want scopeDecision
	}{
		{"terminate own child", instanceReq("ec2:TerminateInstances", scopeChildARN, child, nil), allowed},
		{"stop own child", instanceReq("ec2:StopInstances", scopeChildARN, child, nil), allowed},
		{"extend own child", instanceReq("ec2:CreateTags", scopeChildARN, child, tagKeys("spawn:ttl-deadline")), allowed},
		{"terminate self", instanceReq("ec2:TerminateInstances", scopeSelfARN, managedTags, nil), allowed},
		{"terminate another head's child", instanceReq("ec2:TerminateInstances", scopeOtherARN, foreignChild, nil), explicitDeny},
		{"terminate an unrelated spawn instance", instanceReq("ec2:TerminateInstances", scopeOtherARN, managedTags, nil), explicitDeny},
		{"retag an unrelated spawn instance", instanceReq("ec2:CreateTags", scopeOtherARN, managedTags, tagKeys("spawn:ttl")), explicitDeny},
		{"reparent own child", instanceReq("ec2:CreateTags", scopeChildARN, child, tagKeys(NestedParentTagKey)), explicitDeny},
		{"shell on own child", instanceReq("ssm:SendCommand", scopeChildARN, nil, ssmTag(child)), allowed},
		{"shell on another head's child", instanceReq("ssm:SendCommand", scopeOtherARN, nil, ssmTag(foreignChild)), implicitDeny},
		{"shell on an unrelated spawn instance", instanceReq("ssm:SendCommand", scopeOtherARN, nil, ssmTag(managedTags)), implicitDeny},
		{"session on an unrelated spawn instance", instanceReq("ssm:StartSession", scopeOtherARN, nil, ssmTag(managedTags)), implicitDeny},
		{"launch a child tagged as its own", runReq(selfUserID), allowed},
		{"launch a child tagged as another head's", runReq(otherUserID), implicitDeny},
		{"tag a child at launch", instanceReq("ec2:CreateTags", scopeChildARN, nil, map[string][]string{
			"ec2:CreateAction":             {"RunInstances"},
			"aws:RequestTag/spawn:managed": {"true"},
			"aws:TagKeys":                  {"spawn:managed", "spawn:launch-time", NestedParentTagKey},
		}), allowed},
		{"pass spored-instance-role", passReq(SporedRoleName), allowed},
		{"pass its own role", passReq(NestedSpawnRoleName()), allowed},
		{"pass a per-task role", passReq("spawn-instance-0123456789abcdef"), implicitDeny},
		{"pass another nested role", passReq(NestedSpawnRolePrefix + "deadbeef"), implicitDeny},
	}
	for _, tc := range cases {
		if got := scopeEval(t, docs, tc.req); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The policy hashes to its own name, and names itself: the placeholder must
// not survive into the written document.
func TestNestedSpawnPolicyNamesItsOwnRole(t *testing.T) {
	doc := nestedSpawnPolicyDoc()
	if strings.Contains(doc, nestedSelfRolePlaceholder) {
		t.Fatalf("the nested grant document still carries %s", nestedSelfRolePlaceholder)
	}
	if !strings.Contains(doc, ":role/"+NestedSpawnRoleName()+`"`) {
		t.Errorf("the nested grant document does not name its own role %s", NestedSpawnRoleName())
	}
	if strings.Contains(doc, "spawn-instance-*") {
		t.Errorf("the nested grant document still reaches the spawn-instance-* family")
	}
}

// A nested head may not rewrite who owns an instance, on its children or on
// itself: NestedParentTagKey would hand the instance to another head, and the
// other ownership keys to another user or out of the reaper's scope. Only the
// creates it tags at creation may set them. A direct CreateTags is refused
// whether IAM leaves ec2:CreateAction out of its context or fills it with
// "CreateTags": a live check showed the Null-based exemption this replaced did
// not refuse it.
func TestNestedHeadCannotRewriteOwnershipTags(t *testing.T) {
	c := &Client{}
	docs := []string{bound(t, c.nestedSpawnBaselinePolicyDoc()), bound(t, nestedSpawnPolicyDoc())}
	child := map[string]string{"spawn:managed": "true", NestedParentTagKey: selfUserID}
	for _, key := range []string{NestedParentTagKey, "spawn:managed", "spawn:iam-user", "spawn:config", "spawn:launch-time", "spawn:pre-stop"} {
		for _, target := range []struct {
			name, arn string
			tags      map[string]string
		}{{"its own child", scopeChildARN, child}, {"itself", scopeSelfARN, managedTags}} {
			for _, createAction := range []string{"", "CreateTags"} {
				extra := tagKeys("spawn:ttl-deadline", key)
				if createAction != "" {
					extra["ec2:CreateAction"] = []string{createAction}
				}
				for _, action := range []string{"ec2:CreateTags", "ec2:DeleteTags"} {
					if got := scopeEval(t, docs, instanceReq(action, target.arn, target.tags, extra)); got != explicitDeny {
						t.Errorf("%s of %s on %s (ec2:CreateAction %q): %v, want explicit deny", action, key, target.name, createAction, got)
					}
				}
			}
		}
	}
	// The creates still tag: a child at RunInstances, a group at creation.
	for _, create := range []string{"RunInstances", "CreateSecurityGroup", "CreatePlacementGroup", "ImportKeyPair"} {
		req := instanceReq("ec2:CreateTags", scopeChildARN, nil, map[string][]string{
			"ec2:CreateAction":             {create},
			"aws:RequestTag/spawn:managed": {"true"},
			"aws:TagKeys":                  {"spawn:managed", "spawn:launch-time", NestedParentTagKey},
		})
		if got := scopeEval(t, docs, req); got != allowed {
			t.Errorf("tag-on-create for %s: %v, want allow", create, got)
		}
	}
	// And an ordinary key on its own child still writes (`spawn extend`).
	if got := scopeEval(t, docs, instanceReq("ec2:CreateTags", scopeChildARN, child,
		map[string][]string{"aws:TagKeys": {"spawn:ttl-deadline"}, "ec2:CreateAction": {"CreateTags"}})); got != allowed {
		t.Errorf("extending its own child: %v, want allow", got)
	}
}
