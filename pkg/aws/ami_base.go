package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// GetInstanceAMI returns the AMI ID used to launch an instance
func (c *Client) GetInstanceAMI(ctx context.Context, region string, instanceID string) (string, error) {
	cfg, err := c.getRegionalConfig(ctx, region)
	if err != nil {
		return "", fmt.Errorf("failed to get regional config: %w", err)
	}

	ec2Client := ec2.NewFromConfig(cfg)

	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe instance: %w", err)
	}

	if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance %s not found", instanceID)
	}

	instance := result.Reservations[0].Instances[0]
	if instance.ImageId == nil {
		return "", fmt.Errorf("instance has no AMI ID")
	}

	return *instance.ImageId, nil
}
