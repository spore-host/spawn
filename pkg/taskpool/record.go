package taskpool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// ResultsAPI is the slice of the S3 SDK the worker needs to write a completion
// record itself. *s3.Client satisfies it, and so does any S3API.
type ResultsAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// recordFailedBeforeRun writes the failed completion record (and its .exitcode
// sibling) for a task the worker acks without its job script ever running. The
// job script is what normally writes these, so without this the task would be
// acked with no record at all and `spawn task status` would report it unknown
// forever. Two routes reach it:
//
//   - the staged spec does not parse, or is refused on a pool (#1149): class is
//     spec_error, which is not retryable, because the same bytes fail the same
//     way on any worker;
//   - the execer failed before the script started (#1237): the script could
//     not be rendered (spec_error), or could not be written or started on this
//     worker (instance_health, retryable: a fresh worker usually succeeds).
//
// The record mirrors the one the terminal-flush hook writes when a task never
// returned a status: state "failed", exit_code -1, with class as retry_class.
//
// The key is the one `task status` reads. A spec that is valid JSON but fails
// validation can still name its results_prefix, so that is honoured when it can
// be read; otherwise the record goes to the default results prefix.
func recordFailedBeforeRun(ctx context.Context, client ResultsAPI, accountID, region, taskID string, specJSON []byte, class taskproto.RetryClass, now time.Time) (string, error) {
	var loose struct {
		ResultsPrefix string `json:"results_prefix"`
	}
	_ = json.Unmarshal(specJSON, &loose) // best effort: malformed JSON → default prefix

	prefix := taskproto.EffectiveResultsPrefix(
		&taskproto.TaskSpec{TaskID: taskID, ResultsPrefix: loose.ResultsPrefix}, accountID, region)
	bucket, key, ok := taskproto.SplitS3URI(prefix)
	if !ok {
		return "", fmt.Errorf("results prefix %q is not an s3:// URI", prefix)
	}

	ts := now.UTC().Format(time.RFC3339)
	rec := taskproto.CompletionRecord{
		TaskID:     taskID,
		RunID:      uuid.NewString(),
		ExitCode:   -1,
		State:      taskproto.StateFailed,
		StartedAt:  ts,
		EndedAt:    ts,
		RetryClass: class,
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("marshal completion record: %w", err)
	}

	completionKey := key + "/completion.json"
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(completionKey),
		Body: bytes.NewReader(body), ContentType: aws.String("application/json"),
	}); err != nil {
		return "", fmt.Errorf("write completion record s3://%s/%s: %w", bucket, completionKey, err)
	}
	// .exitcode is the plain-integer legacy signal; the record is what matters,
	// so a failure here is reported but the record above already landed.
	exitKey := key + "/.exitcode"
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(exitKey),
		Body: bytes.NewReader([]byte(strconv.Itoa(rec.ExitCode))),
	}); err != nil {
		return "", fmt.Errorf("write .exitcode s3://%s/%s: %w", bucket, exitKey, err)
	}
	return fmt.Sprintf("s3://%s/%s", bucket, completionKey), nil
}
