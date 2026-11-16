package staging

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

// The schedules bucket this resolves is now LOAD-BEARING (#804, #805).
//
// resolveSchedulesBucket prefers the shared `spawn-schedules-<region>` and falls
// back to an account-scoped `spawn-schedules-<account>-<region>` when the shared
// name is taken by another account. #804 made the caller persist which one it
// chose, because the scheduler-handler used to rebuild the name from the region
// and so read a bucket it had never written to in any account on the fallback
// path.
//
// This file replaces staging_test.go, whose five tests covered only
// UploadToPrimaryRegion / RecordMetadata / GetMetadata / ListStagedData /
// DeleteStaging — all of which #805 removed with `spawn stage`. That left the
// live half of this package, the half #804 had just made consequential, with no
// behavioural coverage at all. Swapping dead-code coverage for live-code coverage
// is the point of doing both in one change.
func newStagingClient(t *testing.T, accountID string) (*Client, *s3.Client) {
	t.Helper()
	env := testutil.SubstrateServer(t)
	return NewClient(env.AWSConfig, accountID), s3.NewFromConfig(env.AWSConfig)
}

// With the shared name free, the shared name is used.
func TestResolveSchedulesBucketPrefersTheSharedName(t *testing.T) {
	c, _ := newStagingClient(t, "123456789012")

	bucket, err := c.resolveSchedulesBucket(context.Background(), "us-east-1", kindScheduleParams)
	if err != nil {
		t.Fatalf("resolveSchedulesBucket: %v", err)
	}
	if bucket != "spawn-schedules-us-east-1" {
		t.Errorf("bucket = %q, want the shared spawn-schedules-us-east-1", bucket)
	}
}

// It returns the name it actually resolved, which is the whole point of #804.
//
// A function that creates the right bucket but reports the wrong name is exactly
// the failure that broke scheduled sweeps: the caller persists this string, and
// the handler reads from it.
func TestResolveSchedulesBucketReturnsTheBucketItCreated(t *testing.T) {
	ctx := context.Background()
	c, s3c := newStagingClient(t, "123456789012")

	bucket, err := c.resolveSchedulesBucket(ctx, "us-west-2", kindScheduleParams)
	if err != nil {
		t.Fatalf("resolveSchedulesBucket: %v", err)
	}
	// The returned name must be a bucket that exists, not merely a plausible one.
	if _, herr := s3c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); herr != nil {
		t.Errorf("resolveSchedulesBucket returned %q, which does not exist: %v", bucket, herr)
	}
}

// The account-scoped fallback is reachable and carries the account id.
//
// Driven through ensureBucket directly: the emulator has no notion of a bucket
// owned by a DIFFERENT account, so the preferred name cannot be made to fail the
// way it does in production. Asserting the fallback's shape and that it works is
// what is testable here; the preference order is covered above.
func TestEnsureBucketCreatesTheAccountScopedFallback(t *testing.T) {
	ctx := context.Background()
	const account = "123456789012"
	c, s3c := newStagingClient(t, account)

	fallback := "spawn-schedules-" + account + "-eu-west-1"
	got, err := c.ensureBucket(ctx, fallback, "eu-west-1", kindScheduleParams)
	if err != nil {
		t.Fatalf("ensureBucket(%q): %v", fallback, err)
	}
	if got != fallback {
		t.Errorf("ensureBucket returned %q, want %q", got, fallback)
	}
	if !strings.Contains(got, account) {
		t.Errorf("the fallback name %q does not carry the account id, so it would collide with "+
			"another account exactly as the shared name does", got)
	}
	if _, herr := s3c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(got)}); herr != nil {
		t.Errorf("ensureBucket returned %q but it does not exist: %v", got, herr)
	}
}

// Calling it twice must be idempotent and return the same name.
//
// Every schedule creation calls this, so a second call failing — or worse,
// silently returning a different bucket — would make the persisted name wrong for
// schedules created after the first.
func TestEnsureBucketIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c, _ := newStagingClient(t, "123456789012")

	first, err := c.resolveSchedulesBucket(ctx, "us-east-1", kindScheduleParams)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := c.resolveSchedulesBucket(ctx, "us-east-1", kindScheduleParams)
	if err != nil {
		t.Fatalf("second call on an existing bucket: %v", err)
	}
	if first != second {
		t.Errorf("resolveSchedulesBucket returned %q then %q; the persisted bucket name would be "+
			"wrong for every schedule created after the first", first, second)
	}
}

// A schedules bucket `spawn schedule create` makes is part of spore.host's
// service (spore-host/spore-host#750) and says so in its tags; one made for a
// batch queue's config, in the launching account, does not.
func TestEnsureBucketTagsOnlyAScheduleParamsBucketAsService(t *testing.T) {
	ctx := context.Background()
	c, s3c := newStagingClient(t, "123456789012")
	for _, tc := range []struct {
		bucket  string
		kind    paramsKind
		service bool
	}{
		{"spawn-schedules-us-east-1", kindScheduleParams, true},
		{"spawn-schedules-123456789012-us-east-1", kindQueueConfig, false},
	} {
		if _, err := c.ensureBucket(ctx, tc.bucket, "us-east-1", tc.kind); err != nil {
			t.Fatalf("ensureBucket(%s): %v", tc.bucket, err)
		}
		got := map[string]string{}
		if out, err := s3c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(tc.bucket)}); err == nil {
			for _, tg := range out.TagSet {
				got[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
			}
		} else if tc.service {
			t.Fatalf("GetBucketTagging(%s): %v", tc.bucket, err)
		}
		if tc.service {
			for k, v := range schedulesServiceTags() {
				if got[k] != v {
					t.Errorf("%s: %s = %q, want %q", tc.bucket, k, got[k], v)
				}
			}
		} else if _, ok := got["spore-host:role"]; ok {
			t.Errorf("%s: a queue-config bucket is tagged %v; it is the launching account's, not the service's", tc.bucket, got)
		}
	}
}
