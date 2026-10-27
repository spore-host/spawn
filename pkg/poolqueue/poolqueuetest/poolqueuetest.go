// Package poolqueuetest is one abandoned-pool scenario, with in-memory SQS,
// S3 and EC2, that `spawn cleanup --pool-queues` and the TTL reaper's tests
// both run (#1351). Each asserts it deletes exactly Want, so the CLI and the
// reaper are held to the same answer on the same account, not merely to the
// same package.
package poolqueuetest

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/poolqueue"
)

const (
	// Account is the scenario's account.
	Account = "111122223333"
	// Region holds the queues and specs.
	Region = "us-east-1"
	// OtherRegion is enabled in the account but holds only a worker of the
	// "busy" pool, so a sweep that checks only Region would delete it.
	OtherRegion = "ap-south-1"
)

// Scenario is the account. Now is when it was built; every age in it is
// relative to that, by at least an hour either side of a boundary, so a sweep
// run moments later with its own clock sees the same thing.
type Scenario struct {
	Now time.Time
	SQS *SQS
	S3  *S3
	EC2 map[string]*EC2
}

// WantDeletedQueues and WantDeletedSpecRuns are what any correct sweep in
// reap mode deletes from the scenario, and what report mode says it would.
var (
	WantDeletedQueues   = []string{poolqueue.QueueName("abandoned")}
	WantDeletedSpecRuns = []string{"abandoned", "drained"}
)

// New builds the scenario. Queues in Region:
//
//	abandoned  spawn's tags, empty, 3 days old, no worker   -> deleted
//	busy       same, but a worker in OtherRegion holds its profile
//	messages   same as abandoned, one visible message
//	young      same as abandoned, 1 hour old
//	legacy     untagged (pre-v0.147.0), otherwise abandoned
//	foreign    spawn:managed=true, but its run-id tag names another run
//
// Specs: abandoned, drained (no queue) -> deleted; busy, legacy -> kept.
func New() *Scenario {
	// The wall clock, not a fixed date: the reaper's run reads time.Now
	// itself, and a fixed date would let "young" age past the grace.
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-72 * time.Hour)
	q := func(runID string) *Queue {
		return &Queue{Tags: poolqueue.Tags(runID), Created: old, Visible: "0", InFlight: "0", Delayed: "0"}
	}
	queues := map[string]*Queue{}
	for _, id := range []string{"abandoned", "busy", "messages", "young", "legacy", "foreign"} {
		queues[poolqueue.QueueName(id)] = q(id)
	}
	queues[poolqueue.QueueName("messages")].Visible = "1"
	queues[poolqueue.QueueName("young")].Created = now.Add(-time.Hour)
	queues[poolqueue.QueueName("legacy")].Tags = map[string]string{}
	queues[poolqueue.QueueName("foreign")].Tags[poolqueue.RunIDTagKey] = "someone-else"

	objects := map[string]time.Time{}
	for _, id := range []string{"abandoned", "drained", "busy", "legacy"} {
		objects[poolqueue.SpecRunPrefix(id)+"task-1.json"] = old
	}
	objects["tasks/some-task/completion.json"] = old

	busyProfile := "arn:aws:iam::" + Account + ":instance-profile/" + instancerole.PoolWorkerRoleName("busy")
	return &Scenario{
		Now: now,
		SQS: &SQS{Queues: queues},
		S3:  &S3{Objects: objects},
		EC2: map[string]*EC2{
			Region:      {},
			OtherRegion: {Instances: []ec2types.Instance{{IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: aws.String(busyProfile)}}}},
		},
	}
}

// Regions are the scenario's enabled regions, sorted.
func (s *Scenario) Regions() []string { return []string{OtherRegion, Region} }

// EC2Clients is the per-region map poolqueue.WorkersInUse takes.
func (s *Scenario) EC2Clients() map[string]instancerole.EC2Describer {
	m := map[string]instancerole.EC2Describer{}
	for r, c := range s.EC2 {
		m[r] = c
	}
	return m
}

