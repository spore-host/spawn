package taskpool

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// ---- fakes -----------------------------------------------------------------

// fakeSQS is an in-memory SQS with just enough visibility-timeout behavior to
// drive the worker loop deterministically. A received message is hidden until
// its (test-controlled) redelivery time; Delete removes it. Time is injected so
// tests don't sleep.
type fakeSQS struct {
	mu           sync.Mutex
	msgs         map[string]*fakeMsg // receiptHandle-independent: keyed by internal id
	nextID       int
	now          func() time.Time
	sendErr      error
	recvErr      error
	visDur       time.Duration
	getURLMisses int  // return NonExistentQueue this many times before resolving
	queueDeleted bool // DeleteQueue ran: every queue call now fails NonExistentQueue
	receives     int  // ReceiveMessage calls
	getURLs      int  // GetQueueUrl calls
}

type fakeMsg struct {
	id           string
	body         string
	invisibleTil time.Time // before now → claimable
	deleted      bool
}

func newFakeSQS(now func() time.Time) *fakeSQS {
	return &fakeSQS{msgs: map[string]*fakeMsg{}, now: now, visDur: 30 * time.Second}
}

func (f *fakeSQS) CreateQueue(_ context.Context, in *sqs.CreateQueueInput, _ ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error) {
	return &sqs.CreateQueueOutput{QueueUrl: aws.String("mem://" + aws.ToString(in.QueueName))}, nil
}
func (f *fakeSQS) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getURLs++
	if f.queueDeleted {
		return nil, &types.QueueDoesNotExist{}
	}
	// Simulate the create→GetQueueUrl consistency window: return NonExistentQueue
	// for the first getURLMisses calls, then resolve.
	if f.getURLMisses > 0 {
		f.getURLMisses--
		return nil, &types.QueueDoesNotExist{}
	}
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String("mem://" + aws.ToString(in.QueueName))}, nil
}
func (f *fakeSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("m%d", f.nextID)
	f.msgs[id] = &fakeMsg{id: id, body: aws.ToString(in.MessageBody)}
	return &sqs.SendMessageOutput{MessageId: aws.String(id)}, nil
}
func (f *fakeSQS) ReceiveMessage(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	f.receives++
	deleted := f.queueDeleted
	f.mu.Unlock()
	if deleted {
		return nil, &types.QueueDoesNotExist{}
	}
	if f.recvErr != nil {
		return nil, f.recvErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	for _, m := range f.msgs {
		if m.deleted || m.invisibleTil.After(now) {
			continue
		}
		m.invisibleTil = now.Add(f.visDur)
		// ReceiptHandle encodes the internal id so Delete can find it.
		return &sqs.ReceiveMessageOutput{Messages: []types.Message{{
			Body: aws.String(m.body), ReceiptHandle: aws.String(m.id),
		}}}, nil
	}
	return &sqs.ReceiveMessageOutput{}, nil
}
func (f *fakeSQS) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.msgs[aws.ToString(in.ReceiptHandle)]; ok {
		m.deleted = true
	}
	return &sqs.DeleteMessageOutput{}, nil
}
func (f *fakeSQS) GetQueueAttributes(_ context.Context, _ *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vis, inflight := 0, 0
	now := f.now()
	for _, m := range f.msgs {
		if m.deleted {
			continue
		}
		if m.invisibleTil.After(now) {
			inflight++
		} else {
			vis++
		}
	}
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{
		string(types.QueueAttributeNameApproximateNumberOfMessages):           fmt.Sprintf("%d", vis),
		string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible): fmt.Sprintf("%d", inflight),
	}}, nil
}
func (f *fakeSQS) DeleteQueue(_ context.Context, _ *sqs.DeleteQueueInput, _ ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queueDeleted {
		return nil, &types.QueueDoesNotExist{}
	}
	f.queueDeleted = true
	return &sqs.DeleteQueueOutput{}, nil
}

