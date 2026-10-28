package scheduler

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	awsscheduler "github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1382: everything `spawn schedule create` makes for a schedule — the params
// object, the EventBridge schedule, the record — lives exactly as long as the
// schedule, and each has a named deleter.

type lifetimeEnv struct {
	client *Client
	s3     *s3.Client
	eb     *recordingEB
	db     *dynamodb.Client
}

// recordingEB passes calls to Substrate and keeps the create/update inputs.
type recordingEB struct {
	*awsscheduler.Client
	creates []*awsscheduler.CreateScheduleInput
	updates []*awsscheduler.UpdateScheduleInput
}

func (r *recordingEB) CreateSchedule(ctx context.Context, in *awsscheduler.CreateScheduleInput, o ...func(*awsscheduler.Options)) (*awsscheduler.CreateScheduleOutput, error) {
	r.creates = append(r.creates, in)
	return r.Client.CreateSchedule(ctx, in, o...)
}

func (r *recordingEB) UpdateSchedule(ctx context.Context, in *awsscheduler.UpdateScheduleInput, o ...func(*awsscheduler.Options)) (*awsscheduler.UpdateScheduleOutput, error) {
	r.updates = append(r.updates, in)
	return r.Client.UpdateSchedule(ctx, in, o...)
}

func newLifetimeEnv(t *testing.T) *lifetimeEnv {
	t.Helper()
	env := testutil.SubstrateServer(t)
	db := env.DynamoClient()
	createSchedulesTable(t, db)
	eb := &recordingEB{Client: awsscheduler.NewFromConfig(env.AWSConfig)}
	c := NewClientWithTableName(env.AWSConfig, testLambdaARN, testRoleARN, testAccountID, TableName)
	c.schedulerClient = eb
	return &lifetimeEnv{client: c, s3: s3.NewFromConfig(env.AWSConfig), eb: eb, db: db}
}

// create makes a schedule the way the CLI does: params uploaded first, then
// CreateSchedule.
func (e *lifetimeEnv) create(t *testing.T, id string, mut func(*ScheduleRecord)) *ScheduleRecord {
	t.Helper()
	ctx := context.Background()
	bucket := "spawn-schedules-us-east-1"
	_, _ = e.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	key := ParamsKeyPrefix + id + "/params.yaml"
	if _, err := e.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("params: []\n")}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	rec := &ScheduleRecord{
		ScheduleID: id, UserID: testAccountID, ScheduleName: id,
		ScheduleExpression: "cron(0 2 * * ? *)", ScheduleType: string(ScheduleTypeRecurring), Timezone: "UTC",
		S3ParamsKey: key, S3ParamsBucket: bucket, Region: "us-east-1",
		Status: string(ScheduleStatusActive),
	}
	if mut != nil {
		mut(rec)
	}
	if _, err := e.client.CreateSchedule(ctx, rec); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return rec
}

func (e *lifetimeEnv) paramsExist(t *testing.T, rec *ScheduleRecord) bool {
	t.Helper()
	_, err := e.s3.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(rec.S3ParamsBucket), Key: aws.String(rec.S3ParamsKey)})
	return err == nil
}

func (e *lifetimeEnv) ebSchedule(t *testing.T, id string) *awsscheduler.GetScheduleOutput {
	t.Helper()
	out, err := e.eb.GetSchedule(context.Background(), &awsscheduler.GetScheduleInput{Name: aws.String(id)})
	var nf *schedulertypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		return nil
	}
	if err != nil {
		t.Fatalf("GetSchedule(%s): %v", id, err)
	}
	return out
}

func (e *lifetimeEnv) rawRecord(t *testing.T, id string) map[string]dynamodbtypes.AttributeValue {
	t.Helper()
	out, err := e.db.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName: aws.String(TableName),
		Key:       map[string]dynamodbtypes.AttributeValue{"schedule_id": &dynamodbtypes.AttributeValueMemberS{Value: id}},
	})
	if err != nil {
		t.Fatalf("GetItem(%s): %v", id, err)
	}
	return out.Item
}

