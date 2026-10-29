// Package schedulesweep deletes schedule params objects that no schedule
// record names any more (#1398).
//
// `spawn schedule create` uploads a schedule's params to
// s3://spawn-schedules-<region>/schedules/<id>/params.yaml (or the
// account-scoped spawn-schedules-<account>-<region> when the shared name is
// taken), then writes the schedule's record to the spawn-schedules table.
// Since #1382 the params have no lifecycle expiry: `spawn schedule cancel`
// and scheduler-handler (when a schedule runs out) delete them. Three paths
// still leave them behind once the record's TTL deletes the record: a one-time
// schedule whose only firing failed, an --end-after schedule paused past its
// end, and an --end-after schedule whose cadence is longer than the record's
// TTL margin. After that nothing names the object. This package is its
// deleter (the owner's principle, spore-host#610: anything spore.host creates
// it removes once it is no longer needed).
//
// Two callers share it, so there is one definition of "orphaned":
//
//   - the TTL reaper (lambda/ttl-reaper), once per run, on its own (infra
//     account) credentials, under REAPER_SCHEDULE_PARAMS=off|report|reap;
//   - `spawn cleanup --schedule-params`, from a workstation, dry-run aware.
//
// An object is deleted only when ALL of Decide's conditions hold:
//
//   - it is in a bucket named spawn-schedules-<region> or
//     spawn-schedules-<account>-<region> for the caller's account, and every
//     call names that account as the expected bucket owner;
//   - its key is exactly schedules/<id>/params.yaml;
//   - no record in the table has that schedule_id, or names that bucket and
//     key, read in full with a consistent read AFTER the objects are listed;
//   - it was last modified at least Grace (scheduler.RecordTTLMargin) ago.
//     The CLI uploads the params before it writes the record, so a schedule
//     being created right now has params and no record yet;
//   - its tags say it is a schedule's params and not a batch queue config:
//     Component=scheduler, a ScheduleID tag equal to <id>, no QueueID tag,
//     and <id> has the form scheduler.GenerateScheduleID gives a schedule.
//
// The last condition is how a queue config is told apart. `spawn launch
// --batch-queue` uploads its config to the same bucket, under the same key
// layout (schedules/<queue id>/params.yaml), so the key cannot tell them
// apart. From #1399 a queue config is tagged Component=queue&QueueID=<id> and
// has its own 7-day lifecycle rule; before #1399 it was tagged exactly like
// schedule params (Component=scheduler&ScheduleID=<queue id>), and only its
// id, queue-<timestamp>, differs from a schedule's sched-<timestamp>. An
// object whose tags cannot be read, that carries no Component tag, or whose
// tags or id fit neither pattern is kept: when in doubt, nothing is deleted.
//
// It fails closed. If the table cannot be read completely (any Scan page
// fails, or any record does not decode), Sweep returns an error and deletes
// nothing anywhere. If one bucket cannot be listed, nothing in it is deleted.
//
// Kept free of pkg/aws and pkg/staging so the TTL reaper (a nested module)
// can import it cheaply; the tag values are pinned to pkg/staging's by a test.
package schedulesweep

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/scheduler"
)

const (
	// BucketPrefix starts every schedules bucket's name. The reaper's S3
	// grants are scoped to exactly this prefix.
	BucketPrefix = "spawn-schedules-"

	// KeyPrefix is where params live in a schedules bucket. The reaper's
	// list grant is conditioned on it and its object grants scoped to it.
	KeyPrefix = scheduler.ParamsKeyPrefix

	// ParamsFile is the object name under schedules/<id>/.
	ParamsFile = "params.yaml"

	// Grace is how old an orphaned params object must be before it may be
	// deleted: the record's own TTL margin, so the sweep never races a
	// schedule being created (params uploaded, record not yet written).
	Grace = scheduler.RecordTTLMargin

	// The object tags pkg/staging writes on an upload to a schedules bucket
	// (staging.QueueConfigTagKey and friends; TestTagsMatchStaging pins
	// them).
	ComponentTagKey   = "Component"
	ScheduleTagValue  = "scheduler"
	QueueTagValue     = "queue"
	ScheduleIDTagKey  = "ScheduleID"
	QueueIDTagKey     = "QueueID"
	scheduleIDPattern = `^sched-[0-9]{8}-[0-9]{6}$`
)

