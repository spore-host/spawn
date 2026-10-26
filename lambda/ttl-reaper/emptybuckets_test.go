package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/bucketsweep"
)

// fakeBucketS3 holds one region's buckets by name; a name with no entry does
// not exist.
type fakeBucketS3 struct {
	tags    map[string]map[string]string
	objects map[string]int
	denied  bool
	writes  []string
	owners  []string
}

func (f *fakeBucketS3) note(write string, owner *string) {
	f.owners = append(f.owners, aws.ToString(owner))
	if write != "" {
		f.writes = append(f.writes, write)
	}
}

func (f *fakeBucketS3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	if f.denied {
		return nil, &smithy.GenericAPIError{Code: "Forbidden"}
	}
	if _, ok := f.tags[aws.ToString(in.Bucket)]; !ok {
		return nil, &smithy.GenericAPIError{Code: "NotFound"}
	}
	return &s3.HeadBucketOutput{}, nil
}
func (f *fakeBucketS3) GetBucketTagging(_ context.Context, in *s3.GetBucketTaggingInput, _ ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	if f.denied {
		return nil, &smithy.GenericAPIError{Code: "AccessDenied"}
	}
	tags, ok := f.tags[aws.ToString(in.Bucket)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchBucket"}
	}
	out := &s3.GetBucketTaggingOutput{}
	for k, v := range tags {
		out.TagSet = append(out.TagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}
func (f *fakeBucketS3) PutBucketTagging(_ context.Context, in *s3.PutBucketTaggingInput, _ ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error) {
	f.note("PutBucketTagging "+aws.ToString(in.Bucket), in.ExpectedBucketOwner)
	m := map[string]string{}
	for _, t := range in.Tagging.TagSet {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.tags[aws.ToString(in.Bucket)] = m
	return &s3.PutBucketTaggingOutput{}, nil
}
func (f *fakeBucketS3) DeleteBucketTagging(_ context.Context, in *s3.DeleteBucketTaggingInput, _ ...func(*s3.Options)) (*s3.DeleteBucketTaggingOutput, error) {
	f.note("DeleteBucketTagging "+aws.ToString(in.Bucket), in.ExpectedBucketOwner)
	f.tags[aws.ToString(in.Bucket)] = map[string]string{}
	return &s3.DeleteBucketTaggingOutput{}, nil
}
func (f *fakeBucketS3) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	out := &s3.ListObjectVersionsOutput{}
	for range f.objects[aws.ToString(in.Bucket)] {
		out.Versions = append(out.Versions, s3types.ObjectVersion{Key: aws.String("k")})
	}
	return out, nil
}
func (f *fakeBucketS3) ListMultipartUploads(_ context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	return &s3.ListMultipartUploadsOutput{}, nil
}
func (f *fakeBucketS3) GetBucketLifecycleConfiguration(_ context.Context, in *s3.GetBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error) {
	f.note("", in.ExpectedBucketOwner)
	return nil, &smithy.GenericAPIError{Code: "NoSuchLifecycleConfiguration"}
}
func (f *fakeBucketS3) DeleteBucket(_ context.Context, in *s3.DeleteBucketInput, _ ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	f.note("DeleteBucket "+aws.ToString(in.Bucket), in.ExpectedBucketOwner)
	if f.objects[aws.ToString(in.Bucket)] > 0 {
		return nil, &smithy.GenericAPIError{Code: "BucketNotEmpty"}
	}
	delete(f.tags, aws.ToString(in.Bucket))
	return &s3.DeleteBucketOutput{}, nil
}

const ebAcct = "111111111111"

var ebNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// ebBuckets: staging empty for 10 days (due), results empty and unstamped.
func ebBuckets() (*fakeBucketS3, string, string) {
	bs := bucketsweep.Buckets(ebAcct, "us-east-1")
	return &fakeBucketS3{tags: map[string]map[string]string{
		bs[0].Name: {"spawn:managed": "true", bucketsweep.EmptySinceTagKey: ebNow.Add(-10 * 24 * time.Hour).Format(time.RFC3339)},
		bs[1].Name: {"spawn:managed": "true"},
	}}, bs[0].Name, bs[1].Name
}

func ebReaper(f *fakeBucketS3, mode string, dryRun bool) *reaper {
	return &reaper{
		accounts: []account{{
			label: "acct", accountID: ebAcct,
			ec2For: func(string) ec2API { return emptyEC2{} },
			s3For:  func(string) s3API { return f },
		}},
		regions:      []string{"us-east-1"},
		maxAge:       defaultMaxAge,
		dryRun:       dryRun,
		emptyBuckets: mode,
	}
}

func ebSweep(f *fakeBucketS3, mode string, dryRun bool) (Summary, string) {
	r := ebReaper(f, mode, dryRun)
	var sum Summary
	out := captureLogs(func() {
		r.reapEmptyBucketsRegion(context.Background(), r.accounts[0], "us-east-1", ebNow, &sum)
	})
	return sum, out
}

func TestEmptyBuckets_ReapStampsAndDeletes(t *testing.T) {
	f, due, fresh := ebBuckets()
	sum, out := ebSweep(f, emptyBucketsReap, false)
	if _, ok := f.tags[due]; ok {
		t.Errorf("%s, empty 10 days, not deleted:\n%s", due, out)
	}
	if f.tags[fresh][bucketsweep.EmptySinceTagKey] != ebNow.Format(time.RFC3339) {
		t.Errorf("%s not stamped on its first empty sighting: %v", fresh, f.tags[fresh])
	}
	if sum.EmptyBucketsScanned != 2 || sum.EmptyBucketsDeleted != 1 || sum.EmptyBucketsStamped != 1 || sum.EmptyBucketsWouldDelete != 0 {
		t.Errorf("summary = %+v", sum)
	}
	if !strings.Contains(out, "DELETED "+due) {
		t.Errorf("no DELETED line for %s:\n%s", due, out)
	}
	for _, o := range f.owners {
		if o != ebAcct {
			t.Fatalf("a call named owner %q, want %s", o, ebAcct)
		}
	}
}

// Report mode, and a dry run whatever the mode, write nothing — not even the
// stamp — and say what reap would do.
func TestEmptyBuckets_ReportAndDryRunChangeNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		mode   string
		dryRun bool
	}{
		"report":         {emptyBucketsReport, false},
		"dry run + reap": {emptyBucketsReap, true},
	} {
		t.Run(name, func(t *testing.T) {
			f, due, fresh := ebBuckets()
			sum, out := ebSweep(f, tc.mode, tc.dryRun)
			if len(f.writes) != 0 {
				t.Errorf("wrote: %v", f.writes)
			}
			if sum.EmptyBucketsWouldDelete != 1 || sum.EmptyBucketsDeleted != 0 || sum.EmptyBucketsStamped != 0 {
				t.Errorf("summary = %+v", sum)
			}
			if !strings.Contains(out, "WOULD delete "+due) || !strings.Contains(out, fresh+" is empty; WOULD stamp") {
				t.Errorf("report does not say what reap would do:\n%s", out)
			}
		})
	}
}

