// Package poolqueue decides when an undrained pool's SQS queue, and the task
// specs its submitters staged, are no longer needed, and removes them (#1351).
//
// `spawn pool create` makes a run-scoped queue spawn-pool-<run id>
// (pkg/taskpool), and every `spawn pool submit` stages the task's spec JSON at
// s3://spawn-results-<account>-<region>/staging/pool-specs/<run id>/<task>.json.
// `spawn pool drain` deletes the queue. A pool nobody drains used to keep both
// forever: SQS may delete a queue after 30 days without activity, which is not
// a deleter spawn controls, and nothing at all removed the specs. The owner's
// principle (spore-host#610) is that anything spore.host creates it removes
// once it is no longer needed; this package is the deleter.
//
// Two callers share it, so there is one definition of "no longer needed":
//
//   - the TTL reaper (lambda/ttl-reaper), once per member account per run,
//     under REAPER_POOL_QUEUES=off|report|reap;
//   - `spawn cleanup --pool-queues`, from a workstation, dry-run aware.
//
// A queue is deleted only when ALL of Decide's conditions hold:
//
//   - it is named spawn-pool-<run id> and tagged spawn:managed=true with a
//     spawn:pool-run-id tag naming that same run id. pkg/taskpool.CreateQueue
//     writes both tags at creation from v0.147.0;
//   - no instance in any region enabled in the account, pending through
//     stopped, holds the pool's worker instance profile or carries its
//     spawn:pool-run-id tag (WorkersInUse). A region that cannot be read means
//     nothing is deleted;
//   - it is empty: ApproximateNumberOfMessages and ...NotVisible are both 0
//     (an unreadable count is not zero), and so is ...Delayed when SQS
//     reports it;
//   - it was created at least Grace ago.
//
// A run's staged specs are deleted when its queue is gone (already, or by this
// sweep), no instance holds the pool's workers' profile or tag, and the newest
// spec was staged at least Grace ago. They are spawn's own machinery, not task
// output: a spec is a copy of what `pool submit` was given, and the results of
// the task live elsewhere (the task's results prefix), which is never touched.
//
// Untagged queues are kept, whatever their name. Queues made before v0.147.0
// carry no tags, and an untagged spawn-pool-* queue cannot be told from one
// someone else made with that name, so the sweep reports them and leaves them;
// `spawn pool drain --run-id <id>` deletes a spawn one. Their specs are kept
// too, since their queue still exists.
//
// Kept free of pkg/aws so the TTL reaper (a nested module) can import it.
package poolqueue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/taskproto"
)

const (
	// NamePrefix starts every pool queue's name. The reaper's SQS grants are
	// scoped to exactly this prefix.
	NamePrefix = "spawn-pool-"

	// ManagedTagKey/ManagedTagValue mark a queue as spawn's.
	ManagedTagKey   = "spawn:managed"
	ManagedTagValue = "true"

	// RunIDTagKey is the pool's run id, the same tag its role, profile and
	// workers carry.
	RunIDTagKey = instancerole.PoolRunIDTagKey

	// SpecPrefix is the SpecStore prefix `spawn pool` stages specs under, in
	// the account's results bucket (SpecBucket).
	SpecPrefix = "staging"

	// specsDir is the directory under SpecPrefix that holds one directory of
	// staged specs per run.
	specsDir = "pool-specs"

	// Grace is how old a queue (or a run's newest staged spec) must be before
	// it may be deleted. `pool create` makes the queue before it creates the
	// workers' profile and launches them, and a submitter stages specs after
	// the queue exists; a day clears both, and any create retried after a
	// capacity error.
	Grace = 24 * time.Hour
)

// Reasons a queue or a run's specs are kept. Exported so callers and tests
// can match them without parsing prose.
const (
	KeepNotPool       = "not a spawn-pool-* queue"
	KeepUntagged      = "queue not tagged spawn:managed=true (made before v0.147.0, or not spawn's); 'spawn pool drain --run-id <id>' deletes a spawn one"
	KeepUnmatched     = "queue's spawn:pool-run-id tag does not match its name"
	KeepInUse         = "an instance holds the pool's worker profile or tag"
	KeepCountsUnknown = "queue's message counts could not be read"
	KeepNotEmpty      = "queue holds messages"
	KeepNoCreated     = "queue creation time unknown"
	KeepYoung         = "queue younger than the 24h grace"

	SpecKeepQueueExists = "the pool's queue still exists"
	SpecKeepYoung       = "a spec was staged within the 24h grace"
)