var scheduleIDRe = regexp.MustCompile(scheduleIDPattern)

// Reasons an object is kept. Exported so callers and tests can match them
// without parsing prose.
const (
	KeepNotParams   = "not a schedules/<id>/params.yaml object"
	KeepLive        = "a schedule record names it"
	KeepNoModified  = "last-modified time unknown"
	KeepYoung       = "younger than the record TTL margin (30 days)"
	KeepQueueConfig = "a batch queue config (it has its own 7-day lifecycle rule)"
	KeepUntagged    = "not tagged as a schedule's params (no Component=scheduler tag)"
	KeepTagMismatch = "its ScheduleID tag does not match its key"
	KeepNotSchedID  = "its id is not one spawn gives a schedule (sched-YYYYMMDD-HHMMSS)"
	KeepTagsUnread  = "its tags could not be read"
)

// Facts is everything Decide needs about one object.
type Facts struct {
	Key          string
	LastModified time.Time
	// Named: a record has this schedule ID, or names this bucket and key.
	Named bool
	// Tags is nil when they were not read (or could not be).
	Tags map[string]string
}

// ScheduleID is the id in a params key, and false for any other key.
func ScheduleID(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, KeyPrefix)
	if !ok {
		return "", false
	}
	id, file, ok := strings.Cut(rest, "/")
	if !ok || id == "" || file != ParamsFile {
		return "", false
	}
	return id, true
}

// LooksLikeScheduleID reports whether id has the form
// scheduler.GenerateScheduleID gives a schedule.
func LooksLikeScheduleID(id string) bool { return scheduleIDRe.MatchString(id) }

// decideUntagged is every condition that does not need the object's tags, so
// Sweep reads tags only for an object that is otherwise deletable.
func decideUntagged(f Facts, now time.Time) (bool, string) {
	_, ok := ScheduleID(f.Key)
	switch {
	case !ok:
		return false, KeepNotParams
	case f.Named:
		return false, KeepLive
	case f.LastModified.IsZero():
		return false, KeepNoModified
	case now.Sub(f.LastModified) < Grace:
		return false, KeepYoung
	}
	return true, ""
}

// Decide is THE predicate. It returns true only when every condition holds,
// and otherwise the first reason to keep the object.
func Decide(f Facts, now time.Time) (bool, string) {
	if ok, why := decideUntagged(f, now); !ok {
		return false, why
	}
	id, _ := ScheduleID(f.Key)
	switch {
	case f.Tags == nil:
		return false, KeepTagsUnread
	case f.Tags[ComponentTagKey] == QueueTagValue, f.Tags[QueueIDTagKey] != "":
		return false, KeepQueueConfig
	case f.Tags[ComponentTagKey] != ScheduleTagValue:
		return false, KeepUntagged
	case f.Tags[ScheduleIDTagKey] != id:
		return false, KeepTagMismatch
	case !LooksLikeScheduleID(id):
		// A pre-#1399 queue config: tagged like params, named queue-<ts>.
		return false, KeepNotSchedID
	}
	return true, ""
}

// BucketRegion is the region a schedules bucket's name says it is in, and
// false for a name that is not one of the two pkg/staging gives the
// account's schedules buckets.
func BucketRegion(bucket, account string) (string, bool) {
	rest, ok := strings.CutPrefix(bucket, BucketPrefix)
	if !ok || rest == "" {
		return "", false
	}
	if account != "" {
		if r, ok := strings.CutPrefix(rest, account+"-"); ok {
			rest = r
		}
	}
	if !regionRe.MatchString(rest) {
		return "", false
	}
	return rest, true
}

var regionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// S3API is the S3 slice Sweep uses. *s3.Client satisfies it; the reaper's
// own interface must declare at least these, so pkg/reaperiam's drift gate
// sees every S3 call it makes.
type S3API interface {
	ListBuckets(ctx context.Context, in *s3.ListBucketsInput, optFns ...func(*s3.Options)) (*s3.ListBucketsOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObjectTagging(ctx context.Context, in *s3.GetObjectTaggingInput, optFns ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// Clients are what a sweep reads and deletes with: the schedules table and
// an S3 client per region. All of them must be the account's that
// `spawn schedule create` writes to (spawn's infra account).
type Clients struct {
	DB    instancerole.ScheduleScanner
	Table string
	S3    func(region string) S3API
}

// NewClients reads the table where `spawn schedule create` writes it
// (scheduler.TableName in scheduler.TableRegion) and each bucket in its own
// region, with cfg's credentials.
func NewClients(cfg aws.Config) Clients {
	tc := cfg.Copy()
	tc.Region = scheduler.TableRegion
	return Clients{
		DB:    dynamodb.NewFromConfig(tc),
		Table: scheduler.TableName,
		S3: func(region string) S3API {
			c := cfg.Copy()
			c.Region = region
			return s3.NewFromConfig(c)
		},
	}
}

// Options are one sweep.
type Options struct {
	// Account owns the buckets; every S3 call names it as the expected owner.
	Account string
	Now     time.Time
	// Delete allows the deletes. False reports what they would be.
	Delete bool
}

// Decision is the outcome for one object.
type Decision struct {
	Bucket       string
	Region       string
	Key          string
	LastModified time.Time
	// Delete: Decide said it may go. Deleted: it went. Err: its tags could
	// not be read (Delete false), or the delete failed (Delete true).
	Delete  bool
	Deleted bool
	Reason  string
	Err     error
}

// BucketError is a schedules bucket that could not be listed: nothing in it
// was deleted.
type BucketError struct {
	Bucket string
	Err    error
}

// Result is one sweep.
type Result struct {
	// Buckets are the schedules buckets swept. Foreign are spawn-schedules-*
	// buckets in the account whose name is not one spawn gives its schedules
	// buckets (KeepNotOurBucket): nothing in them is read or deleted.
	Buckets []string
	Foreign []string
	Objects []Decision
	Errors  []BucketError
}

// Count returns (scanned, deleted, wouldDelete, skipped, failed) over the
// params objects (keys that are not schedules/<id>/params.yaml are not
// counted). failed counts objects whose tags could not be read or whose
// delete failed, and buckets that could not be listed; they are not in
// skipped.
func (r Result) Count() (scanned, deleted, wouldDelete, skipped, failed int) {
	for _, d := range r.Objects {
		if d.Reason == KeepNotParams {
			continue
		}
		scanned++
		switch {
		case d.Deleted:
			deleted++
		case d.Err != nil:
			failed++
		case d.Delete:
			wouldDelete++
		default:
			skipped++
		}
	}
	return scanned, deleted, wouldDelete, skipped, failed + len(r.Errors)
}

// ErrTableUnreadable wraps the error Sweep returns when the schedules table
// could not be read completely. Nothing was deleted.
var ErrTableUnreadable = errors.New("the schedules table could not be read completely, so no params object can be shown to be orphaned")

// Sweep lists the account's spawn-schedules-* buckets and the params in each,
// then reads every schedule record, decides each object and, with
// opts.Delete, deletes those Decide allows. With Delete false nothing is
// changed, and the result says exactly what Delete would do.
//
// A ListBuckets error, or a table that cannot be read completely (wrapped in
// ErrTableUnreadable), is returned and nothing is deleted.
func Sweep(ctx context.Context, c Clients, opts Options) (Result, error) {
	var res Result
	if opts.Account == "" {
		return res, errors.New("no account to name as the schedules buckets' expected owner")
	}
	if c.S3 == nil {
		return res, errors.New("no S3 client for the schedules buckets")
	}
	buckets, err := listBuckets(ctx, c.S3(scheduler.TableRegion))
	if err != nil {
		return res, err
	}

	type listed struct {
		bucket, region string
		objs           []s3types.Object
	}
	var all []listed
	for _, b := range buckets {
		region, ok := BucketRegion(b.name, opts.Account)
		if !ok {
			res.Foreign = append(res.Foreign, b.name)
			continue
		}
		if b.region != "" {
			region = b.region
		}
		res.Buckets = append(res.Buckets, b.name)
		objs, err := listParams(ctx, c.S3(region), b.name, opts.Account)
		if err != nil {
			res.Errors = append(res.Errors, BucketError{Bucket: b.name, Err: err})
			continue
		}
		all = append(all, listed{b.name, region, objs})
	}

	// After the listing: a record written by the time of this read is seen,
	// so an object listed above whose schedule exists is never orphaned.
	records, err := instancerole.ScanSchedules(ctx, c.DB, c.Table)
	if err != nil {
		return res, fmt.Errorf("%w: %w", ErrTableUnreadable, err)
	}
	ids := map[string]bool{}
	keys := map[string]bool{}
	for i := range records {
		rec := &records[i]
		ids[rec.ScheduleID] = true
		if rec.S3ParamsKey != "" {
			keys[rec.ParamsBucketName(scheduler.ParamsBucketTemplate)+"/"+rec.S3ParamsKey] = true
		}
	}

	for _, l := range all {
		api := c.S3(l.region)
		for _, o := range l.objs {
			key := aws.ToString(o.Key)
			id, _ := ScheduleID(key)
			f := Facts{
				Key:          key,
				LastModified: aws.ToTime(o.LastModified),
				Named:        (id != "" && ids[id]) || keys[l.bucket+"/"+key],
			}
			d := Decision{Bucket: l.bucket, Region: l.region, Key: key, LastModified: f.LastModified}
			if ok, why := decideUntagged(f, opts.Now); !ok {
				d.Reason = why
				res.Objects = append(res.Objects, d)
				continue
			}
			tags, err := objectTags(ctx, api, l.bucket, key, opts.Account)
			if err != nil {
				d.Reason, d.Err = KeepTagsUnread, err
				res.Objects = append(res.Objects, d)
				continue
			}
			f.Tags = tags
			d.Delete, d.Reason = Decide(f, opts.Now)
			if d.Delete && opts.Delete {
				if _, err := api.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(l.bucket), Key: aws.String(key), ExpectedBucketOwner: aws.String(opts.Account),
				}); err != nil {
					d.Err = fmt.Errorf("delete s3://%s/%s: %w", l.bucket, key, err)
				} else {
					d.Deleted = true
				}
			}
			res.Objects = append(res.Objects, d)
		}
	}
	return res, nil
}

type bucket struct{ name, region string }

// listBuckets returns the account's spawn-schedules-* buckets, sorted.
func listBuckets(ctx context.Context, api S3API) ([]bucket, error) {
	var out []bucket
	in := &s3.ListBucketsInput{Prefix: aws.String(BucketPrefix), MaxBuckets: aws.Int32(1000)}
	for {
		page, err := api.ListBuckets(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("list %s* buckets: %w", BucketPrefix, err)
		}
		for _, b := range page.Buckets {
			name := aws.ToString(b.Name)
			if strings.HasPrefix(name, BucketPrefix) {
				out = append(out, bucket{name: name, region: aws.ToString(b.BucketRegion)})
			}
		}
		tok := aws.ToString(page.ContinuationToken)
		if tok == "" || tok == aws.ToString(in.ContinuationToken) {
			break
		}
		in.ContinuationToken = aws.String(tok)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// listParams lists every object under KeyPrefix in one bucket. A bucket
// that no longer exists is empty.
func listParams(ctx context.Context, api S3API, bucket, owner string) ([]s3types.Object, error) {
	var out []s3types.Object
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(KeyPrefix), ExpectedBucketOwner: aws.String(owner),
	}
	for {
		page, err := api.ListObjectsV2(ctx, in)
		if err != nil {
			if isNoSuchBucket(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("list s3://%s/%s: %w", bucket, KeyPrefix, err)
		}
		for _, o := range page.Contents {
			if strings.HasPrefix(aws.ToString(o.Key), KeyPrefix) {
				out = append(out, o)
			}
		}
		if !aws.ToBool(page.IsTruncated) || aws.ToString(page.NextContinuationToken) == "" {
			break
		}
		in.ContinuationToken = page.NextContinuationToken
	}
	return out, nil
}

func objectTags(ctx context.Context, api S3API, bucket, key, owner string) (map[string]string, error) {
	out, err := api.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String(bucket), Key: aws.String(key), ExpectedBucketOwner: aws.String(owner),
	})
	if err != nil {
		return nil, fmt.Errorf("get tags of s3://%s/%s: %w", bucket, key, err)
	}
	tags := map[string]string{}
	for _, t := range out.TagSet {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags, nil
}

func isNoSuchBucket(err error) bool {
	var nsb *s3types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket"
}
