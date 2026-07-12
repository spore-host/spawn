package sweep

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

func setupSweepStoreTable(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	_, err := db.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName:   aws.String(defaultDynamoTableName),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("sweep_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("sweep_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		var inUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &inUse) {
			t.Fatalf("create sweep table: %v", err)
		}
	}
}

func newStoreTestClient(t *testing.T) (*Store, *dynamodb.Client) {
	t.Helper()
	env := testutil.SubstrateServer(t)
	db := env.DynamoClient()
	setupSweepStoreTable(t, db)
	return NewStoreWithTableName(db, defaultDynamoTableName), db
}

// putSweep writes a record using the same package that produces it in
// production (CreateSweepRecord marshals SweepRecord), so the store reads real
// items.
func TestSweepStore_GetAndList(t *testing.T) {
	store, db := newStoreTestClient(t)
	ctx := context.Background()

	// Seed two records directly via the store's own client using MarshalMap
	// (exercises the exact SweepRecord wire format).
	seed := func(rec SweepRecord) {
		item := marshalSweep(t, rec)
		if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(defaultDynamoTableName),
			Item:      item,
		}); err != nil {
			t.Fatalf("seed put: %v", err)
		}
	}
	seed(SweepRecord{SweepID: "s1", SweepName: "one", UserID: "u1", Status: "RUNNING", AWSAccountID: "111122223333", Region: "us-east-1"})
	seed(SweepRecord{SweepID: "s2", SweepName: "two", UserID: "u2", Status: "COMPLETED"})

	// Get present + missing.
	got, err := store.Get(ctx, "s1")
	if err != nil || got == nil {
		t.Fatalf("Get(s1): %v (%v)", got, err)
	}
	if got.SweepName != "one" || got.AWSAccountID != "111122223333" || got.Region != "us-east-1" {
		t.Errorf("Get(s1) round-trip mismatch: %+v", got)
	}
	if missing, _ := store.Get(ctx, "nope"); missing != nil {
		t.Errorf("expected nil for missing sweep, got %+v", missing)
	}

	// List returns both.
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List returned %d, want 2", len(all))
	}
}

func marshalSweep(t *testing.T, rec SweepRecord) map[string]dynamodbtypes.AttributeValue {
	t.Helper()
	item, err := attributevalue.MarshalMap(rec)
	if err != nil {
		t.Fatalf("marshal sweep: %v", err)
	}
	return item
}
