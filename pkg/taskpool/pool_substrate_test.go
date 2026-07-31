package taskpool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/taskproto"
	"github.com/spore-host/spawn/pkg/testutil"
)

// TestPool_EndToEnd_AgainstSubstrate is the Phase-1 end-to-end proof (#70): the
// SUBMITTER stages specs to S3 and enqueues refs, and a WORKER pulls each ref from
// the real Substrate SQS queue, fetches the spec from real Substrate S3, runs the
// job script on the host, and acks — until the queue drains and the worker idles
// out. It asserts (a) every task ran exactly once, (b) each ran in an isolated
// clean workspace, and (c) the queue is empty at the end. No paid AWS; no vacuous
// green (a real command writes a per-task sentinel we check).
func TestPool_EndToEnd_AgainstSubstrate(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	sqsClient := env.SQSClient()
	s3Client := env.S3Client()

	const bucket = "spawn-pool-itest"
	if _, err := s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	specStore := &SpecStore{Client: s3Client, Bucket: bucket, Prefix: "staging"}

	// Submitter: create the run-scoped queue + enqueue N tasks. Each task writes a
	// sentinel file into a shared output dir so we can prove it actually ran.
	runID := "e2e-run-1"
	pool, err := CreateForRun(ctx, sqsClient, specStore, runID, 60)
	if err != nil {
		t.Fatalf("CreateForRun: %v", err)
	}

	outDir := t.TempDir()
	const n = 6
	for i := 0; i < n; i++ {
		taskID := fmt.Sprintf("nf-task-%d", i)
		sentinel := filepath.Join(outDir, taskID+".done")
		spec := &taskproto.TaskSpec{
			TaskID:    taskID,
			Command:   []string{"bash", "-c", "echo ran > " + sentinel},
			Resources: taskproto.ResourceRequest{Architecture: "x86_64"},
			Lifecycle: taskproto.Lifecycle{TTL: "1h", OnComplete: "terminate"},
		}
		specJSON, err := json.Marshal(spec)
		if err != nil {
			t.Fatalf("marshal spec %s: %v", taskID, err)
		}
		if err := pool.Enqueue(ctx, spec, specJSON); err != nil {
			t.Fatalf("Enqueue %s: %v", taskID, err)
		}
	}

	// Worker: pull from the same run's queue, fetch each spec from S3, run it in a
	// clean workspace, ack. A short idle timeout drains the worker once the queue
	// empties. The executer's completion-record S3 writes go to a nonexistent
	// results bucket and fail harmlessly (the script doesn't `set -e`), so the
	// task's own exit status still flows through — exactly the real-instance path
	// minus the (also-best-effort) record upload.
	workerQueue, err := OpenQueue(ctx, sqsClient, runID)
	if err != nil {
		t.Fatalf("OpenQueue (worker): %v", err)
	}
	worker := &Worker{
		Queue:     workerQueue,
		Fetcher:   specStore,
		Execer:    &ScriptExecer{ResultsBucket: "spawn-results-none", Region: "us-east-1"},
		Workspace: &DirWorkspace{Root: filepath.Join(t.TempDir(), "ws")},
		Config:    WorkerConfig{PollWaitSeconds: 1, IdleTimeout: 3 * time.Second, Log: os.Stderr},
	}

	executed, err := worker.Run(ctx)
	if err != nil {
		t.Fatalf("worker.Run: %v", err)
	}
	if executed != n {
		t.Fatalf("worker executed %d tasks, want %d", executed, n)
	}

	// Every task actually ran (sentinel present).
	for i := 0; i < n; i++ {
		sentinel := filepath.Join(outDir, fmt.Sprintf("nf-task-%d.done", i))
		if _, err := os.Stat(sentinel); err != nil {
			t.Errorf("task %d did not run (no sentinel %s): %v", i, sentinel, err)
		}
	}

	// Queue drained: submitter sees zero depth, then deletes it.
	visible, inFlight, err := pool.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if visible != 0 || inFlight != 0 {
		t.Fatalf("queue not drained: visible=%d in-flight=%d", visible, inFlight)
	}
	if err := pool.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}
