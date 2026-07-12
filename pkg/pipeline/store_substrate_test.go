package pipeline

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

const testPipelineTable = "spawn-pipeline-orchestration"

func setupPipelineTable(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(testPipelineTable),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("pipeline_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("pipeline_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		var inUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &inUse) {
			t.Fatalf("create pipeline table: %v", err)
		}
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	env := testutil.SubstrateServer(t)
	db := env.DynamoClient()
	setupPipelineTable(t, db)
	return NewStoreWithTableName(db, testPipelineTable)
}

func TestPipelineStore_PutGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	max := 12.50
	want := &PipelineState{
		PipelineID:     "pl-abc",
		PipelineName:   "demo",
		UserID:         "123456789012",
		CreatedAt:      now,
		UpdatedAt:      now,
		Status:         StatusInitializing,
		S3ConfigKey:    "s3://b/k",
		S3Bucket:       "b",
		S3Prefix:       "p",
		ResultS3Bucket: "rb",
		ResultS3Prefix: "rp",
		OnFailure:      "stop",
		TotalStages:    3,
		Stages:         []StageState{},
		MaxCostUSD:     &max,
	}
	if err := s.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(ctx, "pl-abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil for an existing pipeline")
	}
	if got.PipelineName != "demo" || got.Status != StatusInitializing || got.TotalStages != 3 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.MaxCostUSD == nil || *got.MaxCostUSD != max {
		t.Errorf("MaxCostUSD lost: %v", got.MaxCostUSD)
	}

	// Missing pipeline → (nil, nil).
	missing, err := s.Get(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("Get(missing): %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil for missing pipeline, got %+v", missing)
	}
}

func TestPipelineStore_ListByUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, p := range []*PipelineState{
		{PipelineID: "a", UserID: "u1", Status: StatusRunning},
		{PipelineID: "b", UserID: "u2", Status: StatusRunning},
		{PipelineID: "c", UserID: "u1", Status: StatusCompleted},
	} {
		if err := s.Put(ctx, p); err != nil {
			t.Fatalf("Put %s: %v", p.PipelineID, err)
		}
	}

	got, err := s.ListByUser(ctx, "u1")
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 pipelines for u1, got %d", len(got))
	}
	for _, p := range got {
		if p.UserID != "u1" {
			t.Errorf("ListByUser returned a pipeline for %s", p.UserID)
		}
	}
}

func TestPipelineStore_SetCancelRequested(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, &PipelineState{PipelineID: "pl-x", Status: StatusRunning}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.SetCancelRequested(ctx, "pl-x"); err != nil {
		t.Fatalf("SetCancelRequested: %v", err)
	}
	got, err := s.Get(ctx, "pl-x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.CancelRequested {
		t.Error("cancel_requested was not set")
	}
}

// TestPipelineStore_WireFormatMatchesLegacyMap guards the refactor's core
// invariant (#326): marshaling the typed PipelineState must produce the SAME
// DynamoDB attribute set that cmd/pipeline.go's old map[string]interface{} write
// produced, because the pipeline-orchestrator Lambda and other readers share the
// wire format. If a struct field/tag ever drifts from the historical map keys,
// this test fails.
func TestPipelineStore_WireFormatMatchesLegacyMap(t *testing.T) {
	now := time.Now().UTC()
	max := 5.0

	// The exact map cmd/pipeline.go used to marshal on launch (with MaxCostUSD set).
	legacy := map[string]interface{}{
		"pipeline_id":      "pl-1",
		"pipeline_name":    "n",
		"user_id":          "u",
		"created_at":       now,
		"updated_at":       now,
		"status":           "INITIALIZING",
		"cancel_requested": false,
		"s3_config_key":    "s3://b/k",
		"s3_bucket":        "b",
		"s3_prefix":        "p",
		"result_s3_bucket": "rb",
		"result_s3_prefix": "rp",
		"on_failure":       "stop",
		"current_cost_usd": 0.0,
		"total_stages":     2,
		"completed_stages": 0,
		"failed_stages":    0,
		"stages":           []interface{}{},
		"max_cost_usd":     max,
	}
	legacyItem, err := attributevalue.MarshalMap(legacy)
	if err != nil {
		t.Fatalf("marshal legacy map: %v", err)
	}

	// The equivalent typed value the store now marshals.
	state := &PipelineState{
		PipelineID: "pl-1", PipelineName: "n", UserID: "u",
		CreatedAt: now, UpdatedAt: now,
		Status: StatusInitializing, CancelRequested: false,
		S3ConfigKey: "s3://b/k", S3Bucket: "b", S3Prefix: "p",
		ResultS3Bucket: "rb", ResultS3Prefix: "rp", OnFailure: "stop",
		CurrentCostUSD: 0.0, TotalStages: 2, CompletedStages: 0, FailedStages: 0,
		Stages: []StageState{}, MaxCostUSD: &max,
	}
	structItem, err := attributevalue.MarshalMap(state)
	if err != nil {
		t.Fatalf("marshal struct: %v", err)
	}

	// The attribute key SETS must be identical.
	legacyKeys := attrKeys(legacyItem)
	structKeys := attrKeys(structItem)
	if !reflect.DeepEqual(legacyKeys, structKeys) {
		t.Errorf("wire attribute keys differ.\n legacy: %v\n struct: %v", legacyKeys, structKeys)
	}
}

func attrKeys(m map[string]dynamodbtypes.AttributeValue) map[string]bool {
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}
