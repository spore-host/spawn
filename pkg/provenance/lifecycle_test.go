package provenance

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// The rules are Decisions 3, read from the design: cheap -> Standard-IA at 30
// days and Glacier IR at 90, moderate -> Standard-IA at 60, each only for
// objects tagged spawn:retain=false and at least 128 KiB. They tier only:
// phase 3 must never carry an expiration, which would delete.
func TestRulesAreTheDecidedTiering(t *testing.T) {
	want := map[string]map[int32]s3types.TransitionStorageClass{
		RuleIDPrefix + "cheap":    {30: s3types.TransitionStorageClassStandardIa, 90: s3types.TransitionStorageClassGlacierIr},
		RuleIDPrefix + "moderate": {60: s3types.TransitionStorageClassStandardIa},
	}
	rules := Rules()
	if len(rules) != len(want) {
		t.Fatalf("%d rules, want %d (cheap and moderate; expensive and unclassified are never tiered)", len(rules), len(want))
	}
	for _, r := range rules {
		id := aws.ToString(r.ID)
		w, ok := want[id]
		if !ok {
			t.Errorf("unexpected rule %q", id)
			continue
		}
		if r.Status != s3types.ExpirationStatusEnabled {
			t.Errorf("%s: status %q", id, r.Status)
		}
		if r.Expiration != nil || r.NoncurrentVersionExpiration != nil || r.AbortIncompleteMultipartUpload != nil {
			t.Errorf("%s carries a deleting action; phase 3 only tiers", id)
		}
		got := map[int32]s3types.TransitionStorageClass{}
		for _, tr := range r.Transitions {
			got[aws.ToInt32(tr.Days)] = tr.StorageClass
		}
		if len(got) != len(w) {
			t.Errorf("%s transitions %v, want %v", id, got, w)
		}
		for d, sc := range w {
			if got[d] != sc {
				t.Errorf("%s: day %d -> %q, want %q", id, d, got[d], sc)
			}
		}
		and := r.Filter.And
		tags := map[string]string{}
		for _, tg := range and.Tags {
			tags[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
		}
		if tags[TagRetain] != "false" {
			t.Errorf("%s doesn't require spawn:retain=false, so retain=true couldn't take an object out of it", id)
		}
		if tags[TagClass] != strings.TrimPrefix(id, RuleIDPrefix) {
			t.Errorf("%s matches class %q", id, tags[TagClass])
		}
		// ObjectSizeGreaterThan is strict: 128 KiB itself must be included.
		if aws.ToInt64(and.ObjectSizeGreaterThan) != 128*1024-1 {
			t.Errorf("%s: ObjectSizeGreaterThan %d, want 131071 so objects of 128 KiB and up match", id, aws.ToInt64(and.ObjectSizeGreaterThan))
		}
	}
}

// --- a fake bucket lifecycle ---

type fakeLifecycle struct {
	rules    []s3types.LifecycleRule
	minSize  s3types.TransitionDefaultMinimumObjectSize
	none     bool
	puts     []*s3.PutBucketLifecycleConfigurationInput
	deletes  int
	owners   []string
	dropOnRW string // a rule ID the fake silently drops on write, to test verification
}

func (f *fakeLifecycle) GetBucketLifecycleConfiguration(_ context.Context, in *s3.GetBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error) {
	f.owners = append(f.owners, aws.ToString(in.ExpectedBucketOwner))
	if f.none {
		return nil, &smithy.GenericAPIError{Code: "NoSuchLifecycleConfiguration"}
	}
	return &s3.GetBucketLifecycleConfigurationOutput{Rules: f.rules, TransitionDefaultMinimumObjectSize: f.minSize}, nil
}

func (f *fakeLifecycle) PutBucketLifecycleConfiguration(_ context.Context, in *s3.PutBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error) {
	f.owners = append(f.owners, aws.ToString(in.ExpectedBucketOwner))
	f.puts = append(f.puts, in)
	f.rules = nil
	for _, r := range in.LifecycleConfiguration.Rules {
		if aws.ToString(r.ID) != f.dropOnRW {
			f.rules = append(f.rules, r)
		}
	}
	if in.TransitionDefaultMinimumObjectSize != "" {
		f.minSize = in.TransitionDefaultMinimumObjectSize
	}
	f.none = false
	return &s3.PutBucketLifecycleConfigurationOutput{}, nil
}

func (f *fakeLifecycle) DeleteBucketLifecycle(_ context.Context, in *s3.DeleteBucketLifecycleInput, _ ...func(*s3.Options)) (*s3.DeleteBucketLifecycleOutput, error) {
	f.owners = append(f.owners, aws.ToString(in.ExpectedBucketOwner))
	f.deletes++
	f.rules, f.none = nil, true
	return &s3.DeleteBucketLifecycleOutput{}, nil
}

func userRule(id string) s3types.LifecycleRule {
	return s3types.LifecycleRule{ID: aws.String(id), Status: s3types.ExpirationStatusEnabled,
		Filter:     &s3types.LifecycleRuleFilter{Prefix: aws.String("scratch/")},
		Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(7)}}
}

