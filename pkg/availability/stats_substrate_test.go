package availability

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spore-host/spawn/pkg/testutil"
)

func setupAvailabilityTable(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(availabilityTableName),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("stat_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("stat_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		var resourceInUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &resourceInUse) {
			t.Fatalf("create %s table: %v", availabilityTableName, err)
		}
	}
}

func TestGetStats_NotFound(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	setupAvailabilityTable(t, db)

	stats, err := GetStats(ctx, db, "us-east-1", "t3.micro")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}

	// Should return zero-value stats, not an error
	if stats.SuccessCount != 0 {
		t.Errorf("SuccessCount = %d, want 0", stats.SuccessCount)
	}
	if stats.ConsecutiveFails != 0 {
		t.Errorf("ConsecutiveFails = %d, want 0", stats.ConsecutiveFails)
	}
	if stats.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", stats.Region)
	}
}

func TestRecordSuccess(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	setupAvailabilityTable(t, db)

	if err := RecordSuccess(ctx, db, "us-east-1", "c5.xlarge"); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}

	stats, err := GetStats(ctx, db, "us-east-1", "c5.xlarge")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}

	if stats.SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", stats.SuccessCount)
	}
	if stats.ConsecutiveFails != 0 {
		t.Errorf("ConsecutiveFails = %d, want 0", stats.ConsecutiveFails)
	}
}

func TestRecordFailure(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	setupAvailabilityTable(t, db)

	if err := RecordFailure(ctx, db, "us-west-2", "p3.2xlarge", true, "InsufficientInstanceCapacity"); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	stats, err := GetStats(ctx, db, "us-west-2", "p3.2xlarge")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}

	if stats.ConsecutiveFails != 1 {
		t.Errorf("ConsecutiveFails = %d, want 1", stats.ConsecutiveFails)
	}
	if stats.LastErrorCode != "InsufficientInstanceCapacity" {
		t.Errorf("LastErrorCode = %q, want InsufficientInstanceCapacity", stats.LastErrorCode)
	}
}

func TestIsInBackoff_After3Failures(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	setupAvailabilityTable(t, db)

	region := "eu-west-1"
	instanceType := "g4dn.xlarge"

	for i := 0; i < 3; i++ {
		if err := RecordFailure(ctx, db, region, instanceType, true, "InsufficientInstanceCapacity"); err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
	}

	inBackoff, err := IsInBackoff(ctx, db, region, instanceType)
	if err != nil {
		t.Fatalf("IsInBackoff: %v", err)
	}

	if !inBackoff {
		t.Error("expected to be in backoff after 3 consecutive failures")
	}
}

func TestListStatsByRegions(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	db := env.DynamoClient()
	setupAvailabilityTable(t, db)

	instanceType := "m5.large"
	regions := []string{"us-east-1", "us-west-2"}

	for _, region := range regions {
		if err := RecordSuccess(ctx, db, region, instanceType); err != nil {
			t.Fatalf("RecordSuccess %s: %v", region, err)
		}
	}

	result, err := ListStatsByRegions(ctx, db, regions, instanceType)
	if err != nil {
		t.Fatalf("ListStatsByRegions: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("got %d entries, want 2", len(result))
	}
	for _, region := range regions {
		stats, ok := result[region]
		if !ok {
			t.Errorf("missing stats for region %s", region)
			continue
		}
		if stats.SuccessCount != 1 {
			t.Errorf("region %s: SuccessCount = %d, want 1", region, stats.SuccessCount)
		}
	}
}
