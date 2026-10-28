package staging

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	tm "github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/spawn/pkg/s3transfer"
)

// StagingMetadata represents metadata about staged data
type StagingMetadata struct {
	StagingID   string    `dynamodbav:"staging_id"`
	LocalPath   string    `dynamodbav:"local_path"`
	S3Key       string    `dynamodbav:"s3_key"`
	Regions     []string  `dynamodbav:"regions"`
	SweepID     string    `dynamodbav:"sweep_id,omitempty"`
	CreatedAt   time.Time `dynamodbav:"created_at"`
	Destination string    `dynamodbav:"destination,omitempty"`
	SizeBytes   int64     `dynamodbav:"size_bytes"`
	SHA256      string    `dynamodbav:"sha256"`
	TTL         int64     `dynamodbav:"ttl"` // Unix timestamp for automatic deletion
}

// Client handles data staging operations
type Client struct {
	cfg          aws.Config
	dynamoClient *dynamodb.Client
	accountID    string
}

// NewClient creates a new staging client. cfg supplies credentials; its region
// does not choose where schedule buckets live (see s3For).
func NewClient(cfg aws.Config, accountID string) *Client {
	return &Client{
		cfg:          cfg,
		dynamoClient: dynamodb.NewFromConfig(cfg),
		accountID:    accountID,
	}
}

// s3For returns an S3 client for a bucket in region.
//
// The schedule buckets are per region (spawn-schedules-<region>), but the
// caller's config is not: `spawn schedule create` loads the infra config in
// us-east-1 whatever --region says. Sent from a client in the wrong region,
// HeadBucket on an existing bucket elsewhere gets a 301, PutObject gets
// PermanentRedirect, and CreateBucket with a LocationConstraint that differs
// from the endpoint's region is rejected (IllegalLocationConstraintException).
// So every S3 call here goes through a client in the bucket's own region.
func (c *Client) s3For(region string) *s3.Client {
	return s3.NewFromConfig(c.cfg, func(o *s3.Options) { o.Region = region })
}

// resolveSchedulesBucket returns the S3 bucket name to use for schedules in this account.
// It prefers the shared name (spawn-schedules-{region}) but falls back to an account-scoped
// name (spawn-schedules-{accountID}-{region}) if the shared name is owned by another account.
// The bucket is created on demand if it doesn't exist, with no expiry on schedule params:
// a schedule's params live exactly as long as the schedule, and its deleters are
// `spawn schedule cancel` and scheduler-handler when the schedule runs out (#1382).
// Its lifecycle is then reconciled: see reconcileSchedulesLifecycle.
func (c *Client) resolveSchedulesBucket(ctx context.Context, region string, k paramsKind) (string, error) {
	preferred := fmt.Sprintf("spawn-schedules-%s", region)
	fallback := fmt.Sprintf("spawn-schedules-%s-%s", c.accountID, region)

	// Try preferred name first
	bucket, err := c.ensureBucket(ctx, preferred, region)
	if err != nil {
		// Fall back to account-scoped name
		if bucket, err = c.ensureBucket(ctx, fallback, region); err != nil {
			return "", err
		}
	}
	// After the name is chosen, not inside ensureBucket: a lifecycle failure on
	// the preferred bucket must not send the upload to the fallback one.
	if err := reconcileSchedulesLifecycle(ctx, c.s3For(region), bucket, k); err != nil {
		if k == kindQueueConfig {
			// The queue config still uploads; it only loses its expiry.
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
			return bucket, nil
		}
		return "", err
	}
	return bucket, nil
}

// ensureBucket ensures a single named bucket exists; returns the name if it does (or was created).
// Returns an error if the bucket is owned by another account or creation fails.
func (c *Client) ensureBucket(ctx context.Context, bucket, region string) (string, error) {
	s3Client := c.s3For(region)
	_, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return bucket, nil // already exists in our account
	}

	// Attempt to create
	input := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	if _, createErr := s3Client.CreateBucket(ctx, input); createErr != nil {
		if strings.Contains(createErr.Error(), "BucketAlreadyOwnedByYou") {
			return bucket, nil // we own it
		}
		return "", createErr // BucketAlreadyExists (other account) or other error
	}

	// Block public access
	s3Client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{ //nolint:errcheck
		Bucket: aws.String(bucket),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	})

	// No lifecycle rule here. This used to put `expire-schedules` (7 days on
	// schedules/, "queue configs are ephemeral"), which deleted every
	// recurring schedule's params a week in (#1382). Queue configs get their
	// own tag-scoped rule from reconcileSchedulesLifecycle.
	return bucket, nil
}

