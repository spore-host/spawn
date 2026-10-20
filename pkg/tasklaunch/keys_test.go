package tasklaunch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

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

	c, e := CompletionKey(taskPrefix), exitCodeKey(taskPrefix)
	cDir := c[:strings.LastIndex(c, "/")]
	eDir := e[:strings.LastIndex(e, "/")]
	if cDir != eDir {
		t.Errorf("completion.json is in %q but .exitcode is in %q", cDir, eDir)
	}
}

// TestClearStaleCompletion_DeletesBothKeys: the pre-launch clear must cover BOTH
// per-task result objects the wrapper writes — completion.json AND the sibling
// .exitcode — since both are keyed by task_id alone and both are re-read by
// consumers.
func TestClearStaleCompletion_DeletesBothKeys(t *testing.T) {
	store := &fakeResultStore{}
	// The prefix includes the task id, because that is what
	// EffectiveResultsPrefix returns and what both production callers pass. This
	// test previously passed a bare "tasks", encoding a calling convention no
	// caller used — the ambiguity that produced #715.
	if err := clearStaleCompletion(context.Background(), store, "us-west-2", "spawn-results-1-us-west-2", "tasks/cookbook-plink-r1"); err != nil {
		t.Fatalf("clearStaleCompletion: %v", err)
	}
	want := []string{"tasks/cookbook-plink-r1/completion.json", "tasks/cookbook-plink-r1/.exitcode"}
	if len(store.deleted) != len(want) {
		t.Fatalf("deleted %v, want %v", store.deleted, want)
	}
	for i, k := range want {
		if store.deleted[i] != k {
			t.Errorf("deleted[%d] = %q, want %q", i, store.deleted[i], k)
		}
	}
}

// TestClearStaleCompletion_ErrorIsReportedNotSwallowed: a delete failure surfaces
// to the caller (which logs it and launches anyway — the run-id check is the
// suspenders), and every key is still attempted rather than aborting on the first.
func TestClearStaleCompletion_ErrorIsReportedNotSwallowed(t *testing.T) {
	store := &fakeResultStore{delErr: errors.New("AccessDenied")}
	err := clearStaleCompletion(context.Background(), store, "us-east-1", "b", "tasks/t")
	if err == nil {
		t.Fatal("expected the delete error to be returned for logging")
	}
	if len(store.deleted) != 2 {
		t.Errorf("attempted %d deletes, want both keys attempted even after a failure", len(store.deleted))
	}
}
