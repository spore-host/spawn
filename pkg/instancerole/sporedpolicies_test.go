package instancerole

import (
	"context"
	"errors"
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
	// versions by policy ARN, and the (arn, versionId) pairs deleted. The
	// sweep has no version calls any more (#1664); the fake keeps them so a
	// test can prove none is made.
	versions        map[string][]iamtypes.PolicyVersion
	deletedVersions []string
	// tags by policy ARN; tagErr: ARNs whose ListPolicyTags fails.
	tags   map[string]map[string]string
	tagErr map[string]bool
}

func (f *fakePolicies) ListPolicyTags(_ context.Context, in *iam.ListPolicyTagsInput, _ ...func(*iam.Options)) (*iam.ListPolicyTagsOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if f.tagErr[arn] {
		return nil, errors.New("AccessDenied: not authorized to perform iam:ListPolicyTags")
	}
	var out []iamtypes.Tag
	for k, v := range f.tags[arn] {
		out = append(out, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return &iam.ListPolicyTagsOutput{Tags: out}, nil
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

func (f *fakePolicies) ListPolicyVersions(_ context.Context, in *iam.ListPolicyVersionsInput, _ ...func(*iam.Options)) (*iam.ListPolicyVersionsOutput, error) {
	return &iam.ListPolicyVersionsOutput{Versions: f.versions[aws.ToString(in.PolicyArn)]}, nil
}

func (f *fakePolicies) DeletePolicyVersion(_ context.Context, in *iam.DeletePolicyVersionInput, _ ...func(*iam.Options)) (*iam.DeletePolicyVersionOutput, error) {
	f.deletedVersions = append(f.deletedVersions, aws.ToString(in.PolicyArn)+" "+aws.ToString(in.VersionId))
	return &iam.DeletePolicyVersionOutput{}, nil
}

// boundaryPolicy is the spawn-instance boundary as ListPolicies returns it:
// its use is counted by PermissionsBoundaryUsageCount, not AttachmentCount.
func boundaryPolicy(usage int32, age time.Duration) iamtypes.Policy {
	return iamtypes.Policy{
		Arn:                           aws.String("arn:aws:iam::111122223333:policy" + SporedPolicyPath + BoundaryPolicyName),
		Path:                          aws.String(SporedPolicyPath),
		PolicyName:                    aws.String(BoundaryPolicyName),
		AttachmentCount:               aws.Int32(0),
		PermissionsBoundaryUsageCount: aws.Int32(usage),
		CreateDate:                    aws.Time(sweepNow.Add(-age)),
	}
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

// #1664: the spawn-instance boundary is permanent. The sweep lists it as kept
// and never deletes it, nor any of its versions: not while a role uses it, not
// unused and old (which is what it used to delete), not young.
func TestSweepSporedPolicies_NeverDeletesTheBoundary(t *testing.T) {
	old := 72 * time.Hour
	boundaryARN := "arn:aws:iam::111122223333:policy" + SporedPolicyPath + BoundaryPolicyName
	for name, pol := range map[string]iamtypes.Policy{
		"in use":         boundaryPolicy(3, old),
		"unused and old": boundaryPolicy(0, old),
		"unused, young":  boundaryPolicy(0, time.Hour),
	} {
		f := &fakePolicies{
			policies: []iamtypes.Policy{pol},
			versions: map[string][]iamtypes.PolicyVersion{boundaryARN: {
				{VersionId: aws.String("v1"), IsDefaultVersion: false},
				{VersionId: aws.String("v2"), IsDefaultVersion: true},
			}},
		}
		for _, reap := range []bool{false, true} {
			res, err := SweepSporedPolicies(context.Background(), f, sweepNow, reap)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 || res[0].Delete || res[0].Deleted || res[0].Reason != KeepBoundaryPermanent {
				t.Errorf("%s, reap=%t: %+v, want kept as %q", name, reap, res, KeepBoundaryPermanent)
			}
		}
		if len(f.deleted) != 0 || len(f.deletedVersions) != 0 {
			t.Errorf("%s: deleted %v and versions %v of the boundary", name, f.deleted, f.deletedVersions)
		}
	}
}

// The boundary is recognised by its ARN as well as by its name, so a listing
// that reports one but not the other cannot slip it through.
func TestSweepSporedPolicies_BoundaryByARN(t *testing.T) {
	pol := boundaryPolicy(0, 72*time.Hour)
	pol.PolicyName = nil
	f := &fakePolicies{policies: []iamtypes.Policy{pol}}
	res, err := SweepSporedPolicies(context.Background(), f, sweepNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 0 || len(res) != 1 || res[0].Reason != KeepBoundaryPermanent {
		t.Fatalf("a boundary named only by ARN: %+v, deleted %v", res, f.deleted)
	}
}

// #1664: a spored policy tagged spore-host:permanent=true is kept, however
// unused and old it is.
func TestSweepSporedPolicies_KeepsPermanentTagged(t *testing.T) {
	old := 72 * time.Hour
	perm := policy(SporedPolicyPath, "spored-policy-v1", 0, old)
	plain := policy(SporedPolicyPath, "spored-policy-v2", 0, old)
	f := &fakePolicies{
		policies: []iamtypes.Policy{perm, plain},
		tags: map[string]map[string]string{
			aws.ToString(perm.Arn):  {"spore-host:permanent": "true"},
			aws.ToString(plain.Arn): {"spore-host:managed-by": "spawn"},
		},
	}
	res, err := SweepSporedPolicies(context.Background(), f, sweepNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.deleted, "|") != aws.ToString(plain.Arn) {
		t.Errorf("deleted %v, want only %s", f.deleted, aws.ToString(plain.Arn))
	}
	for _, d := range res {
		if d.Name == "spored-policy-v1" && (d.Delete || d.Deleted || d.Reason != KeepPolicyPermanent) {
			t.Errorf("the permanent-tagged policy: %+v, want kept as %q", d, KeepPolicyPermanent)
		}
	}
}

// A candidate whose tags cannot be read cannot be shown not to be permanent,
// so it is kept, with the error.
func TestSweepSporedPolicies_UnreadableTagsKeep(t *testing.T) {
	pol := policy(SporedPolicyPath, "spored-policy-v1", 0, 72*time.Hour)
	f := &fakePolicies{policies: []iamtypes.Policy{pol}, tagErr: map[string]bool{aws.ToString(pol.Arn): true}}
	res, err := SweepSporedPolicies(context.Background(), f, sweepNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v with unreadable tags", f.deleted)
	}
	if len(res) != 1 || res[0].Delete || res[0].Reason != KeepPolicyTagsUnreadable || res[0].Err == nil {
		t.Errorf("unreadable tags: %+v, want kept as %q with the error", res, KeepPolicyTagsUnreadable)
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
