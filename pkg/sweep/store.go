package sweep

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Store provides read access to sweep orchestration records in DynamoDB. It
// mirrors the pkg/alerts.Client pattern: it wraps a *dynamodb.Client the caller
// builds (from the correct account's config) and holds the table name so tests
// can override it. Reads/unmarshals the existing SweepRecord (hash key sweep_id).
//
// Writers of this table live elsewhere (the sweep-orchestrator Lambda, a
// separate module, and this package's detached.go free functions), so the item
// format must not change.
type Store struct {
	db        *dynamodb.Client
	tableName string
}

// NewStore returns a Store using the default sweep-orchestration table.
func NewStore(db *dynamodb.Client) *Store {
	return &Store{db: db, tableName: defaultDynamoTableName}
}

// NewStoreWithTableName returns a Store using an explicit table name (tests).
func NewStoreWithTableName(db *dynamodb.Client, tableName string) *Store {
	return &Store{db: db, tableName: tableName}
}

// Get reads one sweep by ID. Returns (nil, nil) when it does not exist.
func (s *Store) Get(ctx context.Context, sweepID string) (*SweepRecord, error) {
	result, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"sweep_id": &types.AttributeValueMemberS{Value: sweepID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get sweep: %w", err)
	}
	if len(result.Item) == 0 {
		return nil, nil
	}
	var rec SweepRecord
	if err := attributevalue.UnmarshalMap(result.Item, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal sweep: %w", err)
	}
	return &rec, nil
}

// List scans the table and returns all sweep records. (There is no user GSI, so
// callers filter by user/status/region in memory, as before.)
func (s *Store) List(ctx context.Context) ([]SweepRecord, error) {
	out, err := s.db.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String(s.tableName),
	})
	if err != nil {
		return nil, fmt.Errorf("scan sweeps: %w", err)
	}
	records := make([]SweepRecord, 0, len(out.Items))
	for _, item := range out.Items {
		var rec SweepRecord
		if err := attributevalue.UnmarshalMap(item, &rec); err != nil {
			continue
		}
		records = append(records, rec)
	}
	return records, nil
}
