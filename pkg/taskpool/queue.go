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
//     message reappears after the timeout and another worker retries it. This
//     is the only recovery from a lost worker: the pool does not replace
//     workers after `spawn pool create` returns (#1149), it reruns their tasks.
//
// At-least-once, not exactly-once: a worker that finishes a task but dies before
// DeleteMessage causes a redelivery. That is safe here because a task's outputs
// land at deterministic S3 keys (idempotent overwrite) and its completion record
// is the authoritative signal — a re-run just rewrites the same bytes.
package taskpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	smithy "github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/poolqueue"
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
	// tagErr is why CreateQueue had to create the queue without its
	// poolqueue.Tags; nil when it is tagged (or was only opened).
	tagErr error
}

// QueueName returns the SQS queue name for a run id. Deterministic so a worker
// booted with only the run id can resolve the same queue the submitter created.
// SQS names allow [A-Za-z0-9_-] up to 80 chars; the run id is expected to already
// be within that alphabet (the pool uses a slugged run id).
func QueueName(runID string) string {
	return poolqueue.QueueName(runID)
}

// CreateQueue creates (idempotently) the run-scoped queue and returns a handle.
// visibilityTimeout is how long a claimed task stays invisible to other workers —
// it MUST exceed the longest expected single-task runtime, or a slow task would be
// redelivered and double-run while still in flight. `spawn pool create` passes
// --visibility-timeout, a fixed 900s unless overridden; nothing derives it from
// a task's TTL. A create for an existing name returns that queue (SQS CreateQueue is
// idempotent when attributes match), so first-worker-or-submitter-wins is safe.
//
// The queue is created tagged poolqueue.Tags(runID) — spawn:managed=true and
// spawn:pool-run-id — which is what lets `spawn cleanup --pool-queues` and the
// reaper recognise and delete it once the pool is abandoned (#1351). Tagging on
// create also needs sqs:TagQueue; a caller without it gets an untagged queue
// rather than a failed pool, and TagErr says why (only `spawn pool drain` then
// deletes it).
func CreateQueue(ctx context.Context, client SQSAPI, runID string, visibilityTimeout int) (*Queue, error) {
	name := QueueName(runID)
	in := &sqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			"VisibilityTimeout": fmt.Sprintf("%d", visibilityTimeout),
			// A generous retention so a queue outlives a slow run but a leaked queue
			// still expires. 12h: far longer than any interactive fan-out.
			"MessageRetentionPeriod": "43200",
		},
		Tags: poolqueue.Tags(runID),
	}
	out, err := client.CreateQueue(ctx, in)
	var tagErr error
	if err != nil && isAccessDenied(err) {
		tagErr = err
		in.Tags = nil
		out, err = client.CreateQueue(ctx, in)
	}
	if err != nil {
		return nil, fmt.Errorf("create queue %s: %w", name, err)
	}
	return &Queue{client: client, url: aws.ToString(out.QueueUrl), name: name, tagErr: tagErr}, nil
}

// TagErr is non-nil when CreateQueue could not tag the queue (the caller
// lacks sqs:TagQueue) and created it untagged. Such a queue has no deleter
// but `spawn pool drain`.
func (q *Queue) TagErr() error { return q.tagErr }

// isAccessDenied reports whether err is an authorization refusal, the one
// failure CreateQueue retries without tags.
func isAccessDenied(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return strings.Contains(apiErr.ErrorCode(), "AccessDenied")
}

// openQueueMaxAttempts bounds OpenQueue's retry on a not-yet-visible queue.
// GetQueueUrl is eventually consistent after CreateQueue, so a worker that boots
// and resolves the queue can race the submitter's create and see NonExistentQueue
// for a short window (observed in a #70 smoke: a worker exited at boot because its
// first GetQueueUrl 400'd, seconds after the queue was created). 10 attempts ×
// ~3s ≈ 30s of tolerance covers the consistency window without hanging forever on
// a genuinely-absent queue.
const openQueueMaxAttempts = 10

