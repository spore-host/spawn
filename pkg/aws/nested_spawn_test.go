package aws

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/testutil"
)

type nestedPolicyStatement struct {
	Sid       string
	Effect    string
	Action    interface{}
	Resource  interface{}
	Condition map[string]map[string]interface{}
}

func parseStatements(t *testing.T, doc string) []nestedPolicyStatement {
	t.Helper()
	var pol struct{ Statement []nestedPolicyStatement }
	if err := json.Unmarshal([]byte(doc), &pol); err != nil {
		t.Fatalf("policy is not JSON: %v\n%s", err, doc)
	}
	return pol.Statement
}

// isReadAction: Describe/Get/List calls change nothing.
func isReadAction(a string) bool {
	_, op, _ := strings.Cut(a, ":")
	for _, p := range []string{"Describe", "Get", "List"} {
		if strings.HasPrefix(op, p) {
			return true
		}
	}
	return false
}

// The issue's suggestion was one statement granting every action on "*". The
// owner's decision is the opposite: every mutating grant is scoped by a
// condition or by resource type, and "*" is only for reads, which AWS gives no
// resource-level permissions.
func TestNestedSpawnPolicyScopesEveryMutatingGrant(t *testing.T) {
	for _, st := range parseStatements(t, nestedSpawnPolicyDoc()) {
		if st.Effect != "Allow" {
			t.Errorf("%s: unexpected %s statement in the grant document", st.Sid, st.Effect)
			continue
		}
		resources := stringList(st.Resource)
		onStar := false
		for _, r := range resources {
			if r == "*" {
				onStar = true
			}
		}
		for _, a := range stringList(st.Action) {
			if strings.HasPrefix(a, "iam:") && a != "iam:PassRole" {
				t.Errorf("%s grants %s; the nested role must not write or read IAM beyond PassRole", st.Sid, a)
			}
			if strings.Contains(a, "*") {
				t.Errorf("%s grants the wildcard action %s", st.Sid, a)
			}
			if onStar && !isReadAction(a) && len(st.Condition) == 0 {
				t.Errorf("%s grants the mutating %s on \"*\" with no condition", st.Sid, a)
			}
		}
	}
}

// RunInstances may create only tagged children: the resources it creates are
// conditioned on aws:RequestTag/spawn:managed=true, and the unconditioned
// statement names only resource types RunInstances references, never the
// instance or volume.
func TestNestedSpawnChildrenMustBeTaggedManaged(t *testing.T) {
	var tagged, referenced int
	for _, st := range parseStatements(t, nestedSpawnPolicyDoc()) {
		if !reflect.DeepEqual(stringList(st.Action), []string{"ec2:RunInstances"}) {
			continue
		}
		cond := st.Condition["StringEquals"]["aws:RequestTag/spawn:managed"]
		for _, r := range stringList(st.Resource) {
			creates := strings.Contains(r, ":instance/") || strings.Contains(r, ":volume/")
			switch {
			case creates && cond != "true":
				t.Errorf("%s: RunInstances on %s without aws:RequestTag/spawn:managed=true; an untagged child would be invisible to the reaper", st.Sid, r)
			case creates:
				tagged++
			case r == "*":
				t.Errorf("%s: RunInstances on \"*\"", st.Sid)
			default:
				referenced++
			}
		}
	}
	if tagged != 2 || referenced == 0 {
		t.Errorf("found %d tagged create resources (want instance+volume) and %d referenced ones", tagged, referenced)
	}
}

