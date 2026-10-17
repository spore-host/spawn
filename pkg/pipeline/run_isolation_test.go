package pipeline

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// mustPutInput is an unconditional write of state, for seeding a record the
// way an earlier run (or an orchestrator) left it.
func mustPutInput(t *testing.T, s *Store, state *PipelineState) *dynamodb.PutItemInput {
	t.Helper()
	item, err := attributevalue.MarshalMap(state)
	if err != nil {
		t.Fatal(err)
	}
	return &dynamodb.PutItemInput{TableName: aws.String(s.tableName), Item: item}
}

// ---- #943: one pipeline id, many runs ---------------------------------------

// Two runs under the same s3_prefix must not share a single key: a marker or an
// output one run writes must be invisible to the other. They were keyed by
// s3_prefix alone, so a second run found the first run's COMPLETE and marked
// its stages done before they had started.
func TestTwoRunsUnderOnePrefixShareNoKeys(t *testing.T) {
	a := &PipelineState{PipelineID: "ml", S3Prefix: "pipelines/ml", RunID: "20261015t100000z-aaaaaaaa"}
	b := &PipelineState{PipelineID: "ml", S3Prefix: "pipelines/ml", RunID: "20261015t110000z-bbbbbbbb"}

	keys := func(s *PipelineState) []string {
		p := s.DataPrefix()
		return []string{
			StageMarkerKey(p, "train", MarkerComplete),
			StageMarkerKey(p, "train", MarkerFailed),
			StageInstanceMarkerKey(p, "train", 0, MarkerComplete),
			StageInstanceMarkerKey(p, "train", 0, MarkerFailed),
			StageOutputPrefix(p, "train") + "model.h5",
			StageConfigKey(p, s.PipelineID),
		}
	}
	seen := map[string]bool{}
	for _, k := range keys(a) {
		seen[k] = true
		if !strings.HasPrefix(k, "pipelines/ml/runs/"+a.RunID+"/") {
			t.Errorf("run a's key %q is not under its run prefix", k)
		}
	}
	for _, k := range keys(b) {
		if seen[k] {
			t.Errorf("runs a and b share the key %q", k)
		}
	}
}

// A record from before run ids keeps the layout its instances were launched
// with, so a pipeline in flight across the upgrade still finds its markers.
func TestRecordWithoutRunIDKeepsTheOldLayout(t *testing.T) {
	s := &PipelineState{S3Prefix: "pfx"}
	if got := StageInstanceMarkerKey(s.DataPrefix(), "train", 0, MarkerComplete); got != "pfx/stages/train/0/COMPLETE" {
		t.Errorf("legacy marker key = %q", got)
	}
}

// Run ids end up in S3 keys, EC2 tags and security/placement group names.
func TestRunIDsAreUniqueAndNameSafe(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ok := regexp.MustCompile(`^20261015t120000z-[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := NewRunID(now)
		if err != nil {
			t.Fatal(err)
		}
		if !ok.MatchString(id) {
			t.Fatalf("run id %q is not <utc-time>-<8 hex>", id)
		}
		if seen[id] {
			t.Fatalf("run id %q repeated within one second", id)
		}
		seen[id] = true
	}
}

// Put must refuse to replace a run in flight, and leave it as it was. It was an
// unconditional PutItem, so re-launching a definition replaced the running
// pipeline's record with an empty INITIALIZING one.
func TestPutRefusesToReplaceARunInFlight(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	running := &PipelineState{PipelineID: "ml", RunID: "r1", Status: StatusRunning,
		Stages: []StageState{{StageID: "train", Status: StageStatusRunning}}}
	if err := s.Put(ctx, running); err != nil {
		t.Fatal(err)
	}

	for _, st := range []PipelineStatus{StatusInitializing, StatusRunning} {
		running.Status = st
		if _, err := s.db.PutItem(ctx, mustPutInput(t, s, running)); err != nil {
			t.Fatal(err)
		}
		err := s.Put(ctx, &PipelineState{PipelineID: "ml", RunID: "r2", Status: StatusInitializing, Stages: []StageState{}})
		var active *ActiveRunError
		if !errors.As(err, &active) {
			t.Fatalf("Put over a %s run: err = %v, want *ActiveRunError", st, err)
		}
		if active.Existing == nil || active.Existing.RunID != "r1" {
			t.Errorf("ActiveRunError.Existing = %+v, want run r1", active.Existing)
		}
		got, _ := s.Get(ctx, "ml")
		if got.RunID != "r1" || len(got.Stages) != 1 {
			t.Errorf("the run in flight was overwritten: %+v", got)
		}
	}
}

// A finished run may be replaced by a new one.
func TestPutReplacesAFinishedRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, st := range []PipelineStatus{StatusCompleted, StatusFailed, StatusCancelled} {
		old := &PipelineState{PipelineID: "ml-" + string(st), RunID: "r1", Status: st}
		if _, err := s.db.PutItem(ctx, mustPutInput(t, s, old)); err != nil {
			t.Fatal(err)
		}
		if err := s.Put(ctx, &PipelineState{PipelineID: old.PipelineID, RunID: "r2", Status: StatusInitializing}); err != nil {
			t.Errorf("Put over a %s run: %v", st, err)
		}
		if got, _ := s.Get(ctx, old.PipelineID); got == nil || got.RunID != "r2" {
			t.Errorf("after Put over a %s run, record = %+v, want run r2", st, got)
		}
	}
}

// Replace (launch --replace) overwrites the run it cancelled, and only that
// run: if another launch has taken the id meanwhile, it refuses.
func TestReplaceOverwritesOnlyTheRunItCancelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, &PipelineState{PipelineID: "ml", RunID: "r1", Status: StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace(ctx, &PipelineState{PipelineID: "ml", RunID: "r3", Status: StatusInitializing}, "r2"); err == nil {
		t.Error("Replace aimed at run r2 overwrote run r1")
	}
	if err := s.Replace(ctx, &PipelineState{PipelineID: "ml", RunID: "r2", Status: StatusInitializing}, "r1"); err != nil {
		t.Fatalf("Replace of the cancelled run r1: %v", err)
	}
	if got, _ := s.Get(ctx, "ml"); got.RunID != "r2" {
		t.Errorf("record is run %q after Replace, want r2", got.RunID)
	}

	// A legacy record (no run id) is replaced with prevRunID "".
	if _, err := s.db.PutItem(ctx, mustPutInput(t, s, &PipelineState{PipelineID: "old", Status: StatusRunning})); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace(ctx, &PipelineState{PipelineID: "old", RunID: "r9", Status: StatusInitializing}, ""); err != nil {
		t.Errorf("Replace of a legacy record: %v", err)
	}
}

// The cancel --replace sends is aimed at one run and must not land on another.
func TestCancelForRunOnlyCancelsThatRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, &PipelineState{PipelineID: "ml", RunID: "r2", Status: StatusRunning}); err != nil {
		t.Fatal(err)
	}
	var active *ActiveRunError
	if err := s.SetCancelRequestedForRun(ctx, "ml", "r1"); !errors.As(err, &active) {
		t.Errorf("cancel for run r1 on run r2: err = %v, want *ActiveRunError", err)
	}
	if got, _ := s.Get(ctx, "ml"); got.CancelRequested {
		t.Error("a cancel aimed at run r1 cancelled run r2")
	}
	if err := s.SetCancelRequestedForRun(ctx, "ml", "r2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "ml"); !got.CancelRequested {
		t.Error("cancel for run r2 did not set cancel_requested")
	}
}
