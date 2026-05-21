package sweep

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

func setupSweepTable(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
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
		var resourceInUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &resourceInUse) {
			t.Fatalf("create %s table: %v", defaultDynamoTableName, err)
		}
	}
}

func TestCreateAndQuerySweepRecord(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupSweepTable(t, env.DynamoClient())

	record := &SweepRecord{
		SweepID:       "sweep-sub-001",
		SweepName:     "test sweep",
		MaxConcurrent: 5,
		TotalParams:   10,
		Region:        "us-east-1",
		Status:        "PENDING",
	}

	if err := CreateSweepRecord(ctx, env.AWSConfig, record); err != nil {
		t.Fatalf("CreateSweepRecord: %v", err)
	}

	got, err := QuerySweepStatus(ctx, env.AWSConfig, "sweep-sub-001")
	if err != nil {
		t.Fatalf("QuerySweepStatus: %v", err)
	}

	if got.SweepName != record.SweepName {
		t.Errorf("SweepName = %q, want %q", got.SweepName, record.SweepName)
	}
	if got.MaxConcurrent != record.MaxConcurrent {
		t.Errorf("MaxConcurrent = %d, want %d", got.MaxConcurrent, record.MaxConcurrent)
	}
	if got.UserID == "" {
		t.Error("UserID should be set by CreateSweepRecord via STS")
	}
}

func TestSaveSweepState(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupSweepTable(t, env.DynamoClient())

	record := &SweepRecord{
		SweepID:   "sweep-sub-002",
		SweepName: "save state sweep",
		Status:    "RUNNING",
		Launched:  3,
	}

	if err := SaveSweepState(ctx, env.AWSConfig, record); err != nil {
		t.Fatalf("SaveSweepState: %v", err)
	}

	got, err := LoadSweepStateFromDynamoDB(ctx, env.AWSConfig, "sweep-sub-002")
	if err != nil {
		t.Fatalf("LoadSweepStateFromDynamoDB: %v", err)
	}

	if got.Status != record.Status {
		t.Errorf("Status = %q, want %q", got.Status, record.Status)
	}
	if got.Launched != record.Launched {
		t.Errorf("Launched = %d, want %d", got.Launched, record.Launched)
	}
}

func TestUploadParamsToS3(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	bucket := getS3BucketName(nil, "us-east-1")
	_, err := env.S3Client().CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}

	params := &ParamFileFormat{
		Defaults: map[string]interface{}{
			"region":        "us-east-1",
			"instance_type": "t3.micro",
		},
		Params: []map[string]interface{}{
			{"name": "job1"},
			{"name": "job2"},
		},
	}

	sweepID := "sweep-sub-003"
	s3URI, err := UploadParamsToS3(ctx, env.AWSConfig, params, sweepID, "us-east-1")
	if err != nil {
		t.Fatalf("UploadParamsToS3: %v", err)
	}

	if s3URI == "" {
		t.Error("S3 URI should not be empty")
	}

	expectedPrefix := "s3://" + bucket + "/"
	if len(s3URI) < len(expectedPrefix) || s3URI[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("S3 URI %q does not start with expected prefix %q", s3URI, expectedPrefix)
	}
}
