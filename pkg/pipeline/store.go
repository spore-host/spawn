package pipeline

import (
	"context"
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
	db        *dynamodb.Client
	tableName string
}

// NewStore returns a Store backed by db, using the default orchestration table.
func NewStore(db *dynamodb.Client) *Store {
	return &Store{db: db, tableName: DefaultTableName}
}

// NewStoreWithTableName returns a Store backed by db, using an explicit table
// name (used by tests against a local table).
func NewStoreWithTableName(db *dynamodb.Client, tableName string) *Store {
	return &Store{db: db, tableName: tableName}
}

// Put writes the pipeline state, marshaling the typed PipelineState so the
// on-the-wire attribute names match what the orchestrator reads.
func (s *Store) Put(ctx context.Context, state *PipelineState) error {
	item, err := attributevalue.MarshalMap(state)
	if err != nil {
		return fmt.Errorf("marshal pipeline state: %w", err)
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      item,
	})
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
