package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

// uploadFile sends job results from spored through the S3 transfer manager
// (#932). A result larger than one 5 MiB part takes the multipart path, which
// otherwise only runs on an instance; the object must hold what was written.
func TestQueueRunner_UploadFileRoundTripsAMultipartResult(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	bucket := "test-agent-upload"
	setupAgentBucket(t, env, bucket)

	runner, err := NewQueueRunnerWithAWSConfig(ctx,
		writeQueueConfig(t, minimalQueueConfig(bucket, "results/up")),
		filepath.Join(t.TempDir(), "state.json"), env.AWSConfig)
	if err != nil {
		t.Fatalf("NewQueueRunnerWithAWSConfig: %v", err)
	}

	body := make([]byte, 7*1024*1024+5)
	for i := range body {
		body[i] = byte(i*13 + i/4093)
	}
	p := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}

	const key = "results/up/jobs/j1/out.bin"
	if err := runner.uploadFile(p, bucket, key); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}

	got, err := env.S3Client().GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = got.Body.Close() }()
	b, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, body) {
		t.Errorf("stored %d bytes that differ from the %d written", len(b), len(body))
	}
}
