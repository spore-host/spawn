package staging

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
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

// GenerateStagingID creates a unique staging ID
func GenerateStagingID() string {
	return fmt.Sprintf("stage-%s", time.Now().Format("20060102-150405"))
}

// UploadToPrimaryRegion uploads a file to the primary region's bucket
func (c *Client) UploadToPrimaryRegion(ctx context.Context, localPath, stagingID, primaryRegion string) (string, int64, string, error) {
	bucket := fmt.Sprintf("spawn-data-%s", primaryRegion)
	s3Key := fmt.Sprintf("staging/%s/%s", stagingID, filepath.Base(localPath))

	// Open file
	file, err := os.Open(localPath)
	if err != nil {
		return "", 0, "", fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Calculate SHA256 and size
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, "", fmt.Errorf("calculate hash: %w", err)
	}
	sha256sum := fmt.Sprintf("%x", hash.Sum(nil))

	// Reset file pointer
	if _, err := file.Seek(0, 0); err != nil {
		return "", 0, "", fmt.Errorf("seek file: %w", err)
	}

	// Upload to S3
	_, err = c.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(s3Key),
		Body:   file,
		Metadata: map[string]string{
			"staging-id": stagingID,
			"sha256":     sha256sum,
		},
		Tagging: aws.String("Project=spawn&Component=data-staging&StagingID=" + stagingID),
	})
	if err != nil {
		return "", 0, "", fmt.Errorf("upload to s3: %w", err)
	}

	return s3Key, size, sha256sum, nil
}

// ReplicateToRegion copies an S3 object from one region to another
func (c *Client) ReplicateToRegion(ctx context.Context, sourceRegion, sourceBucket, sourceKey, destRegion string) error {
	destBucket := fmt.Sprintf("spawn-data-%s", destRegion)

	// Use S3 copy operation
	copySource := fmt.Sprintf("%s/%s", sourceBucket, sourceKey)

	_, err := c.s3Client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:           aws.String(destBucket),
		Key:              aws.String(sourceKey),
		CopySource:       aws.String(copySource),
		TaggingDirective: s3types.TaggingDirectiveCopy,
	})
	if err != nil {
		return fmt.Errorf("copy to %s: %w", destRegion, err)
	}

	return nil
}

// RecordMetadata stores staging metadata in DynamoDB
func (c *Client) RecordMetadata(ctx context.Context, metadata StagingMetadata) error {
	// Set TTL to 8 days from now (1 day after S3 lifecycle)
	metadata.TTL = time.Now().Add(8 * 24 * time.Hour).Unix()

	item := map[string]dynamodbtypes.AttributeValue{
		"staging_id": &dynamodbtypes.AttributeValueMemberS{Value: metadata.StagingID},
		"local_path": &dynamodbtypes.AttributeValueMemberS{Value: metadata.LocalPath},
		"s3_key":     &dynamodbtypes.AttributeValueMemberS{Value: metadata.S3Key},
		"created_at": &dynamodbtypes.AttributeValueMemberS{Value: metadata.CreatedAt.Format(time.RFC3339)},
		"size_bytes": &dynamodbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", metadata.SizeBytes)},
		"sha256":     &dynamodbtypes.AttributeValueMemberS{Value: metadata.SHA256},
		"ttl":        &dynamodbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", metadata.TTL)},
	}

	// Add regions list
	regionsAttrs := make([]dynamodbtypes.AttributeValue, len(metadata.Regions))
	for i, region := range metadata.Regions {
		regionsAttrs[i] = &dynamodbtypes.AttributeValueMemberS{Value: region}
	}
	item["regions"] = &dynamodbtypes.AttributeValueMemberL{Value: regionsAttrs}

	// Add optional fields
	if metadata.SweepID != "" {
		item["sweep_id"] = &dynamodbtypes.AttributeValueMemberS{Value: metadata.SweepID}
	}
	if metadata.Destination != "" {
		item["destination"] = &dynamodbtypes.AttributeValueMemberS{Value: metadata.Destination}
	}

	_, err := c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("spawn-staged-data"),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put dynamodb item: %w", err)
	}

	return nil
}

