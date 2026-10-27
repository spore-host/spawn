package poolqueue

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/spawn/pkg/instancerole"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

const acct = "111122223333"

// fakeQueue is one queue in fakeSQS.
type fakeQueue struct {
	tags                       map[string]string
	created                    time.Time
	visible, inFlight, delayed string
}

// fakeSQS holds queues by name and records deletes.
type fakeSQS struct {
	queues  map[string]*fakeQueue
	deleted []string
	listErr error
}

func url(name string) string { return "https://sqs.us-east-1.amazonaws.com/" + acct + "/" + name }

func nameOf(u *string) string { s := aws.ToString(u); return s[strings.LastIndex(s, "/")+1:] }

func (f *fakeSQS) ListQueues(_ context.Context, in *sqs.ListQueuesInput, _ ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := &sqs.ListQueuesOutput{}
	for n := range f.queues {
		if strings.HasPrefix(n, aws.ToString(in.QueueNamePrefix)) {
			out.QueueUrls = append(out.QueueUrls, url(n))
		}
	}
	return out, nil
}

func (f *fakeSQS) ListQueueTags(_ context.Context, in *sqs.ListQueueTagsInput, _ ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error) {
	return &sqs.ListQueueTagsOutput{Tags: f.queues[nameOf(in.QueueUrl)].tags}, nil
}

func (f *fakeSQS) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	q := f.queues[nameOf(in.QueueUrl)]
	a := map[string]string{
		"ApproximateNumberOfMessages":           q.visible,
		"ApproximateNumberOfMessagesNotVisible": q.inFlight,
		"ApproximateNumberOfMessagesDelayed":    q.delayed,
	}
	if !q.created.IsZero() {
		a["CreatedTimestamp"] = strconv.FormatInt(q.created.Unix(), 10)
	}
	return &sqs.GetQueueAttributesOutput{Attributes: a}, nil
}

func (f *fakeSQS) DeleteQueue(_ context.Context, in *sqs.DeleteQueueInput, _ ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	n := nameOf(in.QueueUrl)
	f.deleted = append(f.deleted, n)
	delete(f.queues, n)
	return &sqs.DeleteQueueOutput{}, nil
}

// abandoned is a queue every condition says may go: spawn's tags, empty, two
// days old. Tests change one thing about it.
func abandoned(runID string) *fakeQueue {
	return &fakeQueue{tags: Tags(runID), created: now.Add(-48 * time.Hour), visible: "0", inFlight: "0", delayed: "0"}
}

// fakeS3 is the results bucket: keys with their LastModified.
type fakeS3 struct {
	objects map[string]time.Time
	deleted []string
	owners  []string
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.owners = append(f.owners, aws.ToString(in.ExpectedBucketOwner))
	out := &s3.ListObjectsV2Output{}
	prefix := aws.ToString(in.Prefix)
	seen := map[string]bool{}
	var keys []string
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if d := aws.ToString(in.Delimiter); d != "" {
			if i := strings.Index(k[len(prefix):], d); i >= 0 {
				cp := k[:len(prefix)+i+1]
				if !seen[cp] {
					seen[cp] = true
					out.CommonPrefixes = append(out.CommonPrefixes, s3types.CommonPrefix{Prefix: aws.String(cp)})
				}
				continue
			}
		}
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), LastModified: aws.Time(f.objects[k])})
	}
	return out, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.owners = append(f.owners, aws.ToString(in.ExpectedBucketOwner))
	for _, o := range in.Delete.Objects {
		f.deleted = append(f.deleted, aws.ToString(o.Key))
		delete(f.objects, aws.ToString(o.Key))
	}
	return &s3.DeleteObjectsOutput{}, nil
}