// openQueueRetryDelay is the wait between OpenQueue attempts. A package var (not a
// const) so tests can shrink it; production uses ~3s.
var openQueueRetryDelay = 3 * time.Second

// OpenQueue resolves an EXISTING run-scoped queue by run id (the worker path: the
// worker knows only the run id and looks up the URL the submitter created).
//
// It retries a NonExistentQueue error, because GetQueueUrl is eventually
// consistent after the submitter's CreateQueue — a freshly-booted worker can
// resolve the queue before that create has propagated. Any OTHER error (auth,
// throttle, malformed) returns immediately. A queue that never appears within the
// attempt budget returns the last NonExistentQueue error, so the caller fails
// loudly rather than silently no-op'ing.
func OpenQueue(ctx context.Context, client SQSAPI, runID string) (*Queue, error) {
	name := QueueName(runID)
	var lastErr error
	for attempt := 0; attempt < openQueueMaxAttempts; attempt++ {
		if attempt > 0 {
			// Context-aware backoff so a cancelled worker exits promptly.
			t := time.NewTimer(openQueueRetryDelay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
		}
		out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err == nil {
			return &Queue{client: client, url: aws.ToString(out.QueueUrl), name: name}, nil
		}
		lastErr = err
		if !isNonExistentQueue(err) {
			return nil, fmt.Errorf("resolve queue %s: %w", name, err)
		}
		// NonExistentQueue → the create hasn't propagated yet; retry.
	}
	return nil, fmt.Errorf("resolve queue %s: not visible after %d attempts: %w", name, openQueueMaxAttempts, lastErr)
}

// isNonExistentQueue reports whether err is SQS's NonExistentQueue (the create→
// GetQueueUrl consistency case OpenQueue retries). Matched on the API error code
// via smithy, so it's robust to message wording.
func isNonExistentQueue(err error) bool {
	var qde *types.QueueDoesNotExist
	if errors.As(err, &qde) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "AWS.SimpleQueueService.NonExistentQueue" || code == "QueueDoesNotExist"
	}
	return false
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
//
// A nil count is unknown: SQS (or an SQS-compatible provider) omitted the
// attribute or returned something that is not a count. Unknown is not zero,
// and a caller must not read it as an empty queue (#1362); the parsing is
// poolqueue.Count, the same rule the pool-queue sweep applies.
func (q *Queue) Depth(ctx context.Context) (visible, inFlight *int, err error) {
	out, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(q.url),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("queue depth for %s: %w", q.name, err)
	}
	return knownCount(out.Attributes, types.QueueAttributeNameApproximateNumberOfMessages),
		knownCount(out.Attributes, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible), nil
}

// Incarnation identifies this particular queue among every queue the run's
// name has had: SQS's CreatedTimestamp. A re-create of a live queue returns
// the same queue and so the same incarnation; a queue created after the
// previous one was deleted (a failed `pool create`, retried) has a new one.
// `pool create` derives its workers' RunInstances idempotency tokens from it
// (#1448).
func (q *Queue) Incarnation(ctx context.Context) (string, error) {
	out, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(q.url),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameCreatedTimestamp},
	})
	if err != nil {
		return "", fmt.Errorf("creation time of queue %s: %w", q.name, err)
	}
	ts := out.Attributes[string(types.QueueAttributeNameCreatedTimestamp)]
	if ts == "" {
		return "", fmt.Errorf("creation time of queue %s: SQS did not return %s", q.name, types.QueueAttributeNameCreatedTimestamp)
	}
	return ts, nil
}

// knownCount is poolqueue.Count as a pointer: nil when the count is unknown.
func knownCount(attrs map[string]string, name types.QueueAttributeName) *int {
	n, ok := poolqueue.Count(attrs, name)
	if !ok {
		return nil
	}
	return &n
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