// GetMetadata retrieves staging metadata from DynamoDB
func (c *Client) GetMetadata(ctx context.Context, stagingID string) (*StagingMetadata, error) {
	result, err := c.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("spawn-staged-data"),
		Key: map[string]dynamodbtypes.AttributeValue{
			"staging_id": &dynamodbtypes.AttributeValueMemberS{Value: stagingID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get dynamodb item: %w", err)
	}

	if result.Item == nil {
		return nil, fmt.Errorf("staging ID not found: %s", stagingID)
	}

	metadata := &StagingMetadata{}

	if v, ok := result.Item["staging_id"].(*dynamodbtypes.AttributeValueMemberS); ok {
		metadata.StagingID = v.Value
	}
	if v, ok := result.Item["local_path"].(*dynamodbtypes.AttributeValueMemberS); ok {
		metadata.LocalPath = v.Value
	}
	if v, ok := result.Item["s3_key"].(*dynamodbtypes.AttributeValueMemberS); ok {
		metadata.S3Key = v.Value
	}
	if v, ok := result.Item["created_at"].(*dynamodbtypes.AttributeValueMemberS); ok {
		metadata.CreatedAt, _ = time.Parse(time.RFC3339, v.Value)
	}
	if v, ok := result.Item["size_bytes"].(*dynamodbtypes.AttributeValueMemberN); ok {
		_, _ = fmt.Sscanf(v.Value, "%d", &metadata.SizeBytes)
	}
	if v, ok := result.Item["sha256"].(*dynamodbtypes.AttributeValueMemberS); ok {
		metadata.SHA256 = v.Value
	}
	if v, ok := result.Item["regions"].(*dynamodbtypes.AttributeValueMemberL); ok {
		metadata.Regions = make([]string, len(v.Value))
		for i, region := range v.Value {
			if r, ok := region.(*dynamodbtypes.AttributeValueMemberS); ok {
				metadata.Regions[i] = r.Value
			}
		}
	}
	if v, ok := result.Item["sweep_id"].(*dynamodbtypes.AttributeValueMemberS); ok {
		metadata.SweepID = v.Value
	}
	if v, ok := result.Item["destination"].(*dynamodbtypes.AttributeValueMemberS); ok {
		metadata.Destination = v.Value
	}

	return metadata, nil
}

// ListStagedData lists all staged data
func (c *Client) ListStagedData(ctx context.Context) ([]StagingMetadata, error) {
	result, err := c.dynamoClient.Scan(ctx, &dynamodb.ScanInput{
		TableName: aws.String("spawn-staged-data"),
	})
	if err != nil {
		return nil, fmt.Errorf("scan dynamodb: %w", err)
	}

	var staged []StagingMetadata
	for _, item := range result.Items {
		metadata := StagingMetadata{}
		if v, ok := item["staging_id"].(*dynamodbtypes.AttributeValueMemberS); ok {
			metadata.StagingID = v.Value
		}
		if v, ok := item["local_path"].(*dynamodbtypes.AttributeValueMemberS); ok {
			metadata.LocalPath = v.Value
		}
		if v, ok := item["s3_key"].(*dynamodbtypes.AttributeValueMemberS); ok {
			metadata.S3Key = v.Value
		}
		if v, ok := item["created_at"].(*dynamodbtypes.AttributeValueMemberS); ok {
			metadata.CreatedAt, _ = time.Parse(time.RFC3339, v.Value)
		}
		if v, ok := item["size_bytes"].(*dynamodbtypes.AttributeValueMemberN); ok {
			_, _ = fmt.Sscanf(v.Value, "%d", &metadata.SizeBytes)
		}
		if v, ok := item["regions"].(*dynamodbtypes.AttributeValueMemberL); ok {
			metadata.Regions = make([]string, len(v.Value))
			for i, region := range v.Value {
				if r, ok := region.(*dynamodbtypes.AttributeValueMemberS); ok {
					metadata.Regions[i] = r.Value
				}
			}
		}
		staged = append(staged, metadata)
	}

	return staged, nil
}

// DeleteStaging removes staging metadata from DynamoDB
func (c *Client) DeleteStaging(ctx context.Context, stagingID string) error {
	_, err := c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String("spawn-staged-data"),
		Key: map[string]dynamodbtypes.AttributeValue{
			"staging_id": &dynamodbtypes.AttributeValueMemberS{Value: stagingID},
		},
	})
	return err
}

// UploadScheduleParams uploads a parameter file for scheduled execution
func (c *Client) UploadScheduleParams(ctx context.Context, localPath, scheduleID, region string) (string, int64, string, error) {
	bucket := fmt.Sprintf("spawn-schedules-%s", region)
	s3Key := fmt.Sprintf("schedules/%s/params.yaml", scheduleID)

	// Open file
	file, err := os.Open(localPath)
	if err != nil {
		return "", 0, "", fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	// Calculate SHA256 and size
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, "", fmt.Errorf("calculate hash: %w", err)
	}
	sha256sum := fmt.Sprintf("%x", hash.Sum(nil))

	// Reset file pointer
	if _, err := file.Seek(0, 0); err != nil {
		return "", 0, "", fmt.Errorf("seek file: %w", err)
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
		return "", 0, "", fmt.Errorf("upload to s3: %w", err)
	}

	return s3Key, size, sha256sum, nil
}
