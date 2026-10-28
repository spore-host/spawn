package main

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spore-host/spawn/pkg/scheduler"
)

type fakeCompleter struct{ calls []string }

func (f *fakeCompleter) CompleteSchedule(_ context.Context, r *scheduler.ScheduleRecord, _ time.Time) error {
	f.calls = append(f.calls, r.ScheduleID)
	return nil
}

// #1382: the launch that uses up a schedule is its last, so the handler
// completes it then (deleting its EventBridge schedule and params) rather than
// leaving it to fire into "max executions reached" forever.
func TestLastRunCompletesTheSchedule(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		rec  ScheduleRecord
		want bool
	}{
		{"one-time", ScheduleRecord{ScheduleType: "one-time", Status: "active", ExecutionCount: 1}, true},
		{"max reached", ScheduleRecord{ScheduleType: "recurring", Status: "active", MaxExecutions: 3, ExecutionCount: 3}, true},
		{"max not reached", ScheduleRecord{ScheduleType: "recurring", Status: "active", MaxExecutions: 3, ExecutionCount: 2}, false},
		{"unbounded", ScheduleRecord{ScheduleType: "recurring", Status: "active", ExecutionCount: 500}, false},
		{"end_after still ahead", ScheduleRecord{ScheduleType: "recurring", Status: "active", EndAfter: now.Add(time.Hour)}, false},
	}
	for _, tc := range cases {
		if got := lastRun(&tc.rec, now); got != tc.want {
			t.Errorf("%s: lastRun = %v, want %v", tc.name, got, tc.want)
		}
	}
	f := &fakeCompleter{}
	rec := &ScheduleRecord{ScheduleID: "sched-x"}
	if err := completeRunOut(context.Background(), f, rec, now); err != nil || len(f.calls) != 1 {
		t.Errorf("completeRunOut: err=%v calls=%v", err, f.calls)
	}
}

// #1383: the params bucket is in the schedule's Region, not this function's;
// a client in the wrong region gets a 301.
func TestParamsAreReadInTheBucketRegion(t *testing.T) {
	cfg := aws.Config{Region: "us-east-1"}
	if got := paramsS3Client(cfg, "us-west-2").Options().Region; got != "us-west-2" {
		t.Errorf("params client region = %s, want us-west-2", got)
	}
}
