package staging

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

// UploadScheduleParams goes through the S3 transfer manager (#932). The
// scheduler-handler reads the object back, and the sha256 metadata and the
// tags ride on the upload request, so all three must survive the transfer.
//
// The tags are checked on the request rather than read back: Substrate stores
// metadata from PutObject but drops its x-amz-tagging header
// (GetObjectTagging returns an empty set), scttfrdmn/substrate#1434.
func TestUploadScheduleParamsStoresBodyMetadataAndTags(t *testing.T) {
	ctx := context.Background()
	env := testutil.SubstrateServer(t)
	cfg := env.AWSConfig.Copy()
	rec := testutil.NewS3Recorder(cfg.HTTPClient)
	cfg.HTTPClient = rec
	c, s3c := NewClient(cfg, "123456789012"), s3.NewFromConfig(env.AWSConfig)

	const body = "defaults:\n  instance_type: t3.micro\nparams:\n  - {x: 1}\n"
	p := filepath.Join(t.TempDir(), "params.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	bucket, key, size, sum, err := c.UploadScheduleParams(ctx, p, "sched-1", "us-east-1")
	if err != nil {
		t.Fatalf("UploadScheduleParams: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("size = %d, want %d", size, len(body))
	}

	got, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject s3://%s/%s: %v", bucket, key, err)
	}
	defer func() { _ = got.Body.Close() }()
	b, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != body {
		t.Errorf("stored body = %q, want %q", b, body)
	}
	if got.Metadata["sha256"] != sum || got.Metadata["schedule-id"] != "sched-1" {
		t.Errorf("metadata = %v, want sha256=%s schedule-id=sched-1", got.Metadata, sum)
	}

	var puts []testutil.S3Request
	for _, q := range rec.Requests() {
		if q.PutObject() && q.Header.Get("X-Amz-Tagging") != "" {
			puts = append(puts, q)
		}
	}
	const wantTags = "Project=spawn&Component=scheduler&ScheduleID=sched-1"
	if len(puts) != 1 || puts[0].Header.Get("X-Amz-Tagging") != wantTags {
		t.Errorf("want one PutObject carrying x-amz-tagging %q, got %d", wantTags, len(puts))
	}
}