// QueueName is the queue for a run id. Deterministic, so a worker booted with
// only the run id resolves the queue the submitter created.
func QueueName(runID string) string { return NamePrefix + runID }

// Tags are the tags a pool's queue is created with.
func Tags(runID string) map[string]string {
	return map[string]string{ManagedTagKey: ManagedTagValue, RunIDTagKey: runID}
}

// SpecBucket is the bucket a pool in account/region stages its specs in: the
// account's results bucket, so a pool needs no bucket of its own.
func SpecBucket(account, region string) string {
	return taskproto.DefaultResultsBucket(account, region)
}

// SpecsRoot is the key prefix holding every run's staged specs.
func SpecsRoot() string { return SpecPrefix + "/" + specsDir + "/" }

// SpecRunPrefix is the key prefix holding one run's staged specs.
func SpecRunPrefix(runID string) string { return SpecsRoot() + runID + "/" }

// SpecKey is the key of one staged spec. pkg/taskpool.SpecStore builds its
// keys with this, so the prefix the sweep deletes and the prefix submitters
// write cannot drift apart. prefix is the SpecStore's Prefix ("staging" for
// every spawn pool); taskFile is the already-sanitised task id.
func SpecKey(prefix, runID, taskFile string) string {
	p := strings.TrimSuffix(prefix, "/")
	return fmt.Sprintf("%s/%s/%s/%s.json", p, specsDir, runID, taskFile)
}

// Facts is everything Decide needs about one queue.
type Facts struct {
	Name string
	Tags map[string]string
	// InUse: some instance in a checked region, in a live state, holds the
	// pool's worker profile or tag.
	InUse bool
	// CountsKnown is false when any of the three message counts was missing
	// or not a number; Messages is their sum.
	CountsKnown bool
	Messages    int
	Created     time.Time
}

// RunID is the run id a pool queue name carries, and false for a name that is
// not a pool queue's.
func RunID(queueName string) (string, bool) {
	id, ok := strings.CutPrefix(queueName, NamePrefix)
	return id, ok && id != ""
}

// Decide is THE predicate. It returns true only when every condition holds,
// and otherwise the first reason to keep the queue.
func Decide(f Facts, now time.Time) (bool, string) {
	runID, ok := RunID(f.Name)
	switch {
	case !ok:
		return false, KeepNotPool
	case f.Tags[ManagedTagKey] != ManagedTagValue:
		return false, KeepUntagged
	case f.Tags[RunIDTagKey] != runID:
		return false, KeepUnmatched
	case f.InUse:
		return false, KeepInUse
	case !f.CountsKnown:
		return false, KeepCountsUnknown
	case f.Messages != 0:
		return false, KeepNotEmpty
	case f.Created.IsZero():
		return false, KeepNoCreated
	case now.Sub(f.Created) < Grace:
		return false, KeepYoung
	}
	return true, ""
}

// InUse is what WorkersInUse found: the instance profiles attached to live
// instances, and the spawn:pool-run-id tags they carry.
type InUse struct {
	Profiles map[string]bool
	RunIDs   map[string]bool
}

// Holds reports whether a live instance belongs to runID's pool: it has the
// pool's worker profile (either naming) or the pool's run-id tag.
func (u InUse) Holds(runID string) bool {
	return u.RunIDs[runID] ||
		u.Profiles[instancerole.PoolWorkerRoleName(runID)] ||
		u.Profiles[instancerole.LegacyPoolWorkerRoleName(runID)]
}

// liveStates are the instance states in which an instance may still need its
// queue: anything not yet terminated.
var liveStates = []string{"pending", "running", "stopping", "stopped", "shutting-down"}