// paramsKind is what an upload to the schedules bucket is.
type paramsKind int

const (
	// kindScheduleParams: a schedule's params. They must outlive no schedule
	// and be outlived by none, so nothing expires them.
	kindScheduleParams paramsKind = iota
	// kindQueueConfig: a batch queue's config, read once when its instance
	// boots. It shares the bucket and the schedules/ prefix (the queue's
	// instance role is scoped to schedules/*/params.yaml), and is told apart
	// by its Component tag.
	kindQueueConfig
)

// The schedules bucket's lifecycle rules.
const (
	// QueueConfigRuleID expires batch queue configs: objects under
	// schedules/ tagged QueueConfigTagKey=QueueConfigTagValue.
	QueueConfigRuleID     = "expire-queue-configs"
	QueueConfigExpiryDays = 7
	QueueConfigTagKey     = "Component"
	QueueConfigTagValue   = "queue"
	// ScheduleParamsTagValue is the Component tag on schedule params. It is
	// not QueueConfigTagValue, so QueueConfigRuleID never matches them.
	ScheduleParamsTagValue = "scheduler"
)

// RetiredScheduleLifecycleRuleIDs are the rules that expired schedule params
// while their schedule lived (#1382): `expire-schedules`, which this package
// put on every bucket it created (7 days on schedules/), and
// `DeleteOldScheduleParams`, which the production spawn-schedules-<region>
// buckets carry (90 days, whole bucket; it predates this package).
var RetiredScheduleLifecycleRuleIDs = []string{"expire-schedules", "DeleteOldScheduleParams"}

// lifecycleAPI is the part of S3 reconcileSchedulesLifecycle uses.
type lifecycleAPI interface {
	GetBucketLifecycleConfiguration(context.Context, *s3.GetBucketLifecycleConfigurationInput, ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error)
	PutBucketLifecycleConfiguration(context.Context, *s3.PutBucketLifecycleConfigurationInput, ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error)
	DeleteBucketLifecycle(context.Context, *s3.DeleteBucketLifecycleInput, ...func(*s3.Options)) (*s3.DeleteBucketLifecycleOutput, error)
}

// queueConfigRule is the lifecycle rule that expires queue configs and
// nothing else.
func queueConfigRule() s3types.LifecycleRule {
	return s3types.LifecycleRule{
		ID:     aws.String(QueueConfigRuleID),
		Status: s3types.ExpirationStatusEnabled,
		Filter: &s3types.LifecycleRuleFilter{And: &s3types.LifecycleRuleAndOperator{
			Prefix: aws.String("schedules/"),
			Tags:   []s3types.Tag{{Key: aws.String(QueueConfigTagKey), Value: aws.String(QueueConfigTagValue)}},
		}},
		Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(QueueConfigExpiryDays)},
	}
}

