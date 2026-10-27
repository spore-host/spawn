package taskpool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	smithy "github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/poolqueue"
	"github.com/spore-host/spawn/pkg/testutil"
)

// createRecorder records each CreateQueue input, and refuses a tagged one
// with denyTags (a caller without sqs:TagQueue).
type createRecorder struct {
	*fakeSQS
	inputs   []*sqs.CreateQueueInput
	denyTags bool
}

func (c *createRecorder) CreateQueue(ctx context.Context, in *sqs.CreateQueueInput, o ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error) {
	cp := *in
	c.inputs = append(c.inputs, &cp)
	if c.denyTags && len(in.Tags) > 0 {
		return nil, &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to perform sqs:TagQueue"}
	}
	return c.fakeSQS.CreateQueue(ctx, in, o...)
}

// TestCreateQueueTagsTheQueue: a pool's queue is created carrying exactly
// poolqueue.Tags, the tags the abandoned-pool sweep requires (#1351). Without
// them the reaper and `spawn cleanup --pool-queues` must keep the queue
// forever.
func TestCreateQueueTagsTheQueue(t *testing.T) {
	rec := &createRecorder{fakeSQS: newFakeSQS(time.Now)}
	q, err := CreateQueue(context.Background(), rec, "run-7", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.inputs) != 1 {
		t.Fatalf("CreateQueue called %d times, want 1", len(rec.inputs))
	}
	want := poolqueue.Tags("run-7")
	got := rec.inputs[0].Tags
	if len(got) != len(want) {
		t.Errorf("created with tags %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tag %s = %q, want %q", k, got[k], v)
		}
	}
	if q.TagErr() != nil {
		t.Errorf("TagErr = %v on a tagged create", q.TagErr())
	}
}

// TestCreateQueueWithoutTagQueueStillCreatesThePool: a caller who may create
// queues but not tag them gets its pool, untagged, and TagErr says why, so
// `pool create` can warn that only `pool drain` will delete it.
func TestCreateQueueWithoutTagQueueStillCreatesThePool(t *testing.T) {
	rec := &createRecorder{fakeSQS: newFakeSQS(time.Now), denyTags: true}
	q, err := CreateQueue(context.Background(), rec, "run-8", 30)
	if err != nil {
		t.Fatalf("a denied tag failed the pool: %v", err)
	}
	if q.TagErr() == nil || !strings.Contains(q.TagErr().Error(), "AccessDenied") {
		t.Errorf("TagErr = %v, want the AccessDenied", q.TagErr())
	}
	if n := len(rec.inputs); n != 2 || len(rec.inputs[1].Tags) != 0 {
		t.Errorf("want a tagged attempt then one untagged retry, got %d attempt(s)", n)
	}
}

// keyS3 records PutObject keys.
type keyS3 struct {
	countingSpecS3
	keys []string
}

func (k *keyS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	k.keys = append(k.keys, aws.ToString(in.Key))
	return &s3.PutObjectOutput{}, nil
}

// TestStagedSpecsAreWhereTheSweepLooks: a spec staged the way `spawn pool`
// stages it (prefix poolqueue.SpecPrefix) lands under
// poolqueue.SpecRunPrefix, the one prefix the sweep deletes, so a change to
// either layout cannot leave specs the deleter never sees.
func TestStagedSpecsAreWhereTheSweepLooks(t *testing.T) {
	store := &keyS3{}
	s := &SpecStore{Client: store, Bucket: "b", Prefix: poolqueue.SpecPrefix}
	if _, err := s.Stage(context.Background(), "run-9", "nf/task 1", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if len(store.keys) != 1 || !strings.HasPrefix(store.keys[0], poolqueue.SpecRunPrefix("run-9")) {
		t.Errorf("staged at %v, not under %s", store.keys, poolqueue.SpecRunPrefix("run-9"))
	}
}

// zeroCounts fills in the two message counts as 0 when the provider omits
// them, which Substrate does: it does not model ApproximateNumberOfMessages*
// at all.
type zeroCounts struct{ *sqs.Client }

func (z zeroCounts) GetQueueAttributes(ctx context.Context, in *sqs.GetQueueAttributesInput, o ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	out, err := z.Client.GetQueueAttributes(ctx, in, o...)
	if err == nil {
		for _, k := range []string{"ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible"} {
			if _, ok := out.Attributes[k]; !ok {
				out.Attributes[k] = "0"
			}
		}
	}
	return out, err
}

// TestCreatedQueueIsSweptAgainstSubstrate: against a real SQS implementation,
// the tags CreateQueue writes are the ones the sweep reads back with
// ListQueueTags, and its ListQueues/DeleteQueue calls work. Substrate omits
// the message counts, so swept as-is nothing tagged is deleted (an unread
// count is not zero); with the counts reported as 0 and a day gone by, the
// pool's queue goes and an untagged spawn-pool-* queue (pre-v0.147.0) stays.
func TestCreatedQueueIsSweptAgainstSubstrate(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := env.SQSClient()

	if _, err := CreateQueue(ctx, client, "sweep-abandoned", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(QueueName("sweep-legacy"))}); err != nil {
		t.Fatal(err)
	}
	inUse := poolqueue.InUse{Profiles: map[string]bool{}, RunIDs: map[string]bool{}}
	opts := poolqueue.Options{Region: "us-east-1", Now: time.Now().Add(poolqueue.Grace + time.Hour), Delete: true}
	decide := func(api poolqueue.SQSAPI) map[string]poolqueue.QueueDecision {
		t.Helper()
		res, err := poolqueue.Sweep(ctx, api, nil, inUse, opts)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]poolqueue.QueueDecision{}
		for _, d := range res.Queues {
			got[d.Name] = d
		}
		return got
	}

	got := decide(client)
	if d := got[QueueName("sweep-abandoned")]; d.Deleted || d.Reason != poolqueue.KeepCountsUnknown {
		t.Errorf("with no message counts the tagged queue was %+v, want kept as %q", d, poolqueue.KeepCountsUnknown)
	}
	if d := got[QueueName("sweep-legacy")]; d.Deleted || d.Reason != poolqueue.KeepUntagged {
		t.Errorf("the untagged queue: %+v, want kept as %q", d, poolqueue.KeepUntagged)
	}

	got = decide(zeroCounts{client})
	if d := got[QueueName("sweep-abandoned")]; !d.Deleted {
		t.Errorf("the abandoned tagged queue was not deleted: %+v", d)
	}
	if d := got[QueueName("sweep-legacy")]; d.Deleted || d.Reason != poolqueue.KeepUntagged {
		t.Errorf("the untagged queue: %+v, want kept as %q", d, poolqueue.KeepUntagged)
	}
	if _, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(QueueName("sweep-abandoned"))}); err == nil {
		t.Error("the deleted queue still resolves")
	}
	if _, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(QueueName("sweep-legacy"))}); err != nil {
		t.Errorf("the untagged queue is gone: %v", err)
	}
}
