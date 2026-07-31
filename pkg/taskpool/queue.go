// Package taskpool implements the run-scoped task queue and fungible-worker
// runner behind spawn's pooled execution mode (#70).
//
// The problem it solves: at wide fan-out, launching one ephemeral instance per
// task is dispatch-bound AND pays a full instance-boot tax per task, so short
// tasks self-terminate faster than new ones launch and steady-state concurrency
// can never reach N (Little's Law). A worker POOL inverts this: a fixed set of
// fungible workers (provisioned as a cohort — see pkg/taskcohort) pull task specs
// from a shared queue and REUSE across jobs, so per-job cost is stage+run only.
//
// This file is the queue seam. It is deliberately small and provider-thin: the
// claim/redelivery semantics ride on SQS's visibility timeout —
//
//   - Submit  → SendMessage (the body is a task-spec reference, not the spec)
//   - Claim   → ReceiveMessage; the message is invisible to other workers for
//     VisibilityTimeout, so the receiving worker "owns" it for that window.
//   - Ack     → DeleteMessage after the task's completion record is durably
//     written. If the worker dies / is Spot-reclaimed before it acks, the
//     message reappears after the timeout and another worker retries it — which
//     is exactly cohort's "reconcile replaces reaped workers" at the task level.
//
// At-least-once, not exactly-once: a worker that finishes a task but dies before
// DeleteMessage causes a redelivery. That is safe here because a task's outputs
// land at deterministic S3 keys (idempotent overwrite) and its completion record
// is the authoritative signal — a re-run just rewrites the same bytes.
package taskpool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQSAPI is the slice of the SQS SDK the queue needs. An interface (not the
// concrete *sqs.Client) so unit tests can inject a fake and the integration test
// can pass a real client pointed at Substrate — the same seam pattern
// pkg/taskcohort/LaunchAPI uses.
type SQSAPI interface {
	CreateQueue(ctx context.Context, in *sqs.CreateQueueInput, opts ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error)
	GetQueueUrl(ctx context.Context, in *sqs.GetQueueUrlInput, opts ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	GetQueueAttributes(ctx context.Context, in *sqs.GetQueueAttributesInput, opts ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	DeleteQueue(ctx context.Context, in *sqs.DeleteQueueInput, opts ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error)
}

// TaskRef is what travels on the queue: a small pointer to a TaskSpec staged in
// S3, NOT the spec itself. Specs can carry long staging scripts (nf-spawn embeds
// its whole staging script as the command), which can exceed SQS's 256 KiB
// message cap — so the submitter stages the spec JSON to S3 and enqueues its URI.
// The worker fetches the spec by URI when it claims the ref.
type TaskRef struct {
	// TaskID is the spec's task id — also the completion-record key the submitter
	// polls. Carried on the ref so a worker (and logs) can name the task without
	// fetching the spec first.
	TaskID string `json:"task_id"`
	// SpecURI is the s3:// location of the staged TaskSpec JSON.
	SpecURI string `json:"spec_uri"`
}

// Queue is a run-scoped SQS task queue. One queue per pipeline run: its name
// embeds the run id, so concurrent runs never share workers or tasks, and drain
// deletes exactly this run's queue.
type Queue struct {
	client SQSAPI
	url    string
	name   string
}

// QueueName returns the SQS queue name for a run id. Deterministic so a worker
// booted with only the run id can resolve the same queue the submitter created.
// SQS names allow [A-Za-z0-9_-] up to 80 chars; the run id is expected to already
// be within that alphabet (the pool uses a slugged run id).
func QueueName(runID string) string {
	return "spawn-pool-" + runID
}

// CreateQueue creates (idempotently) the run-scoped queue and returns a handle.
// visibilityTimeout is how long a claimed task stays invisible to other workers —
// it MUST exceed the longest expected single-task runtime, or a slow task would be
// redelivered and double-run while still in flight. The caller sizes it from the
// task TTL. A create for an existing name returns that queue (SQS CreateQueue is
// idempotent when attributes match), so first-worker-or-submitter-wins is safe.
func CreateQueue(ctx context.Context, client SQSAPI, runID string, visibilityTimeout int) (*Queue, error) {
	name := QueueName(runID)
	out, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			"VisibilityTimeout": fmt.Sprintf("%d", visibilityTimeout),
			// A generous retention so a queue outlives a slow run but a leaked queue
			// still expires. 12h: far longer than any interactive fan-out.
			"MessageRetentionPeriod": "43200",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create queue %s: %w", name, err)
	}
	return &Queue{client: client, url: aws.ToString(out.QueueUrl), name: name}, nil
}

// OpenQueue resolves an EXISTING run-scoped queue by run id (the worker path: the
// worker knows only the run id and looks up the URL the submitter created).
func OpenQueue(ctx context.Context, client SQSAPI, runID string) (*Queue, error) {
	name := QueueName(runID)
	out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return nil, fmt.Errorf("resolve queue %s: %w", name, err)
	}
	return &Queue{client: client, url: aws.ToString(out.QueueUrl), name: name}, nil
}

// URL returns the queue's SQS URL.
func (q *Queue) URL() string { return q.url }

// Submit enqueues one task ref. Non-blocking best-effort dispatch: the submitter
// enqueues all N refs (fast — one SendMessage each, no instance launch) and the
// workers drain them. This is what makes dispatch O(SendMessage) instead of
// O(RunInstances) per task.
func (q *Queue) Submit(ctx context.Context, ref TaskRef) error {
	body, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("marshal task ref %s: %w", ref.TaskID, err)
	}
	_, err = q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(q.url),
		MessageBody: aws.String(string(body)),
	})
	if err != nil {
		return fmt.Errorf("submit task %s: %w", ref.TaskID, err)
	}
	return nil
}

