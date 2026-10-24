package tasklaunch

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskiam"
)

// A task's wrapper and terminal-flush hook are the per-launch half of its user
// data, and they grow with its command and manifests. A release build stages
// them in S3 instead (#1274): Launch uploads them to the results prefix as one
// content-addressed object, bootstrap-<sha256>.sh, before RunInstances, and the
// user data carries only the reference, which the bootstrap fetches with the
// instance role and verifies before writing the two files.
//
// One object, not two: the bootstrap needs both or neither (the flush hook
// without its wrapper flushes nothing), so one fetch, one hash and one failure
// mode is the whole protocol. It is the same shell the inline bootstrap
// carries (launcher.StagedFilesObject), so staging changes where the bytes
// come from and nothing about what they do.

// stagedFilesStore is the S3 access staging needs, beyond the AWS interface:
// upload the object, and find a previous attempt's to clear. *aws.Client
// satisfies it. Launch type-asserts Deps.AWS for it rather than adding it to
// AWS, so an embedder's AWS implementation still compiles; one without it
// launches inline, as every task did before #1274.
type stagedFilesStore interface {
	PutS3Object(ctx context.Context, region, bucket, key string, body []byte) error
	ListS3Keys(ctx context.Context, region, bucket, prefix string) ([]string, error)
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

// stagedObject is a staged object to upload before RunInstances.
type stagedObject struct {
	Bucket, Key string
	Body        []byte
}

// stageTaskFiles moves the command and flush hook out of cfg and opts and into
// an object under the task's results prefix: opts gets the reference the
// bootstrap fetches, and the returned object is what Launch uploads. The
// object's bytes and the sha256 the user data checks come from the same
// launcher.StagedFilesObject call, so they cannot disagree.
func stageTaskFiles(command string, opts *launcher.Options, region, resultsBucket, resultsKeyPrefix string) *stagedObject {
	body := launcher.StagedFilesObject(command, opts.TaskFlushScript)
	ref := launcher.NewStagedFiles("", region, command, opts.TaskFlushScript)
	key := taskiam.StagedFilesKey(resultsKeyPrefix, ref.SHA256)
	ref.URI = "s3://" + resultsBucket + "/" + key
	opts.StagedFiles = &ref
	return &stagedObject{Bucket: resultsBucket, Key: key, Body: body}
}

// stagedFilesClearer is what clearing a previous attempt's staged objects
// needs.
type stagedFilesClearer interface {
	stagedFilesStore
	objectDeleter
}

// clearStaleStagedFiles deletes a previous attempt's staged objects under the
// task's prefix. Launch calls it with the stale completion records, before
// this attempt has staged anything, so everything it finds is old. Best
// effort, like clearStaleCompletion: a leftover object is small and harmless,
// so a failure is reported, never fatal. A previous attempt still booting when
// its task_id is re-run loses its object and retires itself; it was about to
// race this attempt for the same completion record anyway.
func clearStaleStagedFiles(ctx context.Context, store stagedFilesClearer, region, resultsBucket, resultsKeyPrefix string) error {
	keys, err := store.ListS3Keys(ctx, region, resultsBucket, taskiam.StagedFilesKeyPrefix(resultsKeyPrefix))
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		if err := store.DeleteS3Object(ctx, region, resultsBucket, k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// uploadStagedFiles uploads staged before RunInstances and returns the
// options to launch with. When the upload fails (the launching principal may
// not write the bucket a spec names, which it never needed to before #1274),
// the launch goes ahead with the files inline if they fit, as it would have
// before, and fails only if they do not.
func uploadStagedFiles(ctx context.Context, store stagedFilesStore, warn io.Writer, region string, staged *stagedObject, cfg aws.LaunchConfig, opts launcher.Options) (launcher.Options, error) {
	err := store.PutS3Object(ctx, region, staged.Bucket, staged.Key, staged.Body)
	if err == nil {
		return opts, nil
	}
	inline := opts
	inline.StagedFiles = nil
	if _, ierr := taskUserDataSize(cfg, inline); ierr != nil {
		return opts, fmt.Errorf("stage the task wrapper and flush hook at s3://%s/%s: %w; without staging: %w", staged.Bucket, staged.Key, err, ierr)
	}
	fmt.Fprintf(warn, "warning: could not stage the task wrapper and flush hook at s3://%s/%s (%v); sending them in the user data instead\n", staged.Bucket, staged.Key, err)
	return inline, nil
}

// stagedFilesNote is the dry run's note under the User data line.
func stagedFilesNote(p *TaskPlan) string {
	if p.StagedBytes == 0 {
		return ""
	}
	return fmt.Sprintf("the task wrapper and flush hook (%d bytes) are staged in S3 under the results prefix, not sent in the user data (#1274)", p.StagedBytes)
}