// liveCount is the number of not-deleted messages (for assertions).
func (f *fakeSQS) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.msgs {
		if !m.deleted {
			n++
		}
	}
	return n
}

// fakeFetcher returns a fixed spec JSON regardless of URI.
type fakeFetcher struct {
	byURI map[string][]byte
	err   error
}

func (f fakeFetcher) Fetch(_ context.Context, uri string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if b, ok := f.byURI[uri]; ok {
		return b, nil
	}
	return []byte(`{"task_id":"x"}`), nil
}

// recordingExecer records the workspace dir each task ran in and returns a
// scripted exit code / error per task id.
type recordingExecer struct {
	mu       sync.Mutex
	calls    int
	exitFor  func(call int) (int, error)
	execedIn []string
}

func (e *recordingExecer) Exec(_ context.Context, _ []byte, dir string) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	call := e.calls
	e.calls++
	e.execedIn = append(e.execedIn, dir)
	if e.exitFor != nil {
		return e.exitFor(call)
	}
	return 0, nil
}

// countingWorkspace hands out a unique dir per task and records resets, so a test
// can assert Reset is always called (workspace isolation on reuse).
type countingWorkspace struct {
	mu       sync.Mutex
	acquired int
	resets   []string
	acquErr  error
}

func (c *countingWorkspace) Acquire(taskID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.acquErr != nil {
		return "", c.acquErr
	}
	c.acquired++
	return fmt.Sprintf("/ws/%s-%d", taskID, c.acquired), nil
}
func (c *countingWorkspace) Reset(dir string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resets = append(c.resets, dir)
	return nil
}
func (c *countingWorkspace) resetCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.resets)
}

// ---- tests -----------------------------------------------------------------

