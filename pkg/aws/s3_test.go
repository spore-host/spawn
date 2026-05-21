package aws

import (
	"context"
	"testing"

	"github.com/spore-host/spawn/pkg/testutil"
)

func TestCreateS3BucketIfNotExists(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := NewClientFromConfig(env.AWSConfig)

	t.Run("create new bucket", func(t *testing.T) {
		if err := client.CreateS3BucketIfNotExists(ctx, "test-bucket-new", "us-east-1"); err != nil {
			t.Fatalf("CreateS3BucketIfNotExists: %v", err)
		}
	})

	t.Run("idempotent on existing bucket", func(t *testing.T) {
		// First call creates, second call should return nil (bucket exists).
		if err := client.CreateS3BucketIfNotExists(ctx, "test-bucket-idempotent", "us-east-1"); err != nil {
			t.Fatalf("first call: %v", err)
		}
		if err := client.CreateS3BucketIfNotExists(ctx, "test-bucket-idempotent", "us-east-1"); err != nil {
			t.Fatalf("second call (should be no-op): %v", err)
		}
	})
}

func TestCreateS3BucketWithTags(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := NewClientFromConfig(env.AWSConfig)

	tests := []struct {
		name   string
		config S3BucketConfig
	}{
		{
			name: "bucket with stack name tag",
			config: S3BucketConfig{
				BucketName:      "test-tagged-bucket-001",
				Region:          "us-east-1",
				StackName:       "my-fsx-stack",
				StorageCapacity: 1200,
			},
		},
		{
			name: "bucket with import/export paths",
			config: S3BucketConfig{
				BucketName: "test-tagged-bucket-002",
				Region:     "us-east-1",
				StackName:  "import-stack",
				ImportPath: "s3://source-bucket/data",
				ExportPath: "s3://dest-bucket/results",
			},
		},
		{
			name: "minimal bucket",
			config: S3BucketConfig{
				BucketName: "test-tagged-bucket-003",
				Region:     "us-east-1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := client.CreateS3BucketWithTags(ctx, tt.config); err != nil {
				t.Fatalf("CreateS3BucketWithTags: %v", err)
			}
		})
	}
}