func TestNestedSpawnPassRoleIsScopedToSpawnRolesAndEC2(t *testing.T) {
	var found int
	for _, st := range parseStatements(t, nestedSpawnPolicyDoc()) {
		for _, a := range stringList(st.Action) {
			if a != "iam:PassRole" {
				continue
			}
			found++
			res := stringList(st.Resource)
			sort.Strings(res)
			want := []string{
				"arn:aws:iam::" + sporedAccountPlaceholder + ":role/spawn-instance-*",
				"arn:aws:iam::" + sporedAccountPlaceholder + ":role/" + SporedRoleName,
			}
			sort.Strings(want)
			if !reflect.DeepEqual(res, want) {
				t.Errorf("iam:PassRole on %v, want exactly %v", res, want)
			}
			if st.Condition["StringEquals"]["iam:PassedToService"] != "ec2.amazonaws.com" {
				t.Errorf("iam:PassRole is not limited to iam:PassedToService=ec2.amazonaws.com: %v", st.Condition)
			}
		}
	}
	if found != 1 {
		t.Errorf("found %d iam:PassRole grants, want 1", found)
	}
	// The family PassRole reaches must be the family the reaper deletes, or a
	// nested child of a nested spore could not be cleaned up.
	if !instancerole.IsCandidate(NestedSpawnRoleName()) {
		t.Errorf("%s is outside pkg/instancerole's sweep, so nothing deletes it", NestedSpawnRoleName())
	}
}

// The nested baseline differs from the default one in exactly one place: the
// reaper-control-tag Deny is lifted for tag-on-create only. Without that, IAM
// refuses every child RunInstances, which must carry spawn:managed and
// spawn:launch-time.
func TestNestedSpawnBaselineDiffersOnlyAtTagOnCreate(t *testing.T) {
	c := &Client{}
	def := parseStatements(t, c.sporedBaselinePolicyDoc())
	nested := parseStatements(t, c.nestedSpawnBaselinePolicyDoc())
	if len(def) != len(nested) {
		t.Fatalf("nested baseline has %d statements, default %d", len(nested), len(def))
	}
	diffs := 0
	for i := range def {
		if reflect.DeepEqual(def[i], nested[i]) {
			continue
		}
		diffs++
		d, n := def[i], nested[i]
		if d.Effect != "Deny" {
			t.Errorf("statement %d (%s) differs and is not the Deny", i, d.Sid)
			continue
		}
		if !reflect.DeepEqual(n.Condition["Null"], map[string]interface{}{"ec2:CreateAction": "true"}) {
			t.Errorf("the nested Deny's added condition is %v, want Null ec2:CreateAction=true", n.Condition["Null"])
		}
		delete(n.Condition, "Null")
		if !reflect.DeepEqual(d, n) {
			t.Errorf("the nested Deny differs beyond the Null condition:\n default %+v\n nested  %+v", d, n)
		}
	}
	if diffs != 1 {
		t.Errorf("%d statements differ, want exactly the one Deny", diffs)
	}
}