// WorkersInUse lists every live instance in the given regions once, and
// records its profile and pool run-id tag.
//
// It fails on the first region it cannot read: an incomplete answer would make
// a pool with workers there look abandoned, so the caller must delete nothing.
// It filters only on the instance state and reads the profile and tags from
// the response, rather than asking EC2 to match them: a filter that silently
// matched nothing would read as "no pool has workers".
func WorkersInUse(ctx context.Context, regions map[string]instancerole.EC2Describer) (InUse, error) {
	u := InUse{Profiles: map[string]bool{}, RunIDs: map[string]bool{}}
	if len(regions) == 0 {
		return u, errors.New("no regions to check for pool workers")
	}
	names := make([]string, 0, len(regions))
	for r := range regions {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, region := range names {
		p := ec2.NewDescribeInstancesPaginator(regions[region], &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{{Name: aws.String("instance-state-name"), Values: liveStates}},
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return InUse{}, fmt.Errorf("%s: describe instances: %w", region, err)
			}
			for _, res := range page.Reservations {
				for _, inst := range res.Instances {
					if inst.IamInstanceProfile != nil {
						u.Profiles[instancerole.ProfileNameFromARN(aws.ToString(inst.IamInstanceProfile.Arn))] = true
					}
					for _, t := range inst.Tags {
						if aws.ToString(t.Key) == RunIDTagKey && aws.ToString(t.Value) != "" {
							u.RunIDs[aws.ToString(t.Value)] = true
						}
					}
				}
			}
		}
	}
	return u, nil
}

// SQSAPI is the SQS slice Sweep uses. *sqs.Client satisfies it; the reaper's
// own sqsAPI must declare at least these, so pkg/reaperiam's drift gate sees
// every SQS call it makes.
type SQSAPI interface {
	ListQueues(ctx context.Context, in *sqs.ListQueuesInput, optFns ...func(*sqs.Options)) (*sqs.ListQueuesOutput, error)
	ListQueueTags(ctx context.Context, in *sqs.ListQueueTagsInput, optFns ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error)
	GetQueueAttributes(ctx context.Context, in *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	DeleteQueue(ctx context.Context, in *sqs.DeleteQueueInput, optFns ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error)
}

// S3API is the S3 slice the spec half of Sweep uses.
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// Options are one region's sweep.
type Options struct {
	// Account owns the specs bucket; every S3 call names it as the expected
	// owner. Empty skips the spec half.
	Account string
	Region  string
	Now     time.Time
	// Delete allows the deletes. False reports what they would be.
	Delete bool
}

// QueueDecision is the outcome for one queue.
type QueueDecision struct {
	Name    string
	Created time.Time
	// Delete: Decide said it may go. Deleted: it went. Err: its facts could
	// not be read, or the delete failed (then Delete is true, Deleted false).
	Delete  bool
	Deleted bool
	Reason  string
	Err     error
}

// SpecDecision is the outcome for one run's staged specs.
type SpecDecision struct {
	RunID   string
	Bucket  string
	Prefix  string
	Objects int
	Newest  time.Time
	Delete  bool
	Deleted bool
	Reason  string
	Err     error
}

// Result is one region's sweep.
type Result struct {
	Region string
	Queues []QueueDecision
	Specs  []SpecDecision
	// SpecsErr is set when the specs could not be listed; the queue half
	// still ran.
	SpecsErr error
}

// Count returns (scanned, deleted, wouldDelete, skipped) over the queues.
// wouldDelete counts queues that qualified but were not deleted because
// Delete was false; a failed delete counts as skipped.
func (r Result) Count() (scanned, deleted, wouldDelete, skipped int) {
	for _, d := range r.Queues {
		scanned++
		switch {
		case d.Deleted:
			deleted++
		case d.Delete && d.Err == nil:
			wouldDelete++
		default:
			skipped++
		}
	}
	return
}

// SpecCount returns (deleted, wouldDelete, failed) over the runs' specs.
func (r Result) SpecCount() (deleted, wouldDelete, failed int) {
	for _, d := range r.Specs {
		switch {
		case d.Deleted:
			deleted++
		case d.Err != nil:
			failed++
		case d.Delete:
			wouldDelete++
		}
	}
	if r.SpecsErr != nil {
		failed++
	}
	return
}

// Sweep lists the region's spawn-pool-* queues, decides each against inUse
// (from WorkersInUse) and opts.Now and, with opts.Delete, deletes those Decide
// allows; then it does the same for the runs' staged specs (when s3api is
// non-nil and opts.Account is set). With Delete false nothing is changed, and
// the result says exactly what Delete would do: a queue that would be deleted
// counts as gone when its run's specs are decided.
//
// A queue-listing error is returned and nothing is deleted.
func Sweep(ctx context.Context, sqsapi SQSAPI, s3api S3API, inUse InUse, opts Options) (Result, error) {
	res := Result{Region: opts.Region}
	urls, err := listQueues(ctx, sqsapi)
	if err != nil {
		return res, err
	}
	// alive: run ids whose queue still exists after this sweep (or would,
	// in report mode). A queue whose facts could not be read is alive.
	alive := map[string]bool{}
	for _, url := range urls {
		name := url[strings.LastIndex(url, "/")+1:]
		runID, _ := RunID(name)
		d := QueueDecision{Name: name}
		f, err := gather(ctx, sqsapi, url, name, inUse)
		if err != nil {
			d.Err, d.Reason = err, "could not read the queue"
			alive[runID] = true
			res.Queues = append(res.Queues, d)
			continue
		}
		d.Created = f.Created
		d.Delete, d.Reason = Decide(f, opts.Now)
		if !d.Delete {
			alive[runID] = true
		}
		if d.Delete && opts.Delete {
			if _, err := sqsapi.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(url)}); err != nil && !isQueueGone(err) {
				d.Err = fmt.Errorf("delete queue: %w", err)
				alive[runID] = true
			} else {
				d.Deleted = true
			}
		}
		res.Queues = append(res.Queues, d)
	}
	if s3api != nil && opts.Account != "" {
		res.Specs, res.SpecsErr = sweepSpecs(ctx, s3api, inUse, alive, opts)
	}
	return res, nil
}

