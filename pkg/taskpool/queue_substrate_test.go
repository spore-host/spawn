package taskpool

import (
	"context"
	"testing"

	"github.com/spore-host/spawn/pkg/testutil"
)

// TestQueue_AgainstSubstrate exercises the full queue lifecycle against a real
// SQS implementation (Substrate's SQS plugin, in-process) rather than the
// in-memory fake — so the claim/ack/depth/delete semantics are validated against
// the AWS JSON protocol the production *sqs.Client speaks, not a fake's quirks.
//
// Substrate's SQS plugin supports the aws-sdk-go-v2 JSON 1.0 protocol
// (substrate CHANGELOG #236), so the standard SDK client works unmodified.
func TestQueue_AgainstSubstrate(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()
	client := env.SQSClient()

	// Create a run-scoped queue.
	q, err := CreateQueue(ctx, client, "itest-run-1", 30)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	if q.URL() == "" {
		t.Fatal("CreateQueue returned empty URL")
	}

	// Submit three task refs.
	refs := []TaskRef{
		{TaskID: "nf-aaa", SpecURI: "s3://b/specs/aaa.json"},
		{TaskID: "nf-bbb", SpecURI: "s3://b/specs/bbb.json"},
		{TaskID: "nf-ccc", SpecURI: "s3://b/specs/ccc.json"},
	}
	for _, r := range refs {
		if err := q.Submit(ctx, r); err != nil {
			t.Fatalf("Submit %s: %v", r.TaskID, err)
		}
	}

	// Reopen by run id (the worker path — knows only the run id) and drain via
	// claim → ack, asserting each ref round-trips intact and the queue empties.
	wq, err := OpenQueue(ctx, client, "itest-run-1")
	if err != nil {
		t.Fatalf("OpenQueue: %v", err)
	}

	got := map[string]string{}
	for i := 0; i < len(refs); i++ {
		claimed, err := wq.Claim(ctx, 0)
		if err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
		if claimed == nil {
			t.Fatalf("Claim %d: got nil, expected a task (queue should have %d left)", i, len(refs)-i)
		}
		got[claimed.Ref.TaskID] = claimed.Ref.SpecURI
		if err := wq.Ack(ctx, claimed); err != nil {
			t.Fatalf("Ack %s: %v", claimed.Ref.TaskID, err)
		}
	}

	// Every submitted ref was claimed exactly once, body intact.
	for _, r := range refs {
		if got[r.TaskID] != r.SpecURI {
			t.Errorf("task %s: claimed SpecURI = %q, want %q", r.TaskID, got[r.TaskID], r.SpecURI)
		}
	}

	// Queue is drained: a further claim returns nil.
	extra, err := wq.Claim(ctx, 0)
	if err != nil {
		t.Fatalf("final Claim: %v", err)
	}
	if extra != nil {
		t.Fatalf("expected empty queue, got task %s", extra.Ref.TaskID)
	}

	// Clean up the run-scoped queue.
	if err := q.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
