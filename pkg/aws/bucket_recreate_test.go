package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/bucketsweep"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// fakeCreateS3 is createBucketIfNotExists's S3.
type fakeCreateS3 struct {
	headErr      error
	existingTags []types.Tag
	calls        []string
	created      *s3.CreateBucketInput
	tags         []types.Tag
}

func (f *fakeCreateS3) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.calls = append(f.calls, "HeadBucket")
	return &s3.HeadBucketOutput{}, f.headErr
}
func (f *fakeCreateS3) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.calls = append(f.calls, "CreateBucket")
	f.created = in
	return &s3.CreateBucketOutput{}, nil
}
func (f *fakeCreateS3) PutBucketTagging(_ context.Context, in *s3.PutBucketTaggingInput, _ ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error) {
	f.calls = append(f.calls, "PutBucketTagging")
	f.tags = in.Tagging.TagSet
	return &s3.PutBucketTaggingOutput{}, nil
}
func (f *fakeCreateS3) GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	f.calls = append(f.calls, "GetBucketTagging")
	if f.existingTags == nil {
		return nil, &smithy.GenericAPIError{Code: "NoSuchTagSet"}
	}
	return &s3.GetBucketTaggingOutput{TagSet: f.existingTags}, nil
}
func (f *fakeCreateS3) DeleteBucketTagging(context.Context, *s3.DeleteBucketTaggingInput, ...func(*s3.Options)) (*s3.DeleteBucketTaggingOutput, error) {
	f.calls = append(f.calls, "DeleteBucketTagging")
	return &s3.DeleteBucketTaggingOutput{}, nil
}

