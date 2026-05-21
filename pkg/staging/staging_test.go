package staging

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

func setupStagingBucket(t *testing.T, s3Client *s3.Client, bucket string) {
	t.Helper()
	ctx := context.Background()
	_, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
}

func setupStagedDataTable(t *testing.T, db *dynamodb.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String("spawn-staged-data"),
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{AttributeName: aws.String("staging_id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{AttributeName: aws.String("staging_id"), KeyType: dynamodbtypes.KeyTypeHash},
		},
	})
	if err != nil {
		var resourceInUse *dynamodbtypes.ResourceInUseException
		if !errors.As(err, &resourceInUse) {
			t.Fatalf("create spawn-staged-data table: %v", err)
		}
	}
}

func TestUploadToPrimaryRegion(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupStagingBucket(t, env.S3Client(), "spawn-data-us-east-1")

	client := NewClient(env.AWSConfig, "123456789012")

	// Write a temp file to upload.
	dir := t.TempDir()
	fpath := filepath.Join(dir, "testdata.bin")
	if err := os.WriteFile(fpath, []byte("hello staging world"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	stagingID := "stage-test-001"
	s3Key, size, sha256sum, err := client.UploadToPrimaryRegion(ctx, fpath, stagingID, "us-east-1")
	if err != nil {
		t.Fatalf("UploadToPrimaryRegion: %v", err)
	}

	if s3Key == "" {
		t.Error("S3 key should not be empty")
	}
	if size == 0 {
		t.Error("size should not be zero")
	}
	if sha256sum == "" {
		t.Error("sha256 should not be empty")
	}
}

func TestRecordAndGetMetadata(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupStagedDataTable(t, env.DynamoClient())

	client := NewClient(env.AWSConfig, "123456789012")

	meta := StagingMetadata{
		StagingID:   "stage-meta-001",
		LocalPath:   "/tmp/data.bin",
		S3Key:       "staging/stage-meta-001/data.bin",
		Regions:     []string{"us-east-1", "us-west-2"},
		SweepID:     "sweep-001",
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
		SizeBytes:   1024,
		SHA256:      "abc123",
		Destination: "/remote/path",
	}

	if err := client.RecordMetadata(ctx, meta); err != nil {
		t.Fatalf("RecordMetadata: %v", err)
	}

	got, err := client.GetMetadata(ctx, "stage-meta-001")
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}

	if got.StagingID != meta.StagingID {
		t.Errorf("StagingID = %q, want %q", got.StagingID, meta.StagingID)
	}
	if got.S3Key != meta.S3Key {
		t.Errorf("S3Key = %q, want %q", got.S3Key, meta.S3Key)
	}
	if got.SizeBytes != meta.SizeBytes {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, meta.SizeBytes)
	}
	if len(got.Regions) != 2 {
		t.Errorf("Regions len = %d, want 2", len(got.Regions))
	}
}

func TestGetMetadata_NotFound(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupStagedDataTable(t, env.DynamoClient())

	client := NewClient(env.AWSConfig, "123456789012")

	if _, err := client.GetMetadata(ctx, "stage-missing"); err == nil {
		t.Error("GetMetadata on missing ID should return error")
	}
}

func TestListStagedData(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupStagedDataTable(t, env.DynamoClient())

	client := NewClient(env.AWSConfig, "123456789012")

	for i, id := range []string{"stage-list-001", "stage-list-002", "stage-list-003"} {
		_ = i
		meta := StagingMetadata{
			StagingID: id,
			S3Key:     "staging/" + id + "/data.bin",
			SHA256:    "abc",
			CreatedAt: time.Now(),
		}
		if err := client.RecordMetadata(ctx, meta); err != nil {
			t.Fatalf("RecordMetadata %s: %v", id, err)
		}
	}

	got, err := client.ListStagedData(ctx)
	if err != nil {
		t.Fatalf("ListStagedData: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d items, want 3", len(got))
	}
}

func TestDeleteStaging(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupStagedDataTable(t, env.DynamoClient())

	client := NewClient(env.AWSConfig, "123456789012")

	meta := StagingMetadata{
		StagingID: "stage-del-001",
		S3Key:     "staging/stage-del-001/data.bin",
		SHA256:    "abc",
		CreatedAt: time.Now(),
	}
	if err := client.RecordMetadata(ctx, meta); err != nil {
		t.Fatalf("RecordMetadata: %v", err)
	}

	if err := client.DeleteStaging(ctx, "stage-del-001"); err != nil {
		t.Fatalf("DeleteStaging: %v", err)
	}

	if _, err := client.GetMetadata(ctx, "stage-del-001"); err == nil {
		t.Error("GetMetadata after delete should return error")
	}
}
