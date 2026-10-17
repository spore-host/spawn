package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DefaultTableName is the DynamoDB table that holds pipeline orchestration
// records. The lambda/pipeline-orchestrator (same module) uses the same table
// and reads/writes the same PipelineState item, so the attribute names must not
// change.
const DefaultTableName = "spawn-pipeline-orchestration"

// Store provides DynamoDB persistence for PipelineState. It mirrors the
// pkg/alerts.Client pattern: it wraps a *dynamodb.Client the caller builds (from
// the correct account's config) and holds the table name so tests can override
// it. The item schema is PipelineState (hash key pipeline_id).
type Store struct {
	db        storeAPI
	tableName string
}

// storeAPI is the DynamoDB surface the store uses.
type storeAPI interface {
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// ActiveRunError is returned by Put and Replace when the pipeline id already
// has a record that is not in a terminal state, so writing would replace a
// run that is still in flight. Existing is that record, when it could be read.
type ActiveRunError struct {
	PipelineID string
	Existing   *PipelineState
}

func (e *ActiveRunError) Error() string {
	if e.Existing == nil {
		return fmt.Sprintf("pipeline %s already has a run in flight", e.PipelineID)
	}
	return fmt.Sprintf("pipeline %s already has a run in flight (run %q, status %s)",
		e.PipelineID, e.Existing.RunID, e.Existing.Status)
}

// terminalStatusValues are the expression values for "the record's status is
// terminal": a run in one of these may be replaced by a new one.
func terminalStatusValues() map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		":completed": &types.AttributeValueMemberS{Value: string(StatusCompleted)},
		":failed":    &types.AttributeValueMemberS{Value: string(StatusFailed)},
		":cancelled": &types.AttributeValueMemberS{Value: string(StatusCancelled)},
	}
}

const terminalStatusCondition = "#status IN (:completed, :failed, :cancelled)"

// NewStore returns a Store backed by db, using the default orchestration table.
func NewStore(db *dynamodb.Client) *Store {
	return &Store{db: db, tableName: DefaultTableName}
}

// NewStoreWithTableName returns a Store backed by db, using an explicit table
// name (used by tests against a local table).
func NewStoreWithTableName(db *dynamodb.Client, tableName string) *Store {
	return &Store{db: db, tableName: tableName}
}

// Put creates the record for a new run of the pipeline. It is CONDITIONAL:
// it writes only if the pipeline id has no record, or its record is in a
// terminal state. Otherwise it returns *ActiveRunError and writes nothing.
//
// It was an unconditional PutItem, so launching a definition whose run was
// still in flight replaced that run's record with a fresh INITIALIZING one with
// no stages. The running orchestrator chain then reloaded a record that no
// longer listed its instances, and two chains ran against one record (#943).
// The condition is evaluated by DynamoDB, so two concurrent launches cannot
// both pass it.
func (s *Store) Put(ctx context.Context, state *PipelineState) error {
	return s.put(ctx, state, "attribute_not_exists(pipeline_id) OR "+terminalStatusCondition, nil)
}

// Replace writes the record for a new run over the run prevRunID, which the
// caller has already asked to cancel (`spawn pipeline launch --force`). It
// writes if the record is absent or terminal, as Put does, or if it still
// belongs to prevRunID; if a different run has taken the id in the meantime it
// returns *ActiveRunError rather than replacing that one too. An orchestrator
// chain still running prevRunID notices on its next poll that the record has
// moved to another run, terminates its own instances and stops.
func (s *Store) Replace(ctx context.Context, state *PipelineState, prevRunID string) error {
	cond := "attribute_not_exists(pipeline_id) OR " + terminalStatusCondition
	vals := map[string]types.AttributeValue{}
	if prevRunID == "" {
		cond += " OR attribute_not_exists(run_id)"
	} else {
		cond += " OR run_id = :prev"
		vals[":prev"] = &types.AttributeValueMemberS{Value: prevRunID}
	}
	return s.put(ctx, state, cond, vals)
}