func tagMapOf(tags []types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// TestResultsBucketIsRecreatedAsFirstMade: the empty-bucket sweep (#1301) may
// delete spawn-results-<account>-<region>, because `spawn task run` re-creates
// it on next use. Re-created, it must be what it was: in its region, tagged
// spawn's (so the sweep recognises it again) with its purpose, and claiming
// nothing it is not — it used to be tagged as an FSx backing bucket.
func TestResultsBucketIsRecreatedAsFirstMade(t *testing.T) {
	bucket := taskproto.DefaultResultsBucket(stOwner, stRegion)
	f := &fakeCreateS3{headErr: &smithy.GenericAPIError{Code: "NotFound"}}
	if err := createPlainBucket(context.Background(), f, bucket, stRegion); err != nil {
		t.Fatal(err)
	}
	if f.created == nil || aws.ToString(f.created.Bucket) != bucket ||
		f.created.CreateBucketConfiguration == nil || f.created.CreateBucketConfiguration.LocationConstraint != types.BucketLocationConstraint(stRegion) {
		t.Fatalf("CreateBucket = %+v", f.created)
	}
	// Nothing on the create call overrides S3's defaults for a new bucket
	// (Block Public Access on, BucketOwnerEnforced, SSE-S3).
	if f.created.ACL != "" || f.created.ObjectOwnership != "" {
		t.Errorf("CreateBucket overrides S3's defaults: ACL %q, ownership %q", f.created.ACL, f.created.ObjectOwnership)
	}
	tags := tagMapOf(f.tags)
	if tags["spawn:managed"] != "true" || tags["spawn:purpose"] != resultsBucketPurpose {
		t.Errorf("tags = %v, want spawn:managed=true and spawn:purpose=%s", tags, resultsBucketPurpose)
	}
	if _, ok := tags["spawn:fsx-backing-bucket"]; ok {
		t.Errorf("the results bucket is tagged as an FSx backing bucket: %v", tags)
	}
}

// TestReusingASweptBucketClearsItsEmptyStamp: a launch about to use a bucket
// the sweep may delete takes off spawn:empty-since, keeping every other tag.
func TestReusingASweptBucketClearsItsEmptyStamp(t *testing.T) {
	bucket := taskproto.DefaultResultsBucket(stOwner, stRegion)
	f := &fakeCreateS3{existingTags: []types.Tag{
		{Key: aws.String("spawn:managed"), Value: aws.String("true")},
		{Key: aws.String(bucketsweep.EmptySinceTagKey), Value: aws.String("2026-10-01T00:00:00Z")},
	}}
	if err := createPlainBucket(context.Background(), f, bucket, stRegion); err != nil {
		t.Fatal(err)
	}
	tags := tagMapOf(f.tags)
	if _, ok := tags[bucketsweep.EmptySinceTagKey]; ok || tags["spawn:managed"] != "true" {
		t.Errorf("tags after reuse = %v (calls %v), want the stamp gone and spawn:managed kept", tags, f.calls)
	}

	// A bucket the sweep never looks at is not touched.
	g := &fakeCreateS3{}
	if err := createPlainBucket(context.Background(), g, "my-lab-data", stRegion); err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 1 {
		t.Errorf("calls on an existing non-spawn bucket = %v, want only HeadBucket", g.calls)
	}
}

func TestEnsureStagingBucketClearsItsEmptyStamp(t *testing.T) {
	f := &fakeStagingS3{lifecycle: stagingLifecycleRules(), existingTags: []types.Tag{
		{Key: aws.String("spawn:managed"), Value: aws.String("true")},
		{Key: aws.String("spawn:purpose"), Value: aws.String("bootstrap-staging")},
		{Key: aws.String(bucketsweep.EmptySinceTagKey), Value: aws.String("2026-10-01T00:00:00Z")},
	}}
	if err := ensureStagingBucket(context.Background(), f, stBucket, stRegion, stOwner); err != nil {
		t.Fatal(err)
	}
	tags := tagMapOf(f.tags)
	if _, ok := tags[bucketsweep.EmptySinceTagKey]; ok || tags["spawn:purpose"] != "bootstrap-staging" || tags["spawn:managed"] != "true" {
		t.Errorf("tags after ensure = %v (calls %v), want the stamp gone and the rest kept", tags, f.calls)
	}
	for i, o := range f.owners {
		if o != stOwner {
			t.Errorf("%s did not name the owning account", f.calls[i])
		}
	}
}

// TestRecreatedBucketsAreSpawnsToTheSweep: what each creator tags a bucket
// with is what the sweep requires, so a bucket it deletes and spawn re-creates
// is cleaned up again the next time it sits empty.
func TestRecreatedBucketsAreSpawnsToTheSweep(t *testing.T) {
	staging := &fakeStagingS3{headErr: &smithy.GenericAPIError{Code: "NotFound"}}
	if err := ensureStagingBucket(context.Background(), staging, stBucket, stRegion, stOwner); err != nil {
		t.Fatal(err)
	}
	results := taskproto.DefaultResultsBucket(stOwner, stRegion)
	for name, tags := range map[string]map[string]string{
		stBucket: tagMapOf(staging.tags),
		results:  tagMapOf(plainBucketTags(results)),
	} {
		if tags[bucketsweep.ManagedTagKey] != "true" {
			t.Errorf("%s is created with tags %v, which the sweep does not recognise as spawn's", name, tags)
		}
	}
}

// TestFootprintShowsTheEmptyStamp: `spawn footprint` lists spawn's buckets by
// name; for one the sweep is aging, it says since when and when it goes.
func TestFootprintShowsTheEmptyStamp(t *testing.T) {
	got := emptySinceDetail(map[string]string{bucketsweep.EmptySinceTagKey: "2026-10-01T00:00:00Z"})
	if !strings.Contains(got, "empty since 2026-10-01T00:00:00Z") || !strings.Contains(got, "2026-10-08") {
		t.Errorf("detail = %q, want the stamp and the date it becomes deletable", got)
	}
	if got := emptySinceDetail(map[string]string{"spawn:managed": "true"}); got != "" {
		t.Errorf("detail for an unstamped bucket = %q, want empty", got)
	}
	for _, name := range []string{taskproto.StagingBucket(stOwner, stRegion), taskproto.DefaultResultsBucket(stOwner, stRegion)} {
		if !bucketsweep.IsSweptName(name) || !matchesFootprintName(name) {
			t.Errorf("%s is not both swept and in the footprint", name)
		}
	}
}
