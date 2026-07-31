package taskpool

import (
	"context"
	"fmt"
	"io"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// Pool is the SUBMITTER side of pooled execution: it owns the run-scoped queue and
// the spec store, and provides the enqueue path a workflow adapter (or the CLI)
// drives. Worker PROVISIONING is done by the caller via pkg/taskcohort (a cohort
// of fungible workers) — Pool deliberately does not import taskcohort, so the two
// concerns compose without coupling: the queue works the same whether workers come
// from a cohort, a hand-launched instance, or a local process in a test.
//
// Lifecycle (matches the design's create → submit → drain):
//
//	p, _ := taskpool.CreateForRun(ctx, sqs, specStore, runID, visibilityTimeout)
//	// (caller provisions N workers via taskcohort, pointed at runID)
//	for _, spec := range specs { p.Enqueue(ctx, spec) }   // stage + submit, fast
//	// (workers pull, run, ack; adapter polls each spec's completion.json)
//	p.Drain(ctx)                                          // delete the queue
type Pool struct {
	RunID string
	Queue *Queue
	Specs *SpecStore
	Log   io.Writer
}

// CreateForRun creates the run-scoped queue and returns a Pool bound to it. The
// spec store must already point at a writable bucket/prefix. visibilityTimeout is
// the per-task claim window in seconds — size it above the longest single-task
// runtime so an in-flight task is never redelivered while still running.
func CreateForRun(ctx context.Context, sqsAPI SQSAPI, specs *SpecStore, runID string, visibilityTimeout int) (*Pool, error) {
	q, err := CreateQueue(ctx, sqsAPI, runID, visibilityTimeout)
	if err != nil {
		return nil, err
	}
	return &Pool{RunID: runID, Queue: q, Specs: specs}, nil
}

// CreateForRunWithConfig is the CLI convenience: it builds the SQS + S3 clients
// from an aws.Config, creates the run-scoped queue, and returns a ready Pool. It
// exists so cmd/ can drive a pool WITHOUT importing the SQS/S3 SDK packages
// directly — the AWS-SDK dependency stays inside pkg/ (the #326/#327 cmd-layer
// boundary). specBucket/specPrefix locate staged specs.
func CreateForRunWithConfig(ctx context.Context, cfg awssdk.Config, runID, specBucket, specPrefix string, visibilityTimeout int) (*Pool, error) {
	sqsClient := sqs.NewFromConfig(cfg)
	specs := &SpecStore{Client: s3.NewFromConfig(cfg), Bucket: specBucket, Prefix: specPrefix}
	return CreateForRun(ctx, sqsClient, specs, runID, visibilityTimeout)
}

// OpenForRunWithConfig resolves an EXISTING run-scoped pool from an aws.Config —
// the submit/status/drain path. Like CreateForRunWithConfig, it keeps the SDK
// import inside pkg/.
func OpenForRunWithConfig(ctx context.Context, cfg awssdk.Config, runID, specBucket, specPrefix string) (*Pool, error) {
	q, err := OpenQueue(ctx, sqs.NewFromConfig(cfg), runID)
	if err != nil {
		return nil, err
	}
	specs := &SpecStore{Client: s3.NewFromConfig(cfg), Bucket: specBucket, Prefix: specPrefix}
	return &Pool{RunID: runID, Queue: q, Specs: specs}, nil
}

// Enqueue stages one task's spec to S3 and submits its ref to the queue. This is
// the O(SendMessage) dispatch that replaces O(RunInstances)-per-task: staging +
// one SendMessage, no instance launch. The spec's TaskID names the completion
// record the adapter polls, so the caller needs nothing back but the error.
func (p *Pool) Enqueue(ctx context.Context, spec *taskproto.TaskSpec, specJSON []byte) error {
	if spec.TaskID == "" {
		return fmt.Errorf("taskpool: spec has no task_id")
	}
	uri, err := p.Specs.Stage(ctx, p.RunID, spec.TaskID, specJSON)
	if err != nil {
		return err
	}
	if err := p.Queue.Submit(ctx, TaskRef{TaskID: spec.TaskID, SpecURI: uri}); err != nil {
		return err
	}
	p.logf("enqueued task %s (spec %s)", spec.TaskID, uri)
	return nil
}

// Drain deletes the run-scoped queue. Call it once the run is done (all completion
// records observed). Workers self-terminate on idle-timeout independently, so this
// is the queue-side cleanup, not a worker kill — the tag-reaper is the backstop for
// any worker that outlives its idle window.
func (p *Pool) Drain(ctx context.Context) error {
	p.logf("draining pool queue for run %s", p.RunID)
	return p.Queue.Delete(ctx)
}

// Depth reports the queue's visible + in-flight task counts — used to decide when
// a run has drained (both zero → nothing left to run). Approximate; the
// authoritative per-task signal is each completion record.
func (p *Pool) Depth(ctx context.Context) (visible, inFlight int, err error) {
	return p.Queue.Depth(ctx)
}

func (p *Pool) logf(format string, a ...interface{}) {
	if p.Log == nil {
		return
	}
	fmt.Fprintf(p.Log, "[taskpool] "+format+"\n", a...)
}