func (s *Store) put(ctx context.Context, state *PipelineState, cond string, extra map[string]types.AttributeValue) error {
	item, err := attributevalue.MarshalMap(state)
	if err != nil {
		return fmt.Errorf("marshal pipeline state: %w", err)
	}
	vals := terminalStatusValues()
	for k, v := range extra {
		vals[k] = v
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                 aws.String(s.tableName),
		Item:                      item,
		ConditionExpression:       aws.String(cond),
		ExpressionAttributeNames:  map[string]string{"#status": "status"},
		ExpressionAttributeValues: vals,
	})
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		existing, gerr := s.Get(ctx, state.PipelineID)
		if gerr != nil {
			existing = nil
		}
		return &ActiveRunError{PipelineID: state.PipelineID, Existing: existing}
	}
	if err != nil {
		return fmt.Errorf("put pipeline: %w", err)
	}
	return nil
}

// Get reads one pipeline by ID. Returns (nil, nil) when the pipeline does not
// exist so callers can distinguish "not found" from an error.
func (s *Store) Get(ctx context.Context, pipelineID string) (*PipelineState, error) {
	result, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"pipeline_id": &types.AttributeValueMemberS{Value: pipelineID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get pipeline: %w", err)
	}
	if result.Item == nil {
		return nil, nil
	}
	var state PipelineState
	if err := attributevalue.UnmarshalMap(result.Item, &state); err != nil {
		return nil, fmt.Errorf("unmarshal pipeline state: %w", err)
	}
	return &state, nil
}

// ListByUser scans the table and returns the pipelines owned by userID. (The
// table has no user GSI; the caller previously scanned + filtered in memory, so
// this preserves that behavior.)
func (s *Store) ListByUser(ctx context.Context, userID string) ([]PipelineState, error) {
	out, err := s.db.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String(s.tableName),
	})
	if err != nil {
		return nil, fmt.Errorf("scan pipelines: %w", err)
	}
	var pipelines []PipelineState
	for _, item := range out.Items {
		var p PipelineState
		if err := attributevalue.UnmarshalMap(item, &p); err != nil {
			continue
		}
		if p.UserID != userID {
			continue
		}
		pipelines = append(pipelines, p)
	}
	return pipelines, nil
}

// SetCancelRequestedForRun is SetCancelRequested, only if the record still
// belongs to runID ("" for a record written before run ids), so a cancel aimed
// at one run never lands on the run that replaced it. It returns
// *ActiveRunError if the record has moved to another run.
func (s *Store) SetCancelRequestedForRun(ctx context.Context, pipelineID, runID string) error {
	cond := "attribute_not_exists(run_id)"
	vals := map[string]types.AttributeValue{
		":true": &types.AttributeValueMemberBOOL{Value: true},
		":now":  &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
	}
	if runID != "" {
		cond = "run_id = :run"
		vals[":run"] = &types.AttributeValueMemberS{Value: runID}
	}
	_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"pipeline_id": &types.AttributeValueMemberS{Value: pipelineID},
		},
		UpdateExpression:          aws.String("SET cancel_requested = :true, updated_at = :now"),
		ConditionExpression:       aws.String("attribute_exists(pipeline_id) AND " + cond),
		ExpressionAttributeValues: vals,
	})
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		existing, _ := s.Get(ctx, pipelineID)
		return &ActiveRunError{PipelineID: pipelineID, Existing: existing}
	}
	if err != nil {
		return fmt.Errorf("set cancel_requested: %w", err)
	}
	return nil
}

// SetCancelRequested flags a pipeline for cancellation; the orchestrator Lambda
// observes the flag and tears the pipeline down.
func (s *Store) SetCancelRequested(ctx context.Context, pipelineID string) error {
	_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"pipeline_id": &types.AttributeValueMemberS{Value: pipelineID},
		},
		UpdateExpression: aws.String("SET cancel_requested = :true, updated_at = :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":true": &types.AttributeValueMemberBOOL{Value: true},
			":now":  &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("set cancel_requested: %w", err)
	}
	return nil
}
