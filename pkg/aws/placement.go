package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// CreatePlacementGroup creates a cluster placement group for MPI and waits
// until it reaches the "available" state before returning. EC2's CreatePlacementGroup
// is eventually consistent — passing a group in "pending" state to RunInstances
// returns InvalidPlacementGroup.Unknown (fixes #317).
// region must match the launch region; passing an empty string falls back to
// the client's default region.
func (c *Client) CreatePlacementGroup(ctx context.Context, name, region string) error {
	ec2Client := c.regionalEC2(region)

	_, err := ec2Client.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
		GroupName: aws.String(name),
		Strategy:  types.PlacementStrategyCluster,
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypePlacementGroup,
				Tags: []types.Tag{
					{Key: aws.String("spawn:managed"), Value: aws.String("true")},
					{Key: aws.String("spawn:purpose"), Value: aws.String("mpi")},
				},
			},
		},
	})

	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil // Already exists — still need to confirm it's available below
		}
		return fmt.Errorf("create placement group: %w", err)
	}

	// Poll until the placement group reaches "available". Typically takes <5s.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := ec2Client.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{
			GroupNames: []string{name},
		})
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if len(out.PlacementGroups) > 0 && out.PlacementGroups[0].State == types.PlacementGroupStateAvailable {
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("placement group %q did not become available within 30s", name)
}

// DeletePlacementGroup removes a placement group
func (c *Client) DeletePlacementGroup(ctx context.Context, name string) error {
	ec2Client := ec2.NewFromConfig(c.cfg)

	_, err := ec2Client.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{
		GroupName: aws.String(name),
	})
	return err
}

// ValidateInstanceTypeForPlacementGroup checks if instance type supports cluster placement
func (c *Client) ValidateInstanceTypeForPlacementGroup(ctx context.Context, instanceType string) error {
	// Only certain instance families support cluster placement groups:
	// - Compute optimized: c4, c5, c5n, c6g, c6gn, c7g
	// - Memory optimized: r4, r5, r5n, r6g, x1, x1e
	// - Storage optimized: d2, h1, i3, i3en
	// - Accelerated: p2, p3, p4, g3, g4dn, inf1

	supportedPrefixes := []string{
		"c4.", "c5.", "c5n.", "c6g.", "c6gn.", "c7g.",
		"r4.", "r5.", "r5n.", "r6g.",
		"x1.", "x1e.",
		"d2.", "h1.", "i3.", "i3en.",
		"p2.", "p3.", "p4.", "g3.", "g4dn.", "inf1.",
	}

	for _, prefix := range supportedPrefixes {
		if strings.HasPrefix(instanceType, prefix) {
			return nil
		}
	}

	return fmt.Errorf("instance type %s does not support cluster placement groups", instanceType)
}

// ValidateInstanceTypeForEFAInRegion checks if instance type supports EFA by
// querying the EC2 API in the specified launch region. Some instance types
// (e.g. hpc6a.48xlarge) only exist in certain regions and DescribeInstanceTypes
// returns InvalidInstanceType when queried from a different region.
func (c *Client) ValidateInstanceTypeForEFAInRegion(ctx context.Context, instanceType, region string) error {
	ec2Client := c.regionalEC2(region)

	output, err := ec2Client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []types.InstanceType{types.InstanceType(instanceType)},
	})
	if err != nil {
		return fmt.Errorf("describe instance type %s: %w", instanceType, err)
	}
	if len(output.InstanceTypes) == 0 {
		return fmt.Errorf("instance type %s not found", instanceType)
	}

	info := output.InstanceTypes[0]
	if info.NetworkInfo == nil || !aws.ToBool(info.NetworkInfo.EfaSupported) {
		return fmt.Errorf("instance type %s does not support EFA", instanceType)
	}

	return nil
}

// ValidateInstanceTypeForNestedVirtualization checks that the instance type
// supports nested virtualization (running KVM/Hyper-V inside the instance),
// queried in the launch region. Reads ProcessorInfo.SupportedFeatures rather
// than hardcoding the supported families, so new families work automatically.
func (c *Client) ValidateInstanceTypeForNestedVirtualization(ctx context.Context, instanceType, region string) error {
	ec2Client := c.regionalEC2(region)

	output, err := ec2Client.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []types.InstanceType{types.InstanceType(instanceType)},
	})
	if err != nil {
		return fmt.Errorf("describe instance type %s: %w", instanceType, err)
	}
	if len(output.InstanceTypes) == 0 {
		return fmt.Errorf("instance type %s not found", instanceType)
	}

	info := output.InstanceTypes[0]
	if info.ProcessorInfo != nil {
		for _, f := range info.ProcessorInfo.SupportedFeatures {
			if f == types.SupportedAdditionalProcessorFeatureNestedVirtualization {
				return nil
			}
		}
	}
	return fmt.Errorf("instance type %s does not support nested virtualization "+
		"(supported on C8i/M8i/R8i and other types whose ProcessorInfo advertises it)", instanceType)
}
