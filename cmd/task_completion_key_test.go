package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// TestReaderAndWriterAgreeOnTheCompletionKey is the spawn#715 gate, and it is a
// round-trip rather than a restatement of the code.
//
// The writer never builds a key: it formats the resolved prefix directly,
//
//	fmt.Fprintf(out, "Completion:   %s/completion.json\n", resultsPrefix)
//
// so the printed URI IS the contract. The reader built a key from the same
// prefix with completionKey(). Those two must produce the same object, and for
// four releases they did not — EffectiveResultsPrefix already ends in the task
// id and completionKey appended it again:
//
//	writer:  tasks/<task_id>/completion.json
//	reader:  tasks/<task_id>/<task_id>/completion.json
//
// Asserting completionKey() returns some expected string would not have caught
// that; only comparing it against the writer's own path does.
func TestReaderAndWriterAgreeOnTheCompletionKey(t *testing.T) {
	cases := []struct {
		name          string
		resultsPrefix string // spec.ResultsPrefix ("" = spawn's default bucket)
	}{
		{"spawn's default results bucket", ""},
		{"a caller's own prefix", "s3://my-bucket/my-records"},
		{"a bare bucket", "s3://my-bucket"},
		{"a prefix with a trailing slash", "s3://my-bucket/records/"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const taskID = "cookbook-polish-align-r2"
			spec := &taskproto.TaskSpec{TaskID: taskID, ResultsPrefix: tc.resultsPrefix}
			fullPrefix := taskproto.EffectiveResultsPrefix(spec, "942542972736", "us-west-2")

			// What the writer tells the user — and what spored actually writes.
			writerURI := fullPrefix + "/completion.json"

			// What the reader goes and fetches.
			bucket, taskPrefix, ok := taskproto.SplitS3URI(fullPrefix)
			if !ok {
				t.Fatalf("SplitS3URI(%q) failed", fullPrefix)
			}
			readerURI := fmt.Sprintf("s3://%s/%s", bucket, completionKey(taskPrefix))

			if readerURI != writerURI {
				t.Errorf("reader and writer disagree on the completion record's location:\n"+
					"  writer prints/writes: %s\n  reader fetches:        %s\n\n"+
					"Whichever is wrong, they cannot differ: `task status` reports every "+
					"finished task as running and `--wait` polls to TTL and exits 1 on a task "+
					"that succeeded (#715).", writerURI, readerURI)
			}
		})
	}
}

// TestCompletionKeyIsThePublishedAdapterContract: tasks/<task_id>/completion.json
// is polled by six adapter repos (nf-spawn, miniwdl-spawn, cwl-spawn,
// snakemake-executor-plugin-spawn, airflow-spawn, pegasus-spawn). Moving it
// breaks all six, so the default must stay exactly this shape.
func TestCompletionKeyIsThePublishedAdapterContract(t *testing.T) {
	const taskID = "my-task"
	spec := &taskproto.TaskSpec{TaskID: taskID}
	full := taskproto.EffectiveResultsPrefix(spec, "111122223333", "us-east-1")
	_, taskPrefix, ok := taskproto.SplitS3URI(full)
	if !ok {
		t.Fatal("SplitS3URI failed on the default prefix")
	}

	got := completionKey(taskPrefix)
	want := "tasks/" + taskID + "/completion.json"
	if got != want {
		t.Errorf("completion key = %q, want %q — six adapter repos poll this exact path", got, want)
	}
	// Specifically: the task id must appear ONCE.
	if n := strings.Count(got, taskID); n != 1 {
		t.Errorf("task id appears %d times in %q; doubling it is #715", n, got)
	}
}