// DeletedSpecRuns are the run ids whose every staged spec is gone.
func (s *Scenario) DeletedSpecRuns() []string {
	var out []string
	for _, id := range []string{"abandoned", "drained", "busy", "legacy"} {
		gone := true
		for k := range s.S3.Objects {
			if strings.HasPrefix(k, poolqueue.SpecRunPrefix(id)) {
				gone = false
			}
		}
		if gone {
			out = append(out, id)
		}
	}
	return out
}

// Queue is one queue.
type Queue struct {
	Tags                       map[string]string
	Created                    time.Time
	Visible, InFlight, Delayed string
}

// SQS is the account's queues in Region; Deleted records DeleteQueue calls.
type SQS struct {
	mu      sync.Mutex
	Queues  map[string]*Queue
	Deleted []string
}

func queueURL(name string) string {
	return "https://sqs." + Region + ".amazonaws.com/" + Account + "/" + name
}

func queueName(u *string) string { s := aws.ToString(u); return s[strings.LastIndex(s, "/")+1:] }

// ListQueues implements poolqueue.SQSAPI.
func (f *SQS) ListQueues(_ context.Context, in *sqs.ListQueuesInput, _ ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &sqs.ListQueuesOutput{}
	for n := range f.Queues {
		if strings.HasPrefix(n, aws.ToString(in.QueueNamePrefix)) {
			out.QueueUrls = append(out.QueueUrls, queueURL(n))
		}
	}
	sort.Strings(out.QueueUrls)
	return out, nil
}

// ListQueueTags implements poolqueue.SQSAPI.
func (f *SQS) ListQueueTags(_ context.Context, in *sqs.ListQueueTagsInput, _ ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &sqs.ListQueueTagsOutput{Tags: f.Queues[queueName(in.QueueUrl)].Tags}, nil
}

// GetQueueAttributes implements poolqueue.SQSAPI.
func (f *SQS) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.Queues[queueName(in.QueueUrl)]
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{
		"CreatedTimestamp":                      strconv.FormatInt(q.Created.Unix(), 10),
		"ApproximateNumberOfMessages":           q.Visible,
		"ApproximateNumberOfMessagesNotVisible": q.InFlight,
		"ApproximateNumberOfMessagesDelayed":    q.Delayed,
	}}, nil
}

// DeleteQueue implements poolqueue.SQSAPI.
func (f *SQS) DeleteQueue(_ context.Context, in *sqs.DeleteQueueInput, _ ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := queueName(in.QueueUrl)
	f.Deleted = append(f.Deleted, n)
	delete(f.Queues, n)
	return &sqs.DeleteQueueOutput{}, nil
}

// S3 is the results bucket's objects. Every call must name Account as the
// expected owner; BadOwner records any that did not.
type S3 struct {
	mu       sync.Mutex
	Objects  map[string]time.Time
	BadOwner []string
}

func (f *S3) owner(o *string) {
	if aws.ToString(o) != Account {
		f.BadOwner = append(f.BadOwner, aws.ToString(o))
	}
}

// ListObjectsV2 implements poolqueue.S3API.
func (f *S3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner(in.ExpectedBucketOwner)
	out := &s3.ListObjectsV2Output{}
	if aws.ToString(in.Bucket) != poolqueue.SpecBucket(Account, Region) {
		return out, nil
	}
	prefix, delim := aws.ToString(in.Prefix), aws.ToString(in.Delimiter)
	var keys []string
	for k := range f.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if i := strings.Index(k[len(prefix):], delim); delim != "" && i >= 0 {
			if cp := k[:len(prefix)+i+1]; !seen[cp] {
				seen[cp] = true
				out.CommonPrefixes = append(out.CommonPrefixes, s3types.CommonPrefix{Prefix: aws.String(cp)})
			}
			continue
		}
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), LastModified: aws.Time(f.Objects[k])})
	}
	return out, nil
}

// DeleteObjects implements poolqueue.S3API.
func (f *S3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner(in.ExpectedBucketOwner)
	for _, o := range in.Delete.Objects {
		delete(f.Objects, aws.ToString(o.Key))
	}
	return &s3.DeleteObjectsOutput{}, nil
}

// EC2 is one region's live instances.
type EC2 struct{ Instances []ec2types.Instance }

// DescribeInstances implements instancerole.EC2Describer.
func (f *EC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: f.Instances}}}, nil
}
