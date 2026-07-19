package aws

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// DescribeAvailabilityZones returns the names of the standard, available
// Availability Zones in region (e.g. "us-east-1a"), sorted for determinism.
// It filters to zone-type "availability-zone" — Local Zones and Wavelength
// zones are excluded because they don't support cluster placement groups or
// most MPI-capable instance types, so they can't participate in an MPI cohort's
// AZ-fallback chain. Mirrors getAllRegions (client.go).
func (c *Client) DescribeAvailabilityZones(ctx context.Context, region string) ([]string, error) {
	ec2Client := c.regionalEC2(region)

	out, err := ec2Client.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []types.Filter{
			{Name: aws.String("state"), Values: []string{"available"}},
			{Name: aws.String("zone-type"), Values: []string{"availability-zone"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe availability zones in %s: %w", region, err)
	}

	zones := make([]string, 0, len(out.AvailabilityZones))
	for _, z := range out.AvailabilityZones {
		if z.ZoneName != nil {
			zones = append(zones, *z.ZoneName)
		}
	}
	sort.Strings(zones)
	return zones, nil
}
