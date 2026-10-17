package aws

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/spore-host/spawn/pkg/testutil"
)

// #1080: a Spot replacement carries no tags, so the tag-scoped terminate cannot
// end it, and an OS shutdown only STOPS it. The TerminateOnlySelf statement is
// what lets it end itself. Its exact form was proven on real AWS (IAM stores it;
// another instance refused, itself allowed, itself+other refused, a real
// self-terminate succeeded), so these tests pin that form. A rewrite to another
// key or variable is back in unverified territory: #1069 was a policy variable
// the simulator and Access Analyzer accepted and PutRolePolicy refused.

var provenSelfTerminate = map[string]interface{}{
	"Sid":      "TerminateOnlySelf",
	"Effect":   "Allow",
	"Action":   "ec2:TerminateInstances",
	"Resource": "*",
	"Condition": map[string]interface{}{
		"StringEquals": map[string]interface{}{"aws:ARN": "${ec2:SourceInstanceARN}"},
	},
}

func selfTerminateIn(t *testing.T, what, doc string) {
	t.Helper()
	var parsed struct{ Statement []map[string]interface{} }
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("%s is not JSON: %v", what, err)
	}
	var found int
	for _, st := range parsed.Statement {
		if st["Sid"] == "TerminateOnlySelf" {
			found++
			if !reflect.DeepEqual(st, provenSelfTerminate) {
				b, _ := json.Marshal(st)
				t.Errorf("%s: TerminateOnlySelf is not the form proven on real AWS:\n  got  %s", what, b)
			}
		}
	}
	if found != 1 {
		t.Errorf("%s carries %d TerminateOnlySelf statements, want exactly 1", what, found)
	}
}

func TestEverySporedPolicyLetsTheInstanceTerminateOnlyItself(t *testing.T) {
	selfTerminateIn(t, "dynamic baseline (CreateOrGetInstanceProfile)", (&Client{}).sporedBaselinePolicyDoc())
	selfTerminateIn(t, "static shared role (SetupSporedIAMRole)", sporedDCVRolePolicy)
	b, err := json.Marshal((&Client{}).buildInlinePolicy([]string{"s3:ReadOnly"}))
	if err != nil {
		t.Fatal(err)
	}
	selfTerminateIn(t, "inline --iam-policy role", string(b))
}

// And as stored: what each writer actually sends to IAM, read back.
func TestTheSelfTerminateGrantReachesIAM(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	c := NewClientFromConfig(env.AWSConfig)
	ic := env.IAMClient()
	if _, err := c.SetupSporedIAMRole(ctx); err != nil {
		t.Fatalf("SetupSporedIAMRole: %v", err)
	}
	selfTerminateIn(t, "stored spored-policy", storedPolicy(t, ic, SporedRoleName, "spored-policy"))
	if _, err := c.CreateOrGetInstanceProfile(ctx, IAMRoleConfig{RoleName: "spawn-test-1080", Policies: []string{"s3:ReadOnly"}}); err != nil {
		t.Fatalf("CreateOrGetInstanceProfile: %v", err)
	}
	selfTerminateIn(t, "stored spored-baseline-policy", storedPolicy(t, ic, "spawn-test-1080", "spored-baseline-policy"))
	selfTerminateIn(t, "stored spawn-inline-policy", storedPolicy(t, ic, "spawn-test-1080", "spawn-inline-policy"))
}