func sweep(t *testing.T, q *fakeSQS, b *fakeS3, inUse InUse, del bool) Result {
	t.Helper()
	var s3api S3API
	if b != nil {
		s3api = b
	}
	res, err := Sweep(context.Background(), q, s3api, inUse, Options{Account: acct, Region: "us-east-1", Now: now, Delete: del})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func noWorkers() InUse { return InUse{Profiles: map[string]bool{}, RunIDs: map[string]bool{}} }

func decision(t *testing.T, res Result, name string) QueueDecision {
	t.Helper()
	for _, d := range res.Queues {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no decision for %s in %+v", name, res.Queues)
	return QueueDecision{}
}

// TestAbandonedQueueIsDeleted is the property #1351 exists for: an empty,
// spawn-tagged pool queue a day old, with no instance belonging to its pool,
// qualifies in both modes, report says so and changes nothing, and reap
// deletes exactly it.
func TestAbandonedQueueIsDeleted(t *testing.T) {
	for _, del := range []bool{false, true} {
		q := &fakeSQS{queues: map[string]*fakeQueue{QueueName("run-1"): abandoned("run-1")}}
		res := sweep(t, q, nil, noWorkers(), del)
		d := decision(t, res, QueueName("run-1"))
		if !d.Delete || d.Reason != "" {
			t.Errorf("delete=%t: an abandoned queue was kept: %q", del, d.Reason)
		}
		_, deleted, would, _ := res.Count()
		switch {
		case del && (deleted != 1 || strings.Join(q.deleted, ",") != QueueName("run-1")):
			t.Errorf("reap deleted %v (count %d), want exactly %s", q.deleted, deleted, QueueName("run-1"))
		case !del && (would != 1 || len(q.deleted) != 0):
			t.Errorf("report: would=%d, deleted %v; want would=1 and nothing deleted", would, q.deleted)
		}
	}
}

// TestQueuesThatMustBeKept: each case changes one fact about an abandoned
// queue, and each must survive reap with its own reason.
func TestQueuesThatMustBeKept(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(q *fakeQueue)
		inUse  InUse
		reason string
	}{
		{"worker holds the pool profile", nil,
			InUse{Profiles: map[string]bool{instancerole.PoolWorkerRoleName("run-1"): true}}, KeepInUse},
		{"worker holds the pre-v0.147 pool profile", nil,
			InUse{Profiles: map[string]bool{instancerole.LegacyPoolWorkerRoleName("run-1"): true}}, KeepInUse},
		{"instance carries the pool tag", nil,
			InUse{RunIDs: map[string]bool{"run-1": true}}, KeepInUse},
		{"visible messages", func(q *fakeQueue) { q.visible = "3" }, noWorkers(), KeepNotEmpty},
		{"in-flight messages", func(q *fakeQueue) { q.inFlight = "1" }, noWorkers(), KeepNotEmpty},
		{"delayed messages", func(q *fakeQueue) { q.delayed = "1" }, noWorkers(), KeepNotEmpty},
		{"count unreadable", func(q *fakeQueue) { q.visible = "" }, noWorkers(), KeepCountsUnknown},
		{"inside the grace", func(q *fakeQueue) { q.created = now.Add(-23 * time.Hour) }, noWorkers(), KeepYoung},
		{"no creation time", func(q *fakeQueue) { q.created = time.Time{} }, noWorkers(), KeepNoCreated},
		{"untagged (pre-v0.147)", func(q *fakeQueue) { q.tags = map[string]string{} }, noWorkers(), KeepUntagged},
		{"managed=false", func(q *fakeQueue) { q.tags[ManagedTagKey] = "false" }, noWorkers(), KeepUntagged},
		{"someone else's run-id tag", func(q *fakeQueue) { q.tags[RunIDTagKey] = "run-2" }, noWorkers(), KeepUnmatched},
		{"run-id tag missing", func(q *fakeQueue) { delete(q.tags, RunIDTagKey) }, noWorkers(), KeepUnmatched},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fq := abandoned("run-1")
			if c.mod != nil {
				c.mod(fq)
			}
			q := &fakeSQS{queues: map[string]*fakeQueue{QueueName("run-1"): fq}}
			res := sweep(t, q, nil, c.inUse, true)
			d := decision(t, res, QueueName("run-1"))
			if d.Delete || len(q.deleted) != 0 {
				t.Fatalf("deleted %v", q.deleted)
			}
			if d.Reason != c.reason {
				t.Errorf("kept for %q, want %q", d.Reason, c.reason)
			}
		})
	}
}

// TestDecideRefusesANonPoolName: the predicate itself, for a name ListQueues'
// prefix filter would never return (CLAUDE.md rule 2: test the guard where
// it lives).
func TestDecideRefusesANonPoolName(t *testing.T) {
	for _, n := range []string{"my-queue", NamePrefix, "spawn-poolx"} {
		if ok, why := Decide(Facts{Name: n, Tags: Tags(strings.TrimPrefix(n, NamePrefix)), CountsKnown: true, Created: now.Add(-48 * time.Hour)}, now); ok || why != KeepNotPool {
			t.Errorf("Decide(%q) = %t, %q; want kept as %q", n, ok, why, KeepNotPool)
		}
	}
}

// TestListFailureDeletesNothing: a queue listing error is returned, not
// treated as "no queues", and no spec is touched.
func TestListFailureDeletesNothing(t *testing.T) {
	q := &fakeSQS{listErr: errors.New("throttled")}
	b := &fakeS3{objects: map[string]time.Time{SpecRunPrefix("gone") + "a.json": now.Add(-72 * time.Hour)}}
	if _, err := Sweep(context.Background(), q, b, noWorkers(), Options{Account: acct, Region: "us-east-1", Now: now, Delete: true}); err == nil {
		t.Fatal("a failed ListQueues returned no error")
	}
	if len(b.deleted) != 0 {
		t.Errorf("deleted specs %v after the queue listing failed", b.deleted)
	}
}

