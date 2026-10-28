package staging

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// #1382: a schedule's params are read by scheduler-handler on EVERY firing, so
// nothing may expire them while the schedule lives. ensureBucket used to give
// every bucket it created `expire-schedules` (7 days on schedules/), and the
// production buckets carry `DeleteOldScheduleParams` (90 days, whole bucket).

// expiresObject reports whether r expires an object with this key and tags.
func expiresObject(r s3types.LifecycleRule, key string, tags map[string]string) bool {
	if r.Status != s3types.ExpirationStatusEnabled || r.Expiration == nil {
		return false
	}
	// A rule with no Filter applies to every object.
	prefix, want := "", []s3types.Tag(nil)
	if f := r.Filter; f != nil {
		switch {
		case f.And != nil:
			prefix, want = aws.ToString(f.And.Prefix), f.And.Tags
		case f.Tag != nil:
			want = []s3types.Tag{*f.Tag}
		default:
			prefix = aws.ToString(f.Prefix)
		}
	}
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	for _, t := range want {
		if tags[aws.ToString(t.Key)] != aws.ToString(t.Value) {
			return false
		}
	}
	return true
}

func bucketRules(t *testing.T, s3c *s3.Client, bucket string) []s3types.LifecycleRule {
	t.Helper()
	out, err := s3c.GetBucketLifecycleConfiguration(context.Background(), &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	if isNoLifecycle(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("GetBucketLifecycleConfiguration(%s): %v", bucket, err)
	}
	return out.Rules
}

// The tags UploadScheduleParams puts on a schedule's params, parsed back.
func scheduleParamsTags(id string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(paramsTagging(kindScheduleParams, id), "&") {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// A schedules bucket created by UploadScheduleParams expires nothing a
// schedule's params are.
func TestNewSchedulesBucketDoesNotExpireScheduleParams(t *testing.T) {
	ctx := context.Background()
	c, s3c := newStagingClient(t, "123456789012")

	bucket, key, _, _, err := c.UploadScheduleParams(ctx, writeParamsFile(t), "sched-1", "us-east-1")
	if err != nil {
		t.Fatalf("UploadScheduleParams: %v", err)
	}
	for _, r := range bucketRules(t, s3c, bucket) {
		if expiresObject(r, key, scheduleParamsTags("sched-1")) {
			t.Errorf("new schedules bucket %s has lifecycle rule %q expiring %s; a recurring schedule "+
				"fails every firing once its params are gone (#1382)", bucket, aws.ToString(r.ID), key)
		}
	}

	// And creation itself puts none, not only the reconcile that follows it.
	fresh := "spawn-schedules-123456789012-us-east-1"
	if _, err := c.ensureBucket(ctx, fresh, "us-east-1"); err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}
	for _, r := range bucketRules(t, s3c, fresh) {
		if expiresObject(r, key, scheduleParamsTags("sched-1")) {
			t.Errorf("ensureBucket created %s with lifecycle rule %q expiring schedule params", fresh, aws.ToString(r.ID))
		}
	}
}

// Queue configs share the bucket and prefix and still expire, by their own
// tag-scoped rule — which does not reach schedule params.
func TestQueueConfigBucketGetsItsOwnTagScopedRule(t *testing.T) {
	ctx := context.Background()
	c, s3c := newStagingClient(t, "123456789012")

	bucket, key, _, _, err := c.UploadQueueConfig(ctx, writeParamsFile(t), "queue-1", "us-east-1")
	if err != nil {
		t.Fatalf("UploadQueueConfig: %v", err)
	}
	queueTags := map[string]string{QueueConfigTagKey: QueueConfigTagValue}
	var expiresQueue bool
	for _, r := range bucketRules(t, s3c, bucket) {
		expiresQueue = expiresQueue || expiresObject(r, key, queueTags)
		if expiresObject(r, "schedules/sched-1/params.yaml", scheduleParamsTags("sched-1")) {
			t.Errorf("rule %q on %s expires schedule params too", aws.ToString(r.ID), bucket)
		}
	}
	if !expiresQueue {
		t.Errorf("no lifecycle rule on %s expires the queue config %s", bucket, key)
	}
	if !strings.Contains(paramsTagging(kindQueueConfig, "queue-1"), QueueConfigTagKey+"="+QueueConfigTagValue) {
		t.Errorf("queue config tagging %q lacks the tag its rule matches", paramsTagging(kindQueueConfig, "queue-1"))
	}
}

// fakeLifecycle records what reconcileSchedulesLifecycle writes.
type fakeLifecycle struct {
	rules   []s3types.LifecycleRule
	none    bool
	puts    [][]s3types.LifecycleRule
	deletes int
}

func (f *fakeLifecycle) GetBucketLifecycleConfiguration(context.Context, *s3.GetBucketLifecycleConfigurationInput, ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error) {
	if f.none {
		return nil, &smithy.GenericAPIError{Code: "NoSuchLifecycleConfiguration"}
	}
	return &s3.GetBucketLifecycleConfigurationOutput{Rules: f.rules}, nil
}

func (f *fakeLifecycle) PutBucketLifecycleConfiguration(_ context.Context, in *s3.PutBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error) {
	f.puts = append(f.puts, in.LifecycleConfiguration.Rules)
	return &s3.PutBucketLifecycleConfigurationOutput{}, nil
}

func (f *fakeLifecycle) DeleteBucketLifecycle(context.Context, *s3.DeleteBucketLifecycleInput, ...func(*s3.Options)) (*s3.DeleteBucketLifecycleOutput, error) {
	f.deletes++
	return &s3.DeleteBucketLifecycleOutput{}, nil
}

func rule(id, prefix string, days int32) s3types.LifecycleRule {
	return s3types.LifecycleRule{
		ID: aws.String(id), Status: s3types.ExpirationStatusEnabled,
		Filter:     &s3types.LifecycleRuleFilter{Prefix: aws.String(prefix)},
		Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(days)},
	}
}

func ids(rules []s3types.LifecycleRule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, aws.ToString(r.ID))
	}
	return out
}