// reconcileSchedulesLifecycle is the migration for #1382, run on every
// upload to a schedules bucket.
//
// For schedule params it removes the RetiredScheduleLifecycleRuleIDs and keeps
// every other rule: Get, then Put the rest, or delete the configuration if
// nothing is left. For a queue config it adds QueueConfigRuleID if missing and
// removes nothing: a queue-only bucket's old `expire-schedules` still expires
// the queue configs older CLIs uploaded untagged. A bucket whose rules need no
// change is not written.
func reconcileSchedulesLifecycle(ctx context.Context, api lifecycleAPI, bucket string, k paramsKind) error {
	out, err := api.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
	var rules []s3types.LifecycleRule
	var minSize s3types.TransitionDefaultMinimumObjectSize
	switch {
	case err == nil:
		rules, minSize = out.Rules, out.TransitionDefaultMinimumObjectSize
	case isNoLifecycle(err):
	default:
		return fmt.Errorf("read the lifecycle of schedules bucket %s (to remove rules that expire "+
			"schedule params while the schedule lives, #1382): %w", bucket, err)
	}

	kept := make([]s3types.LifecycleRule, 0, len(rules)+1)
	changed, haveQueueRule := false, false
	for _, r := range rules {
		id := aws.ToString(r.ID)
		if k == kindScheduleParams && slices.Contains(RetiredScheduleLifecycleRuleIDs, id) {
			changed = true
			continue
		}
		haveQueueRule = haveQueueRule || id == QueueConfigRuleID
		kept = append(kept, r)
	}
	if k == kindQueueConfig && !haveQueueRule {
		kept = append(kept, queueConfigRule())
		changed = true
	}
	if !changed {
		return nil
	}
	if len(kept) == 0 {
		if _, err := api.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: aws.String(bucket)}); err != nil {
			return fmt.Errorf("remove the expiring lifecycle from schedules bucket %s (#1382): %w", bucket, err)
		}
		return nil
	}
	in := &s3.PutBucketLifecycleConfigurationInput{
		Bucket:                 aws.String(bucket),
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: kept},
	}
	if minSize != "" {
		in.TransitionDefaultMinimumObjectSize = minSize
	}
	if _, err := api.PutBucketLifecycleConfiguration(ctx, in); err != nil {
		return fmt.Errorf("rewrite the lifecycle of schedules bucket %s (#1382): %w", bucket, err)
	}
	return nil
}

// isNoLifecycle reports a bucket with no lifecycle configuration.
func isNoLifecycle(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchLifecycleConfiguration"
}

// UploadScheduleParams uploads a parameter file for scheduled execution to the
// schedules bucket in region, which is resolved (and created) on demand. The
// upload uses a client in region, not the caller's config region.
// Nothing expires the object; the schedule's deleters remove it (#1382).
// Returns (bucket, s3Key, size, sha256, error).
func (c *Client) UploadScheduleParams(ctx context.Context, localPath, scheduleID, region string) (string, string, int64, string, error) {
	return c.uploadParams(ctx, localPath, scheduleID, region, kindScheduleParams)
}

// UploadQueueConfig uploads a batch queue's config to the same bucket and key
// layout as UploadScheduleParams, tagged so the bucket's QueueConfigRuleID
// expires it after QueueConfigExpiryDays. The queue's instance reads it once,
// at boot. Returns (bucket, s3Key, size, sha256, error).
func (c *Client) UploadQueueConfig(ctx context.Context, localPath, queueID, region string) (string, string, int64, string, error) {
	return c.uploadParams(ctx, localPath, queueID, region, kindQueueConfig)
}

func (c *Client) uploadParams(ctx context.Context, localPath, scheduleID, region string, k paramsKind) (string, string, int64, string, error) {
	bucket, err := c.resolveSchedulesBucket(ctx, region, k)
	if err != nil {
		return "", "", 0, "", fmt.Errorf("resolve schedules bucket: %w", err)
	}
	s3Key := fmt.Sprintf("schedules/%s/params.yaml", scheduleID)

	// Open file
	file, err := os.Open(localPath)
	if err != nil {
		return "", "", 0, "", fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Calculate SHA256 and size
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", "", 0, "", fmt.Errorf("calculate hash: %w", err)
	}
	sha256sum := fmt.Sprintf("%x", hash.Sum(nil))

	// Reset file pointer
	if _, err := file.Seek(0, 0); err != nil {
		return "", "", 0, "", fmt.Errorf("seek file: %w", err)
	}

	// Upload to S3
	_, err = s3transfer.New(c.s3For(region)).UploadObject(ctx, &tm.UploadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(s3Key),
		Body:   file,
		Metadata: map[string]string{
			"schedule-id": scheduleID,
			"sha256":      sha256sum,
		},
		Tagging: aws.String(paramsTagging(k, scheduleID)),
	})
	if err != nil {
		return "", "", 0, "", fmt.Errorf("upload to s3: %w", err)
	}

	return bucket, s3Key, size, sha256sum, nil
}

// paramsTagging is the object tagging of an upload of kind k.
func paramsTagging(k paramsKind, id string) string {
	if k == kindQueueConfig {
		return "Project=spawn&" + QueueConfigTagKey + "=" + QueueConfigTagValue + "&QueueID=" + id
	}
	return "Project=spawn&" + QueueConfigTagKey + "=" + ScheduleParamsTagValue + "&ScheduleID=" + id
}