// Never widen the default role (owner decision on #39): setting up the nested
// role leaves spored-instance-role exactly as a default launch leaves it.
func TestNestedSpawnLeavesTheDefaultRoleUnchanged(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	c := NewClientFromConfig(env.AWSConfig)
	ic := env.IAMClient()
	if _, err := c.SetupSporedIAMRole(ctx); err != nil {
		t.Fatalf("SetupSporedIAMRole: %v", err)
	}
	before := roleSnapshot(t, ic, SporedRoleName)
	profile, err := c.SetupNestedSpawnProfile(ctx)
	if err != nil {
		t.Fatalf("SetupNestedSpawnProfile: %v", err)
	}
	if after := roleSnapshot(t, ic, SporedRoleName); !reflect.DeepEqual(before, after) {
		t.Errorf("setting up the nested role changed %s:\n before %v\n after  %v", SporedRoleName, before, after)
	}
	for name, doc := range before {
		for _, a := range []string{"ec2:RunInstances", "iam:PassRole", "ec2:ImportKeyPair"} {
			if strings.Contains(doc, `"`+a+`"`) {
				t.Errorf("%s/%s grants %s", SporedRoleName, name, a)
			}
		}
	}

	// And the nested role is what the flag promises.
	if profile != NestedSpawnRoleName() {
		t.Fatalf("profile = %q, want %q", profile, NestedSpawnRoleName())
	}
	acct, _, err := c.GetCallerIdentityInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	grant := storedPolicy(t, ic, profile, "spawn-scoped-policy")
	if strings.Contains(grant, sporedAccountPlaceholder) {
		t.Errorf("stored nested grant still carries %s", sporedAccountPlaceholder)
	}
	want, _ := bindPolicyAccount(nestedSpawnPolicyDoc(), acct)
	if !jsonEqual(t, grant, want) {
		t.Errorf("stored nested grant is not the nested policy:\n%s", grant)
	}
	baseline := storedPolicy(t, ic, profile, "spored-baseline-policy")
	wantBase, _ := bindPolicyAccount(c.nestedSpawnBaselinePolicyDoc(), acct)
	if !jsonEqual(t, baseline, wantBase) {
		t.Errorf("nested role's baseline is not the tag-on-create variant:\n got  %s\n want %s", baseline, wantBase)
	}
	role, err := ic.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(profile)})
	if err != nil {
		t.Fatal(err)
	}
	managed := false
	for _, tag := range role.Role.Tags {
		if aws.ToString(tag.Key) == instancerole.ManagedTagKey && aws.ToString(tag.Value) == instancerole.ManagedTagValue {
			managed = true
		}
	}
	if !managed {
		t.Errorf("nested role is not tagged %s=%s, so pkg/instancerole keeps it forever", instancerole.ManagedTagKey, instancerole.ManagedTagValue)
	}
	ip, err := ic.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(profile)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ip.InstanceProfile.Roles) != 1 || aws.ToString(ip.InstanceProfile.Roles[0].RoleName) != profile {
		t.Errorf("instance profile %s does not hold role %s", profile, profile)
	}
}

func roleSnapshot(t *testing.T, ic *iam.Client, role string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	names, err := ic.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(role)})
	if err != nil {
		t.Fatalf("ListRolePolicies %s: %v", role, err)
	}
	for _, n := range names.PolicyNames {
		out["inline:"+n] = storedPolicy(t, ic, role, n)
	}
	attached, err := ic.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(role)})
	if err != nil {
		t.Fatalf("ListAttachedRolePolicies %s: %v", role, err)
	}
	for _, p := range attached.AttachedPolicies {
		out["managed:"+aws.ToString(p.PolicyArn)] = ""
	}
	if len(out) == 0 {
		t.Fatalf("%s has no policies; the snapshot would prove nothing", role)
	}
	return out
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y map[string]interface{}
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, a)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return reflect.DeepEqual(normalizePolicyJSON(x), normalizePolicyJSON(y))
}

// normalizePolicyJSON folds a one-element list to its element: IAM treats
// ["a"] and "a" alike, and the emulator stores the short form.
func normalizePolicyJSON(v interface{}) interface{} {
	switch x := v.(type) {
	case []interface{}:
		if len(x) == 1 {
			return normalizePolicyJSON(x[0])
		}
		out := make([]interface{}, len(x))
		for i, e := range x {
			out[i] = normalizePolicyJSON(e)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, e := range x {
			out[k] = normalizePolicyJSON(e)
		}
		return out
	}
	return v
}

func TestNestedSpawnRoleFromCallerARN(t *testing.T) {
	n := NestedSpawnRoleName()
	cases := map[string]string{
		"arn:aws:sts::123456789012:assumed-role/" + n + "/i-0abc":               n,
		"arn:aws:sts::123456789012:assumed-role/" + SporedRoleName + "/i-0abc":  "",
		"arn:aws:sts::123456789012:assumed-role/spawn-instance-ab12cd34/i-0abc": "",
		"arn:aws:iam::123456789012:user/" + n:                                   "",
		"arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_x/" + n:          "",
		"": "",
	}
	for arn, want := range cases {
		got, ok := NestedSpawnRoleFromCallerARN(arn)
		if got != want || ok != (want != "") {
			t.Errorf("NestedSpawnRoleFromCallerARN(%q) = %q, %v; want %q", arn, got, ok, want)
		}
	}
}
