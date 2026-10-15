package main

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/pipeline"
	"github.com/spore-host/spawn/pkg/testutil"
)

// useSubstrateTable points the orchestrator's DynamoDB client at an emulator
// table for the test.
func useSubstrateTable(t *testing.T) *pipeline.Store {
	t.Helper()
	env := testutil.SubstrateServer(t)
	db := env.DynamoClient()
	const table = "pipeline-orchestration-test"
	if _, err := db.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName:   aws.String(table),
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
	oldDB, oldTable := dynamodbClient, tableName
	t.Cleanup(func() { dynamodbClient, tableName = oldDB, oldTable })
	dynamodbClient, tableName = db, table
	return pipeline.NewStoreWithTableName(db, table)
}

// #943: the orchestrator writes only over its own run's record. Its save was an
// unconditional PutItem, so a chain whose run had been replaced overwrote the
// new run's record with its own stale state on every save.
func TestSaveDoesNotOverwriteAnotherRunsRecord(t *testing.T) {
	store := useSubstrateTable(t)
	ctx := context.Background()
	if err := store.Put(ctx, &pipeline.PipelineState{PipelineID: "pl", RunID: "r2", Status: pipeline.StatusInitializing}); err != nil {
		t.Fatal(err)
	}

	stale := &pipeline.PipelineState{PipelineID: "pl", RunID: "r1", Status: pipeline.StatusRunning}
	if err := savePipelineState(ctx, stale); !errors.Is(err, errSuperseded) {
		t.Errorf("save by run r1 over run r2's record: err = %v, want errSuperseded", err)
	}
	legacy := &pipeline.PipelineState{PipelineID: "pl", Status: pipeline.StatusRunning}
	if err := savePipelineState(ctx, legacy); !errors.Is(err, errSuperseded) {
		t.Errorf("save by a pre-run-id chain over run r2's record: err = %v, want errSuperseded", err)
	}
	if got, _ := store.Get(ctx, "pl"); got.RunID != "r2" || got.Status != pipeline.StatusInitializing {
		t.Errorf("run r2's record was overwritten: %+v", got)
	}

	// Its own run saves normally.
	own := &pipeline.PipelineState{PipelineID: "pl", RunID: "r2", Status: pipeline.StatusRunning}
	if err := savePipelineState(ctx, own); err != nil {
		t.Fatalf("save by run r2: %v", err)
	}
	if got, _ := store.Get(ctx, "pl"); got.Status != pipeline.StatusRunning {
		t.Errorf("run r2's own save did not land: %+v", got)
	}
}

// A record from before run ids is still saved by its own (run-id-less) chain.
func TestSaveOfALegacyRecord(t *testing.T) {
	useSubstrateTable(t)
	ctx := context.Background()
	if err := savePipelineState(ctx, &pipeline.PipelineState{PipelineID: "old", Status: pipeline.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := savePipelineState(ctx, &pipeline.PipelineState{PipelineID: "old", Status: pipeline.StatusCompleted}); err != nil {
		t.Errorf("legacy chain's second save: %v", err)
	}
}

// An invocation for a run the record no longer holds (a re-invocation of a run
// replaced by --replace) exits before doing anything. The new run has its own
// chain; this one carrying on would be a second chain on the new run's record.
func TestInvocationForAReplacedRunExits(t *testing.T) {
	store := useSubstrateTable(t)
	ctx := context.Background()
	oldS3 := s3Client
	t.Cleanup(func() { s3Client = oldS3 })
	s3Client = testutil.SubstrateServer(t).S3Client() // empty: any definition download fails

	if err := store.Put(ctx, &pipeline.PipelineState{PipelineID: "pl", RunID: "r2", Status: pipeline.StatusInitializing,
		S3ConfigKey: "s3://spawn-pipelines-us-east-1/pipelines/pl/runs/r2/config.json"}); err != nil {
		t.Fatal(err)
	}
	if err := handler(ctx, PipelineEvent{PipelineID: "pl", RunID: "r1"}); err != nil {
		t.Errorf("invocation for replaced run r1: %v, want a quiet exit", err)
	}
	if got, _ := store.Get(ctx, "pl"); got.Status != pipeline.StatusInitializing || got.RunID != "r2" {
		t.Errorf("the replaced run's invocation touched run r2's record: %+v", got)
	}
	// Its own run's invocation proceeds (and here fails, on the missing definition).
	if err := handler(ctx, PipelineEvent{PipelineID: "pl", RunID: "r2"}); err == nil {
		t.Error("the run's own invocation did not proceed to load its definition")
	}
}
