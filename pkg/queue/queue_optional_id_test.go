package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noIDQueue is a user-authored queue file with no queue_id, shaped like the
// example in the batch-queue guide.
const noIDQueue = `{"queue_name": "my-pipeline", "jobs": [{"job_id": "a", "command": "true", "timeout": "1m"}]}`

func writeQueue(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "queue.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadUserConfigGeneratesMissingQueueID is the #1131 gate for --batch-queue:
// the CLI generates a queue ID "if not set", which is only reachable if the
// loader generates it before validation rejects the empty field.
func TestLoadUserConfigGeneratesMissingQueueID(t *testing.T) {
	cfg, err := LoadUserConfig(writeQueue(t, noIDQueue))
	if err != nil {
		t.Fatalf("a queue file without queue_id was rejected: %v", err)
	}
	if !strings.HasPrefix(cfg.QueueID, "queue-") {
		t.Errorf("QueueID = %q, want a generated queue-<timestamp> ID", cfg.QueueID)
	}
}

// An explicit queue_id is kept as written.
func TestLoadUserConfigKeepsExplicitQueueID(t *testing.T) {
	cfg, err := LoadUserConfig(writeQueue(t, `{"queue_id": "mine", "queue_name": "q", "jobs": [{"job_id": "a", "command": "true", "timeout": "1m"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueueID != "mine" {
		t.Errorf("QueueID = %q, want the file's own %q", cfg.QueueID, "mine")
	}
}

// LoadConfig stays strict: the on-instance runner reads the copy the CLI
// uploaded (which always has an ID), and inventing a different one there would
// diverge from the ID the CLI used for S3 paths and tags.
func TestLoadConfigStillRequiresQueueID(t *testing.T) {
	if _, err := LoadConfig(writeQueue(t, noIDQueue)); err == nil || !strings.Contains(err.Error(), "queue_id is required") {
		t.Errorf("LoadConfig accepted a queue with no queue_id (err=%v); the runner must not invent an ID", err)
	}
}
