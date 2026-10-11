package staging

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
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
	s3Client     *s3.Client
	dynamoClient *dynamodb.Client
	uploader     *manager.Uploader
	accountID    string
}

// NewClient creates a new staging client
func NewClient(cfg aws.Config, accountID string) *Client {
	s3Client := s3.NewFromConfig(cfg)
	return &Client{
		s3Client:     s3Client,
		dynamoClient: dynamodb.NewFromConfig(cfg),
		uploader:     manager.NewUploader(s3Client),
		accountID:    accountID,
	}
}

// resolveSchedulesBucket returns the S3 bucket name to use for schedules in this account.
// It prefers the shared name (spawn-schedules-{region}) but falls back to an account-scoped
// name (spawn-schedules-{accountID}-{region}) if the shared name is owned by another account.
// The bucket is created on demand with a 7-day lifecycle rule if it doesn't exist.
func (c *Client) resolveSchedulesBucket(ctx context.Context, region string) (string, error) {
	preferred := fmt.Sprintf("spawn-schedules-%s", region)
	fallback := fmt.Sprintf("spawn-schedules-%s-%s", c.accountID, region)

	// Try preferred name first
	if bucket, err := c.ensureBucket(ctx, preferred, region); err == nil {
		return bucket, nil
	}
	// Fall back to account-scoped name
	return c.ensureBucket(ctx, fallback, region)
}

// ensureBucket ensures a single named bucket exists; returns the name if it does (or was created).
// Returns an error if the bucket is owned by another account or creation fails.
func (c *Client) ensureBucket(ctx context.Context, bucket, region string) (string, error) {
	_, err := c.s3Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
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
	if _, createErr := c.s3Client.CreateBucket(ctx, input); createErr != nil {
		if strings.Contains(createErr.Error(), "BucketAlreadyOwnedByYou") {
			return bucket, nil // we own it
		}
		return "", createErr // BucketAlreadyExists (other account) or other error
	}

	// Block public access
	c.s3Client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{ //nolint:errcheck
		Bucket: aws.String(bucket),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	})

	// 7-day lifecycle rule — queue configs are ephemeral
	expireDays := int32(7)
	_, err = c.s3Client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucket),
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{
			Rules: []s3types.LifecycleRule{
				{
					ID:     aws.String("expire-schedules"),
					Status: s3types.ExpirationStatusEnabled,
					Filter: &s3types.LifecycleRuleFilter{
						Prefix: aws.String("schedules/"),
					},
					Expiration: &s3types.LifecycleExpiration{
						Days: &expireDays,
					},
				},
			},
		},
	})
	return bucket, err
}

// UploadScheduleParams uploads a parameter file for scheduled execution.
// The destination bucket is resolved (and created) on demand.
// UploadScheduleParams uploads a parameter file for scheduled execution.
// Returns (bucket, s3Key, size, sha256, error).
func (c *Client) UploadScheduleParams(ctx context.Context, localPath, scheduleID, region string) (string, string, int64, string, error) {
	bucket, err := c.resolveSchedulesBucket(ctx, region)
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
	_, err = c.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(s3Key),
		Body:   file,
		Metadata: map[string]string{
			"schedule-id": scheduleID,
			"sha256":      sha256sum,
		},
		Tagging: aws.String("Project=spawn&Component=scheduler&ScheduleID=" + scheduleID),
	})
	if err != nil {
		return "", "", 0, "", fmt.Errorf("upload to s3: %w", err)
	}

	return bucket, s3Key, size, sha256sum, nil
}
