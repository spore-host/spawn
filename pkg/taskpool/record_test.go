package taskpool

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// fakeResults is an in-memory results bucket: it records every PutObject by
// s3:// URI, and how many messages were still on the queue at the moment of the
// write, so a test can tell a record written BEFORE the ack from one after.
type fakeResults struct {
	mu        sync.Mutex
	objects   map[string]string
	liveAtPut map[string]int
	queue     *fakeSQS
	err       error
}

func (r *fakeResults) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if r.err != nil {
		return nil, r.err
	}
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	live := r.queue.liveCount()
	r.mu.Lock()
	defer r.mu.Unlock()
	uri := "s3://" + aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	r.objects[uri] = string(b)
	r.liveAtPut[uri] = live
	return &s3.PutObjectOutput{}, nil
}

func runOneTask(t *testing.T, spec string, results *fakeResults) (*fakeSQS, *recordingExecer) {
	t.Helper()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSQS(clk.now)
	if results != nil {
		results.queue = f
	}
	ex := &recordingExecer{}
	w := newWorker(t, f, ex, &countingWorkspace{}, clk, 5*time.Second)
	w.Fetcher = fakeFetcher{byURI: map[string][]byte{"s3://b/specs/bad.json": []byte(spec)}}
	if results != nil {
		w.Results = results
	}
	w.AccountID = "123456789012"
	w.Region = "us-west-2"

	if err := w.Queue.Submit(context.Background(), TaskRef{TaskID: "nf-bad", SpecURI: "s3://b/specs/bad.json"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	done := make(chan struct{})
	go func() { _, _ = w.Run(context.Background()); close(done) }()
	if !drainClock(t, clk, done) {
		t.Fatal("worker did not drain")
	}
	return f, ex
}

// The published adapter contract (cmd/task.go, `task status`):
// s3://spawn-results-<account>-<region>/tasks/<task_id>/completion.json.
const wantDefaultRecord = "s3://spawn-results-123456789012-us-west-2/tasks/nf-bad/completion.json"

// TestWorker_UnparseableSpecWritesFailedRecordBeforeAck is the #1149 gate: a
// spec that fails to parse used to be acked with no completion record, so
// `spawn task status` reported the task unknown forever. The worker must write
// a failed, non-retryable record at the key `task status` reads, and do so
// before it acks.
func TestWorker_UnparseableSpecWritesFailedRecordBeforeAck(t *testing.T) {
	cases := map[string]string{
		"malformed JSON":     `{not json`,
		"fails validation":   `{"task_id":"nf-bad"}`,
		"unknown spec field": `{"task_id":"nf-bad","command":["true"],"cpus":4}`,
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := taskproto.ParseSpec([]byte(spec)); err == nil {
				t.Fatalf("fixture %q parses; it must not", spec)
			}
			results := &fakeResults{objects: map[string]string{}, liveAtPut: map[string]int{}}
			f, ex := runOneTask(t, spec, results)

			if ex.calls != 0 {
				t.Errorf("execer ran %d time(s) for an unparseable spec", ex.calls)
			}
			body, ok := results.objects[wantDefaultRecord]
			if !ok {
				t.Fatalf("no completion record at %s; wrote %v", wantDefaultRecord, keys(results.objects))
			}
			rec, err := taskproto.ParseCompletionRecord([]byte(body))
			if err != nil {
				t.Fatalf("record does not parse: %v", err)
			}
			if rec.TaskID != "nf-bad" || rec.State != taskproto.StateFailed || rec.ExitCode != -1 {
				t.Errorf("record = %+v, want task nf-bad, state failed, exit -1", rec)
			}
			if rec.RetryClass != "spec_error" {
				t.Errorf("retry_class = %q, want spec_error", rec.RetryClass)
			}
			if rec.RetryClass.Retryable() {
				t.Error("spec_error is retryable; the same bytes fail on every worker")
			}
			if rec.RunID == "" {
				t.Error("record has no run_id")
			}
			exitURI := strings.TrimSuffix(wantDefaultRecord, "completion.json") + ".exitcode"
			if got := results.objects[exitURI]; got != "-1" {
				t.Errorf(".exitcode = %q, want -1", got)
			}
			if live := results.liveAtPut[wantDefaultRecord]; live != 1 {
				t.Errorf("record written with %d message(s) on the queue, want 1 (written before the ack)", live)
			}
			if f.liveCount() != 0 {
				t.Errorf("unparseable task not acked: %d remain", f.liveCount())
			}
		})
	}
}

// TestWorker_UnparseableSpecHonoursResultsPrefix: a spec that is valid JSON but
// fails validation can still say where its records go (spawn#646), and a
// `task status --results-prefix` caller looks there.
func TestWorker_UnparseableSpecHonoursResultsPrefix(t *testing.T) {
	results := &fakeResults{objects: map[string]string{}, liveAtPut: map[string]int{}}
	runOneTask(t, `{"task_id":"nf-bad","results_prefix":"s3://mine/runs/7/"}`, results)
	if _, ok := results.objects["s3://mine/runs/7/nf-bad/completion.json"]; !ok {
		t.Fatalf("record not under the spec's results_prefix; wrote %v", keys(results.objects))
	}
}

// TestWorker_UnparseableSpecAckedEvenIfRecordWriteFails: leaving the message
// would redeliver it every visibility timeout, resetting the idle clock, so a
// pool with one bad spec would never drain.
func TestWorker_UnparseableSpecAckedEvenIfRecordWriteFails(t *testing.T) {
	results := &fakeResults{err: errors.New("AccessDenied")}
	f, _ := runOneTask(t, `{not json`, results)
	if f.liveCount() != 0 {
		t.Fatalf("task left on the queue after a failed record write: %d remain", f.liveCount())
	}
}

// TestFakeFetcherDefaultParses guards the other tests' meaning: the worker skips
// the execer for a spec that does not parse, so a default fixture that failed
// ParseSpec would make every execer assertion vacuous.
func TestFakeFetcherDefaultParses(t *testing.T) {
	b, _ := fakeFetcher{}.Fetch(context.Background(), "s3://any")
	if _, err := taskproto.ParseSpec(b); err != nil {
		t.Fatalf("fakeFetcher default spec does not parse: %v", err)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
