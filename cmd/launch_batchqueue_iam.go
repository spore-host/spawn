package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/queue"
	"github.com/spore-host/spawn/pkg/taskiam"
)

// batchQueueProfiler is the one IAM call a batch-queue launch makes to get its
// instance profile. *aws.Client satisfies it; tests count calls through it.
type batchQueueProfiler interface {
	CreateOrGetInstanceProfile(ctx context.Context, config aws.IAMRoleConfig) (string, error)
}

// batchQueueInstanceProfile resolves the instance profile of a `spawn launch
// --batch-queue` instance (#1344).
//
// --instance-profile and --iam-role are attached verbatim, as before. With
// neither, the queue gets a spawn-instance-* role of its own carrying
// BatchQueuePolicy, the way `spawn task run` gets a scoped role. It used to run
// on the shared spored-instance-role, which grants no write on any user bucket,
// so `spored run-queue` failed every results upload with AccessDenied unless
// the user brought a hand-made role. CreateOrGetInstanceProfile adds the spored
// baseline and SSM, and tags the role spawn:managed=true, so the reaper deletes
// it once no instance uses it (#1300).
func batchQueueInstanceProfile(ctx context.Context, p batchQueueProfiler, explicitProfile, explicitRole, scheduleBucket string, qc *queue.QueueConfig) (string, error) {
	if explicitProfile != "" {
		return explicitProfile, nil
	}
	if explicitRole != "" {
		return explicitRole, nil
	}
	pol, err := batchQueuePolicy(scheduleBucket, qc.ResultS3Bucket, qc.ResultS3Prefix)
	if err != nil {
		return "", err
	}
	return p.CreateOrGetInstanceProfile(ctx, aws.IAMRoleConfig{InlinePolicyJSON: pol})
}

// batchQueuePolicy is the inline policy of a batch queue's role: read the
// queue file the bootstrap downloads (`aws s3 cp` of
// s3://<scheduleBucket>/schedules/<queue_id>/params.yaml, staging.UploadQueueConfig),
// and write under the results prefix, which is every key QueueRunner uploads
// to (<prefix>/jobs/<job>/..., <prefix>/queue-state.json).
//
// The read grant covers every queue's file in the schedules bucket, not just
// this one's, so queues with the same results location share one role rather
// than minting a role per queue ID.
func batchQueuePolicy(scheduleBucket, resultsBucket, resultsPrefix string) (string, error) {
	if scheduleBucket == "" {
		return "", fmt.Errorf("batch queue: no schedules bucket to read the queue file from")
	}
	var stmts []string
	stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject"],"Resource":[%q]}`,
		"arn:aws:s3:::"+scheduleBucket+"/schedules/*/params.yaml"))
	if err := validateBatchQueueResults(resultsBucket, resultsPrefix); err != nil {
		return "", err
	}
	if resultsBucket != "" {
		// Keys are "<prefix>/<rest>" exactly as QueueRunner forms them, so the
		// resource is too, even for an empty or slash-ended prefix.
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":[%s],"Resource":[%q]}`,
			taskiam.IAMActionList(taskiam.ObjectWriteActions),
			"arn:aws:s3:::"+resultsBucket+"/"+resultsPrefix+"/*"))
	}
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(stmts, ",") + `]}`, nil
}

// validateBatchQueueResults rejects a results location that cannot be put in
// a policy Resource as written: not a plain bucket name, a spawn staging
// bucket, or a prefix carrying a wildcard or an IAM policy variable, any of
// which would widen the grant or make the policy invalid. Called before any
// AWS call, and again when the policy is built.
func validateBatchQueueResults(bucket, prefix string) error {
	if bucket == "" {
		return nil
	}
	if err := validateS3BucketName("result_s3_bucket", bucket); err != nil {
		return err
	}
	if strings.ContainsAny(prefix, "*?$") {
		return fmt.Errorf("result_s3_prefix %q: wildcards and policy variables (*, ?, $) are not allowed; "+
			"the prefix becomes the IAM resource the queue may write to", prefix)
	}
	return nil
}
