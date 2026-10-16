package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/sweep"
)

// #1026: what a firing schedule hands the orchestrator.
//
// The orchestrator is invoked with a sweep ID only, loads the sweep record by
// that ID, and downloads params from the s3://bucket/key in the record's
// s3_params_key, json-decoding them. Before the fix this handler wrote no record
// at all, and the only key it had was the schedule's bare
// "schedules/<id>/params.yaml" — YAML, in a bucket the orchestrator's role
// cannot read. These assertions are taken from the orchestrator's side of that
// contract (lambda/sweep-orchestrator: handler, loadSweepState, downloadParams),
// not from this file.

func firingSchedule() *ScheduleRecord {
	return &ScheduleRecord{
		ScheduleID:     "sched-1",
		UserID:         "966362334030",
		SweepName:      "nightly",
		S3ParamsKey:    "schedules/sched-1/params.yaml",
		S3ParamsBucket: "spawn-schedules-us-west-2",
		MaxConcurrent:  4,
		LaunchDelay:    "5s",
		Region:         "us-west-2",
		AWSAccountID:   "435415984226",
		Status:         "active",
	}
}

func firingParams() *ParamFileFormat {
	return &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t3.micro", "ami": "ami-0123456789abcdef0"},
		Params:   []map[string]interface{}{{"alpha": 1}, {"alpha": 2}},
	}
}

func TestPrepareSweepPointsTheOrchestratorAtStagedJSON(t *testing.T) {
	const bucket = "spawn-sweeps-us-east-1"
	p, err := prepareSweep(firingSchedule(), firingParams(), "nightly-20261015-000000", bucket, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("prepareSweep: %v", err)
	}

	// The key the record carries must be the s3:// URI of the object this
	// handler stages — not the schedule's own key.
	wantURI := "s3://" + bucket + "/" + p.ParamsKey
	if p.Record.S3ParamsKey != wantURI {
		t.Errorf("record s3_params_key = %q, want %q (the staged object)", p.Record.S3ParamsKey, wantURI)
	}
	if p.ParamsBucket != bucket {
		t.Errorf("params staged in %q, want %q", p.ParamsBucket, bucket)
	}
	// The orchestrator role reads spawn-sweeps-<region>/sweeps/* only.
	if !strings.HasPrefix(p.ParamsKey, "sweeps/") {
		t.Errorf("params key %q is outside sweeps/, which the orchestrator role cannot read", p.ParamsKey)
	}

	// The staged body is JSON the orchestrator can decode, carrying every row.
	var decoded sweep.ParamFileFormat
	if err := json.Unmarshal(p.ParamsBody, &decoded); err != nil {
		t.Fatalf("staged params are not JSON: %v", err)
	}
	if len(decoded.Params) != 2 || decoded.Defaults["ami"] != "ami-0123456789abcdef0" {
		t.Errorf("staged params lost content: %+v", decoded)
	}

	// The record is what loadSweepState needs.
	r := p.Record
	if r.SweepID != "nightly-20261015-000000" || r.Status != "INITIALIZING" || r.TotalParams != 2 {
		t.Errorf("record = %+v", r)
	}
	if r.AWSAccountID != "435415984226" {
		t.Errorf("aws_account_id = %q, want the schedule's compute account", r.AWSAccountID)
	}
	if r.Region != "us-west-2" {
		t.Errorf("region = %q, want the schedule's", r.Region)
	}
	if r.Source != "scheduled" || r.ScheduleID != "sched-1" {
		t.Errorf("source/schedule_id = %q/%q", r.Source, r.ScheduleID)
	}

	// And the payload names the same sweep.
	var ev sweep.OrchestratorEvent
	if err := json.Unmarshal(p.Payload, &ev); err != nil || ev.SweepID != r.SweepID {
		t.Errorf("payload %s does not name sweep %s (err %v)", p.Payload, r.SweepID, err)
	}
}

// A schedule written before aws_account_id existed has no target account.
// Starting a sweep from it would have the orchestrator assume a role in account
// "" — fail here, in the schedule's history, instead.
func TestPrepareSweepRefusesAScheduleWithNoTargetAccount(t *testing.T) {
	s := firingSchedule()
	s.AWSAccountID = ""
	if _, err := prepareSweep(s, firingParams(), "x", "spawn-sweeps-us-east-1", time.Now()); err == nil {
		t.Fatal("prepareSweep accepted a schedule with no aws_account_id")
	}
}

type callLog struct {
	calls  []string
	item   map[string]dynamotypes.AttributeValue
	table  string
	object string
}

type fakeS3 struct{ l *callLog }

func (f fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.l.calls = append(f.l.calls, "s3:PutObject")
	f.l.object = "s3://" + aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	return &s3.PutObjectOutput{}, nil
}

type fakeDynamo struct{ l *callLog }

func (f fakeDynamo) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.l.calls = append(f.l.calls, "dynamodb:PutItem")
	f.l.item, f.l.table = in.Item, aws.ToString(in.TableName)
	return &dynamodb.PutItemOutput{}, nil
}

type fakeLambda struct{ l *callLog }

func (f fakeLambda) Invoke(_ context.Context, _ *lambdasvc.InvokeInput, _ ...func(*lambdasvc.Options)) (*lambdasvc.InvokeOutput, error) {
	f.l.calls = append(f.l.calls, "lambda:Invoke")
	return &lambdasvc.InvokeOutput{}, nil
}

// The mechanism of #1026: the orchestrator loads the record by sweep ID before
// it does anything, so the record (and the params it points at) must exist
// before the invoke. The handler used to make only the invoke.
func TestStartSweepWritesTheRecordTheOrchestratorLoadsBeforeInvoking(t *testing.T) {
	p, err := prepareSweep(firingSchedule(), firingParams(), "nightly-20261015-000000", "spawn-sweeps-us-east-1", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	l := &callLog{}
	err = startSweep(context.Background(), sweepStarter{
		s3: fakeS3{l}, dynamo: fakeDynamo{l}, lambda: fakeLambda{l},
		sweepTable: "spawn-sweep-orchestration", orchestratorArn: "arn:aws:lambda:us-east-1:1:function:spawn-sweep-orchestrator",
	}, p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"s3:PutObject", "dynamodb:PutItem", "lambda:Invoke"}
	if strings.Join(l.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", l.calls, want)
	}
	if l.table != "spawn-sweep-orchestration" {
		t.Errorf("record written to %q", l.table)
	}
	var written sweep.SweepRecord
	if err := attributevalue.UnmarshalMap(l.item, &written); err != nil {
		t.Fatal(err)
	}
	var ev sweep.OrchestratorEvent
	_ = json.Unmarshal(p.Payload, &ev)
	if written.SweepID != ev.SweepID {
		t.Errorf("record written for %q, orchestrator invoked for %q", written.SweepID, ev.SweepID)
	}
	if written.S3ParamsKey != l.object {
		t.Errorf("record points at %q, params staged at %q", written.S3ParamsKey, l.object)
	}
}
