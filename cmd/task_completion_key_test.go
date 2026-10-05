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

// TestStaleResultKeysTargetTheObjectsThatExist is the consequence of #715 that
// the bug report could not see.
//
// clearStaleCompletion deletes a previous run's records so #608's protection
// holds — a stale record must not answer this run. It used the same broken
// builders, so it had been deleting keys that do not exist: deleting an absent
// key is a no-op success in S3, so it reported success and removed nothing, and
// #608's guard has been inert since v0.117.0.
//
// Fixing only the read would therefore have resurrected #608. These must target
// the same objects the writer produces.
func TestStaleResultKeysTargetTheObjectsThatExist(t *testing.T) {
	const taskID = "reused-task-id"
	spec := &taskproto.TaskSpec{TaskID: taskID}
	full := taskproto.EffectiveResultsPrefix(spec, "111122223333", "us-east-1")
	_, taskPrefix, _ := taskproto.SplitS3URI(full)

	keys := staleResultKeys(taskPrefix)
	if len(keys) != 2 {
		t.Fatalf("staleResultKeys returned %d keys, want completion.json and .exitcode", len(keys))
	}
	for _, k := range keys {
		if strings.Count(k, taskID) != 1 {
			t.Errorf("stale key %q does not name the task id exactly once, so the delete "+
				"targets an object that never existed and #608's guard silently does nothing", k)
		}
		if !strings.HasPrefix(k, "tasks/"+taskID+"/") {
			t.Errorf("stale key %q is not under the task's own prefix", k)
		}
	}
}

// TestExitCodeKeySitsBesideTheCompletionRecord: both are written by the same
// wrapper into the same directory, so they must resolve to the same prefix.
func TestExitCodeKeySitsBesideTheCompletionRecord(t *testing.T) {
	spec := &taskproto.TaskSpec{TaskID: "t1"}
	full := taskproto.EffectiveResultsPrefix(spec, "111122223333", "us-east-1")
	_, taskPrefix, _ := taskproto.SplitS3URI(full)

	c, e := completionKey(taskPrefix), exitCodeKey(taskPrefix)
	cDir := c[:strings.LastIndex(c, "/")]
	eDir := e[:strings.LastIndex(e, "/")]
	if cDir != eDir {
		t.Errorf("completion.json is in %q but .exitcode is in %q", cDir, eDir)
	}
}
