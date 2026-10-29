package schedulesweep_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/schedulesweep"
	"github.com/spore-host/spawn/pkg/schedulesweep/schedulesweeptest"
	"github.com/spore-host/spawn/pkg/staging"
	"github.com/spore-host/spawn/pkg/testutil"
)

// --- Decide ----------------------------------------------------------------

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	old := now.Add(-schedulesweep.Grace - 24*time.Hour)
	id := "sched-20260801-010101"
	key := schedulesweeptest.Key(id)
	ok := schedulesweep.Facts{Key: key, LastModified: old, Tags: schedulesweeptest.ParamsTags(id)}
	if del, why := schedulesweep.Decide(ok, now); !del {
		t.Fatalf("an old, unnamed, schedule-tagged params object is kept: %s", why)
	}
	for _, c := range []struct {
		name string
		mut  func(*schedulesweep.Facts)
		want string
	}{
		{"other key", func(f *schedulesweep.Facts) { f.Key = "schedules/" + id + "/other.yaml" }, schedulesweep.KeepNotParams},
		{"nested key", func(f *schedulesweep.Facts) { f.Key = "schedules/a/b/params.yaml" }, schedulesweep.KeepNotParams},
		{"named", func(f *schedulesweep.Facts) { f.Named = true }, schedulesweep.KeepLive},
		{"no time", func(f *schedulesweep.Facts) { f.LastModified = time.Time{} }, schedulesweep.KeepNoModified},
		{"just inside the margin", func(f *schedulesweep.Facts) { f.LastModified = now.Add(-schedulesweep.Grace + time.Minute) }, schedulesweep.KeepYoung},
		{"tags unread", func(f *schedulesweep.Facts) { f.Tags = nil }, schedulesweep.KeepTagsUnread},
		{"queue config", func(f *schedulesweep.Facts) { f.Tags = schedulesweeptest.QueueTags(id) }, schedulesweep.KeepQueueConfig},
		{"QueueID tag alone", func(f *schedulesweep.Facts) { f.Tags["QueueID"] = id }, schedulesweep.KeepQueueConfig},
		{"untagged", func(f *schedulesweep.Facts) { f.Tags = map[string]string{} }, schedulesweep.KeepUntagged},
		{"tag names another id", func(f *schedulesweep.Facts) { f.Tags["ScheduleID"] = "sched-20260801-999999" }, schedulesweep.KeepTagMismatch},
		{"pre-#1399 queue config", func(f *schedulesweep.Facts) {
			q := "queue-20260801-010101"
			f.Key, f.Tags = schedulesweeptest.Key(q), schedulesweeptest.ParamsTags(q)
		}, schedulesweep.KeepNotSchedID},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := ok
			f.Tags = map[string]string{}
			for k, v := range ok.Tags {
				f.Tags[k] = v
			}
			c.mut(&f)
			del, why := schedulesweep.Decide(f, now)
			if del || why != c.want {
				t.Errorf("Decide = %v %q, want keep %q", del, why, c.want)
			}
		})
	}
}

// The schedule ID form the sweep accepts is the one the CLI generates, and
// not the one a batch queue gets.
func TestScheduleIDFormIsTheGeneratedOne(t *testing.T) {
	if id := scheduler.GenerateScheduleID(); !schedulesweep.LooksLikeScheduleID(id) {
		t.Errorf("scheduler.GenerateScheduleID() = %q, which the sweep would never delete", id)
	}
	if schedulesweep.LooksLikeScheduleID("queue-20260801-010101") {
		t.Error("a queue id looks like a schedule id")
	}
}

