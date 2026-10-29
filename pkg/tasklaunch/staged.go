package tasklaunch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskiam"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// A task's wrapper and terminal-flush hook are the per-launch half of its user
// data, and they grow with its command and manifests. A release build stages
// them in S3 instead (#1274): Launch uploads them as one content-addressed
// object, bootstrap/<task_id>/bootstrap-<sha256>.sh, to the account's
// dedicated staging bucket for the launch region (taskproto.StagingBucket)
// before RunInstances,
// and the user data carries only the reference, which the bootstrap fetches
// with the instance role and verifies before writing the two files.
//
// One object, not two: the bootstrap needs both or neither (the flush hook
// without its wrapper flushes nothing), so one fetch, one hash and one failure
// mode is the whole protocol. It is the same shell the inline bootstrap
// carries (launcher.StagedFilesObject), so staging changes where the bytes
// come from and nothing about what they do.
//
// A dedicated bucket, not the results prefix, so that no grant a task can
// already hold on its data buckets reaches another task's staged wrapper: the
// staging bucket is never listable by a task role, every object in it expires
// after a day, and no spec may name it. See taskiam.StagedFilesResources for
// the trust model.

// stagedFilesStore is the S3 access staging needs, beyond the AWS interface:
// make sure the staging bucket exists and is configured, upload the object,
// and find a previous attempt's to clear. *aws.Client satisfies it. Launch
// type-asserts Deps.AWS for it rather than adding it to AWS, so an embedder's
// AWS implementation still compiles; one without it launches inline, as every
// task did before #1274.
type stagedFilesStore interface {
	EnsureStagingBucket(ctx context.Context, bucket, region, ownerAccount string) error
	PutS3Object(ctx context.Context, region, bucket, key, expectedOwner string, body []byte) error
	ListS3Keys(ctx context.Context, region, bucket, prefix, expectedOwner string) ([]string, error)
	DeleteS3Object(ctx context.Context, region, bucket, key string) error
}

// stagesFiles reports whether a launch through client stages its wrapper and
// flush hook. Staging follows the bootstrap body: a build whose body is inline
// (a dev build, with no release to match, or one with no signing key) keeps
// its command and flush hook inline too, so dev and offline flows are
// unchanged. client may be nil (a plan with no AWS): a real launch, which has
// one, would stage.
func stagesFiles(client AWS) bool {
	if launcher.PublishedBodyVersion() == "" {
		return false
	}
	if client == nil {
		return true
	}
	_, ok := client.(stagedFilesStore)
	return ok
}

// stagingBucketRefusal refuses a spec that names a spawn staging bucket
// anywhere (taskproto.StagingBucketUses): a grant on one would give the task's
// role other tasks' staged wrappers. TaskSpec.Validate refuses the same, but
// Plan does not assume its caller validated.
func stagingBucketRefusal(spec *taskproto.TaskSpec) error {
	if uses := taskproto.StagingBucketUses(spec); len(uses) > 0 {
		return fmt.Errorf("invalid task spec: %s names a spawn staging bucket (%s*), which holds other tasks' staged wrappers; no task may read, write or list it (#1274)",
			strings.Join(uses, ", "), taskproto.StagingBucketPrefix)
	}
	return nil
}

// stagedObject is a staged object to upload before RunInstances.
type stagedObject struct {
	Bucket, Key string
	Body        []byte
}

// stageTaskFiles moves the command and flush hook out of cfg and opts and into
// an object in stagingBucket: opts gets the reference the bootstrap fetches,
// and the returned object is what Launch uploads. The object's bytes and the
// sha256 the user data checks come from the same launcher.StagedFilesObject
// call, so they cannot disagree.
func stageTaskFiles(command string, opts *launcher.Options, region, stagingBucket, taskID string) *stagedObject {
	body := launcher.StagedFilesObject(command, opts.TaskFlushScript)
	ref := launcher.NewStagedFiles("", region, command, opts.TaskFlushScript)
	key := taskiam.StagedFilesKey(taskID, ref.SHA256)
	ref.URI = "s3://" + stagingBucket + "/" + key
	opts.StagedFiles = &ref
	return &stagedObject{Bucket: stagingBucket, Key: key, Body: body}
}

// clearStaleStagedFiles deletes a previous attempt's staged objects for the
// task_id. Launch calls it with the stale completion records, before this
// attempt has staged anything, so everything it finds is old. Best effort,
// like clearStaleCompletion: the bucket's lifecycle deletes them within a day
// anyway, so a failure is reported, never fatal. A previous attempt still
// booting when its task_id is re-run loses its object and retires itself; it
// was about to race this attempt for the same completion record anyway.
func clearStaleStagedFiles(ctx context.Context, store stagedFilesStore, region, stagingBucket, account, taskID string) error {
	keys, err := store.ListS3Keys(ctx, region, stagingBucket, taskiam.StagedFilesKeyPrefix(taskID), account)
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		if err := store.DeleteS3Object(ctx, region, stagingBucket, k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// uploadStagedFiles uploads staged before RunInstances and returns the
// options to launch with. When the upload fails, the launch goes ahead with
// the files inline if they fit, as it would have before, and fails only if
// they do not.
func uploadStagedFiles(ctx context.Context, store stagedFilesStore, warn io.Writer, region, account string, staged *stagedObject, cfg aws.LaunchConfig, opts launcher.Options) (launcher.Options, error) {
	err := store.PutS3Object(ctx, region, staged.Bucket, staged.Key, account, staged.Body)
	if err == nil {
		return opts, nil
	}
	return inlineInstead(warn, fmt.Errorf("upload s3://%s/%s: %w", staged.Bucket, staged.Key, err), cfg, opts)
}

// inlineInstead returns opts with the files inline, after a staging failure,
// if the user data still fits; otherwise an error naming both causes.
func inlineInstead(warn io.Writer, stagingErr error, cfg aws.LaunchConfig, opts launcher.Options) (launcher.Options, error) {
	inline := opts
	inline.StagedFiles = nil
	if _, err := taskUserDataSize(cfg, inline); err != nil {
		return opts, fmt.Errorf("could not stage the task wrapper and flush hook (%w), and without staging: %w", stagingErr, err)
	}
	fmt.Fprintf(warn, "warning: could not stage the task wrapper and flush hook (%v); sending them in the user data instead\n", stagingErr)
	return inline, nil
}

// stagedFilesNote is the dry run's note under the User data line.
func stagedFilesNote(p *TaskPlan) string {
	if p.StagedBytes == 0 {
		return ""
	}
	return fmt.Sprintf("the task wrapper and flush hook (%d bytes) are staged in S3, in this account's spawn-staging bucket for the region, not sent in the user data (#1274)", p.StagedBytes)
}
