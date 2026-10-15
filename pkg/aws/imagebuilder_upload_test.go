package aws

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

// UploadISOToS3 uploads in 64 MiB parts (#932 kept that across the move to
// transfermanager). With the old manager Uploader an ISO smaller than one part
// was a single PutObject and anything larger was multipart in 64 MiB parts;
// transfermanager's own defaults would split at 16 MiB into 8 MiB parts.
func TestUploadISOToS3_Uses64MiBParts(t *testing.T) {
	const part = 64 * 1024 * 1024
	cases := []struct {
		name      string
		size      int64
		wantParts int // 0 = a single PutObject
	}{
		{"under one part", part - 1, 0},
		{"over one part", part + 1, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testutil.SubstrateServer(t)
			cfg := env.AWSConfig.Copy()
			rec := testutil.NewS3Recorder(cfg.HTTPClient)
			cfg.HTTPClient = rec
			ctx := context.Background()

			const bucket = "iso-staging"
			if _, err := s3.NewFromConfig(cfg).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: awssdk.String(bucket)}); err != nil {
				t.Fatalf("CreateBucket: %v", err)
			}

			// A sparse file: the size is what matters, not the bytes.
			p := filepath.Join(t.TempDir(), "win.iso")
			f, err := os.Create(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(tc.size); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			rec.Reset()

			if err := NewClientFromConfig(cfg).UploadISOToS3(ctx, "us-east-1", bucket, "win.ISO", p); err != nil {
				t.Fatalf("UploadISOToS3: %v", err)
			}

			if tc.wantParts == 0 {
				if n := rec.Count(testutil.S3Request.PutObject); n != 1 {
					t.Errorf("PutObject calls = %d, want 1", n)
				}
				if n := rec.Count(testutil.S3Request.CreateMultipartUpload); n != 0 {
					t.Errorf("CreateMultipartUpload calls = %d, want 0", n)
				}
			} else if n := rec.Count(testutil.S3Request.UploadPart); n != tc.wantParts {
				t.Errorf("UploadPart calls = %d, want %d (64 MiB parts)", n, tc.wantParts)
			}

			head, err := s3.NewFromConfig(env.AWSConfig).HeadObject(ctx, &s3.HeadObjectInput{
				Bucket: awssdk.String(bucket), Key: awssdk.String("win.ISO"),
			})
			if err != nil {
				t.Fatalf("HeadObject: %v", err)
			}
			if got := awssdk.ToInt64(head.ContentLength); got != tc.size {
				t.Errorf("stored size = %d, want %d", got, tc.size)
			}
		})
	}
}
