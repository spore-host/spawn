package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBatchQueueFileMayOmitQueueID is the CLI half of the #1131 queue gate. A
// --batch-queue file with no queue_id must get past loading; with the old
// LoadConfig it failed with "queue_id is required" before the ID-generation
// branch could run. --instance-type is left empty so the launch stops at that
// check, after loading and before any AWS call.
func TestBatchQueueFileMayOmitQueueID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "queue.json")
	body := `{"queue_name": "my-pipeline", "jobs": [{"job_id": "a", "command": "true", "timeout": "1m"}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	prevFile, prevTmpl, prevType := batchQueueFile, queueTemplate, instanceType
	t.Cleanup(func() { batchQueueFile, queueTemplate, instanceType = prevFile, prevTmpl, prevType })
	batchQueueFile, queueTemplate, instanceType = p, "", ""

	err := launchWithBatchQueue(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected the --instance-type refusal, got nil")
	}
	if strings.Contains(err.Error(), "queue_id") {
		t.Fatalf("a queue file without queue_id was rejected: %v", err)
	}
	if !strings.Contains(err.Error(), "--instance-type is required") {
		t.Fatalf("expected to stop at the --instance-type check after loading, got: %v", err)
	}
}