func TestEmptyBuckets_OffMakesNoCalls(t *testing.T) {
	f, _, _ := ebBuckets()
	ebSweep(f, emptyBucketsOff, false)
	if len(f.owners) != 0 {
		t.Errorf("off made %d S3 calls", len(f.owners))
	}
}

func TestEmptyBuckets_NonEmptyIsKept(t *testing.T) {
	f, due, _ := ebBuckets()
	f.objects = map[string]int{due: 2}
	sum, _ := ebSweep(f, emptyBucketsReap, false)
	if _, ok := f.tags[due]; !ok || sum.EmptyBucketsDeleted != 0 {
		t.Fatalf("deleted a bucket holding objects (summary %+v)", sum)
	}
	if _, ok := f.tags[due][bucketsweep.EmptySinceTagKey]; ok {
		t.Error("its stamp was not removed")
	}
}

// A role without the #1301 grants is counted and logged with what to redeploy,
// and is not an operational error.
func TestEmptyBuckets_DeniedSaysWhatToRedeploy(t *testing.T) {
	f, _, _ := ebBuckets()
	f.denied = true
	sum, out := ebSweep(f, emptyBucketsReap, false)
	if sum.EmptyBucketsFailed != 2 || sum.Errors != 0 {
		t.Errorf("summary = %+v, want 2 failed and no operational errors", sum)
	}
	if !strings.Contains(out, "redeploy the spawn-ttl-reaper-ec2 stack") {
		t.Errorf("denied line does not say what to redeploy:\n%s", out)
	}
}

// Through run(): the sweep runs per account and region after the instance scan.
func TestRun_EmptyBucketSweepRuns(t *testing.T) {
	f, _, _ := ebBuckets()
	var sum Summary
	var err error
	captureLogs(func() { sum, err = ebReaper(f, emptyBucketsReap, false).run(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	if sum.EmptyBucketsScanned != 2 {
		t.Errorf("run() swept %d buckets, want 2", sum.EmptyBucketsScanned)
	}
}

func TestParseEmptyBuckets(t *testing.T) {
	for in, want := range map[string]string{
		"": emptyBucketsReport, "report": emptyBucketsReport, "REAP": emptyBucketsReap, " off ": emptyBucketsOff,
		"reaap": emptyBucketsReport, "true": emptyBucketsReport,
	} {
		if got := parseEmptyBuckets(in); got != want {
			t.Errorf("parseEmptyBuckets(%q) = %q, want %q", in, got, want)
		}
	}
}
