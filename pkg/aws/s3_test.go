package aws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithyhttp "github.com/aws/smithy-go/transport/http"
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

// TestIsCrossRegionRedirect covers the two shapes a cross-region HeadBucket
// 301 takes (a coded smithy.APIError vs a bare HTTP-301 ResponseError), plus
// the negatives, so a bucket that merely lives in another region is treated as
// "exists, don't create" rather than a hard error (#103).
func TestIsCrossRegionRedirect(t *testing.T) {
	respErr301 := &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 301}},
			Err:      errors.New("moved"),
		},
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"PermanentRedirect code", &fakeAPIError{code: "PermanentRedirect"}, true},
		{"MovedPermanently code", &fakeAPIError{code: "MovedPermanently"}, true},
		{"301 code", &fakeAPIError{code: "301"}, true},
		{"bare http 301", respErr301, true},
		{"wrapped http 301", fmt.Errorf("head bucket: %w", respErr301), true},
		{"NotFound is not a redirect", &fakeAPIError{code: "NotFound"}, false},
		{"plain error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCrossRegionRedirect(tc.err); got != tc.want {
				t.Errorf("isCrossRegionRedirect(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