// clock is a manual clock the worker reads via w.now; ReceiveMessage also reads
// it, so we advance it deliberately to drive the idle-timeout path.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// drainClock advances the injected clock forward until the worker goroutine
// signals done (closed channel), or fails after 5s wall time. Continuous advance
// makes the idle-timeout fire regardless of when the worker last reset its
// deadline — no ordering race with the goroutine.
func drainClock(t *testing.T, clk *clock, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		clk.add(2 * time.Second)
		select {
		case <-done:
			return true
		default:
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func newWorker(t *testing.T, f *fakeSQS, ex TaskExecer, ws WorkspaceProvider, clk *clock, idle time.Duration) *Worker {
	t.Helper()
	q, err := CreateQueue(context.Background(), f, "run-1", 30)
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return &Worker{
		Queue:     q,
		Fetcher:   fakeFetcher{},
		Execer:    ex,
		Workspace: ws,
		Config:    WorkerConfig{PollWaitSeconds: 0, IdleTimeout: idle},
		now:       clk.now,
	}
}

// TestWorker_DrainsAllTasks: N tasks enqueued, one worker drains them all, acks
// each (queue empties), resets the workspace after every task, and then self-
// exits once idle past the deadline.
func TestWorker_DrainsAllTasks(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSQS(clk.now)
	ex := &recordingExecer{}
	ws := &countingWorkspace{}
	w := newWorker(t, f, ex, ws, clk, 10*time.Second)

	for i := 0; i < 5; i++ {
		if err := w.Queue.Submit(context.Background(), TaskRef{TaskID: fmt.Sprintf("nf-%d", i), SpecURI: "s3://b/specs/x.json"}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	// Advance the clock past the idle deadline only AFTER the queue empties: the
	// execer advances the clock a hair per task so the deadline keeps resetting
	// while work exists, then a final jump triggers drain.
	ex.exitFor = func(call int) (int, error) {
		clk.add(1 * time.Second)
		return 0, nil
	}

	// Run in a goroutine; push the clock past idle once tasks are done.
	done := make(chan struct{})
	var executed int
	var runErr error
	go func() {
		executed, runErr = w.Run(context.Background())
		close(done)
	}()

	// Wait for the queue to drain, then keep advancing time past the idle timeout
	// until the worker self-exits. Continuous advance (not a single jump) avoids
	// racing the loop's idleDeadline reset after the last task.
	if !drainClock(t, clk, done) {
		t.Fatal("worker did not drain within 5s")
	}

	if runErr != nil {
		t.Fatalf("Run error: %v", runErr)
	}
	if executed != 5 {
		t.Fatalf("executed = %d, want 5", executed)
	}
	if f.liveCount() != 0 {
		t.Fatalf("queue not empty: %d messages remain", f.liveCount())
	}
	if ws.resetCount() != 5 {
		t.Fatalf("workspace resets = %d, want 5 (one per task, always)", ws.resetCount())
	}
	// Each task ran in a distinct workspace dir (isolation on reuse).
	seen := map[string]bool{}
	for _, d := range ex.execedIn {
		if seen[d] {
			t.Fatalf("workspace dir %q reused across tasks (not isolated)", d)
		}
		seen[d] = true
	}
}

// TestWorker_FailedTaskIsAckedNotRequeued: a task whose exec reports a non-zero
// exit (a legitimate task failure that still wrote a completion record) is acked,
// not spun on. The pool drains; task-success adjudication is the submitter's job.
func TestWorker_FailedTaskIsAckedNotRequeued(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSQS(clk.now)
	ex := &recordingExecer{exitFor: func(call int) (int, error) { return 42, nil }}
	ws := &countingWorkspace{}
	w := newWorker(t, f, ex, ws, clk, 5*time.Second)

	_ = w.Queue.Submit(context.Background(), TaskRef{TaskID: "nf-fail", SpecURI: "s3://b/x.json"})

	done := make(chan struct{})
	var executed int
	go func() { executed, _ = w.Run(context.Background()); close(done) }()

	if !drainClock(t, clk, done) {
		t.Fatal("worker did not drain")
	}
	if executed != 1 {
		t.Fatalf("executed = %d, want 1", executed)
	}
	if f.liveCount() != 0 {
		t.Fatalf("failed task was not acked: %d remain", f.liveCount())
	}
	if ws.resetCount() != 1 {
		t.Fatalf("workspace not reset after failed task: resets = %d", ws.resetCount())
	}
}

// TestWorker_WorkspaceAcquireFailureLeavesForRedelivery: if a worker can't get a
// clean workspace it must NOT ack — the task stays on the queue to redeliver to a
// healthier worker. Models the "reconcile replaces reaped workers" retry at the
// task level.
func TestWorker_WorkspaceAcquireFailureLeavesForRedelivery(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSQS(clk.now)
	ex := &recordingExecer{}
	ws := &countingWorkspace{acquErr: fmt.Errorf("disk full")}
	w := newWorker(t, f, ex, ws, clk, time.Second)

	_ = w.Queue.Submit(context.Background(), TaskRef{TaskID: "nf-x", SpecURI: "s3://b/x.json"})

	// Run once-ish: cancel after a short spin so we can inspect state. The task
	// should NOT have been executed or acked.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = w.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if ex.calls != 0 {
		t.Fatalf("exec ran %d times despite workspace failure; want 0", ex.calls)
	}
	// Message is either visible or in-flight (invisible), but NOT deleted.
	if f.liveCount() != 1 {
		t.Fatalf("task should remain for redelivery; liveCount = %d", f.liveCount())
	}
}

// TestWorker_IdleDrain: an empty queue drains the worker after IdleTimeout with
// zero tasks executed — the scale-to-zero path when a worker comes up but there's
// nothing (or no longer anything) to do.
func TestWorker_IdleDrain(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSQS(clk.now)
	w := newWorker(t, f, &recordingExecer{}, &countingWorkspace{}, clk, 10*time.Second)

	done := make(chan struct{})
	var executed int
	go func() { executed, _ = w.Run(context.Background()); close(done) }()

	if !drainClock(t, clk, done) {
		t.Fatal("idle worker did not drain")
	}
	if executed != 0 {
		t.Fatalf("executed = %d on empty queue, want 0", executed)
	}
}

// TestOpenQueue_RetriesNonExistentQueue: a worker that boots and resolves the
// run queue can race the submitter's CreateQueue (GetQueueUrl is eventually
// consistent), seeing NonExistentQueue for a short window. OpenQueue must retry
// through that window rather than fail — the #70 re-smoke bug where a worker
// exited at boot because its first GetQueueUrl 400'd seconds after create.
func TestOpenQueue_RetriesNonExistentQueue(t *testing.T) {
	old := openQueueRetryDelay
	openQueueRetryDelay = time.Millisecond // don't sleep 3s in a unit test
	defer func() { openQueueRetryDelay = old }()

	f := newFakeSQS(func() time.Time { return time.Unix(1_700_000_000, 0) })
	f.getURLMisses = 3 // 3 NonExistentQueue responses, then resolve

	q, err := OpenQueue(context.Background(), f, "run-1")
	if err != nil {
		t.Fatalf("OpenQueue should retry through NonExistentQueue, got: %v", err)
	}
	if q.URL() == "" {
		t.Fatal("resolved queue has no URL")
	}
}

// TestOpenQueue_GivesUpAfterBudget: a queue that never appears within the attempt
// budget returns an error (fails loud), not a silent success.
func TestOpenQueue_GivesUpAfterBudget(t *testing.T) {
	old := openQueueRetryDelay
	openQueueRetryDelay = time.Millisecond
	defer func() { openQueueRetryDelay = old }()

	f := newFakeSQS(func() time.Time { return time.Unix(1_700_000_000, 0) })
	f.getURLMisses = openQueueMaxAttempts + 5 // never resolves within budget

	if _, err := OpenQueue(context.Background(), f, "run-1"); err == nil {
		t.Fatal("OpenQueue should error when the queue never appears within the budget")
	}
}

// countingSleep replaces the worker's claim-error backoff: it counts the
// retries and advances the fake clock by the backoff instead of waiting. It
// cancels ctx after limit retries, so a worker that never stops retrying ends
// the test (and fails it) rather than hanging it.
func countingSleep(clk *clock, cancel context.CancelFunc, limit int, n *int) func(context.Context, time.Duration) bool {
	return func(ctx context.Context, d time.Duration) bool {
		*n++
		clk.add(d)
		if *n >= limit {
			cancel()
			return false
		}
		return ctx.Err() == nil
	}
}

// TestWorker_QueueDeletedDrainsAtOnce is #1146: after `spawn pool drain`
// deletes the queue, the worker used to retry every claim error once a second
// until its TTL. A deleted queue never comes back, so the worker must drain on
// the first NonExistentQueue — one ReceiveMessage, zero retries — and return
// nil, which is what lets the bootstrap write the completion record.
func TestWorker_QueueDeletedDrainsAtOnce(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSQS(clk.now)
	w := newWorker(t, f, &recordingExecer{}, &countingWorkspace{}, clk, time.Hour)
	if _, err := DrainRun(context.Background(), f, "run-1"); err != nil {
		t.Fatalf("DrainRun: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retries := 0
	w.sleep = countingSleep(clk, cancel, 5, &retries)

	_, err := w.Run(ctx)
	if err != nil {
		t.Fatalf("Run on a deleted queue returned %v, want nil (a clean drain)", err)
	}
	if retries != 0 || f.receives != 1 {
		t.Errorf("worker retried a deleted queue: %d retries, %d ReceiveMessage calls, want 0 and 1", retries, f.receives)
	}
}

// TestWorker_ClaimErrorsHonourTheIdleDeadline is #1146's second half: a worker
// whose every claim fails (throttle, network, denied) is idle, and must drain
// once IdleTimeout has passed rather than retry until TTL. The backoff advances
// the clock by 1s per retry, so a 10s idle window allows about 10 retries; the
// counting sleep's limit of 50 is what ends a worker that ignores the deadline.
func TestWorker_ClaimErrorsHonourTheIdleDeadline(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSQS(clk.now)
	f.recvErr = fmt.Errorf("ThrottlingException: slow down")
	w := newWorker(t, f, &recordingExecer{}, &countingWorkspace{}, clk, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retries := 0
	w.sleep = countingSleep(clk, cancel, 50, &retries)

	_, err := w.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned %v after only claim errors for the idle window, want nil (drain)", err)
	}
	if retries < 1 || retries > 11 {
		t.Errorf("worker retried %d times across a 10s idle window with a 1s backoff, want 1..11", retries)
	}
}

// TestWorker_TransientClaimErrorIsRetried: the deadline must not turn one
// throttle into a drain — a worker with idle time left retries and goes on to
// run the task.
func TestWorker_TransientClaimErrorIsRetried(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSQS(clk.now)
	ex := &recordingExecer{}
	w := newWorker(t, f, ex, &countingWorkspace{}, clk, 10*time.Second)
	if err := w.Queue.Submit(context.Background(), TaskRef{TaskID: "t1", SpecURI: "s3://b/x.json"}); err != nil {
		t.Fatal(err)
	}
	f.recvErr = fmt.Errorf("ThrottlingException")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retries := 0
	w.sleep = func(ctx context.Context, d time.Duration) bool {
		retries++
		f.mu.Lock()
		f.recvErr = nil // the throttle clears after one retry
		f.mu.Unlock()
		return true
	}
	ex.exitFor = func(int) (int, error) {
		clk.add(time.Hour) // past idle once the task ran, so the next empty poll drains
		return 0, nil
	}
	executed, err := w.Run(ctx)
	if err != nil || executed != 1 || retries != 1 {
		t.Errorf("Run = (%d, %v) after %d retries, want (1, nil) after 1", executed, err, retries)
	}
}

// TestDrainRun_Idempotent is #1183: a second drain of the same pool reports
// already-drained with a nil error after ONE lookup (it used to retry the
// missing queue for ~30s and fail).
func TestDrainRun_Idempotent(t *testing.T) {
	f := newFakeSQS(time.Now)
	gone, err := DrainRun(context.Background(), f, "run-1")
	if err != nil || gone {
		t.Fatalf("first drain = (gone %v, %v), want (false, nil)", gone, err)
	}
	before := f.getURLs
	gone, err = DrainRun(context.Background(), f, "run-1")
	if err != nil || !gone {
		t.Fatalf("second drain = (gone %v, %v), want (true, nil)", gone, err)
	}
	if n := f.getURLs - before; n != 1 {
		t.Errorf("second drain made %d GetQueueUrl calls, want 1 (no retry on a deleted queue)", n)
	}
}

// TestDrainRun_OtherErrorsStillFail: idempotence is for a missing queue only.
func TestDrainRun_OtherErrorsStillFail(t *testing.T) {
	gone, err := DrainRun(context.Background(), errSQS{fmt.Errorf("AccessDenied")}, "run-1")
	if err == nil || gone {
		t.Fatalf("DrainRun with a non-NonExistentQueue error = (gone %v, %v), want an error", gone, err)
	}
}

type errSQS struct{ err error }

func (e errSQS) CreateQueue(context.Context, *sqs.CreateQueueInput, ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error) {
	return nil, e.err
}
func (e errSQS) GetQueueUrl(context.Context, *sqs.GetQueueUrlInput, ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	return nil, e.err
}
func (e errSQS) SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	return nil, e.err
}
func (e errSQS) ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	return nil, e.err
}
func (e errSQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	return nil, e.err
}
func (e errSQS) GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	return nil, e.err
}
func (e errSQS) DeleteQueue(context.Context, *sqs.DeleteQueueInput, ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error) {
	return nil, e.err
}
