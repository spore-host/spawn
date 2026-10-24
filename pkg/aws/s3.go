package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3BucketConfig contains configuration for creating/tagging S3 buckets
type S3BucketConfig struct {
	BucketName      string
	Region          string
	StackName       string
	StorageCapacity int32
	ImportPath      string
	ExportPath      string
}

// CreateS3BucketIfNotExists creates an S3 bucket if it doesn't already exist
func (c *Client) CreateS3BucketIfNotExists(ctx context.Context, bucketName, region string) error {
	return c.CreateS3BucketWithTags(ctx, S3BucketConfig{
		BucketName: bucketName,
		Region:     region,
	})
}

// CreateS3BucketWithTags creates an S3 bucket with FSx configuration tags
func (c *Client) CreateS3BucketWithTags(ctx context.Context, config S3BucketConfig) error {
	bucketName := config.BucketName
	region := config.Region
	s3Client := s3.NewFromConfig(c.regionalConfig(region))

	// Check if bucket exists
	_, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucketName),
	})

	if err == nil {
		// Bucket exists
		return nil
	}

	// A bucket that exists in a DIFFERENT region answers HeadBucket with 301
	// MovedPermanently / PermanentRedirect (not NotFound). The bucket exists —
	// it's just elsewhere — so for an existence check, treat that as "exists,
	// don't create" (#103). FSx itself validates cross-region S3 usability and
	// returns a meaningful error if the bucket truly can't back the filesystem.
	if isCrossRegionRedirect(err) {
		return nil
	}

	// Check if error is "not found" - if so, create bucket
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchBucket" {
			// Bucket doesn't exist, create it
			createInput := &s3.CreateBucketInput{
				Bucket: aws.String(bucketName),
			}

			// For regions other than us-east-1, need to specify location constraint
			if region != "us-east-1" {
				createInput.CreateBucketConfiguration = &types.CreateBucketConfiguration{
					LocationConstraint: types.BucketLocationConstraint(region),
				}
			}

			_, err = s3Client.CreateBucket(ctx, createInput)
			if err != nil {
				return fmt.Errorf("failed to create S3 bucket: %w", err)
			}

			// Add tags to identify spawn-managed bucket and store FSx config
			tags := []types.Tag{
				{
					Key:   aws.String("spawn:managed"),
					Value: aws.String("true"),
				},
				{
					Key:   aws.String("spawn:fsx-backing-bucket"),
					Value: aws.String("true"),
				},
			}

			// Add FSx configuration tags if provided
			if config.StackName != "" {
				tags = append(tags, types.Tag{
					Key:   aws.String("spawn:fsx-stack-name"),
					Value: aws.String(config.StackName),
				})
			}
			if config.StorageCapacity > 0 {
				tags = append(tags, types.Tag{
					Key:   aws.String("spawn:fsx-storage-capacity"),
					Value: aws.String(fmt.Sprintf("%d", config.StorageCapacity)),
				})
			}
			if config.ImportPath != "" {
				tags = append(tags, types.Tag{
					Key:   aws.String("spawn:fsx-import-path"),
					Value: aws.String(config.ImportPath),
				})
			}
			if config.ExportPath != "" {
				tags = append(tags, types.Tag{
					Key:   aws.String("spawn:fsx-export-path"),
					Value: aws.String(config.ExportPath),
				})
			}

			_, err = s3Client.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
				Bucket: aws.String(bucketName),
				Tagging: &types.Tagging{
					TagSet: tags,
				},
			})
			if err != nil {
				// Non-fatal error - bucket was created successfully
				return nil
			}

			return nil
		}
	}

	// Some other error occurred
	return fmt.Errorf("failed to check if bucket exists: %w", err)
}

// isCrossRegionRedirect reports whether a HeadBucket error is the 301 redirect
// AWS returns when the bucket exists but lives in another region. The SDK
// surfaces this two ways depending on the code path: a smithy.APIError with a
// PermanentRedirect/MovedPermanently/301 code, or a bare HTTP-301
// awshttp.ResponseError with no error code. Detect both (#103).
func isCrossRegionRedirect(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "PermanentRedirect", "MovedPermanently", "301":
			return true
		}
	}
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == 301 {
		return true
	}
	return false
}

