package bucketsweep

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	acct   = "123456789012"
	region = "eu-west-1"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// fakeS3 is one bucket. Every call is recorded with the ExpectedBucketOwner it
// sent.
type fakeS3 struct {
	missing    bool
	tags       map[string]string // nil = no tag set
	versions   int
	markers    int
	uploads    int
	lifecycle  int // number of lifecycle rules; 0 = none
	notEmptyAt bool

	calls  []string
	owners []string

	deleted bool
	putTags map[string]string
	cleared bool
}

func (f *fakeS3) rec(call string, owner *string) {
	f.calls = append(f.calls, call)
	f.owners = append(f.owners, aws.ToString(owner))
}

func noSuch(code string) error { return &smithy.GenericAPIError{Code: code} }

func (f *fakeS3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.rec("HeadBucket", in.ExpectedBucketOwner)
	if f.missing {
		return nil, noSuch("NotFound")
	}
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeS3) GetBucketTagging(_ context.Context, in *s3.GetBucketTaggingInput, _ ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	f.rec("GetBucketTagging", in.ExpectedBucketOwner)
	if f.missing {
		return nil, noSuch("NoSuchBucket")
	}
	if f.tags == nil {
		return nil, noSuch("NoSuchTagSet")
	}
	out := &s3.GetBucketTaggingOutput{}
	for k, v := range f.tags {
		out.TagSet = append(out.TagSet, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

func (f *fakeS3) PutBucketTagging(_ context.Context, in *s3.PutBucketTaggingInput, _ ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error) {
	f.rec("PutBucketTagging", in.ExpectedBucketOwner)
	f.putTags = map[string]string{}
	for _, t := range in.Tagging.TagSet {
		f.putTags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.tags = f.putTags
	return &s3.PutBucketTaggingOutput{}, nil
}

func (f *fakeS3) DeleteBucketTagging(_ context.Context, in *s3.DeleteBucketTaggingInput, _ ...func(*s3.Options)) (*s3.DeleteBucketTaggingOutput, error) {
	f.rec("DeleteBucketTagging", in.ExpectedBucketOwner)
	f.cleared, f.tags = true, nil
	return &s3.DeleteBucketTaggingOutput{}, nil
}

func (f *fakeS3) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	f.rec("ListObjectVersions", in.ExpectedBucketOwner)
	out := &s3.ListObjectVersionsOutput{}
	for range f.versions {
		out.Versions = append(out.Versions, types.ObjectVersion{Key: aws.String("k")})
	}
	for range f.markers {
		out.DeleteMarkers = append(out.DeleteMarkers, types.DeleteMarkerEntry{Key: aws.String("k")})
	}
	return out, nil
}

func (f *fakeS3) ListMultipartUploads(_ context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	f.rec("ListMultipartUploads", in.ExpectedBucketOwner)
	out := &s3.ListMultipartUploadsOutput{}
	for range f.uploads {
		out.Uploads = append(out.Uploads, types.MultipartUpload{Key: aws.String("k")})
	}
	return out, nil
}

func (f *fakeS3) GetBucketLifecycleConfiguration(_ context.Context, in *s3.GetBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error) {
	f.rec("GetBucketLifecycleConfiguration", in.ExpectedBucketOwner)
	if f.lifecycle == 0 {
		return nil, noSuch("NoSuchLifecycleConfiguration")
	}
	return &s3.GetBucketLifecycleConfigurationOutput{Rules: make([]types.LifecycleRule, f.lifecycle)}, nil
}

func (f *fakeS3) DeleteBucket(_ context.Context, in *s3.DeleteBucketInput, _ ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	f.rec("DeleteBucket", in.ExpectedBucketOwner)
	if f.notEmptyAt || f.versions+f.markers > 0 {
		return nil, noSuch("BucketNotEmpty")
	}
	f.deleted = true
	return &s3.DeleteBucketOutput{}, nil
}

func (f *fakeS3) called(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (f *fakeS3) wrote() bool {
	return f.called("PutBucketTagging") || f.called("DeleteBucketTagging") || f.called("DeleteBucket")
}

var (
	staging = Buckets(acct, region)[0]
	results = Buckets(acct, region)[1]
	reap    = Options{Account: acct, Now: now, Writes: true, Delete: true}
	report  = Options{Account: acct, Now: now}
)

func stampedDaysAgo(days int) map[string]string {
	return map[string]string{
		ManagedTagKey:    "true",
		"spawn:purpose":  "bootstrap-staging",
		EmptySinceTagKey: now.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339),
	}
}

func TestBucketsAreTheTwoSpawnCreates(t *testing.T) {
	if staging.Name != "spawn-staging-"+acct+"-"+region || staging.Kind != KindStaging {
		t.Errorf("staging = %+v", staging)
	}
	if results.Name != "spawn-results-"+acct+"-"+region || results.Kind != KindResults {
		t.Errorf("results = %+v", results)
	}
}

// A bucket that holds anything is never deleted, however old its stamp, and
// its stamp is removed so the grace restarts when it next empties.
func TestABucketHoldingAnythingIsKeptAndUnstamped(t *testing.T) {
	for name, f := range map[string]*fakeS3{
		"objects":          {versions: 1},
		"versions only":    {versions: 3},
		"delete markers":   {markers: 1},
		"multipart upload": {uploads: 1},
	} {
		t.Run(name, func(t *testing.T) {
			f.tags = stampedDaysAgo(30)
			got := Sweep(context.Background(), f, results, reap)
			if got.Action != InUse {
				t.Fatalf("action = %s (%s), want %s", got.Action, got.Reason, InUse)
			}
			if f.called("DeleteBucket") {
				t.Error("deleted a bucket that is not empty")
			}
			if _, ok := f.putTags[EmptySinceTagKey]; ok || f.putTags == nil {
				t.Errorf("tags written = %v, want the stamp removed and the rest kept", f.putTags)
			}
			if f.putTags[ManagedTagKey] != "true" || f.putTags["spawn:purpose"] != "bootstrap-staging" {
				t.Errorf("removing the stamp dropped other tags: %v", f.putTags)
			}
		})
	}
}

// The staging bucket is never listed (#1274: no role spawn makes may list
// it). S3's refusal to delete a bucket holding anything is its emptiness
// check, and a refusal unstamps it like any bucket in use.
func TestStagingBucketIsNeverListed(t *testing.T) {
	listing := []string{"HeadBucket", "ListObjectVersions", "ListMultipartUploads", "GetBucketLifecycleConfiguration"}
	for name, tc := range map[string]struct {
		f    *fakeS3
		want Action
	}{
		"empty, due":          {&fakeS3{tags: stampedDaysAgo(30)}, Deleted},
		"objects, due":        {&fakeS3{tags: stampedDaysAgo(30), versions: 2}, InUse},
		"delete markers, due": {&fakeS3{tags: stampedDaysAgo(30), markers: 1}, InUse},
		"first sighting":      {&fakeS3{tags: map[string]string{ManagedTagKey: "true"}}, Stamped},
		"waiting":             {&fakeS3{tags: stampedDaysAgo(2)}, Waiting},
		"missing":             {&fakeS3{missing: true}, Absent},
	} {
		t.Run(name, func(t *testing.T) {
			got := Sweep(context.Background(), tc.f, staging, reap)
			if got.Action != tc.want {
				t.Errorf("action = %s (%s), want %s", got.Action, got.Reason, tc.want)
			}
			for _, c := range listing {
				if tc.f.called(c) {
					t.Errorf("called %s on the staging bucket, which no spawn role may list", c)
				}
			}
			if tc.want == InUse {
				if tc.f.deleted {
					t.Error("deleted a staging bucket holding objects")
				}
				if _, ok := tc.f.tags[EmptySinceTagKey]; ok {
					t.Errorf("stamp not removed after S3 refused the delete: %v", tc.f.tags)
				}
			}
		})
	}
}

func TestFirstEmptySightingStampsAndDeletesNothing(t *testing.T) {
	f := &fakeS3{tags: map[string]string{ManagedTagKey: "true"}}
	got := Sweep(context.Background(), f, staging, reap)
	if got.Action != Stamped {
		t.Fatalf("action = %s (%s), want %s", got.Action, got.Reason, Stamped)
	}
	if f.called("DeleteBucket") {
		t.Error("deleted a bucket on its first empty sighting")
	}
	if f.putTags[EmptySinceTagKey] != now.Format(time.RFC3339) || f.putTags[ManagedTagKey] != "true" {
		t.Errorf("tags written = %v, want spawn:managed kept and spawn:empty-since=%s", f.putTags, now.Format(time.RFC3339))
	}
}

func TestDeletedOnlyPastTheGrace(t *testing.T) {
	for _, tc := range []struct {
		days int
		want Action
	}{
		{0, Waiting},
		{6, Waiting},
		{7, Deleted},
		{40, Deleted},
	} {
		f := &fakeS3{tags: stampedDaysAgo(tc.days)}
		got := Sweep(context.Background(), f, staging, reap)
		if got.Action != tc.want {
			t.Errorf("empty %d days: action = %s (%s), want %s", tc.days, got.Action, got.Reason, tc.want)
		}
		if f.deleted != (tc.want == Deleted) {
			t.Errorf("empty %d days: deleted = %t", tc.days, f.deleted)
		}
	}
	// A stamp in the future (clock skew, or a hand-set tag) restarts the grace
	// rather than counting as old.
	f := &fakeS3{tags: stampedDaysAgo(-30)}
	if got := Sweep(context.Background(), f, staging, reap); got.Action != Stamped || f.deleted {
		t.Errorf("future stamp: action = %s, deleted = %t", got.Action, f.deleted)
	}
}

// Untagged, foreign or otherwise not spawn's: never deleted, never stamped.
func TestNotSpawnsIsKeptUntouched(t *testing.T) {
	for name, tc := range map[string]struct {
		b Bucket
		f *fakeS3
	}{
		"untagged staging":               {staging, &fakeS3{}},
		"staging tagged managed=false":   {staging, &fakeS3{tags: map[string]string{ManagedTagKey: "false"}}},
		"results tagged by someone else": {results, &fakeS3{tags: map[string]string{"team": "genomics"}}},
		"another account's name": {Bucket{Name: "spawn-results-999999999999-" + region, Region: region, Kind: KindResults},
			&fakeS3{tags: map[string]string{ManagedTagKey: "true"}}},
		"a legacy region-only name": {Bucket{Name: "spawn-results-" + region, Region: region, Kind: KindResults}, &fakeS3{}},
		"right name, wrong region":  {Bucket{Name: results.Name, Region: "us-east-1", Kind: KindResults}, &fakeS3{}},
		"right name, wrong kind":    {Bucket{Name: staging.Name, Region: region, Kind: KindResults}, &fakeS3{}},
	} {
		t.Run(name, func(t *testing.T) {
			tc.f.tags = func() map[string]string {
				if tc.f.tags == nil {
					return nil
				}
				m := tc.f.tags
				m[EmptySinceTagKey] = now.Add(-60 * 24 * time.Hour).Format(time.RFC3339)
				return m
			}()
			got := Sweep(context.Background(), tc.f, tc.b, reap)
			if got.Action != Kept {
				t.Errorf("action = %s (%s), want %s", got.Action, got.Reason, Kept)
			}
			if tc.f.wrote() {
				t.Errorf("wrote to a bucket that is not spawn's: %v", tc.f.calls)
			}
		})
	}
}

// The exception: an UNTAGGED results bucket on spawn's exact name, owned by the
// account (every call names it), is spawn's — its creation-time tagging failed.
func TestUntaggedResultsBucketOnTheExactNameIsSpawns(t *testing.T) {
	f := &fakeS3{}
	if got := Sweep(context.Background(), f, results, reap); got.Action != Stamped {
		t.Fatalf("first sweep: action = %s (%s), want %s", got.Action, got.Reason, Stamped)
	}
	// Its only tag is now the stamp, which must not make it "tagged by someone".
	f.tags[EmptySinceTagKey] = now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)
	if got := Sweep(context.Background(), f, results, reap); got.Action != Deleted {
		t.Fatalf("past the grace: action = %s (%s), want %s", got.Action, got.Reason, Deleted)
	}
	// And unstamping it, when it fills, leaves no tag set behind.
	g := &fakeS3{tags: map[string]string{EmptySinceTagKey: now.Format(time.RFC3339)}, versions: 1}
	if got := Sweep(context.Background(), g, results, reap); got.Action != InUse || !g.cleared {
		t.Errorf("non-empty untagged: action = %s, tag set removed = %t", got.Action, g.cleared)
	}
}

func TestResultsBucketWithLifecycleRulesIsKept(t *testing.T) {
	f := &fakeS3{tags: stampedDaysAgo(30), lifecycle: 2}
	got := Sweep(context.Background(), f, results, reap)
	if got.Action != Kept || !strings.Contains(got.Reason, "lifecycle") {
		t.Errorf("action = %s (%s), want kept for its lifecycle rules", got.Action, got.Reason)
	}
	if f.deleted {
		t.Error("deleted a results bucket carrying tiering rules")
	}
	// The staging bucket's lifecycle is spawn's own, re-applied on re-creation.
	g := &fakeS3{tags: stampedDaysAgo(30), lifecycle: 1}
	if got := Sweep(context.Background(), g, staging, reap); got.Action != Deleted {
		t.Errorf("staging: action = %s (%s), want deleted", got.Action, got.Reason)
	}
}

func TestFSxConfigBucketIsKept(t *testing.T) {
	tags := stampedDaysAgo(30)
	tags[FSxStackTagKey] = "my-stack"
	f := &fakeS3{tags: tags}
	if got := Sweep(context.Background(), f, results, reap); got.Action != Kept || f.deleted {
		t.Errorf("action = %s (%s), deleted = %t", got.Action, got.Reason, f.deleted)
	}
}

// Without Writes the sweep changes nothing — report mode and --dry-run.
func TestReportChangesNothing(t *testing.T) {
	for name, f := range map[string]*fakeS3{
		"would stamp":   {tags: map[string]string{ManagedTagKey: "true"}},
		"would unstamp": {tags: stampedDaysAgo(3), versions: 1},
		"would delete":  {tags: stampedDaysAgo(30)},
	} {
		t.Run(name, func(t *testing.T) {
			got := Sweep(context.Background(), f, staging, report)
			if f.wrote() {
				t.Errorf("report wrote: %v", f.calls)
			}
			if name == "would delete" && got.Action != WouldDelete {
				t.Errorf("action = %s, want %s", got.Action, WouldDelete)
			}
		})
	}
	// Writes without Delete stamps but never deletes (the CLI's first pass).
	f := &fakeS3{tags: stampedDaysAgo(30)}
	if got := Sweep(context.Background(), f, staging, Options{Account: acct, Now: now, Writes: true}); got.Action != WouldDelete || f.deleted {
		t.Errorf("writes only: action = %s, deleted = %t", got.Action, f.deleted)
	}
}

// Every S3 call names the owning account, so a bucket of spawn's name that
// belongs to someone else is refused by S3 rather than read or deleted.
func TestEveryCallNamesTheOwner(t *testing.T) {
	cases := []*fakeS3{
		{tags: stampedDaysAgo(30)},
		{tags: stampedDaysAgo(30), versions: 1},
		{tags: map[string]string{ManagedTagKey: "true"}},
		{},
	}
	for _, b := range []Bucket{staging, results} {
		for _, f := range cases {
			cp := *f
			if f.tags != nil {
				cp.tags = map[string]string{}
				for k, v := range f.tags {
					cp.tags[k] = v
				}
			}
			Sweep(context.Background(), &cp, b, reap)
			if len(cp.calls) == 0 {
				t.Fatal("no calls recorded")
			}
			for i, o := range cp.owners {
				if o != acct {
					t.Errorf("%s %s: ExpectedBucketOwner = %q, want %s", b.Kind, cp.calls[i], o, acct)
				}
			}
		}
	}
	if got := Sweep(context.Background(), &fakeS3{}, staging, Options{Now: now, Writes: true, Delete: true}); got.Action != Failed {
		t.Errorf("no account: action = %s, want %s", got.Action, Failed)
	}
}

func TestAbsentAndRacedBuckets(t *testing.T) {
	if got := Sweep(context.Background(), &fakeS3{missing: true}, staging, reap); got.Action != Absent {
		t.Errorf("missing: action = %s", got.Action)
	}
	f := &fakeS3{tags: stampedDaysAgo(30), notEmptyAt: true}
	if got := Sweep(context.Background(), f, staging, reap); got.Action != InUse || f.deleted {
		t.Errorf("BucketNotEmpty on delete: action = %s, deleted = %t", got.Action, f.deleted)
	}
}

func TestClearEmptySince(t *testing.T) {
	f := &fakeS3{tags: stampedDaysAgo(3)}
	if err := ClearEmptySince(context.Background(), f, staging.Name, acct); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.putTags[EmptySinceTagKey]; ok || f.putTags[ManagedTagKey] != "true" {
		t.Errorf("tags written = %v, want the stamp gone and the rest kept", f.putTags)
	}
	g := &fakeS3{tags: map[string]string{ManagedTagKey: "true"}}
	if err := ClearEmptySince(context.Background(), g, staging.Name, acct); err != nil || g.wrote() {
		t.Errorf("unstamped bucket: err = %v, calls = %v", err, g.calls)
	}
}
