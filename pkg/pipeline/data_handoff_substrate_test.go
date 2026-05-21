package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

const (
	testBucket     = "test-pipeline-data"
	testPrefix     = "pipelines"
	testPipelineID = "pipe-substrate-001"
	testStageID    = "stage-process"
)

func setupDataBucket(t *testing.T, env *testutil.TestEnv) {
	t.Helper()
	_, err := env.S3Client().CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(testBucket),
	})
	if err != nil {
		t.Fatalf("CreateBucket %s: %v", testBucket, err)
	}
}

func TestNewStageDataHandlerWithAWSConfig(t *testing.T) {
	env := testutil.SubstrateServer(t)
	setupDataBucket(t, env)

	h, err := NewStageDataHandlerWithAWSConfig(
		context.Background(),
		testBucket, testPrefix, testPipelineID, testStageID,
		env.AWSConfig,
	)
	if err != nil {
		t.Fatalf("NewStageDataHandlerWithAWSConfig() error = %v", err)
	}
	if h == nil {
		t.Fatal("NewStageDataHandlerWithAWSConfig() returned nil")
	}
	if h.bucket != testBucket {
		t.Errorf("bucket = %q, want %q", h.bucket, testBucket)
	}
	if h.stageID != testStageID {
		t.Errorf("stageID = %q, want %q", h.stageID, testStageID)
	}
}

func TestStageDataHandler_UploadOutputs(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupDataBucket(t, env)

	// Create a temp output file.
	dir := t.TempDir()
	outFile := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(outFile, []byte("hello substrate"), 0600); err != nil {
		t.Fatalf("write output file: %v", err)
	}

	h, err := NewStageDataHandlerWithAWSConfig(ctx, testBucket, testPrefix, testPipelineID, testStageID, env.AWSConfig)
	if err != nil {
		t.Fatalf("NewStageDataHandlerWithAWSConfig() error = %v", err)
	}

	if err := h.UploadStageOutputs(ctx, &DataConfig{
		Mode:  "s3",
		Paths: []string{outFile},
	}); err != nil {
		t.Fatalf("UploadStageOutputs() error = %v", err)
	}

	// Absolute paths preserve directory structure in the S3 key.
	// List objects under the output prefix and confirm result.txt is present.
	list, err := env.S3Client().ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(testBucket),
		Prefix: aws.String(testPrefix + "/stages/" + testStageID + "/output/"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	found := false
	for _, obj := range list.Contents {
		if strings.HasSuffix(*obj.Key, "result.txt") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("result.txt not found in stage output prefix after UploadStageOutputs")
	}
}

func TestStageDataHandler_CompletionMarker(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupDataBucket(t, env)

	h, err := NewStageDataHandlerWithAWSConfig(ctx, testBucket, testPrefix, testPipelineID, testStageID, env.AWSConfig)
	if err != nil {
		t.Fatalf("NewStageDataHandlerWithAWSConfig() error = %v", err)
	}

	// No marker yet.
	exists, err := h.CheckCompletionMarker(ctx, testStageID)
	if err != nil {
		t.Fatalf("CheckCompletionMarker() error = %v", err)
	}
	if exists {
		t.Error("completion marker exists before WriteCompletionMarker was called")
	}

	// Write marker.
	if err := h.WriteCompletionMarker(ctx); err != nil {
		t.Fatalf("WriteCompletionMarker() error = %v", err)
	}

	// Now it should exist.
	exists, err = h.CheckCompletionMarker(ctx, testStageID)
	if err != nil {
		t.Fatalf("CheckCompletionMarker() after write error = %v", err)
	}
	if !exists {
		t.Error("completion marker not found after WriteCompletionMarker")
	}
}

func TestStageDataHandler_DownloadInputs(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	setupDataBucket(t, env)

	const upstreamStage = "stage-ingest"

	// Upload a file as if it came from an upstream stage.
	upstreamKey := testPrefix + "/stages/" + upstreamStage + "/output/data.txt"
	if _, err := env.S3Client().PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(upstreamKey),
		Body:   strings.NewReader("upstream data"),
	}); err != nil {
		t.Fatalf("PutObject upstream: %v", err)
	}

	destDir := t.TempDir()
	h, err := NewStageDataHandlerWithAWSConfig(ctx, testBucket, testPrefix, testPipelineID, testStageID, env.AWSConfig)
	if err != nil {
		t.Fatalf("NewStageDataHandlerWithAWSConfig() error = %v", err)
	}

	if err := h.DownloadStageInputs(ctx, &DataConfig{
		Mode:        "s3",
		SourceStage: upstreamStage,
		DestPath:    destDir,
	}); err != nil {
		t.Fatalf("DownloadStageInputs() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "data.txt"))
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(got) != "upstream data" {
		t.Errorf("downloaded content = %q, want %q", string(got), "upstream data")
	}
}