func listQueues(ctx context.Context, api SQSAPI) ([]string, error) {
	var urls []string
	p := sqs.NewListQueuesPaginator(api, &sqs.ListQueuesInput{
		QueueNamePrefix: aws.String(NamePrefix),
		// NextToken is only returned when MaxResults is set; without it a
		// region with more than 1,000 pool queues would be half-swept.
		MaxResults: aws.Int32(1000),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list queues: %w", err)
		}
		urls = append(urls, page.QueueUrls...)
	}
	sort.Strings(urls)
	return urls, nil
}

// gather reads the Facts for one queue.
func gather(ctx context.Context, api SQSAPI, url, name string, inUse InUse) (Facts, error) {
	runID, _ := RunID(name)
	f := Facts{Name: name, InUse: runID != "" && inUse.Holds(runID)}
	tags, err := api.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: aws.String(url)})
	if err != nil {
		return f, fmt.Errorf("list queue tags: %w", err)
	}
	f.Tags = tags.Tags
	attrs, err := api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameCreatedTimestamp,
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
		},
	})
	if err != nil {
		return f, fmt.Errorf("get queue attributes: %w", err)
	}
	// The two counts the rule names must be present and numeric; an
	// unreadable one is not zero. The delayed count is added when SQS
	// returns it (spawn never sends a delayed message, so it is a backstop;
	// Substrate does not model it).
	f.CountsKnown = true
	for _, a := range []sqstypes.QueueAttributeName{
		sqstypes.QueueAttributeNameApproximateNumberOfMessages,
		sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
	} {
		if _, present := attrs.Attributes[string(a)]; !present && a == sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed {
			continue
		}
		n, ok := Count(attrs.Attributes, a)
		if !ok {
			f.CountsKnown = false
			continue
		}
		f.Messages += n
	}
	if s, err := strconv.ParseInt(attrs.Attributes[string(sqstypes.QueueAttributeNameCreatedTimestamp)], 10, 64); err == nil && s > 0 {
		f.Created = time.Unix(s, 0).UTC()
	}
	return f, nil
}