// `spawn schedule cancel` deletes the params object, the EventBridge
// schedule and the record, and a second cancel succeeds.
func TestDeleteScheduleDeletesParamsEventBridgeScheduleAndRecord(t *testing.T) {
	e := newLifetimeEnv(t)
	rec := e.create(t, "sched-cancel-1", nil)
	if !e.paramsExist(t, rec) || e.ebSchedule(t, rec.ScheduleID) == nil || e.rawRecord(t, rec.ScheduleID) == nil {
		t.Fatal("setup: the schedule's three resources are not all there")
	}

	if err := e.client.DeleteSchedule(context.Background(), rec.ScheduleID); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if e.paramsExist(t, rec) {
		t.Errorf("params s3://%s/%s survive cancel; nothing else deletes them (#1382)", rec.S3ParamsBucket, rec.S3ParamsKey)
	}
	if e.ebSchedule(t, rec.ScheduleID) != nil {
		t.Error("the EventBridge schedule survives cancel")
	}
	if e.rawRecord(t, rec.ScheduleID) != nil {
		t.Error("the record survives cancel")
	}
	if err := e.client.DeleteSchedule(context.Background(), rec.ScheduleID); err != nil {
		t.Errorf("a second cancel of the same schedule failed: %v", err)
	}
}

// A recurring schedule with no end has no record TTL: its record must not
// age out while its EventBridge schedule fires. (It used to be now+90 days.)
func TestRecurringScheduleRecordHasNoTTL(t *testing.T) {
	e := newLifetimeEnv(t)
	for _, mut := range []func(*ScheduleRecord){
		nil,
		func(r *ScheduleRecord) { r.MaxExecutions = 1000 }, // counts runs, not time
	} {
		rec := e.create(t, "sched-rec-"+time.Now().Format("150405.000000"), mut)
		if ttl, ok := e.rawRecord(t, rec.ScheduleID)["ttl"]; ok {
			t.Errorf("active recurring schedule %s (max_executions=%d) carries ttl %v; DynamoDB deletes its "+
				"record while the EventBridge schedule keeps firing (#1382)", rec.ScheduleID, rec.MaxExecutions, ttl)
		}
	}
}

// A finite schedule's TTL comes after its last possible firing: for every
// time FiringAt can still say FiringLaunches, the record is still there.
func TestFiniteScheduleTTLComesAfterItsLastFiring(t *testing.T) {
	far := time.Now().Add(400 * 24 * time.Hour).UTC().Truncate(time.Second)
	cases := map[string]struct {
		rec  ScheduleRecord
		last time.Time
	}{
		"one-time": {ScheduleRecord{ScheduleExpression: "at(" + far.Format("2006-01-02T15:04:05") + ")",
			ScheduleType: string(ScheduleTypeOneTime), Timezone: "UTC"}, far},
		"one-time in a zone west of UTC": {ScheduleRecord{ScheduleExpression: "at(" + far.Format("2006-01-02T15:04:05") + ")",
			ScheduleType: string(ScheduleTypeOneTime), Timezone: "America/Los_Angeles"}, far.Add(9 * time.Hour)},
		"end-after": {ScheduleRecord{ScheduleExpression: "cron(0 2 * * ? *)",
			ScheduleType: string(ScheduleTypeRecurring), EndAfter: far}, far},
	}
	for name, tc := range cases {
		rec := tc.rec
		rec.Status = string(ScheduleStatusActive)
		ttl := rec.RecordTTL()
		if ttl == 0 {
			t.Errorf("%s: finite schedule has no TTL; it would never age out without a cancel", name)
			continue
		}
		if !time.Unix(ttl, 0).After(tc.last) {
			t.Errorf("%s: TTL %s is not after the last firing %s", name, time.Unix(ttl, 0).UTC(), tc.last)
		}
		if rec.FiringAt(tc.last) == FiringLaunches && !time.Unix(ttl, 0).After(tc.last) {
			t.Errorf("%s: the record expires while FiringAt still launches", name)
		}
	}
}

