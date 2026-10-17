package cmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/testutil"
)

// orchestratedStore is the real store on the emulator, plus a stand-in for the
// old run's orchestrator: once the old run is asked to cancel, the next read
// finds it CANCELLED (unless finishes is false, an orchestrator that is gone).
type orchestratedStore struct {
	*pipeline.Store
	t        *testing.T
	finishes bool
	cancels  []string
}

func (o *orchestratedStore) SetCancelRequestedForRun(ctx context.Context, id, runID string) error {
	o.cancels = append(o.cancels, runID)
	return o.Store.SetCancelRequestedForRun(ctx, id, runID)
}

func (o *orchestratedStore) Get(ctx context.Context, id string) (*pipeline.PipelineState, error) {
	st, err := o.Store.Get(ctx, id)
	if err == nil && st != nil && o.finishes && st.CancelRequested && !st.Status.IsTerminal() {
		st.Status = pipeline.StatusCancelled
		if err := o.Store.Replace(ctx, st, st.RunID); err != nil {
			o.t.Fatal(err)
		}
	}
	return st, err
}

func newPipelineTestStore(t *testing.T) *pipeline.Store {
	t.Helper()
	db := testutil.SubstrateServer(t).DynamoClient()
	if _, err := db.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName:   aws.String("pipelines"),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("pipeline_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("pipeline_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return pipeline.NewStoreWithTableName(db, "pipelines")
}

func fastReplace(t *testing.T, wait time.Duration) {
	t.Helper()
	oldWait, oldPoll := replaceCancelWait, replacePollEvery
	t.Cleanup(func() { replaceCancelWait, replacePollEvery = oldWait, oldPoll })
	replaceCancelWait, replacePollEvery = wait, time.Millisecond
}

func seedRunning(t *testing.T, s *pipeline.Store) {
	t.Helper()
	if err := s.Put(context.Background(), &pipeline.PipelineState{PipelineID: "ml", RunID: "r1", Status: pipeline.StatusRunning,
		Stages: []pipeline.StageState{{StageID: "train", Status: pipeline.StageStatusRunning}}}); err != nil {
		t.Fatal(err)
	}
}

// #943: launching a pipeline id whose run is in flight is refused, and the
// running run's record is left alone. It used to be replaced by an empty
// INITIALIZING record, orphaning the running run's instances.
func TestLaunchOverARunInFlightIsRefused(t *testing.T) {
	s := newPipelineTestStore(t)
	seedRunning(t, s)
	o := &orchestratedStore{Store: s, t: t, finishes: true}

	err := createPipelineRecord(context.Background(), o, &pipeline.PipelineState{PipelineID: "ml", RunID: "r2", Status: pipeline.StatusInitializing}, false, io.Discard)
	var active *pipeline.ActiveRunError
	if !errors.As(err, &active) {
		t.Fatalf("err = %v, want *pipeline.ActiveRunError", err)
	}
	if msg := activeRunRefusal(active).Error(); !strings.Contains(msg, "--replace") || !strings.Contains(msg, "spawn pipeline cancel ml") {
		t.Errorf("refusal does not say how to proceed: %s", msg)
	}
	if len(o.cancels) != 0 {
		t.Error("a plain launch cancelled the run in flight")
	}
	got, _ := s.Get(context.Background(), "ml")
	if got.RunID != "r1" || len(got.Stages) != 1 || got.CancelRequested {
		t.Errorf("the run in flight was changed: %+v", got)
	}
}

// --replace cancels the run in flight, lets it finish, and only then writes the
// new run's record.
func TestLaunchReplaceCancelsTheOldRunFirst(t *testing.T) {
	fastReplace(t, time.Minute)
	s := newPipelineTestStore(t)
	seedRunning(t, s)
	o := &orchestratedStore{Store: s, t: t, finishes: true}

	if err := createPipelineRecord(context.Background(), o, &pipeline.PipelineState{PipelineID: "ml", RunID: "r2", Status: pipeline.StatusInitializing}, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(o.cancels) != 1 || o.cancels[0] != "r1" {
		t.Errorf("cancels sent = %v, want one, for run r1", o.cancels)
	}
	got, _ := s.Get(context.Background(), "ml")
	if got.RunID != "r2" || got.Status != pipeline.StatusInitializing || got.CancelRequested {
		t.Errorf("record after --replace = %+v, want the new run r2", got)
	}
}

// If the old run's orchestrator never finishes it, --replace still replaces it
// after the wait (the old chain stops when it sees the record moved).
func TestLaunchReplaceProceedsAfterTheWait(t *testing.T) {
	fastReplace(t, 20*time.Millisecond)
	s := newPipelineTestStore(t)
	seedRunning(t, s)
	o := &orchestratedStore{Store: s, t: t, finishes: false}

	if err := createPipelineRecord(context.Background(), o, &pipeline.PipelineState{PipelineID: "ml", RunID: "r2", Status: pipeline.StatusInitializing}, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(context.Background(), "ml"); got.RunID != "r2" {
		t.Errorf("record is run %q, want r2", got.RunID)
	}
}