// TestSpecsFollowTheirQueue: a run's staged specs are deleted when its queue
// is gone (already, or in this sweep) and kept while the queue lives, while
// the pool has an instance, or while a spec is young. Report mode predicts
// exactly what reap does, including specs of a queue reap would delete.
func TestSpecsFollowTheirQueue(t *testing.T) {
	old := now.Add(-72 * time.Hour)
	setup := func() (*fakeSQS, *fakeS3) {
		q := &fakeSQS{queues: map[string]*fakeQueue{
			QueueName("abandoned"): abandoned("abandoned"),
			QueueName("busy"):      abandoned("busy"),
			QueueName("legacy"):    {tags: map[string]string{}, created: old, visible: "0", inFlight: "0", delayed: "0"},
		}}
		q.queues[QueueName("busy")].visible = "5"
		b := &fakeS3{objects: map[string]time.Time{
			SpecRunPrefix("abandoned") + "t1.json": old,
			SpecRunPrefix("busy") + "t1.json":      old,
			SpecRunPrefix("legacy") + "t1.json":    old,
			SpecRunPrefix("drained") + "t1.json":   old,
			SpecRunPrefix("drained") + "t2.json":   old,
			SpecRunPrefix("young") + "t1.json":     old,
			SpecRunPrefix("young") + "t2.json":     now.Add(-time.Hour),
			SpecRunPrefix("working") + "t1.json":   old,
			"tasks/not-a-spec/completion.json":     old,
			SpecPrefix + "/other/x.json":           old,
		}}
		return q, b
	}
	inUse := InUse{Profiles: map[string]bool{instancerole.PoolWorkerRoleName("working"): true}, RunIDs: map[string]bool{}}
	want := map[string]string{
		"abandoned": "", "drained": "",
		"busy": SpecKeepQueueExists, "legacy": SpecKeepQueueExists,
		"young": SpecKeepYoung, "working": KeepInUse,
	}
	for _, del := range []bool{false, true} {
		q, b := setup()
		res := sweep(t, q, b, inUse, del)
		got := map[string]string{}
		for _, d := range res.Specs {
			if d.Err != nil {
				t.Fatalf("%s: %v", d.RunID, d.Err)
			}
			if d.Delete {
				got[d.RunID] = ""
			} else {
				got[d.RunID] = d.Reason
			}
		}
		for run, reason := range want {
			r, ok := got[run]
			if !ok || r != reason {
				t.Errorf("delete=%t: run %s specs: got %q (decided=%t), want %q", del, run, r, ok, reason)
			}
		}
		sort.Strings(b.deleted)
		wantDeleted := ""
		if del {
			wantDeleted = SpecRunPrefix("abandoned") + "t1.json," + SpecRunPrefix("drained") + "t1.json," + SpecRunPrefix("drained") + "t2.json"
		}
		if strings.Join(b.deleted, ",") != wantDeleted {
			t.Errorf("delete=%t: deleted %v, want %q", del, b.deleted, wantDeleted)
		}
		for _, o := range b.owners {
			if o != acct {
				t.Errorf("an S3 call named expected owner %q, want %s", o, acct)
			}
		}
	}
}

// fakeEC2 returns its instances, or fails.
type fakeEC2 struct {
	insts []ec2types.Instance
	err   error
}

func (f fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: f.insts}}}, nil
}

// TestWorkersInUse: a profile or a pool tag in any region counts, and one
// unreadable region fails the whole answer, so the caller deletes nothing.
func TestWorkersInUse(t *testing.T) {
	ctx := context.Background()
	regions := map[string]instancerole.EC2Describer{
		"us-east-1": fakeEC2{insts: []ec2types.Instance{{IamInstanceProfile: &ec2types.IamInstanceProfile{
			Arn: aws.String("arn:aws:iam::" + acct + ":instance-profile/" + instancerole.PoolWorkerRoleName("a"))}}}},
		"eu-west-1": fakeEC2{insts: []ec2types.Instance{{Tags: []ec2types.Tag{{Key: aws.String(RunIDTagKey), Value: aws.String("b")}}}}},
	}
	u, err := WorkersInUse(ctx, regions)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Holds("a") || !u.Holds("b") || u.Holds("c") {
		t.Errorf("Holds a=%t b=%t c=%t, want true true false", u.Holds("a"), u.Holds("b"), u.Holds("c"))
	}
	regions["ap-south-1"] = fakeEC2{err: errors.New("UnauthorizedOperation")}
	if _, err := WorkersInUse(ctx, regions); err == nil {
		t.Error("an unreadable region did not fail WorkersInUse")
	}
	if _, err := WorkersInUse(ctx, nil); err == nil {
		t.Error("no regions did not fail WorkersInUse")
	}
}

// TestSpecKeyIsUnderSpecRunPrefix: the key a submitter writes is under the
// prefix the sweep deletes, for the prefix spawn pool uses and with a slash.
func TestSpecKeyIsUnderSpecRunPrefix(t *testing.T) {
	for _, p := range []string{SpecPrefix, SpecPrefix + "/"} {
		if k := SpecKey(p, "run-1", "task"); !strings.HasPrefix(k, SpecRunPrefix("run-1")) {
			t.Errorf("SpecKey(%q) = %s, not under %s", p, k, SpecRunPrefix("run-1"))
		}
	}
}
