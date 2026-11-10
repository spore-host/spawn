package instancerole

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// fakePolicies is the account's customer-managed policies, as ListPolicies
// returns them, and the deletes made.
type fakePolicies struct {
	policies []iamtypes.Policy
	listIn   *iam.ListPoliciesInput
	deleted  []string
	// attachedSince: policies attached between the listing and the delete.
	attachedSince map[string]bool
}

func (f *fakePolicies) ListPolicies(_ context.Context, in *iam.ListPoliciesInput, _ ...func(*iam.Options)) (*iam.ListPoliciesOutput, error) {
	f.listIn = in
	var out []iamtypes.Policy
	for _, p := range f.policies {
		if strings.HasPrefix(aws.ToString(p.Path), aws.ToString(in.PathPrefix)) {
			out = append(out, p)
		}
	}
	return &iam.ListPoliciesOutput{Policies: out}, nil
}

func (f *fakePolicies) DeletePolicy(_ context.Context, in *iam.DeletePolicyInput, _ ...func(*iam.Options)) (*iam.DeletePolicyOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if f.attachedSince[arn] {
		return nil, &iamtypes.DeleteConflictException{Message: aws.String("Cannot delete a policy attached to entities.")}
	}
	f.deleted = append(f.deleted, arn)
	return &iam.DeletePolicyOutput{}, nil
}

func policy(path, name string, attachments int32, age time.Duration) iamtypes.Policy {
	return iamtypes.Policy{
		Arn:             aws.String("arn:aws:iam::111122223333:policy" + path + name),
		Path:            aws.String(path),
		PolicyName:      aws.String(name),
		AttachmentCount: aws.Int32(attachments),
		CreateDate:      aws.Time(sweepNow.Add(-age)),
	}
}

var sweepNow = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

// #1573: a spored managed policy goes only when it is spawn's (path and name),
// attached to nothing, and a day old; reap false changes nothing.
func TestSweepSporedPolicies(t *testing.T) {
	old := 72 * time.Hour
	f := &fakePolicies{policies: []iamtypes.Policy{
		policy(SporedPolicyPath, "spored-policy-v1", 0, old),                 // goes
		policy(SporedPolicyPath, "spored-nested-baseline-policy-v3", 0, old), // goes
		policy(SporedPolicyPath, "spored-baseline-policy-v1", 2, old),        // attached
		policy(SporedPolicyPath, "spored-baseline-policy-v2", 0, time.Hour),  // young
		policy(SporedPolicyPath, "spored-policy-v1-mine", 0, old),            // not spawn's name
		policy(SporedPolicyPath, "team-policy", 0, old),                      // not spawn's name
		policy(SporedPolicyPath+"x/", "spored-policy-v1", 0, old),            // another path
		policy("/", "spored-policy-v2", 0, old),                              // another path
		policy(SporedPolicyPath, "spored-policy-v9", 0, old),                 // attached by the time of the delete
	}, attachedSince: map[string]bool{"arn:aws:iam::111122223333:policy/spore-host/spored-policy-v9": true}}

	preview, err := SweepSporedPolicies(context.Background(), f, sweepNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("reap=false deleted %v", f.deleted)
	}
	if f.listIn.Scope != iamtypes.PolicyScopeTypeLocal || aws.ToString(f.listIn.PathPrefix) != SporedPolicyPath {
		t.Errorf("listed scope %q path %q", f.listIn.Scope, aws.ToString(f.listIn.PathPrefix))
	}
	got := map[string]string{}
	for _, d := range preview {
		action := "keep: " + d.Reason
		if d.Delete {
			action = "delete"
		}
		got[d.Name] = action
	}
	want := map[string]string{
		"spored-policy-v1":                 "delete",
		"spored-nested-baseline-policy-v3": "delete",
		"spored-baseline-policy-v1":        "keep: " + KeepPolicyAttached,
		"spored-baseline-policy-v2":        "keep: " + KeepPolicyYoung,
		"spored-policy-v9":                 "delete",
	}
	if len(got) != len(want) {
		t.Errorf("decided %v, want %v", got, want)
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("%s: %q, want %q", n, got[n], w)
		}
	}

	res, err := SweepSporedPolicies(context.Background(), f, sweepNow, true)
	if err != nil {
		t.Fatal(err)
	}
	wantDeleted := []string{
		"arn:aws:iam::111122223333:policy/spore-host/spored-nested-baseline-policy-v3",
		"arn:aws:iam::111122223333:policy/spore-host/spored-policy-v1",
	}
	sort.Strings(f.deleted)
	if strings.Join(f.deleted, "|") != strings.Join(wantDeleted, "|") {
		t.Errorf("deleted %v, want %v", f.deleted, wantDeleted)
	}
	for _, d := range res {
		if d.Name == "spored-policy-v9" && (d.Deleted || d.Delete || d.Err != nil) {
			t.Errorf("a policy attached since the listing: %+v, want kept without error", d)
		}
	}
}

func TestIsSporedPolicyName(t *testing.T) {
	for name, want := range map[string]bool{
		"spored-policy-v1":                  true,
		"spored-baseline-policy-v12":        true,
		"spored-nested-baseline-policy-v1":  true,
		"spored-policy":                     false,
		"spored-policy-v0":                  false,
		"spored-policy-v":                   false,
		"spored-policy-v1a":                 false,
		"spored-policy-vX":                  false,
		"xspored-policy-v1":                 false,
		"SweepCrossAccountEC2Policy-v1":     false,
		"spored-dns-invoke-0a1b2c3d":        false,
		"spored-baseline-policy-v1-suffix":  false,
		"spored-nested-baseline-policy-v-1": false,
	} {
		if got := IsSporedPolicyName(name); got != want {
			t.Errorf("IsSporedPolicyName(%q) = %v, want %v", name, got, want)
		}
	}
}