// SaveSchedule writes RecordTTL, not a flat window from now.
func TestSaveScheduleWritesTheFiniteTTL(t *testing.T) {
	e := newLifetimeEnv(t)
	end := time.Now().Add(400 * 24 * time.Hour).UTC().Truncate(time.Second)
	rec := e.create(t, "sched-end-1", func(r *ScheduleRecord) { r.EndAfter = end })
	n, ok := e.rawRecord(t, rec.ScheduleID)["ttl"].(*dynamodbtypes.AttributeValueMemberN)
	if !ok {
		t.Fatal("an end-after schedule's record has no numeric ttl")
	}
	if want := end.Add(RecordTTLMargin).Unix(); n.Value != strconv.FormatInt(want, 10) {
		t.Errorf("ttl = %s, want %d (end_after + %s)", n.Value, want, RecordTTLMargin)
	}
	// Its EventBridge schedule ends with its record. (Asserted on the
	// request: Substrate drops EndDate and ActionAfterCompletion.)
	in := e.eb.creates[len(e.eb.creates)-1]
	if in.EndDate == nil || !in.EndDate.Equal(end.Add(RecordTTLMargin)) {
		t.Errorf("EventBridge EndDate = %v, want %s", in.EndDate, end.Add(RecordTTLMargin))
	}
	if in.ActionAfterCompletion != schedulertypes.ActionAfterCompletionDelete {
		t.Errorf("ActionAfterCompletion = %q, want DELETE", in.ActionAfterCompletion)
	}
}

// A one-time schedule is one execution, and its EventBridge schedule deletes
// itself once it has fired.
func TestOneTimeScheduleRunsOutAndDeletesItself(t *testing.T) {
	e := newLifetimeEnv(t)
	rec := e.create(t, "sched-once-1", func(r *ScheduleRecord) {
		r.ScheduleType = string(ScheduleTypeOneTime)
		r.ScheduleExpression = "at(2099-01-01T00:00:00)"
	})
	rec.ExecutionCount = 1
	if rec.FiringAt(time.Now()) == FiringLaunches {
		t.Error("a one-time schedule that has run once would launch again (FiringAt)")
	}
	if got := e.eb.creates[len(e.eb.creates)-1].ActionAfterCompletion; got != schedulertypes.ActionAfterCompletionDelete {
		t.Errorf("ActionAfterCompletion = %q, want DELETE: a fired one-time schedule would stay in EventBridge", got)
	}
	// Pausing replaces the whole schedule; it must not drop the self-delete
	// or the target's input.
	if err := e.client.UpdateScheduleStatus(context.Background(), rec.ScheduleID, ScheduleStatusPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	up := e.eb.updates[len(e.eb.updates)-1]
	if up.ActionAfterCompletion != schedulertypes.ActionAfterCompletionDelete || up.Target == nil || aws.ToString(up.Target.Input) == "" {
		t.Errorf("pause dropped ActionAfterCompletion (%q) or the target input", up.ActionAfterCompletion)
	}
}

// CompleteSchedule (scheduler-handler, on a run-out schedule) deletes the
// EventBridge schedule and params, and leaves a record FiringAt reports as
// not launching, with a TTL.
func TestCompleteScheduleDeletesWhatARunOutScheduleLeaves(t *testing.T) {
	e := newLifetimeEnv(t)
	rec := e.create(t, "sched-done-1", func(r *ScheduleRecord) { r.MaxExecutions = 2 })
	now := time.Now()
	if err := e.client.CompleteSchedule(context.Background(), rec, now); err != nil {
		t.Fatalf("CompleteSchedule: %v", err)
	}
	if e.paramsExist(t, rec) {
		t.Error("params survive completion")
	}
	if e.ebSchedule(t, rec.ScheduleID) != nil {
		t.Error("the EventBridge schedule survives completion")
	}
	got, err := e.client.GetSchedule(context.Background(), rec.ScheduleID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if got.FiringAt(now) == FiringLaunches {
		t.Errorf("a completed schedule (status %q) still reads as launching; the reaper's role sweep "+
			"would demand params that are gone (#1378)", got.Status)
	}
	if got.TTL <= now.Unix() {
		t.Errorf("completed schedule's ttl = %d, want after now", got.TTL)
	}
	// And it cannot be resumed into looking live again.
	if err := e.client.UpdateScheduleStatus(context.Background(), rec.ScheduleID, ScheduleStatusActive); err == nil {
		t.Error("a completed schedule was resumed")
	}
}
