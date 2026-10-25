package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// fakeStagingS3 records every call EnsureStagingBucket makes.
type fakeStagingS3 struct {
	headErr   error
	lifecycle []types.LifecycleRule // nil = no lifecycle configuration
	calls     []string
	owners    []string

	created    *s3.CreateBucketInput
	bpa        *types.PublicAccessBlockConfiguration
	ownership  *types.OwnershipControls
	encryption *types.ServerSideEncryptionConfiguration
	rules      []types.LifecycleRule
	tags       []types.Tag
}

func (f *fakeStagingS3) rec(call string, owner *string) {
	f.calls = append(f.calls, call)
	f.owners = append(f.owners, aws.ToString(owner))
}

func (f *fakeStagingS3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.rec("HeadBucket", in.ExpectedBucketOwner)
	return &s3.HeadBucketOutput{}, f.headErr
}
func (f *fakeStagingS3) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.calls = append(f.calls, "CreateBucket")
	f.created = in
	return &s3.CreateBucketOutput{}, nil
}
func (f *fakeStagingS3) PutPublicAccessBlock(_ context.Context, in *s3.PutPublicAccessBlockInput, _ ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error) {
	f.rec("PutPublicAccessBlock", in.ExpectedBucketOwner)
	f.bpa = in.PublicAccessBlockConfiguration
	return &s3.PutPublicAccessBlockOutput{}, nil
}
func (f *fakeStagingS3) PutBucketEncryption(_ context.Context, in *s3.PutBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error) {
	f.rec("PutBucketEncryption", in.ExpectedBucketOwner)
	f.encryption = in.ServerSideEncryptionConfiguration
	return &s3.PutBucketEncryptionOutput{}, nil
}
func (f *fakeStagingS3) PutBucketOwnershipControls(_ context.Context, in *s3.PutBucketOwnershipControlsInput, _ ...func(*s3.Options)) (*s3.PutBucketOwnershipControlsOutput, error) {
	f.rec("PutBucketOwnershipControls", in.ExpectedBucketOwner)
	f.ownership = in.OwnershipControls
	return &s3.PutBucketOwnershipControlsOutput{}, nil
}
func (f *fakeStagingS3) PutBucketLifecycleConfiguration(_ context.Context, in *s3.PutBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error) {
	f.rec("PutBucketLifecycleConfiguration", in.ExpectedBucketOwner)
	f.rules = in.LifecycleConfiguration.Rules
	return &s3.PutBucketLifecycleConfigurationOutput{}, nil
}
func (f *fakeStagingS3) GetBucketLifecycleConfiguration(_ context.Context, in *s3.GetBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error) {
	f.rec("GetBucketLifecycleConfiguration", in.ExpectedBucketOwner)
	if f.lifecycle == nil {
		return nil, &smithy.GenericAPIError{Code: "NoSuchLifecycleConfiguration"}
	}
	return &s3.GetBucketLifecycleConfigurationOutput{Rules: f.lifecycle}, nil
}
func (f *fakeStagingS3) PutBucketTagging(_ context.Context, in *s3.PutBucketTaggingInput, _ ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error) {
	f.rec("PutBucketTagging", in.ExpectedBucketOwner)
	f.tags = in.Tagging.TagSet
	return &s3.PutBucketTaggingOutput{}, nil
}

const (
	stOwner  = "123456789012"
	stRegion = "eu-west-1"
)

var stBucket = taskproto.StagingBucket(stOwner, stRegion)

