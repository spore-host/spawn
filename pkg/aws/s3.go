package aws

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
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
	cfg := c.cfg.Copy()
	cfg.Region = region
	s3Client := s3.NewFromConfig(cfg)

	// Check if bucket exists
	_, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucketName),
	})

	if err == nil {
		// Bucket exists
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

// GetFSxConfigFromS3Bucket retrieves FSx configuration from S3 bucket tags
func (c *Client) GetFSxConfigFromS3Bucket(ctx context.Context, stackName, region string) (*FSxConfig, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	s3Client := s3.NewFromConfig(cfg)

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
