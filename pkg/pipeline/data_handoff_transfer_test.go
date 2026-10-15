package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spore-host/spawn/pkg/testutil"
)

// Stage outputs go up and stage inputs and results come down through the S3
// transfer manager (#932). These round-trip a file larger than one 5 MiB part,
// so the multipart upload and the ranged download both run, and an empty file,
// which a ranged GET answers with 416. Both directions must return the bytes
// that went in.

func transferPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/7919)
	}
	return b
}

// writeStageOutputs writes the files a stage would produce and uploads them as
// stageID's outputs, matched by a relative glob so the keys are the base names.
func writeStageOutputs(t *testing.T, env *testutil.TestEnv, stageID string, files map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	h, err := NewStageDataHandlerWithAWSConfig(context.Background(), testBucket, testPrefix, testPipelineID, stageID, env.AWSConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.UploadStageOutputs(context.Background(), &DataConfig{Mode: "s3", Paths: []string{"*.bin"}}); err != nil {
		t.Fatalf("UploadStageOutputs: %v", err)
	}
}

func assertFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got %d bytes that differ from the %d uploaded", name, len(got), len(want))
		}
	}
}

func TestStageDataHandler_MultipartAndEmptyRoundTrip(t *testing.T) {
	env := testutil.SubstrateServer(t)
	setupDataBucket(t, env)

	files := map[string][]byte{
		"big.bin":   transferPayload(11*1024*1024 + 3), // three parts, the last short
		"empty.bin": {},
	}
	writeStageOutputs(t, env, "stage-ingest", files)

	dest := t.TempDir()
	h, err := NewStageDataHandlerWithAWSConfig(context.Background(), testBucket, testPrefix, testPipelineID, testStageID, env.AWSConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.DownloadStageInputs(context.Background(), &DataConfig{
		Mode: "s3", SourceStage: "stage-ingest", DestPath: dest,
	}); err != nil {
		t.Fatalf("DownloadStageInputs: %v", err)
	}
	assertFiles(t, dest, files)
}

// DownloadResultsToLocal builds its own client from the default credential
// chain, so the emulator is supplied through the environment.
func TestDownloadResultsToLocal_MultipartAndEmpty(t *testing.T) {
	env := testutil.SubstrateServer(t)
	setupDataBucket(t, env)

	files := map[string][]byte{
		"big.bin":   transferPayload(6*1024*1024 + 1),
		"empty.bin": {},
	}
	writeStageOutputs(t, env, testStageID, files)

	empty := filepath.Join(t.TempDir(), "none")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ENDPOINT_URL", env.URL)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_SESSION_TOKEN", "test")

	dest := t.TempDir()
	if err := DownloadResultsToLocal(context.Background(), testBucket, testPrefix, testPipelineID, dest); err != nil {
		t.Fatalf("DownloadResultsToLocal: %v", err)
	}
	assertFiles(t, filepath.Join(dest, testStageID, "output"), files)
}
