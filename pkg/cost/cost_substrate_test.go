package cost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

func setupCostTable(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(dynamoTableName),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("sweep_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("sweep_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		var resourceInUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &resourceInUse) {
			t.Fatalf("create %s table: %v", dynamoTableName, err)
		}
	}
}

func seedSweepRecord(t *testing.T, db *dynamodb.Client, record SweepRecord) {
	t.Helper()
	ctx := context.Background()
	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		t.Fatalf("marshal sweep record: %v", err)
	}
	_, err = db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(dynamoTableName),
		Item:      item,
	})
	if err != nil {
		t.Fatalf("put sweep record: %v", err)
	}
}

func TestGetCostBreakdown_NotFound(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupCostTable(t, env.DynamoClient())

	client := NewClient(env.DynamoClient())

	if _, err := client.GetCostBreakdown(ctx, "nonexistent-sweep"); err == nil {
		t.Error("GetCostBreakdown on missing sweep should return error")
	}
}

func TestGetCostBreakdown_Simple(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupCostTable(t, env.DynamoClient())

	client := NewClient(env.DynamoClient())

	launchedAt := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	record := SweepRecord{
		SweepID:       "sweep-cost-001",
		EstimatedCost: 1.50,
		Instances: []SweepInstance{
			{
				Index:         0,
				Region:        "us-east-1",
				InstanceID:    "i-0001",
				RequestedType: "t3.micro",
				State:         "running",
				LaunchedAt:    launchedAt,
			},
		},
	}
	seedSweepRecord(t, env.DynamoClient(), record)

	breakdown, err := client.GetCostBreakdown(ctx, "sweep-cost-001")
	if err != nil {
		t.Fatalf("GetCostBreakdown: %v", err)
	}

	if breakdown == nil {
		t.Fatal("breakdown should not be nil")
	}
	if breakdown.TotalCost <= 0 {
		t.Errorf("TotalCost = %f, want > 0", breakdown.TotalCost)
	}
	if breakdown.SweepID != "sweep-cost-001" {
		t.Errorf("SweepID = %q, want sweep-cost-001", breakdown.SweepID)
	}
}

func TestGetCostBreakdown_MultiRegion(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupCostTable(t, env.DynamoClient())

	client := NewClient(env.DynamoClient())

	launchedAt := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	record := SweepRecord{
		SweepID: "sweep-cost-002",
		Instances: []SweepInstance{
			{
				Index:         0,
				Region:        "us-east-1",
				InstanceID:    "i-0001",
				RequestedType: "c5.xlarge",
				State:         "running",
				LaunchedAt:    launchedAt,
			},
			{
				Index:         1,
				Region:        "us-west-2",
				InstanceID:    "i-0002",
				RequestedType: "c5.xlarge",
				State:         "running",
				LaunchedAt:    launchedAt,
			},
		},
	}
	seedSweepRecord(t, env.DynamoClient(), record)

	breakdown, err := client.GetCostBreakdown(ctx, "sweep-cost-002")
	if err != nil {
		t.Fatalf("GetCostBreakdown: %v", err)
	}

	if len(breakdown.ByRegion) != 2 {
		t.Errorf("ByRegion len = %d, want 2", len(breakdown.ByRegion))
	}
}
