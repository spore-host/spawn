package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPoolWorkerPolicy_Valid checks the scoped worker IAM policy is well-formed
// and correctly scoped: valid JSON, SQS limited to THIS run's queue ARN (never
// "*"), and the spec/results bucket + spawn-binaries granted. The real-AWS smoke
// caught that workers had NO SQS access at all (bare spored role) — this locks in
// the fix.
func TestPoolWorkerPolicy_Valid(t *testing.T) {
	pol := poolWorkerPolicy("123456789012", "us-east-1", "spawn-results-123456789012-us-east-1", "nf-run-abc")

	// Well-formed JSON.
	var doc struct {
		Version   string
		Statement []struct {
			Effect   string
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(pol), &doc); err != nil {
		t.Fatalf("worker policy is not valid JSON: %v\n%s", err, pol)
	}
	if doc.Version != "2012-10-17" {
		t.Errorf("policy Version = %q", doc.Version)
	}

	// SQS actions must be present and scoped to THIS run's queue ARN, not "*".
	var sqsStmt *struct {
		Effect   string
		Action   []string
		Resource []string
	}
	for i := range doc.Statement {
		for _, a := range doc.Statement[i].Action {
			if strings.HasPrefix(a, "sqs:") {
				sqsStmt = &doc.Statement[i]
				break
			}
		}
	}
	if sqsStmt == nil {
		t.Fatal("worker policy grants NO sqs actions — a worker can't pull from the queue (the bug the smoke found)")
	}
	wantSQS := map[string]bool{"sqs:GetQueueUrl": true, "sqs:ReceiveMessage": true, "sqs:DeleteMessage": true, "sqs:GetQueueAttributes": true}
	for _, a := range sqsStmt.Action {
		delete(wantSQS, a)
	}
	if len(wantSQS) > 0 {
		t.Errorf("worker policy missing SQS actions: %v", wantSQS)
	}
	for _, r := range sqsStmt.Resource {
		if r == "*" || !strings.Contains(r, "spawn-pool-nf-run-abc") {
			t.Errorf("SQS resource must be scoped to the run queue, got %q", r)
		}
	}

	// The spec/results bucket must be reachable (GetObject + PutObject somewhere).
	if !strings.Contains(pol, "spawn-results-123456789012-us-east-1") {
		t.Error("worker policy doesn't grant the spec/results bucket")
	}
	// The bootstrap's spored binary download must be allowed.
	if !strings.Contains(pol, "spawn-binaries-*") {
		t.Error("worker policy doesn't allow the spored binary download")
	}
}