// Claimed is a task a worker has received (and thus owns for the visibility
// window). Ack it after the task's completion record is durably written.
type Claimed struct {
	Ref           TaskRef
	receiptHandle string
}

// Claim receives up to one task, long-polling for waitSeconds (0..20). Returns
// (nil, nil) when the queue is empty after the poll — the worker's signal to
// check its idle deadline and maybe drain. It never blocks forever: the caller
// loops, so an empty return is "nothing right now", not "done".
func (q *Queue) Claim(ctx context.Context, waitSeconds int32) (*Claimed, error) {
	out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(q.url),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     waitSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("claim from %s: %w", q.name, err)
	}
	if len(out.Messages) == 0 {
		return nil, nil
	}
	msg := out.Messages[0]
	var ref TaskRef
	if err := json.Unmarshal([]byte(aws.ToString(msg.Body)), &ref); err != nil {
		// A malformed body can never be executed; delete it so it doesn't loop
		// forever, and report the error so the worker can log+continue.
		_, _ = q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl: aws.String(q.url), ReceiptHandle: msg.ReceiptHandle,
		})
		return nil, fmt.Errorf("claim from %s: malformed task ref (dropped): %w", q.name, err)
	}
	return &Claimed{Ref: ref, receiptHandle: aws.ToString(msg.ReceiptHandle)}, nil
}

// Ack deletes a claimed task from the queue — the worker calls this only AFTER the
// task's completion record is durably written. If the worker dies before Ack, the
// message reappears after the visibility timeout and another worker retries it.
func (q *Queue) Ack(ctx context.Context, c *Claimed) error {
	_, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(q.url),
		ReceiptHandle: aws.String(c.receiptHandle),
	})
	if err != nil {
		return fmt.Errorf("ack task %s: %w", c.Ref.TaskID, err)
	}
	return nil
}

// Depth returns the approximate number of visible + in-flight messages, used by
// the pool to decide when the run has drained (both zero → all tasks done or
// dropped). Approximate is fine: it's a drain heuristic, not a correctness gate
// (the authoritative per-task signal is each completion record).
func (q *Queue) Depth(ctx context.Context) (visible, inFlight int, err error) {
	out, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(q.url),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("queue depth for %s: %w", q.name, err)
	}
	visible = atoiDefault(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)])
	inFlight = atoiDefault(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	return visible, inFlight, nil
}

// Delete removes the run-scoped queue entirely — the submitter's drain step, once
// the run has finished. Best-effort at the call site: a leaked queue also expires
// via MessageRetentionPeriod, but explicit deletion is the clean path.
func (q *Queue) Delete(ctx context.Context) error {
	_, err := q.client.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(q.url)})
	if err != nil {
		return fmt.Errorf("delete queue %s: %w", q.name, err)
	}
	return nil
}

func atoiDefault(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