// The migration removes exactly expire-schedules and DeleteOldScheduleParams
// and keeps every other rule.
func TestReconcileRemovesExactlyTheRetiredRules(t *testing.T) {
	f := &fakeLifecycle{rules: []s3types.LifecycleRule{
		rule("expire-schedules", "schedules/", 7),
		rule("keep-me", "logs/", 30),
		rule("DeleteOldScheduleParams", "", 90),
		queueConfigRule(),
	}}
	if err := reconcileSchedulesLifecycle(context.Background(), f, "b", kindScheduleParams); err != nil {
		t.Fatal(err)
	}
	if len(f.puts) != 1 || f.deletes != 0 {
		t.Fatalf("want one Put and no Delete, got %d puts, %d deletes", len(f.puts), f.deletes)
	}
	if got, want := ids(f.puts[0]), []string{"keep-me", QueueConfigRuleID}; !slices.Equal(got, want) {
		t.Errorf("rules written = %v, want %v", got, want)
	}
}

// With only the retired rules, the configuration is deleted (S3 refuses an
// empty one).
func TestReconcileDeletesALifecycleLeftEmpty(t *testing.T) {
	// The production buckets: one whole-bucket 90-day rule.
	f := &fakeLifecycle{rules: []s3types.LifecycleRule{rule("DeleteOldScheduleParams", "", 90)}}
	if err := reconcileSchedulesLifecycle(context.Background(), f, "b", kindScheduleParams); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 || len(f.puts) != 0 {
		t.Errorf("want DeleteBucketLifecycle once and no Put, got %d deletes, %d puts", f.deletes, len(f.puts))
	}
}

// A bucket with nothing to change is not written, and one with no lifecycle
// at all is not an error.
func TestReconcileLeavesACleanBucketAlone(t *testing.T) {
	for name, f := range map[string]*fakeLifecycle{
		"no lifecycle": {none: true},
		"other rules":  {rules: []s3types.LifecycleRule{rule("keep-me", "logs/", 30)}},
	} {
		if err := reconcileSchedulesLifecycle(context.Background(), f, "b", kindScheduleParams); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if len(f.puts) != 0 || f.deletes != 0 {
			t.Errorf("%s: wrote the lifecycle (%d puts, %d deletes) with nothing to change", name, len(f.puts), f.deletes)
		}
	}
}

// The queue path adds its rule and removes nothing.
func TestReconcileQueueConfigAddsItsRuleAndRemovesNothing(t *testing.T) {
	f := &fakeLifecycle{rules: []s3types.LifecycleRule{rule("expire-schedules", "schedules/", 7)}}
	if err := reconcileSchedulesLifecycle(context.Background(), f, "b", kindQueueConfig); err != nil {
		t.Fatal(err)
	}
	if len(f.puts) != 1 || !slices.Equal(ids(f.puts[0]), []string{"expire-schedules", QueueConfigRuleID}) {
		t.Errorf("queue path wrote %v", f.puts)
	}
}

// A lifecycle that cannot be read fails a schedule's upload (its params could
// expire under it) rather than being skipped.
func TestReconcileFailsClosedForScheduleParams(t *testing.T) {
	f := &errLifecycle{}
	if err := reconcileSchedulesLifecycle(context.Background(), f, "b", kindScheduleParams); err == nil {
		t.Error("an unreadable lifecycle was treated as clean")
	}
}

type errLifecycle struct{ fakeLifecycle }

func (e *errLifecycle) GetBucketLifecycleConfiguration(context.Context, *s3.GetBucketLifecycleConfigurationInput, ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error) {
	return nil, errors.New("AccessDenied")
}