// GetFSxConfigFromS3Bucket retrieves FSx configuration from S3 bucket tags
func (c *Client) GetFSxConfigFromS3Bucket(ctx context.Context, stackName, region string) (*FSxConfig, error) {
	s3Client := s3.NewFromConfig(c.regionalConfig(region))

	// List buckets and check tags
	listResult, err := s3Client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to list buckets: %w", err)
	}

	for _, bucket := range listResult.Buckets {
		// Get bucket tags
		tagsResult, err := s3Client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{
			Bucket: bucket.Name,
		})
		if err != nil {
			// Skip buckets without tags or access errors
			continue
		}

		// Check if this bucket has the matching stack name
		var foundStack bool
		var bucketName string
		var storageCapacity int32
		var importPath string
		var exportPath string

		for _, tag := range tagsResult.TagSet {
			switch *tag.Key {
			case "spawn:fsx-stack-name":
				if *tag.Value == stackName {
					foundStack = true
					bucketName = *bucket.Name
				}
			case "spawn:fsx-storage-capacity":
				_, _ = fmt.Sscanf(*tag.Value, "%d", &storageCapacity)
			case "spawn:fsx-import-path":
				importPath = *tag.Value
			case "spawn:fsx-export-path":
				exportPath = *tag.Value
			}
		}

		if foundStack {
			// Default capacity if not found in tags
			if storageCapacity == 0 {
				storageCapacity = 1200
			}

			return &FSxConfig{
				StackName:       stackName,
				Region:          region,
				StorageCapacity: storageCapacity,
				S3Bucket:        bucketName,
				ImportPath:      importPath,
				ExportPath:      exportPath,
			}, nil
		}
	}

	return nil, fmt.Errorf("no S3 bucket found with stack name: %s", stackName)
}

// ErrS3NoSuchKey is returned by GetS3Object when the object does not exist, so
// callers can distinguish "not there yet" (e.g. a task still running, no
// completion record) from a real error, with errors.Is.
var ErrS3NoSuchKey = errors.New("s3: no such key")

// GetS3Object fetches an object's bytes from the given bucket/key. A missing
// object returns ErrS3NoSuchKey (wrapped) so pollers can treat absence as
// "pending" rather than failure.
func (c *Client) GetS3Object(ctx context.Context, region, bucket, key string) ([]byte, error) {
	s3Client := s3.NewFromConfig(c.regionalConfig(region))
	out, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, fmt.Errorf("%w: s3://%s/%s", ErrS3NoSuchKey, bucket, key)
		}
		// Some endpoints surface a missing key as a generic NoSuchKey/NotFound code.
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return nil, fmt.Errorf("%w: s3://%s/%s", ErrS3NoSuchKey, bucket, key)
		}
		return nil, fmt.Errorf("get s3://%s/%s: %w", bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read s3://%s/%s: %w", bucket, key, err)
	}
	return data, nil
}

