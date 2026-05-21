package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/testutil"
)

// writeQueueConfig writes a minimal queue JSON config to a temp file.
func writeQueueConfig(t *testing.T, cfg map[string]interface{}) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal queue config: %v", err)
	}
	f := filepath.Join(t.TempDir(), "queue.json")
	if err := os.WriteFile(f, data, 0600); err != nil {
		t.Fatalf("write queue config: %v", err)
	}
	return f
}

// minimalQueueConfig returns the base config fields; merges allow per-test overrides.
func minimalQueueConfig(bucket, prefix string) map[string]interface{} {
	return map[string]interface{}{
		"queue_id": "test-queue-1",
		"jobs": []interface{}{
			map[string]interface{}{
				"job_id":  "job-noop",
				"command": "true",
				"timeout": "30s",
			},
		},
		"result_s3_bucket": bucket,
		"result_s3_prefix": prefix,
	}
}

func setupAgentBucket(t *testing.T, env *testutil.TestEnv, name string) {
	t.Helper()
	_, err := env.S3Client().CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(name),
	})
	if err != nil {
		t.Fatalf("CreateBucket %s: %v", name, err)
	}
}

func TestNewQueueRunnerWithAWSConfig_Constructor(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	bucket := "test-agent-ctor"
	setupAgentBucket(t, env, bucket)

	queueFile := writeQueueConfig(t, minimalQueueConfig(bucket, "results/ctor"))
	stateFile := filepath.Join(t.TempDir(), "state.json")

	runner, err := NewQueueRunnerWithAWSConfig(ctx, queueFile, stateFile, env.AWSConfig)
	if err != nil {
		t.Fatalf("NewQueueRunnerWithAWSConfig() error = %v", err)
	}
	if runner == nil {
		t.Fatal("NewQueueRunnerWithAWSConfig() returned nil")
	}
	if runner.config.QueueID != "test-queue-1" {
		t.Errorf("QueueID = %q, want %q", runner.config.QueueID, "test-queue-1")
	}
	if runner.stateFile != stateFile {
		t.Errorf("stateFile = %q, want %q", runner.stateFile, stateFile)
	}
	if len(runner.state.Jobs) != 1 {
		t.Errorf("len(state.Jobs) = %d, want 1", len(runner.state.Jobs))
	}
}

func TestQueueRunnerWithAWSConfig_SaveAndUploadState(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	bucket := "test-agent-state"
	setupAgentBucket(t, env, bucket)

	queueFile := writeQueueConfig(t, minimalQueueConfig(bucket, "results/state"))
	stateFile := filepath.Join(t.TempDir(), "state.json")

	runner, err := NewQueueRunnerWithAWSConfig(ctx, queueFile, stateFile, env.AWSConfig)
	if err != nil {
		t.Fatalf("NewQueueRunnerWithAWSConfig() error = %v", err)
	}

	// saveState writes the initial state to disk.
	if err := runner.saveState(); err != nil {
		t.Fatalf("saveState() error = %v", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("state file not created: %v", err)
	}

	// uploadFinalState uploads it to S3.
	if err := runner.uploadFinalState(); err != nil {
		t.Fatalf("uploadFinalState() error = %v", err)
	}

	expectedKey := "results/state/queue-state.json"
	_, err = env.S3Client().HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(expectedKey),
	})
	if err != nil {
		t.Errorf("queue-state.json not found in S3 at %s: %v", expectedKey, err)
	}
}

func TestNewQueueRunnerWithAWSConfig_ResumesExistingState(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	bucket := "test-agent-resume"
	setupAgentBucket(t, env, bucket)

	queueFile := writeQueueConfig(t, minimalQueueConfig(bucket, "results/resume"))
	stateFile := filepath.Join(t.TempDir(), "state.json")

	// First construction — initializes state.
	runner, err := NewQueueRunnerWithAWSConfig(ctx, queueFile, stateFile, env.AWSConfig)
	if err != nil {
		t.Fatalf("first NewQueueRunnerWithAWSConfig() error = %v", err)
	}
	runner.state.Status = "completed"
	if err := runner.saveState(); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	// Second construction — should load existing state.
	runner2, err := NewQueueRunnerWithAWSConfig(ctx, queueFile, stateFile, env.AWSConfig)
	if err != nil {
		t.Fatalf("second NewQueueRunnerWithAWSConfig() error = %v", err)
	}
	if runner2.state.Status != "completed" {
		t.Errorf("resumed state.Status = %q, want %q", runner2.state.Status, "completed")
	}
}
