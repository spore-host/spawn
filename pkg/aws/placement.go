package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	smithy "github.com/aws/smithy-go"
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

// DeletePlacementGroup removes a placement group in region. An empty region
// falls back to the client's default region, matching CreatePlacementGroup.
//
// region is a parameter because it used to not be (spawn#685). Create pinned the
// client to the launch region while Delete used ec2.NewFromConfig(c.cfg) — the
// default region — so a cohort launched with --region elsewhere created its group
// in one region and tried to delete it in another. That fails
// InvalidPlacementGroup.Unknown, which is not retryable, so the group was
// abandoned on the very first attempt. Worse, a same-named group really present
// in the default region would have been deleted instead.
func (c *Client) DeletePlacementGroup(ctx context.Context, name, region string) error {
	_, err := c.regionalEC2(region).DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{
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

// placementGroupInUse reports whether err is EC2 refusing to delete a placement
// group that still has members.
//
// Matched on both the modeled API code and the message text, mirroring
// isConcurrentModification in iam.go: wrapped errors and emulators often carry
// only the string.
func placementGroupInUse(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidPlacementGroup.InUse" {
		return true
	}
	return strings.Contains(err.Error(), "InvalidPlacementGroup.InUse")
}

// pgDeleteBudget and pgDeleteInterval bound DeletePlacementGroupWithRetry.
//
// Vars so tests need not sit through the real wait. Short on purpose: an empty
// placement group is FREE, so the only cost of giving up is quota pressure and
// clutter. Blocking a CLI exit for minutes over a free resource would be a worse
// trade than printing the one command that finishes the job.
var (
	pgDeleteBudget   = 60 * time.Second
	pgDeleteInterval = 5 * time.Second
)

// SetPGDeleteWaitForTest shrinks the retry window, restoring it via the returned
// func.
func SetPGDeleteWaitForTest(budget, interval time.Duration) func() {
	prevB, prevI := pgDeleteBudget, pgDeleteInterval
	pgDeleteBudget, pgDeleteInterval = budget, interval
	return func() { pgDeleteBudget, pgDeleteInterval = prevB, prevI }
}

// DeletePlacementGroupWithRetry deletes a placement group, waiting out the
// members' termination.
//
// EC2 refuses to delete a group while any instance still references it, and
// TerminateInstances is asynchronous — so a delete issued right after a drain
// loses the race essentially every time. The failed-cohort path did exactly
// that, reporting `InvalidPlacementGroup.InUse` on every failed MPI launch and
// leaving the group behind; nine were found in one region of one account
// (spawn#685).
//
// Only InUse is retried. A permissions or not-found error is returned at once,
// because waiting 60 seconds to re-learn that the caller cannot delete
// placement groups is worse than saying so immediately.
func (c *Client) DeletePlacementGroupWithRetry(ctx context.Context, name, region string) error {
	return retryPlacementGroupDelete(ctx, name, func() error {
		return c.DeletePlacementGroup(ctx, name, region)
	})
}

// retryPlacementGroupDelete holds the retry policy, separated from the AWS call
// so a test can drive the REAL loop — budget, interval, classifier and all —
// against a scripted sequence of errors. Testing a reimplementation of this loop
// would pass even with the retry deleted, which is the regression it guards.
func retryPlacementGroupDelete(ctx context.Context, name string, del func() error) error {
	deadline := time.Now().Add(pgDeleteBudget)
	for attempt := 1; ; attempt++ {
		err := del()
		if err == nil {
			return nil
		}
		if !placementGroupInUse(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("placement group %s still had members after %s (%d attempts): %w",
				name, pgDeleteBudget, attempt, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pgDeleteInterval):
		}
	}
}
