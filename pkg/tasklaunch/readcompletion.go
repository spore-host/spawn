package tasklaunch

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// ErrNoCompletionRecord is returned, wrapped, by ReadCompletion and
// ReadCompletionAt when the task's completion record does not exist (yet). Test
// for it with errors.Is. It is "no record", not "the task is running": an
// instance killed without spored's involvement never writes one (spawn#802), so
// a caller that cares must ask EC2 what became of the instance.
var ErrNoCompletionRecord = errors.New("no completion record")

// ErrEmptyCompletionRecord is returned, wrapped, by ReadCompletion and
// ReadCompletionAt when the completion record exists but is empty (spawn#1179).
// The task did end: the wrapper writes the record as its last step. But a
// wrapper older than the #1179 fix wrote it to the instance's /tmp first, and
// a task that filled that disk left an empty file, which was uploaded as-is.
// What happened is in the task's command.log, which the error names.
var ErrEmptyCompletionRecord = errors.New("completion record is empty")

// CompletionGetter is the S3 read ReadCompletionAt needs. *aws.Client satisfies
// it; GetS3Object must wrap aws.ErrS3NoSuchKey for a missing object, as
// *aws.Client's does.
type CompletionGetter interface {
	GetS3Object(ctx context.Context, region, bucket, key string) ([]byte, error)
}

// CompletionStore is what ReadCompletion needs: the S3 read, and the account id
// spawn's default results bucket is named after. *aws.Client satisfies it.
type CompletionStore interface {
	CompletionGetter
	GetAccountID(ctx context.Context) (string, error)
}

// ReadCompletion fetches and parses taskID's completion record, from where the
// launch told the wrapper to write it: taskproto.EffectiveResultsPrefix, that is
// s3://spawn-results-<account>-<region>/tasks/<task_id> unless resultsPrefix is
// set. resultsPrefix is the spec's results_prefix ("" for spawn's default), the
// value `spawn task status --results-prefix` takes; a task whose spec set one
// can only be found by passing it.
//
// uri is the completion record's s3:// location whenever it could be resolved,
// including when err wraps ErrNoCompletionRecord, so a caller can say where it
// looked. An empty record wraps ErrEmptyCompletionRecord. Every other error is
// a real failure: the account id could not be resolved, resultsPrefix is not an
// s3://bucket/prefix URI, the read failed, or the record does not parse.
//
// This is the one reader of completion records (spawn#1260). `spawn task status`
// and the MCP server both call it, so the key the reader builds cannot drift
// from the writer's again (spawn#715).
func ReadCompletion(ctx context.Context, store CompletionStore, taskID, resultsPrefix, region string) (rec *taskproto.CompletionRecord, uri string, err error) {
	account, err := store.GetAccountID(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("resolve account id: %w", err)
	}
	full := taskproto.EffectiveResultsPrefix(&taskproto.TaskSpec{TaskID: taskID, ResultsPrefix: resultsPrefix}, account, region)
	bucket, taskPrefix, ok := taskproto.SplitS3URI(full)
	if !ok {
		return nil, "", fmt.Errorf("results_prefix %q is not an s3://bucket/prefix URI", resultsPrefix)
	}
	return ReadCompletionAt(ctx, store, region, bucket, taskPrefix)
}

// ReadCompletionAt is ReadCompletion for a caller that already holds the
// resolved location, as `spawn task run --wait` does from its launch:
// resultsBucket and taskPrefix are the two halves of
// taskproto.SplitS3URI(EffectiveResultsPrefix(...)), so taskPrefix already ends
// in the task id (see CompletionKey). Errors are as for ReadCompletion.
func ReadCompletionAt(ctx context.Context, store CompletionGetter, region, resultsBucket, taskPrefix string) (rec *taskproto.CompletionRecord, uri string, err error) {
	key := CompletionKey(taskPrefix)
	uri = "s3://" + resultsBucket + "/" + key
	data, err := store.GetS3Object(ctx, region, resultsBucket, key)
	if errors.Is(err, aws.ErrS3NoSuchKey) {
		return nil, uri, fmt.Errorf("%w at %s", ErrNoCompletionRecord, uri)
	}
	if err != nil {
		return nil, uri, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, uri, fmt.Errorf("%w at %s (the instance's disk was probably full when the task ended); see the task's log at s3://%s/%s",
			ErrEmptyCompletionRecord, uri, resultsBucket, joinTaskKey(taskPrefix, "command.log"))
	}
	rec, err = taskproto.ParseCompletionRecord(data)
	if err != nil {
		return nil, uri, fmt.Errorf("%s: %w", uri, err)
	}
	return rec, uri, nil
}
