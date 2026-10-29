// Package schedulesweeptest is one schedules-bucket scenario, with in-memory
// S3 and DynamoDB, that `spawn cleanup --schedule-params` and the TTL
// reaper's tests both run (#1398). Each asserts it deletes exactly Want, so
// the CLI and the reaper are held to the same answer on the same account,
// not merely to the same package.
package schedulesweeptest

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/schedulesweep"
)

const (
	// Account owns the buckets and the table.
	Account = "111122223333"
	// Region holds the shared-name bucket; OtherRegion the account-scoped one.
	Region      = "us-east-1"
	OtherRegion = "eu-west-1"
)

// The scenario's buckets.
var (
	SharedBucket  = schedulesweep.BucketPrefix + Region
	AccountBucket = schedulesweep.BucketPrefix + Account + "-" + OtherRegion
	// ForeignBucket is in the account and starts spawn-schedules-, but is not
	// a name spawn gives a schedules bucket.
	ForeignBucket = schedulesweep.BucketPrefix + "backups"
)

// Key is a params key.
func Key(id string) string { return schedulesweep.KeyPrefix + id + "/" + schedulesweep.ParamsFile }

// ParamsTags and QueueTags are what pkg/staging tags an upload with.
func ParamsTags(id string) map[string]string {
	return map[string]string{"Project": "spawn", schedulesweep.ComponentTagKey: schedulesweep.ScheduleTagValue, schedulesweep.ScheduleIDTagKey: id}
}
func QueueTags(id string) map[string]string {
	return map[string]string{"Project": "spawn", schedulesweep.ComponentTagKey: schedulesweep.QueueTagValue, schedulesweep.QueueIDTagKey: id}
}

// IDs in the scenario.
const (
	Orphan        = "sched-20260801-010101" // no record, 40 days old           -> deleted
	OrphanOther   = "sched-20260801-020202" // same, in the account bucket      -> deleted
	Live          = "sched-20260801-030303" // a record names it                -> kept
	LiveByKey     = "sched-20260801-040404" // a record with another id names its key -> kept
	Young         = "sched-20261020-050505" // no record, 2 days old            -> kept
	Queue         = "queue-20260801-060606" // Component=queue (#1399 on)       -> kept
	LegacyQueue   = "queue-20260801-070707" // tagged like params (pre-#1399)   -> kept
	Untagged      = "sched-20260801-080808" // no tags                          -> kept
	Mismatched    = "sched-20260801-090909" // ScheduleID tag names another id  -> kept
	ForeignOrphan = "sched-20260801-101010" // orphan in ForeignBucket          -> kept
)

// WantDeleted are the bucket/key pairs any correct sweep in reap mode
// deletes, and what report mode says it would, sorted.
var WantDeleted = []string{
	AccountBucket + "/" + Key(OrphanOther),
	SharedBucket + "/" + Key(Orphan),
}

// WantScanned is how many params objects a sweep counts (every
// schedules/<id>/params.yaml in the two schedules buckets).
const WantScanned = 9

// Scenario is the account. Now is when it was built; every age in it is at
// least a day either side of the 30-day margin.
type Scenario struct {
	Now time.Time
	S3  *S3
	DB  *DB
}

// New builds the scenario.
func New() *Scenario {
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-40 * 24 * time.Hour)
	objs := map[string]*Object{}
	put := func(bucket, id string, mod time.Time, tags map[string]string) {
		objs[bucket+"/"+Key(id)] = &Object{Modified: mod, Tags: tags}
	}
	put(SharedBucket, Orphan, old, ParamsTags(Orphan))
	put(AccountBucket, OrphanOther, old, ParamsTags(OrphanOther))
	put(SharedBucket, Live, old, ParamsTags(Live))
	put(SharedBucket, LiveByKey, old, ParamsTags(LiveByKey))
	put(SharedBucket, Young, now.Add(-48*time.Hour), ParamsTags(Young))
	put(SharedBucket, Queue, old, QueueTags(Queue))
	put(SharedBucket, LegacyQueue, old, ParamsTags(LegacyQueue))
	put(SharedBucket, Untagged, old, map[string]string{})
	mm := ParamsTags(Mismatched)
	mm[schedulesweep.ScheduleIDTagKey] = Orphan
	put(SharedBucket, Mismatched, old, mm)
	put(ForeignBucket, ForeignOrphan, old, ParamsTags(ForeignOrphan))
	objs[SharedBucket+"/schedules/notes.txt"] = &Object{Modified: old, Tags: map[string]string{}}

	return &Scenario{
		Now: now,
		S3: &S3{
			Buckets: map[string]string{SharedBucket: Region, AccountBucket: OtherRegion, ForeignBucket: Region},
			Objects: objs,
		},
		DB: &DB{Records: []scheduler.ScheduleRecord{
			{ScheduleID: Live, Region: Region, S3ParamsBucket: SharedBucket, S3ParamsKey: Key(Live), Status: "active"},
			{ScheduleID: "sched-20260801-111111", Region: Region, S3ParamsBucket: SharedBucket, S3ParamsKey: Key(LiveByKey), Status: "paused"},
		}},
	}
}

