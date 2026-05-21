package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// AMIInfo represents information about an AMI
type AMIInfo struct {
	AMIID        string
	Name         string
	Description  string
	Architecture string
	CreationDate time.Time
	Size         int64 // Size in GB
	Tags         map[string]string

	// spawn-specific fields
	Stack      string
	Version    string
	GPU        bool
	BaseOS     string
	Deprecated bool
}

// CreateAMIInput contains parameters for creating an AMI
type CreateAMIInput struct {
	InstanceID  string
	Name        string
	Description string
	Tags        map[string]string
	NoReboot    bool
}

// CreateAMI creates an AMI from a running instance
func (c *Client) CreateAMI(ctx context.Context, region string, input CreateAMIInput) (string, error) {
	// Create regional client
	cfg, err := c.getRegionalConfig(ctx, region)
	if err != nil {
		return "", fmt.Errorf("failed to get regional config: %w", err)
	}
	ec2Client := ec2.NewFromConfig(cfg)

	// Build tag specifications
	tagSpecs := []types.TagSpecification{}
	if len(input.Tags) > 0 {
		tags := make([]types.Tag, 0, len(input.Tags))
		for key, value := range input.Tags {
			tags = append(tags, types.Tag{
				Key:   aws.String(key),
				Value: aws.String(value),
			})
		}

		// Tag both the AMI and its snapshots
		tagSpecs = append(tagSpecs,
			types.TagSpecification{
				ResourceType: types.ResourceTypeImage,
				Tags:         tags,
			},
			types.TagSpecification{
				ResourceType: types.ResourceTypeSnapshot,
				Tags:         tags,
			},
		)
	}

	// Create the AMI
	result, err := ec2Client.CreateImage(ctx, &ec2.CreateImageInput{
		InstanceId:        aws.String(input.InstanceID),
		Name:              aws.String(input.Name),
		Description:       aws.String(input.Description),
		NoReboot:          aws.Bool(input.NoReboot),
		TagSpecifications: tagSpecs,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create AMI: %w", err)
	}

	return *result.ImageId, nil
}

// WaitForAMI waits for an AMI to become available
func (c *Client) WaitForAMI(ctx context.Context, region string, amiID string, timeout time.Duration) error {
	// Create regional client
	cfg, err := c.getRegionalConfig(ctx, region)
	if err != nil {
		return fmt.Errorf("failed to get regional config: %w", err)
	}
	ec2Client := ec2.NewFromConfig(cfg)

	// Create waiter
	waiter := ec2.NewImageAvailableWaiter(ec2Client)

	// Wait for AMI to be available
	err = waiter.Wait(ctx, &ec2.DescribeImagesInput{
		ImageIds: []string{amiID},
	}, timeout)
	if err != nil {
		return fmt.Errorf("failed waiting for AMI: %w", err)
	}

	return nil
}

// ListAMIs lists AMIs with optional filters
// Filters are applied in-memory after retrieving all AMIs owned by the account
func (c *Client) ListAMIs(ctx context.Context, region string, filters map[string]string) ([]AMIInfo, error) {
	// Create regional client
	cfg, err := c.getRegionalConfig(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("failed to get regional config: %w", err)
	}
	ec2Client := ec2.NewFromConfig(cfg)

	// Get current account ID
	accountID, err := c.GetAccountID(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get account ID: %w", err)
	}

	// Build EC2 filters - only filter by owner
	ec2Filters := []types.Filter{
		{
			Name:   aws.String("owner-id"),
			Values: []string{accountID},
		},
	}

	// List AMIs (all AMIs owned by this account)
	result, err := ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Filters: ec2Filters,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list AMIs: %w", err)
	}

	// Convert to AMIInfo
	amis := make([]AMIInfo, 0, len(result.Images))
	for _, img := range result.Images {
		// Parse tags
		tags := make(map[string]string)
		for _, tag := range img.Tags {
			if tag.Key != nil && tag.Value != nil {
				tags[*tag.Key] = *tag.Value
			}
		}

		// Parse creation date
		var creationDate time.Time
		if img.CreationDate != nil {
			creationDate, _ = time.Parse(time.RFC3339, *img.CreationDate)
		}

		// Calculate total size from block device mappings
		var totalSize int64
		if img.BlockDeviceMappings != nil {
			for _, bdm := range img.BlockDeviceMappings {
				if bdm.Ebs != nil && bdm.Ebs.VolumeSize != nil {
					totalSize += int64(*bdm.Ebs.VolumeSize)
				}
			}
		}

		// Extract spawn-specific tags (check both namespaced and non-namespaced)
		stack := tags["spawn:stack"]
		if stack == "" {
			stack = tags["stack"]
		}
		version := tags["spawn:version"]
		if version == "" {
			version = tags["version"]
		}
		gpu := tags["spawn:gpu"] == "true"
		baseOS := tags["spawn:base"]
		if baseOS == "" {
			baseOS = tags["base"]
		}
		deprecated := tags["spawn:deprecated"] == "true"

		amiInfo := AMIInfo{
			AMIID:        aws.ToString(img.ImageId),
			Name:         aws.ToString(img.Name),
			Description:  aws.ToString(img.Description),
			Architecture: string(img.Architecture),
			CreationDate: creationDate,
			Size:         totalSize,
			Tags:         tags,
			Stack:        stack,
			Version:      version,
			GPU:          gpu,
			BaseOS:       baseOS,
			Deprecated:   deprecated,
		}

		amis = append(amis, amiInfo)
	}

	return amis, nil
}
