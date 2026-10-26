package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/bucketsweep"
)

// fakeBuckets is every bucket in one region, by name.
type fakeBuckets struct {
	tags    map[string]map[string]string // a bucket with no entry does not exist
	objects map[string]int
	writes  []string
	owners  []string
}

func (f *fakeBuckets) note(call string, owner *string) {
	f.owners = append(f.owners, awssdk.ToString(owner))
	if call != "" {
		f.writes = append(f.writes, call)
	}
}

func (f *fakeBuckets) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	if _, ok := f.tags[awssdk.ToString(in.Bucket)]; !ok {
		return nil, &smithy.GenericAPIError{Code: "NotFound"}
	}
	return &s3.HeadBucketOutput{}, nil
}
func (f *fakeBuckets) GetBucketTagging(_ context.Context, in *s3.GetBucketTaggingInput, _ ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	tags, ok := f.tags[awssdk.ToString(in.Bucket)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchBucket"}
	}
	out := &s3.GetBucketTaggingOutput{}
	for k, v := range tags {
		out.TagSet = append(out.TagSet, types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return out, nil
}
func (f *fakeBuckets) PutBucketTagging(_ context.Context, in *s3.PutBucketTaggingInput, _ ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error) {
	f.note("PutBucketTagging "+awssdk.ToString(in.Bucket), in.ExpectedBucketOwner)
	m := map[string]string{}
	for _, t := range in.Tagging.TagSet {
		m[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
	}
	f.tags[awssdk.ToString(in.Bucket)] = m
	return &s3.PutBucketTaggingOutput{}, nil
}
func (f *fakeBuckets) DeleteBucketTagging(_ context.Context, in *s3.DeleteBucketTaggingInput, _ ...func(*s3.Options)) (*s3.DeleteBucketTaggingOutput, error) {
	f.note("DeleteBucketTagging "+awssdk.ToString(in.Bucket), in.ExpectedBucketOwner)
	f.tags[awssdk.ToString(in.Bucket)] = map[string]string{}
	return &s3.DeleteBucketTaggingOutput{}, nil
}
func (f *fakeBuckets) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	out := &s3.ListObjectVersionsOutput{}
	for range f.objects[awssdk.ToString(in.Bucket)] {
		out.Versions = append(out.Versions, types.ObjectVersion{Key: awssdk.String("k")})
	}
	return out, nil
}
func (f *fakeBuckets) ListMultipartUploads(_ context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	return &s3.ListMultipartUploadsOutput{}, nil
}
func (f *fakeBuckets) GetBucketLifecycleConfiguration(_ context.Context, in *s3.GetBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	return nil, &smithy.GenericAPIError{Code: "NoSuchLifecycleConfiguration"}
}
func (f *fakeBuckets) DeleteBucket(_ context.Context, in *s3.DeleteBucketInput, _ ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	f.note("DeleteBucket "+awssdk.ToString(in.Bucket), in.ExpectedBucketOwner)
	if f.objects[awssdk.ToString(in.Bucket)] > 0 {
		return nil, &smithy.GenericAPIError{Code: "BucketNotEmpty"}
	}
	delete(f.tags, awssdk.ToString(in.Bucket))
	return &s3.DeleteBucketOutput{}, nil
}

const cbAcct = "111122223333"

var cbNow = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func cbRegion() (*fakeBuckets, bucketsweep.Bucket, bucketsweep.Bucket) {
	bs := bucketsweep.Buckets(cbAcct, "us-east-1")
	old := cbNow.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	return &fakeBuckets{
		tags: map[string]map[string]string{
			bs[0].Name: {"spawn:managed": "true", bucketsweep.EmptySinceTagKey: old}, // due
			bs[1].Name: {"spawn:managed": "true"},                                    // first sighting
		},
	}, bs[0], bs[1]
}

func runBuckets(t *testing.T, f *fakeBuckets, dryRun, yes bool) (string, error) {
	t.Helper()
	home := t.TempDir() // the cleanup log goes under ~/.spawn
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	savedDry, savedYes := cleanupDryRun, cleanupYes
	t.Cleanup(func() { cleanupDryRun, cleanupYes = savedDry, savedYes })
	cleanupDryRun, cleanupYes = dryRun, yes
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runCleanupBuckets(context.Background(), cmd, cbAcct, []string{"us-east-1"},
		func(string) bucketsweep.API { return f }, cbNow)
	return out.String(), err
}

func TestCleanupBucketsDryRunWritesNothing(t *testing.T) {
	f, due, fresh := cbRegion()
	out, err := runBuckets(t, f, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Errorf("--dry-run wrote: %v", f.writes)
	}
	if !strings.Contains(out, due.Name) || !strings.Contains(out, "1 empty bucket(s) would be deleted") {
		t.Errorf("dry run output does not name what would be deleted:\n%s", out)
	}
	if !strings.Contains(out, fresh.Name+" (us-east-1): empty; would be tagged") {
		t.Errorf("dry run output does not say the fresh bucket would be stamped:\n%s", out)
	}
}

func TestCleanupBucketsStampsThenDeletesOnlyWhatIsDue(t *testing.T) {
	f, due, fresh := cbRegion()
	if _, err := runBuckets(t, f, false, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.tags[due.Name]; ok {
		t.Errorf("%s, empty for 10 days, was not deleted (writes %v)", due.Name, f.writes)
	}
	if f.tags[fresh.Name][bucketsweep.EmptySinceTagKey] != cbNow.Format(time.RFC3339) {
		t.Errorf("%s, empty for the first time, was not stamped: %v", fresh.Name, f.tags[fresh.Name])
	}
	for _, w := range f.writes {
		if w == "DeleteBucket "+fresh.Name {
			t.Errorf("deleted %s on its first empty sighting", fresh.Name)
		}
	}
	for _, o := range f.owners {
		if o != cbAcct {
			t.Fatalf("a call named owner %q, want %s", o, cbAcct)
		}
	}
}

func TestCleanupBucketsKeepsANonEmptyBucket(t *testing.T) {
	f, due, _ := cbRegion()
	f.objects = map[string]int{due.Name: 1}
	if _, err := runBuckets(t, f, false, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.tags[due.Name]; !ok {
		t.Fatal("deleted a bucket holding an object")
	}
	if _, ok := f.tags[due.Name][bucketsweep.EmptySinceTagKey]; ok {
		t.Error("a bucket holding an object kept its empty stamp")
	}
}

// The plain `spawn cleanup` never offers a bucket for removal: RemoveResource
// cannot delete one, and a bucket may hold data.
func TestPlainCleanupNeverOffersABucket(t *testing.T) {
	found := []aws.ManagedResource{{Service: "s3", ResourceType: "bucket", ID: "spawn-staging-1-us-east-1",
		Tags: map[string]string{"spawn:managed": "true"}}}
	_, _, _, notYet, removable := splitCleanupResources(found)
	if len(removable) != 0 || len(notYet) != 1 {
		t.Errorf("removable = %v, notYet = %v; a bucket must be reported, never removable", idsOf(removable), idsOf(notYet))
	}
}