// Clients is the scenario as schedulesweep.Clients.
func (s *Scenario) Clients() schedulesweep.Clients {
	return schedulesweep.Clients{
		DB: s.DB, Table: scheduler.TableName,
		S3: func(region string) schedulesweep.S3API { return &RegionS3{S3: s.S3, Region: region} },
	}
}

// Remaining are the bucket/key pairs still present, sorted.
func (s *Scenario) Remaining() []string {
	s.S3.mu.Lock()
	defer s.S3.mu.Unlock()
	var out []string
	for k := range s.S3.Objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Object is one stored object.
type Object struct {
	Modified time.Time
	Tags     map[string]string
}

// S3 is the account's buckets (name -> region) and objects ("bucket/key").
// Every call must name Account as the expected owner and be made in the
// bucket's region; BadOwner and WrongRegion record any that were not.
// Deleted records DeleteObject calls.
type S3 struct {
	mu          sync.Mutex
	Buckets     map[string]string
	Objects     map[string]*Object
	Deleted     []string
	BadOwner    []string
	WrongRegion []string
	// TagErr, when set, is returned by GetObjectTagging for every object.
	TagErr error
}

// RegionS3 is S3 seen through one region's client.
type RegionS3 struct {
	*S3
	Region string
}

func (f *RegionS3) check(bucket, owner *string) error {
	if aws.ToString(owner) != Account {
		f.BadOwner = append(f.BadOwner, aws.ToString(owner))
	}
	r, ok := f.Buckets[aws.ToString(bucket)]
	if !ok {
		return &s3types.NoSuchBucket{}
	}
	if r != f.Region {
		f.WrongRegion = append(f.WrongRegion, aws.ToString(bucket)+" via "+f.Region)
		return errors.New("PermanentRedirect")
	}
	return nil
}

// ListBuckets implements schedulesweep.S3API.
func (f *RegionS3) ListBuckets(_ context.Context, in *s3.ListBucketsInput, _ ...func(*s3.Options)) (*s3.ListBucketsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &s3.ListBucketsOutput{}
	var names []string
	for n := range f.Buckets {
		if strings.HasPrefix(n, aws.ToString(in.Prefix)) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		out.Buckets = append(out.Buckets, s3types.Bucket{Name: aws.String(n), BucketRegion: aws.String(f.Buckets[n])})
	}
	return out, nil
}

// ListObjectsV2 implements schedulesweep.S3API.
func (f *RegionS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	out := &s3.ListObjectsV2Output{}
	prefix := aws.ToString(in.Bucket) + "/" + aws.ToString(in.Prefix)
	var keys []string
	for k := range f.Objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Contents = append(out.Contents, s3types.Object{
			Key:          aws.String(strings.TrimPrefix(k, aws.ToString(in.Bucket)+"/")),
			LastModified: aws.Time(f.Objects[k].Modified),
		})
	}
	return out, nil
}

// GetObjectTagging implements schedulesweep.S3API.
func (f *RegionS3) GetObjectTagging(_ context.Context, in *s3.GetObjectTaggingInput, _ ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	if f.TagErr != nil {
		return nil, f.TagErr
	}
	o, ok := f.Objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	out := &s3.GetObjectTaggingOutput{}
	for k, v := range o.Tags {
		out.TagSet = append(out.TagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

// DeleteObject implements schedulesweep.S3API.
func (f *RegionS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(in.Bucket, in.ExpectedBucketOwner); err != nil {
		return nil, err
	}
	k := aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	f.Deleted = append(f.Deleted, k)
	delete(f.Objects, k)
	return &s3.DeleteObjectOutput{}, nil
}

// DB is the schedules table. Err, when set, fails every Scan (after the
// first page when ErrAfterFirstPage). Inconsistent counts Scans that did not
// ask for a consistent read.
type DB struct {
	mu                sync.Mutex
	Records           []scheduler.ScheduleRecord
	Err               error
	ErrAfterFirstPage bool
	Scans             int
	Inconsistent      int
}

// Scan implements instancerole.ScheduleScanner, one record per page.
func (f *DB) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Scans++
	if !aws.ToBool(in.ConsistentRead) {
		f.Inconsistent++
	}
	start := 0
	if in.ExclusiveStartKey != nil {
		if v, ok := in.ExclusiveStartKey["schedule_id"].(*ddbtypes.AttributeValueMemberS); ok {
			for i, r := range f.Records {
				if r.ScheduleID == v.Value {
					start = i + 1
				}
			}
		}
	}
	if f.Err != nil && (!f.ErrAfterFirstPage || start > 0) {
		return nil, f.Err
	}
	out := &dynamodb.ScanOutput{}
	if start < len(f.Records) {
		item, err := attributevalue.MarshalMap(f.Records[start])
		if err != nil {
			return nil, err
		}
		out.Items = []map[string]ddbtypes.AttributeValue{item}
		if start+1 < len(f.Records) || f.ErrAfterFirstPage {
			out.LastEvaluatedKey = map[string]ddbtypes.AttributeValue{
				"schedule_id": &ddbtypes.AttributeValueMemberS{Value: f.Records[start].ScheduleID},
			}
		}
	}
	return out, nil
}
