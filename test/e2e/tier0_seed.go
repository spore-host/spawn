//go:build e2e_tier0

package e2e

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Tier 0 control-plane seeding. spawn's stateful commands (team, schedule,
// alerts, sweeps) assume their DynamoDB tables and S3 buckets already exist —
// real deployments provision them via CloudFormation. Substrate starts empty,
// so each test seeds exactly what its command touches before driving the binary.

// seedDynamoTable creates a pay-per-request table with a single string hash key
// and any string-keyed global secondary indexes named by gsi (index name → its
// hash-key attribute).
func seedDynamoTable(t *testing.T, db *dynamodb.Client, name, hashKey string, gsi map[string]string) {
	t.Helper()

	attrs := []ddbtypes.AttributeDefinition{
		{AttributeName: aws.String(hashKey), AttributeType: ddbtypes.ScalarAttributeTypeS},
	}
	var indexes []ddbtypes.GlobalSecondaryIndex
	for idxName, idxKey := range gsi {
		attrs = append(attrs, ddbtypes.AttributeDefinition{
			AttributeName: aws.String(idxKey), AttributeType: ddbtypes.ScalarAttributeTypeS,
		})
		indexes = append(indexes, ddbtypes.GlobalSecondaryIndex{
			IndexName: aws.String(idxName),
			KeySchema: []ddbtypes.KeySchemaElement{
				{AttributeName: aws.String(idxKey), KeyType: ddbtypes.KeyTypeHash},
			},
			Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
		})
	}

	in := &dynamodb.CreateTableInput{
		TableName:            aws.String(name),
		BillingMode:          ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: attrs,
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String(hashKey), KeyType: ddbtypes.KeyTypeHash},
		},
	}
	if len(indexes) > 0 {
		in.GlobalSecondaryIndexes = indexes
	}
	if _, err := db.CreateTable(context.Background(), in); err != nil {
		t.Fatalf("seed dynamo table %s: %v", name, err)
	}
}

// seedTeamTables provisions the two tables `spawn team` reads/writes.
func (e *spawnEnv) seedTeamTables() {
	e.t.Helper()
	db := e.DynamoClient()
	seedDynamoTable(e.t, db, "spawn-teams", "team_id", nil)
	seedDynamoTable(e.t, db, "spawn-team-memberships", "team_id",
		map[string]string{"member_arn-index": "member_arn"})
}

// seedScheduleTable provisions the table `spawn schedule` reads/writes.
func (e *spawnEnv) seedScheduleTable() {
	e.t.Helper()
	seedDynamoTable(e.t, e.DynamoClient(), "spawn-schedules", "id", nil)
}

// seedAlertTables provisions the tables `spawn alerts` reads/writes.
func (e *spawnEnv) seedAlertTables() {
	e.t.Helper()
	db := e.DynamoClient()
	seedDynamoTable(e.t, db, "spawn-alerts", "id", nil)
	seedDynamoTable(e.t, db, "spawn-alert-history", "id", nil)
}

// seedBucket creates an S3 bucket in the emulator.
func (e *spawnEnv) seedBucket(name string) {
	e.t.Helper()
	if _, err := e.S3Client().CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(name),
	}); err != nil {
		e.t.Fatalf("seed bucket %s: %v", name, err)
	}
}