func TestBucketRegion(t *testing.T) {
	for _, c := range []struct {
		name, want string
		ok         bool
	}{
		{"spawn-schedules-us-east-1", "us-east-1", true},
		{"spawn-schedules-111122223333-ap-southeast-2", "ap-southeast-2", true},
		{"spawn-schedules-999999999999-us-east-1", "", false},
		{"spawn-schedules-backups", "", false},
		{"spawn-results-111122223333-us-east-1", "", false},
	} {
		got, ok := schedulesweep.BucketRegion(c.name, "111122223333")
		if got != c.want || ok != c.ok {
			t.Errorf("BucketRegion(%q) = %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// --- the shared scenario ---------------------------------------------------

func sweptKeys(res schedulesweep.Result, deleted bool) []string {
	var out []string
	for _, d := range res.Objects {
		if (deleted && d.Deleted) || (!deleted && d.Delete && d.Err == nil) {
			out = append(out, d.Bucket+"/"+d.Key)
		}
	}
	sort.Strings(out)
	return out
}

func TestSweepScenario(t *testing.T) {
	s := schedulesweeptest.New()
	opts := schedulesweep.Options{Account: schedulesweeptest.Account, Now: s.Now}

	res, err := schedulesweep.Sweep(context.Background(), s.Clients(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.S3.Deleted) != 0 {
		t.Fatalf("report deleted %v", s.S3.Deleted)
	}
	if got := sweptKeys(res, false); strings.Join(got, ",") != strings.Join(schedulesweeptest.WantDeleted, ",") {
		t.Errorf("report would delete %v, want %v", got, schedulesweeptest.WantDeleted)
	}

	opts.Delete = true
	res, err = schedulesweep.Sweep(context.Background(), s.Clients(), opts)
	if err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), s.S3.Deleted...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(schedulesweeptest.WantDeleted, ",") {
		t.Errorf("reap deleted %v, want %v", got, schedulesweeptest.WantDeleted)
	}
	scanned, deleted, would, skipped, failed := res.Count()
	if scanned != schedulesweeptest.WantScanned || deleted != 2 || would != 0 || failed != 0 || skipped != scanned-2 {
		t.Errorf("Count = %d/%d/%d/%d/%d", scanned, deleted, would, skipped, failed)
	}
	if strings.Join(res.Foreign, ",") != schedulesweeptest.ForeignBucket {
		t.Errorf("Foreign = %v, want the misnamed bucket", res.Foreign)
	}
	if len(s.S3.BadOwner) != 0 || len(s.S3.WrongRegion) != 0 {
		t.Errorf("calls without the account as owner %v, or in the wrong region %v", s.S3.BadOwner, s.S3.WrongRegion)
	}
	if s.DB.Inconsistent != 0 {
		t.Errorf("%d Scan(s) without ConsistentRead", s.DB.Inconsistent)
	}
}

// Fail closed: a table that cannot be read completely, on the first page or
// a later one, deletes nothing at all.
func TestUnreadableTableDeletesNothing(t *testing.T) {
	for _, later := range []bool{false, true} {
		s := schedulesweeptest.New()
		s.DB.Err, s.DB.ErrAfterFirstPage = errors.New("ProvisionedThroughputExceededException"), later
		_, err := schedulesweep.Sweep(context.Background(), s.Clients(),
			schedulesweep.Options{Account: schedulesweeptest.Account, Now: s.Now, Delete: true})
		if !errors.Is(err, schedulesweep.ErrTableUnreadable) {
			t.Errorf("later page %v: err = %v, want ErrTableUnreadable", later, err)
		}
		if len(s.S3.Deleted) != 0 {
			t.Errorf("later page %v: deleted %v with the table unreadable", later, s.S3.Deleted)
		}
		if later && s.DB.Scans < 2 {
			t.Errorf("the scan never reached the failing page (%d scans)", s.DB.Scans)
		}
	}
}

// An object whose tags cannot be read is kept, and counted as failed.
func TestUnreadableTagsKeepTheObject(t *testing.T) {
	s := schedulesweeptest.New()
	s.S3.TagErr = errors.New("AccessDenied")
	res, err := schedulesweep.Sweep(context.Background(), s.Clients(),
		schedulesweep.Options{Account: schedulesweeptest.Account, Now: s.Now, Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.S3.Deleted) != 0 {
		t.Errorf("deleted %v without reading their tags", s.S3.Deleted)
	}
	if _, _, _, _, failed := res.Count(); failed == 0 {
		t.Error("unreadable tags were not counted as failed")
	}
}

// --- Substrate ---------------------------------------------------------------

// subEnv is real S3 and DynamoDB (Substrate) with the schedules table, a
// schedules bucket, and params uploaded by pkg/staging itself.
type subEnv struct {
	t       *testing.T
	s3      *s3.Client
	db      *dynamodb.Client
	rec     *testutil.S3Recorder
	stage   *staging.Client
	account string
	bucket  string
	clients schedulesweep.Clients
}

func newSubEnv(t *testing.T, withTable bool) *subEnv {
	t.Helper()
	ctx := context.Background()
	env := testutil.SubstrateServer(t)
	id, err := sts.NewFromConfig(env.AWSConfig).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := env.AWSConfig.Copy()
	rec := testutil.NewS3Recorder(cfg.HTTPClient)
	cfg.HTTPClient = rec
	e := &subEnv{
		t: t, s3: s3.NewFromConfig(env.AWSConfig), db: dynamodb.NewFromConfig(env.AWSConfig), rec: rec,
		stage: staging.NewClient(cfg, aws.ToString(id.Account)), account: aws.ToString(id.Account),
		bucket: "spawn-schedules-us-east-1",
	}
	if withTable {
		if _, err := e.db.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName:            aws.String(scheduler.TableName),
			AttributeDefinitions: []ddbtypes.AttributeDefinition{{AttributeName: aws.String("schedule_id"), AttributeType: ddbtypes.ScalarAttributeTypeS}},
			KeySchema:            []ddbtypes.KeySchemaElement{{AttributeName: aws.String("schedule_id"), KeyType: ddbtypes.KeyTypeHash}},
			BillingMode:          ddbtypes.BillingModePayPerRequest,
		}); err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
	}
	e.clients = schedulesweep.Clients{
		DB: e.db, Table: scheduler.TableName,
		S3: func(string) schedulesweep.S3API { return e.s3 },
	}
	return e
}

// upload puts params (or a queue config) through pkg/staging, then applies
// the tags staging sent: Substrate drops x-amz-tagging on PutObject
// (scttfrdmn/substrate#1434), so they are copied from the recorded request.
func (e *subEnv) upload(id string, queue bool) map[string]string {
	e.t.Helper()
	ctx := context.Background()
	p := filepath.Join(e.t.TempDir(), "params.yaml")
	if err := os.WriteFile(p, []byte("params: []\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.rec.Reset()
	upload := e.stage.UploadScheduleParams
	if queue {
		upload = e.stage.UploadQueueConfig
	}
	bucket, key, _, _, err := upload(ctx, p, id, "us-east-1")
	if err != nil {
		e.t.Fatalf("upload %s: %v", id, err)
	}
	if bucket != e.bucket || key != schedulesweeptest.Key(id) {
		e.t.Fatalf("staging uploaded s3://%s/%s; the sweep looks at s3://%s/%s", bucket, key, e.bucket, schedulesweeptest.Key(id))
	}
	tagging := ""
	for _, q := range e.rec.Requests() {
		if q.PutObject() {
			tagging = q.Header.Get("X-Amz-Tagging")
		}
	}
	if tagging == "" {
		e.t.Fatalf("staging's upload of %s carried no tags", id)
	}
	tags := map[string]string{}
	var set []s3types.Tag
	for _, kv := range strings.Split(tagging, "&") {
		k, v, _ := strings.Cut(kv, "=")
		tags[k] = v
		set = append(set, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	if _, err := e.s3.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Tagging: &s3types.Tagging{TagSet: set},
	}); err != nil {
		e.t.Fatalf("PutObjectTagging: %v", err)
	}
	return tags
}

func (e *subEnv) putRecord(id string) {
	e.t.Helper()
	item, err := attributevalue.MarshalMap(scheduler.ScheduleRecord{
		ScheduleID: id, Region: "us-east-1", S3ParamsBucket: e.bucket, S3ParamsKey: schedulesweeptest.Key(id), Status: "active",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.PutItem(context.Background(), &dynamodb.PutItemInput{TableName: aws.String(scheduler.TableName), Item: item}); err != nil {
		e.t.Fatalf("PutItem: %v", err)
	}
}

func (e *subEnv) exists(id string) bool {
	_, err := e.s3.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(e.bucket), Key: aws.String(schedulesweeptest.Key(id))})
	return err == nil
}

func (e *subEnv) sweep(now time.Time, del bool) (schedulesweep.Result, error) {
	return schedulesweep.Sweep(context.Background(), e.clients, schedulesweep.Options{Account: e.account, Now: now, Delete: del})
}

func decisionFor(res schedulesweep.Result, id string) (schedulesweep.Decision, bool) {
	for _, d := range res.Objects {
		if d.Key == schedulesweeptest.Key(id) {
			return d, true
		}
	}
	return schedulesweep.Decision{}, false
}

// The tags pkg/staging writes are the ones this package reads (both kinds),
// by the upload's own request, not by a second copy of the constants.
func TestTagsMatchStaging(t *testing.T) {
	e := newSubEnv(t, true)
	id, q := scheduler.GenerateScheduleID(), "queue-20260801-010101"
	if got, want := e.upload(id, false), schedulesweeptest.ParamsTags(id); !sameTags(got, want) {
		t.Errorf("staging tags schedule params %v; the sweep expects %v", got, want)
	}
	if got, want := e.upload(q, true), schedulesweeptest.QueueTags(q); !sameTags(got, want) {
		t.Errorf("staging tags a queue config %v; the sweep expects %v", got, want)
	}
	if staging.QueueConfigTagKey != schedulesweep.ComponentTagKey || staging.QueueConfigTagValue != schedulesweep.QueueTagValue ||
		staging.ScheduleParamsTagValue != schedulesweep.ScheduleTagValue {
		t.Error("pkg/staging's tag constants and this package's differ")
	}
}

func sameTags(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Gate (#1398): on real S3 and DynamoDB, an orphan older than the margin is
// reported in report mode (and nothing changes) and deleted in reap mode,
// while params with a live record, a current queue config and a pre-#1399
// queue config, all as old, are kept.
func TestSubstrateOrphanReportedThenDeleted(t *testing.T) {
	e := newSubEnv(t, true)
	orphan, live := "sched-20260801-010101", "sched-20260801-020202"
	queue, legacy := "queue-20260801-030303", "queue-20260801-040404"
	e.upload(orphan, false)
	e.upload(live, false)
	e.putRecord(live)
	e.upload(queue, true)
	// A pre-#1399 queue config was uploaded with schedule params' tags.
	e.upload(legacy, false)

	later := time.Now().Add(schedulesweep.Grace + 24*time.Hour)
	res, err := e.sweep(later, false)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := decisionFor(res, orphan); !d.Delete || d.Deleted {
		t.Errorf("report: orphan decision %+v, want would-delete", d)
	}
	if !e.exists(orphan) {
		t.Fatal("report mode deleted the orphan")
	}

	res, err = e.sweep(later, true)
	if err != nil {
		t.Fatal(err)
	}
	if e.exists(orphan) {
		d, _ := decisionFor(res, orphan)
		t.Errorf("reap kept the orphan: %+v", d)
	}
	for id, why := range map[string]string{
		live:   schedulesweep.KeepLive,
		queue:  schedulesweep.KeepQueueConfig,
		legacy: schedulesweep.KeepNotSchedID,
	} {
		if !e.exists(id) {
			t.Errorf("reap deleted %s", id)
		}
		if d, _ := decisionFor(res, id); d.Reason != why {
			t.Errorf("%s kept for %q, want %q", id, d.Reason, why)
		}
	}
}

// Gate (#1398): a params object younger than the margin is kept even with no
// record: the CLI uploads params before it writes the record.
func TestSubstrateYoungOrphanIsKept(t *testing.T) {
	e := newSubEnv(t, true)
	id := scheduler.GenerateScheduleID()
	e.upload(id, false)
	res, err := e.sweep(time.Now().Add(schedulesweep.Grace-24*time.Hour), true)
	if err != nil {
		t.Fatal(err)
	}
	if !e.exists(id) {
		t.Fatal("deleted params younger than the margin")
	}
	if d, _ := decisionFor(res, id); d.Reason != schedulesweep.KeepYoung {
		t.Errorf("kept for %q, want %q", d.Reason, schedulesweep.KeepYoung)
	}
}

// Gate (#1398): with no readable table, nothing is deleted.
func TestSubstrateMissingTableDeletesNothing(t *testing.T) {
	e := newSubEnv(t, false)
	id := "sched-20260801-010101"
	e.upload(id, false)
	_, err := e.sweep(time.Now().Add(schedulesweep.Grace+24*time.Hour), true)
	if !errors.Is(err, schedulesweep.ErrTableUnreadable) {
		t.Errorf("err = %v, want ErrTableUnreadable", err)
	}
	if !e.exists(id) {
		t.Error("deleted params with the table unreadable")
	}
}