// DeleteS3Object deletes a single object. Deleting a key that does not exist is
// a SUCCESS, not an error — that is S3's own DeleteObject semantics (it returns
// 204 for an absent key in a non-versioned bucket), and callers rely on it: the
// task-run path clears any previous attempt's completion record before launch
// (spawn#608) and the common case is that there is nothing there to clear. A
// NoSuchKey/NotFound from an S3-compatible endpoint that does surface one is
// normalized to nil here for the same reason.
func (c *Client) DeleteS3Object(ctx context.Context, region, bucket, key string) error {
	s3Client := s3.NewFromConfig(c.regionalConfig(region))
	if _, err := s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil
		}
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return nil
		}
		return fmt.Errorf("delete s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

// PutS3Object writes body to bucket/key, replacing any object already there.
// A non-empty expectedOwner makes S3 refuse the write unless that account owns
// the bucket, so nothing is written into a bucket someone else created under
// a name spawn expected to be its own. `spawn task run` stages a launch's
// command and flush hook with it (#1274).
func (c *Client) PutS3Object(ctx context.Context, region, bucket, key, expectedOwner string, body []byte) error {
	s3Client := s3.NewFromConfig(c.regionalConfig(region))
	in := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}
	if expectedOwner != "" {
		in.ExpectedBucketOwner = aws.String(expectedOwner)
	}
	if _, err := s3Client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

// ListS3Keys returns the key of every object in bucket whose key starts with
// prefix, following pagination. A non-empty expectedOwner makes S3 refuse the
// listing unless that account owns the bucket.
func (c *Client) ListS3Keys(ctx context.Context, region, bucket, prefix, expectedOwner string) ([]string, error) {
	s3Client := s3.NewFromConfig(c.regionalConfig(region))
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	}
	if expectedOwner != "" {
		in.ExpectedBucketOwner = aws.String(expectedOwner)
	}
	var keys []string
	p := s3.NewListObjectsV2Paginator(s3Client, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", bucket, prefix, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}

// StagingLifecycleRuleID names the lifecycle rule EnsureStagingBucket installs.
const StagingLifecycleRuleID = "spawn-staging-expire-1d"

// StagingObjectExpiryDays is how long a staged object lives: long enough for
// an instance to boot and fetch it, and no longer.
const StagingObjectExpiryDays = 1

// EnsureStagingBucket makes sure bucket exists in region, belongs to
// ownerAccount, and is configured as a spawn staging bucket (#1274):
//
//   - Block Public Access on, all four settings;
//   - default encryption SSE-S3;
//   - Object Ownership BucketOwnerEnforced (no ACLs);
//   - a lifecycle rule expiring every object, and aborting incomplete
//     multipart uploads, after StagingObjectExpiryDays;
//   - tagged spawn:managed=true and spawn:purpose=bootstrap-staging, so
//     `spawn footprint` and other inventories find it.
//
// A bucket it creates gets all of that. A bucket that already exists is
// checked for the lifecycle rule and, if the rule is missing or differs, gets
// the whole configuration re-applied: a staging bucket whose objects never
// expire would keep every task's wrapper forever, which is the point of the
// rule. A bucket of that name owned by another account is an error, and
// nothing is written to it.
func (c *Client) EnsureStagingBucket(ctx context.Context, bucket, region, ownerAccount string) error {
	return ensureStagingBucket(ctx, s3.NewFromConfig(c.regionalConfig(region)), bucket, region, ownerAccount)
}

// stagingS3 is the S3 API EnsureStagingBucket uses.
type stagingS3 interface {
	HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	PutPublicAccessBlock(context.Context, *s3.PutPublicAccessBlockInput, ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error)
	PutBucketEncryption(context.Context, *s3.PutBucketEncryptionInput, ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error)
	PutBucketOwnershipControls(context.Context, *s3.PutBucketOwnershipControlsInput, ...func(*s3.Options)) (*s3.PutBucketOwnershipControlsOutput, error)
	PutBucketLifecycleConfiguration(context.Context, *s3.PutBucketLifecycleConfigurationInput, ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error)
	GetBucketLifecycleConfiguration(context.Context, *s3.GetBucketLifecycleConfigurationInput, ...func(*s3.Options)) (*s3.GetBucketLifecycleConfigurationOutput, error)
	PutBucketTagging(context.Context, *s3.PutBucketTaggingInput, ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error)
}

func ensureStagingBucket(ctx context.Context, api stagingS3, bucket, region, owner string) error {
	if owner == "" {
		return fmt.Errorf("staging bucket %s: the owning account is required", bucket)
	}
	_, err := api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket), ExpectedBucketOwner: aws.String(owner)})
	switch {
	case err == nil:
		if stagingLifecycleOK(ctx, api, bucket, owner) {
			return nil
		}
		// Exists without the expiry rule: re-apply everything, not just the
		// rule, since whoever removed it may have changed the rest.
		return configureStagingBucket(ctx, api, bucket, owner)
	case isNotFound(err):
	default:
		// 403 here is a bucket of this name owned by someone else (or no
		// permission to look): never stage into it.
		return fmt.Errorf("staging bucket %s is not usable by account %s (it may belong to another account): %w", bucket, owner, err)
	}
	in := &s3.CreateBucketInput{
		Bucket:          aws.String(bucket),
		ObjectOwnership: types.ObjectOwnershipBucketOwnerEnforced,
	}
	if region != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	if _, err := api.CreateBucket(ctx, in); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		if !errors.As(err, &owned) {
			return fmt.Errorf("create staging bucket %s: %w", bucket, err)
		}
	}
	return configureStagingBucket(ctx, api, bucket, owner)
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchBucket":
			return true
		}
	}
	var respErr *awshttp.ResponseError
	return errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404
}

