package aws

import (
	"context"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// InstanceTypeOfferedInRegion reports whether EC2 offers instanceType in region.
//
// This is the question spawn's region auto-detection was never asking (#732): it
// took an instance type as a parameter and ranked regions purely by TCP latency,
// so a launch could be placed in a region that does not offer the type at all —
// failing at RunInstances with EC2's opaque `Unsupported: The requested
// configuration is currently not supported`.
//
// DescribeInstanceTypeOfferings answers it definitively in one call, and is
// authoritative in a way nothing else available here is: an instance type being
// absent from the pricing table, for instance, is suggestive but also what a
// stale table looks like.
//
// The region is passed explicitly and the client is pinned to it, because an
// offering is per-region by definition and answering from the ambient region
// would make every candidate report the same thing.
func (c *Client) InstanceTypeOfferedInRegion(ctx context.Context, region, instanceType string) (bool, error) {
	if instanceType == "" {
		return true, nil // nothing asked for, nothing to exclude
	}
	out, err := c.regionalEC2(region).DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{
		// LocationType region (not availability-zone): "can this region run it at
		// all". AZ-level placement is decided later, and a type offered in only
		// some AZs of a region is still offered in the region.
		LocationType: ec2types.LocationTypeRegion,
		Filters: []ec2types.Filter{
			{Name: awssdk.String("instance-type"), Values: []string{instanceType}},
			{Name: awssdk.String("location"), Values: []string{region}},
		},
	})
	if err != nil {
		return false, err
	}
	return len(out.InstanceTypeOfferings) > 0, nil
}
