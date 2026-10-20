package tasklaunch

import (
	"context"
	"errors"
)

// objectDeleter is the S3 call clearing a previous attempt's records needs.
type objectDeleter interface {
	DeleteS3Object(ctx context.Context, region, bucket, key string) error
}

// CompletionKey / exitCodeKey name the two per-task result objects the on-instance
// wrapper writes (pkg/taskproto/wrapper.go). Both are keyed by task_id ALONE, so
// both are overwritten by every run of the same task_id — which is why both have
// to be cleared at launch.
//
// The key names are deliberately NOT versioned per run (the issue floats
// completion-<runid>.json): tasks/<task_id>/completion.json is the published
// workflow-adapter contract that six adapter repos poll (nf-spawn, miniwdl-spawn,
// cwl-spawn, snakemake-executor-plugin-spawn, airflow-spawn, pegasus-spawn).
// Renaming it would break all six; the additive run_id field inside the JSON is
// invisible to them.
// CompletionKey is the object key of a task's completion record, relative to the
// results bucket.
//
// taskPrefix is the KEY PORTION of taskproto.EffectiveResultsPrefix, which
// already ends in the task id. It must not be appended again — doing so was
// spawn#715: EffectiveResultsPrefix returns ".../tasks/<task_id>", so a builder
// that added the id produced
//
//	tasks/<task_id>/<task_id>/completion.json
//
// while the writer, which formats the prefix directly, put the object at
//
//	tasks/<task_id>/completion.json
//
// Nothing read a completion record for four releases (v0.117.0 through
// v0.121.0): `task status` reported every finished task as "running" and
// `--wait` polled to TTL and exited 1 on tasks that had succeeded in seconds.
// Both halves — EffectiveResultsPrefix gaining the task id, and these builders
// appending it — arrived in the same commit (#658), so neither looked wrong on
// its own.
//
// The parameter is named taskPrefix, and the task id is no longer a parameter at
// all, so the same mistake cannot be made by passing the wrong thing.
func CompletionKey(taskPrefix string) string {
	return joinTaskKey(taskPrefix, "completion.json")
}

func exitCodeKey(taskPrefix string) string {
	return joinTaskKey(taskPrefix, ".exitcode")
}

// joinTaskKey appends a file name to a task's key prefix. An empty prefix means
// the bucket root, which only arises from a results_prefix of bare "s3://bucket"
// — rare, but it must not produce a leading slash.
func joinTaskKey(taskPrefix, name string) string {
	if taskPrefix == "" {
		return name
	}
	return taskPrefix + "/" + name
}

// staleResultKeys lists every result object a previous run of this task could
// have left behind at a key this run will reuse.
func staleResultKeys(taskPrefix string) []string {
	return []string{CompletionKey(taskPrefix), exitCodeKey(taskPrefix)}
}

// clearStaleCompletion deletes the completion artifacts of any PREVIOUS run of
// taskID, so this run cannot be answered with an earlier attempt's verdict
// (spawn#608). Deleting an absent key is a no-op success in S3, so the normal
// first-run case returns nil without the caller special-casing anything. Errors
// are joined and returned for the caller to log — never to abort a launch on.
// taskPrefix, not (prefix, taskID): the task id is already in the prefix, and a
// vestigial id parameter is how #715 happened — a caller passing the wrong one of
// two plausible things with no compiler or vet complaint.
func clearStaleCompletion(ctx context.Context, store objectDeleter, region, resultsBucket, taskPrefix string) error {
	var errs []error
	for _, key := range staleResultKeys(taskPrefix) {
		if err := store.DeleteS3Object(ctx, region, resultsBucket, key); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