// Count reads one of SQS's approximate message counts from a
// GetQueueAttributes result. ok is false when the attribute is missing, not a
// number, or negative: an unreadable count is unknown, never zero. It is the
// one parser for these counts, shared with taskpool.Queue.Depth (#1362).
func Count(attrs map[string]string, name sqstypes.QueueAttributeName) (n int, ok bool) {
	v, present := attrs[string(name)]
	if !present {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// sweepSpecs decides and (with opts.Delete) deletes each run's staged specs.
func sweepSpecs(ctx context.Context, api S3API, inUse InUse, alive map[string]bool, opts Options) ([]SpecDecision, error) {
	bucket := SpecBucket(opts.Account, opts.Region)
	var runs []string
	p := s3.NewListObjectsV2Paginator(api, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(SpecsRoot()), Delimiter: aws.String("/"),
		ExpectedBucketOwner: aws.String(opts.Account),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			if isNoSuchBucket(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("list %s/%s: %w", bucket, SpecsRoot(), err)
		}
		for _, cp := range page.CommonPrefixes {
			id := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(cp.Prefix), SpecsRoot()), "/")
			if id != "" {
				runs = append(runs, id)
			}
		}
	}
	sort.Strings(runs)
	var out []SpecDecision
	for _, runID := range runs {
		d := SpecDecision{RunID: runID, Bucket: bucket, Prefix: SpecRunPrefix(runID)}
		switch {
		case alive[runID]:
			d.Reason = SpecKeepQueueExists
			out = append(out, d)
			continue
		case inUse.Holds(runID):
			d.Reason = KeepInUse
			out = append(out, d)
			continue
		}
		keys, newest, err := listRun(ctx, api, bucket, d.Prefix, opts.Account, opts.Now)
		d.Objects, d.Newest = len(keys), newest
		switch {
		case err != nil:
			d.Err, d.Reason = err, "could not list the run's specs"
		case len(keys) == 0:
			continue
		case opts.Now.Sub(newest) < Grace:
			d.Reason = SpecKeepYoung
		default:
			d.Delete = true
			if opts.Delete {
				if err := deleteKeys(ctx, api, bucket, opts.Account, keys); err != nil {
					d.Err = err
				} else {
					d.Deleted = true
				}
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func listRun(ctx context.Context, api S3API, bucket, prefix, owner string, now time.Time) ([]string, time.Time, error) {
	var keys []string
	var newest time.Time
	p := s3.NewListObjectsV2Paginator(api, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(prefix), ExpectedBucketOwner: aws.String(owner),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("list %s/%s: %w", bucket, prefix, err)
		}
		for _, o := range page.Contents {
			k := aws.ToString(o.Key)
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			keys = append(keys, k)
			lm := aws.ToTime(o.LastModified)
			if lm.IsZero() {
				// An object with no time cannot be shown to be old.
				lm = now
			}
			if lm.After(newest) {
				newest = lm
			}
		}
	}
	return keys, newest, nil
}

// deleteKeys deletes keys in batches of 1,000 (DeleteObjects' limit).
func deleteKeys(ctx context.Context, api S3API, bucket, owner string, keys []string) error {
	for start := 0; start < len(keys); start += 1000 {
		end := min(start+1000, len(keys))
		objs := make([]s3types.ObjectIdentifier, 0, end-start)
		for _, k := range keys[start:end] {
			objs = append(objs, s3types.ObjectIdentifier{Key: aws.String(k)})
		}
		out, err := api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket), ExpectedBucketOwner: aws.String(owner),
			Delete: &s3types.Delete{Objects: objs, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete specs: %w", err)
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("delete specs: %d object(s) not deleted, first %s: %s %s",
				len(out.Errors), aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
		}
	}
	return nil
}

func isQueueGone(err error) bool {
	var qde *sqstypes.QueueDoesNotExist
	if errors.As(err, &qde) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		c := apiErr.ErrorCode()
		return c == "AWS.SimpleQueueService.NonExistentQueue" || c == "QueueDoesNotExist"
	}
	return false
}

func isNoSuchBucket(err error) bool {
	var nsb *s3types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket"
}

// Clients are the per-region clients a CLI sweep needs.
type Clients struct {
	SQS func(region string) SQSAPI
	S3  func(region string) S3API
	// EC2 holds one client per region enabled in the account, for
	// WorkersInUse.
	EC2 map[string]instancerole.EC2Describer
}

// NewClients builds Clients from cfg, for a caller (the CLI) that should not
// construct SDK clients itself.
func NewClients(ctx context.Context, cfg aws.Config) (Clients, error) {
	regions, err := instancerole.EnabledRegions(ctx, ec2.NewFromConfig(cfg))
	if err != nil {
		return Clients{}, err
	}
	at := func(r string) aws.Config { c := cfg.Copy(); c.Region = r; return c }
	ec2s := make(map[string]instancerole.EC2Describer, len(regions))
	for _, r := range regions {
		ec2s[r] = ec2.NewFromConfig(at(r))
	}
	return Clients{
		SQS: func(r string) SQSAPI { return sqs.NewFromConfig(at(r)) },
		S3:  func(r string) S3API { return s3.NewFromConfig(at(r)) },
		EC2: ec2s,
	}, nil
}