const owner = "123456789012"

// The researcher's own rules survive an apply byte for byte, the bucket's
// minimum-object-size setting is written back as found, and every call names
// the expected owner.
func TestApplyKeepsTheBucketsOtherRulesAndSettings(t *testing.T) {
	f := &fakeLifecycle{rules: []s3types.LifecycleRule{userRule("expire-scratch")},
		minSize: s3types.TransitionDefaultMinimumObjectSizeVariesByStorageClass}
	p, err := PlanLifecycle(context.Background(), f, "b", owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Changed || len(p.Others) != 1 || len(p.Current) != 0 {
		t.Fatalf("plan = changed %v, others %d, current %d", p.Changed, len(p.Others), len(p.Current))
	}
	if err := ApplyLifecycle(context.Background(), f, p, owner); err != nil {
		t.Fatal(err)
	}
	put := f.puts[0]
	if put.TransitionDefaultMinimumObjectSize != s3types.TransitionDefaultMinimumObjectSizeVariesByStorageClass {
		t.Errorf("TransitionDefaultMinimumObjectSize written as %q; the owner's varies_by_storage_class must survive", put.TransitionDefaultMinimumObjectSize)
	}
	if !sameRules(put.LifecycleConfiguration.Rules[:1], []s3types.LifecycleRule{userRule("expire-scratch")}) {
		t.Errorf("the user's rule was not written back unchanged: %+v", put.LifecycleConfiguration.Rules)
	}
	if n := len(put.LifecycleConfiguration.Rules); n != 3 {
		t.Errorf("wrote %d rules, want the user's 1 + spawn's 2", n)
	}
	for _, o := range f.owners {
		if o != owner {
			t.Errorf("a call carried ExpectedBucketOwner %q, want %q", o, owner)
		}
	}
}

func TestApplyIsANoOpWhenAlreadyCurrent(t *testing.T) {
	f := &fakeLifecycle{rules: append([]s3types.LifecycleRule{userRule("u")}, Rules()...)}
	p, err := PlanLifecycle(context.Background(), f, "b", owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Changed {
		t.Errorf("rules already current, but the plan says changed")
	}
	if err := ApplyLifecycle(context.Background(), f, p, owner); err != nil || len(f.puts) != 0 {
		t.Errorf("a no-op apply wrote: err %v, puts %d", err, len(f.puts))
	}
}

func TestPlanWithNoConfigurationAddsTheRules(t *testing.T) {
	f := &fakeLifecycle{none: true}
	p, err := PlanLifecycle(context.Background(), f, "b", owner, false)
	if err != nil || !p.Changed || len(p.Desired) != 2 {
		t.Fatalf("plan = %+v, %v", p, err)
	}
}

// --remove takes spawn's rules out and leaves the rest. With nothing else
// left, the configuration is deleted, since S3 refuses an empty one.
func TestRemove(t *testing.T) {
	f := &fakeLifecycle{rules: append([]s3types.LifecycleRule{userRule("u")}, Rules()...)}
	p, _ := PlanLifecycle(context.Background(), f, "b", owner, true)
	if err := ApplyLifecycle(context.Background(), f, p, owner); err != nil {
		t.Fatal(err)
	}
	if len(f.rules) != 1 || aws.ToString(f.rules[0].ID) != "u" {
		t.Errorf("after remove the bucket has %+v, want only the user's rule", f.rules)
	}
	g := &fakeLifecycle{rules: Rules()}
	p, _ = PlanLifecycle(context.Background(), g, "b", owner, true)
	if err := ApplyLifecycle(context.Background(), g, p, owner); err != nil {
		t.Fatal(err)
	}
	if g.deletes != 1 || len(g.puts) != 0 {
		t.Errorf("removing spawn's only rules: %d deletes, %d puts; want 1 delete", g.deletes, len(g.puts))
	}
}

// A write S3 accepted is not proof: the configuration is read back and
// checked.
func TestApplyVerifiesByReadingBack(t *testing.T) {
	f := &fakeLifecycle{none: true, dropOnRW: RuleIDPrefix + "moderate"}
	p, _ := PlanLifecycle(context.Background(), f, "b", owner, false)
	err := ApplyLifecycle(context.Background(), f, p, owner)
	if err == nil || !strings.Contains(err.Error(), "read back differently") {
		t.Errorf("a rule lost in the write went unnoticed: %v", err)
	}
}

func TestPlanRequiresTheOwner(t *testing.T) {
	if _, err := PlanLifecycle(context.Background(), &fakeLifecycle{}, "b", "", false); err == nil {
		t.Error("planning without an expected owner must fail: a mistyped bucket could be someone else's")
	}
}
