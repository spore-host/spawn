package taskpool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// A pooled task shares its worker, and the Docker socket is root-equivalent on
// it, so container_docker_socket is refused on a pool (#1231).
const dockerSocketSpecJSON = `{"task_id":"nf-bad","command":["true"],"container":"img","container_docker_socket":true,"resources":{},"lifecycle":{"ttl":"1h"}}`

// countingSpecS3 counts PutObject, so a test can tell whether a spec was staged.
type countingSpecS3 struct{ puts int }

func (c *countingSpecS3) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	c.puts++
	return &s3.PutObjectOutput{}, nil
}
func (c *countingSpecS3) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return nil, errors.New("not used")
}

// Submission refuses it before anything is staged or enqueued.
func TestEnqueue_RefusesDockerSocketBeforeStaging(t *testing.T) {
	f := newFakeSQS(time.Now)
	q, err := CreateQueue(context.Background(), f, "run-1", 30)
	if err != nil {
		t.Fatal(err)
	}
	store := &countingSpecS3{}
	p := &Pool{RunID: "run-1", Queue: q, Specs: &SpecStore{Client: store, Bucket: "b"}}

	spec, err := taskproto.ParseSpec([]byte(dockerSocketSpecJSON))
	if err != nil {
		t.Fatalf("fixture must be a valid one-instance spec: %v", err)
	}
	err = p.Enqueue(context.Background(), spec, []byte(dockerSocketSpecJSON))
	if err == nil || !strings.Contains(err.Error(), "not allowed for a pooled task") {
		t.Fatalf("Enqueue = %v, want the pooled refusal", err)
	}
	if store.puts != 0 || f.liveCount() != 0 {
		t.Errorf("refused spec was staged (%d puts) or enqueued (%d messages)", store.puts, f.liveCount())
	}

	spec.ContainerDockerSocket = false
	if err := p.Enqueue(context.Background(), spec, []byte(`{}`)); err != nil {
		t.Fatalf("Enqueue without the socket: %v", err)
	}
	if store.puts != 1 || f.liveCount() != 1 {
		t.Errorf("spec without the socket: %d puts, %d messages, want 1 and 1", store.puts, f.liveCount())
	}
}

// A spec that reached the queue some other way (an older CLI, a direct write to
// the spec prefix) is refused by the worker too: never run, recorded as
// spec_error before the ack, exactly like an unparseable spec (#1149).
func TestWorker_RefusesDockerSocketSpec(t *testing.T) {
	results := &fakeResults{objects: map[string]string{}, liveAtPut: map[string]int{}}
	f, ex := runOneTask(t, dockerSocketSpecJSON, results)
	if ex.calls != 0 {
		t.Errorf("execer ran %d time(s) for a container_docker_socket spec on a pool", ex.calls)
	}
	body, ok := results.objects[wantDefaultRecord]
	if !ok {
		t.Fatalf("no completion record at %s; wrote %v", wantDefaultRecord, keys(results.objects))
	}
	rec, err := taskproto.ParseCompletionRecord([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != taskproto.StateFailed || rec.RetryClass != taskproto.RetrySpecError {
		t.Errorf("record = %+v, want failed / spec_error", rec)
	}
	if results.liveAtPut[wantDefaultRecord] != 1 || f.liveCount() != 0 {
		t.Errorf("record must be written before the ack, and the task acked")
	}
}