// stagingLifecycleRules is the staging bucket's whole lifecycle configuration.
func stagingLifecycleRules() []types.LifecycleRule {
	return []types.LifecycleRule{{
		ID:                             aws.String(StagingLifecycleRuleID),
		Status:                         types.ExpirationStatusEnabled,
		Filter:                         &types.LifecycleRuleFilter{Prefix: aws.String("")},
		Expiration:                     &types.LifecycleExpiration{Days: aws.Int32(StagingObjectExpiryDays)},
		AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(StagingObjectExpiryDays)},
	}}
}

// stagingLifecycleOK reports whether the bucket has the expiry rule, enabled,
// for every object.
func stagingLifecycleOK(ctx context.Context, api stagingS3, bucket, owner string) bool {
	out, err := api.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket), ExpectedBucketOwner: aws.String(owner)})
	if err != nil {
		return false
	}
	for _, r := range out.Rules {
		if aws.ToString(r.ID) == StagingLifecycleRuleID && r.Status == types.ExpirationStatusEnabled &&
			r.Expiration != nil && aws.ToInt32(r.Expiration.Days) == StagingObjectExpiryDays &&
			r.AbortIncompleteMultipartUpload != nil && aws.ToInt32(r.AbortIncompleteMultipartUpload.DaysAfterInitiation) == StagingObjectExpiryDays &&
			(r.Filter == nil || (aws.ToString(r.Filter.Prefix) == "" && r.Filter.And == nil && r.Filter.Tag == nil && r.Filter.ObjectSizeGreaterThan == nil && r.Filter.ObjectSizeLessThan == nil)) {
			return true
		}
	}
	return false
}

func configureStagingBucket(ctx context.Context, api stagingS3, bucket, owner string) error {
	b, o := aws.String(bucket), aws.String(owner)
	steps := []struct {
		what string
		do   func() error
	}{
		{"block public access", func() error {
			_, err := api.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{Bucket: b, ExpectedBucketOwner: o,
				PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
					BlockPublicAcls: aws.Bool(true), IgnorePublicAcls: aws.Bool(true),
					BlockPublicPolicy: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true),
				}})
			return err
		}},
		{"set object ownership", func() error {
			_, err := api.PutBucketOwnershipControls(ctx, &s3.PutBucketOwnershipControlsInput{Bucket: b, ExpectedBucketOwner: o,
				OwnershipControls: &types.OwnershipControls{Rules: []types.OwnershipControlsRule{{ObjectOwnership: types.ObjectOwnershipBucketOwnerEnforced}}}})
			return err
		}},
		{"set default encryption", func() error {
			_, err := api.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{Bucket: b, ExpectedBucketOwner: o,
				ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
					ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256},
				}}}})
			return err
		}},
		{"set the expiry lifecycle", func() error {
			_, err := api.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: b, ExpectedBucketOwner: o,
				LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: stagingLifecycleRules()}})
			return err
		}},
		{"tag", func() error {
			_, err := api.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: b, ExpectedBucketOwner: o,
				Tagging: &types.Tagging{TagSet: []types.Tag{
					{Key: aws.String("spawn:managed"), Value: aws.String("true")},
					{Key: aws.String("spawn:purpose"), Value: aws.String("bootstrap-staging")},
				}}})
			return err
		}},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			return fmt.Errorf("staging bucket %s: %s: %w", bucket, st.what, err)
		}
	}
	return nil
}