// assertConfigured checks the documented configuration of a staging bucket:
// all four Block Public Access settings, SSE-S3, BucketOwnerEnforced, every
// object (and incomplete upload) gone after a day, and the inventory tags.
func assertConfigured(t *testing.T, f *fakeStagingS3) {
	t.Helper()
	b := f.bpa
	if b == nil || !aws.ToBool(b.BlockPublicAcls) || !aws.ToBool(b.IgnorePublicAcls) || !aws.ToBool(b.BlockPublicPolicy) || !aws.ToBool(b.RestrictPublicBuckets) {
		t.Errorf("Block Public Access not fully on: %+v", b)
	}
	if f.ownership == nil || len(f.ownership.Rules) != 1 || f.ownership.Rules[0].ObjectOwnership != types.ObjectOwnershipBucketOwnerEnforced {
		t.Errorf("object ownership = %+v, want BucketOwnerEnforced", f.ownership)
	}
	if e := f.encryption; e == nil || len(e.Rules) != 1 || e.Rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm != types.ServerSideEncryptionAes256 {
		t.Errorf("default encryption = %+v, want SSE-S3", e)
	}
	if len(f.rules) != 1 {
		t.Fatalf("lifecycle rules = %+v, want one", f.rules)
	}
	r := f.rules[0]
	if r.Status != types.ExpirationStatusEnabled || aws.ToInt32(r.Expiration.Days) != 1 || aws.ToInt32(r.AbortIncompleteMultipartUpload.DaysAfterInitiation) != 1 ||
		r.Filter == nil || aws.ToString(r.Filter.Prefix) != "" {
		t.Errorf("lifecycle rule = %+v, want every object expired and every incomplete upload aborted after 1 day", r)
	}
	tags := map[string]string{}
	for _, tg := range f.tags {
		tags[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
	}
	if tags["spawn:managed"] != "true" {
		t.Errorf("tags = %v, want spawn:managed=true", tags)
	}
	for i, o := range f.owners {
		if o != stOwner {
			t.Errorf("%s did not require the owning account (ExpectedBucketOwner %q)", f.calls[i], o)
		}
	}
}

func TestEnsureStagingBucketCreatesItConfigured(t *testing.T) {
	f := &fakeStagingS3{headErr: &smithy.GenericAPIError{Code: "NotFound"}}
	if err := ensureStagingBucket(context.Background(), f, stBucket, stRegion, stOwner); err != nil {
		t.Fatal(err)
	}
	if f.created == nil || aws.ToString(f.created.Bucket) != stBucket || f.created.ObjectOwnership != types.ObjectOwnershipBucketOwnerEnforced ||
		f.created.CreateBucketConfiguration == nil || f.created.CreateBucketConfiguration.LocationConstraint != types.BucketLocationConstraint(stRegion) {
		t.Errorf("CreateBucket = %+v", f.created)
	}
	assertConfigured(t, f)
}

func TestEnsureStagingBucketReappliesAMissingOrChangedRule(t *testing.T) {
	good := stagingLifecycleRules()
	longer := stagingLifecycleRules()
	longer[0].Expiration = &types.LifecycleExpiration{Days: aws.Int32(30)}
	disabled := stagingLifecycleRules()
	disabled[0].Status = types.ExpirationStatusDisabled
	scoped := stagingLifecycleRules()
	scoped[0].Filter = &types.LifecycleRuleFilter{Prefix: aws.String("tmp/")}
	for name, tc := range map[string]struct {
		rules     []types.LifecycleRule
		reapplies bool
	}{
		"rule in place":    {good, false},
		"no lifecycle":     {nil, true},
		"expires too late": {longer, true},
		"rule disabled":    {disabled, true},
		"rule scoped":      {scoped, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeStagingS3{lifecycle: tc.rules}
			if err := ensureStagingBucket(context.Background(), f, stBucket, stRegion, stOwner); err != nil {
				t.Fatal(err)
			}
			if f.created != nil {
				t.Error("created a bucket that exists")
			}
			if !tc.reapplies {
				if f.rules != nil || f.bpa != nil {
					t.Errorf("reconfigured a correctly configured bucket: %v", f.calls)
				}
				return
			}
			assertConfigured(t, f)
		})
	}
}

func TestEnsureStagingBucketRefusesABucketItDoesNotOwn(t *testing.T) {
	for name, err := range map[string]error{
		"another account's (403)": &smithy.GenericAPIError{Code: "Forbidden"},
		"other failure":           errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeStagingS3{headErr: err}
			got := ensureStagingBucket(context.Background(), f, stBucket, stRegion, stOwner)
			if got == nil || !strings.Contains(got.Error(), "not usable") {
				t.Fatalf("err = %v, want a refusal", got)
			}
			if len(f.calls) != 1 {
				t.Errorf("went on to call %v on a bucket it does not own", f.calls[1:])
			}
		})
	}
	if err := ensureStagingBucket(context.Background(), &fakeStagingS3{}, stBucket, stRegion, ""); err == nil {
		t.Error("ensured a staging bucket without an owning account to check")
	}
}

// TestStagingBucketIsInTheFootprint: `spawn footprint` finds spore.host
// buckets by name, so the staging bucket must match its name rules.
func TestStagingBucketIsInTheFootprint(t *testing.T) {
	if !matchesFootprintName(taskproto.StagingBucket(stOwner, stRegion)) {
		t.Errorf("%s is not matched by the footprint inventory", taskproto.StagingBucket(stOwner, stRegion))
	}
}
