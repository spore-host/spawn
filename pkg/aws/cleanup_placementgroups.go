package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// placementGroupAPI is the slice of EC2 that the placement-group sweep needs.
type placementGroupAPI interface {
	DescribePlacementGroups(ctx context.Context, in *ec2.DescribePlacementGroupsInput, optFns ...func(*ec2.Options)) (*ec2.DescribePlacementGroupsOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// scanPlacementGroups lists spawn-managed cluster placement groups and returns
// them as ManagedResources, with State set to "empty" or "in-use".
//
// Scanned separately rather than coming from the Resource Groups Tagging API
// like security groups and volumes do — spawn tags every placement group it
// creates with spawn:managed=true, but RGT does not return them, so they were
// invisible to both 'spawn orphans' and 'spawn cleanup' (spawn#685). An MPI
// cohort creates one group per AZ it tries, and before the fixes in this change
// a rejected or failed cohort abandoned every one of them: nine were found in a
// single region of one account, none of which any spawn command could see.
//
// A placement group is free, so this is quota hygiene rather than cost. The
// per-VPC limit is what eventually bites — and it bites at launch time, which is
// the worst moment to discover it.
//
// DiscoverOptions.OnlyMine is deliberately NOT applied, matching scanAddresses:
// spawn does not stamp spawn:iam-user on a placement group, so honouring the
// filter would drop every one of them and leave the sweep reporting nothing at
// all. Visibility wins for a leak report. (The same omission makes the
// tagging-API side of --mine silently empty for security groups and IAM
// profiles, which is spawn#708 — not papered over here.)
func (c *Client) scanPlacementGroups(ctx context.Context, cfg aws.Config, region string) ([]ManagedResource, error) {
	var api placementGroupAPI = ec2.NewFromConfig(cfg)
	return scanPlacementGroupsWith(ctx, api, region)
}

// scanPlacementGroupsWith is the testable core: it takes the narrow API slice so
// the membership logic is exercised without real AWS. The membership question is
// the part worth testing — deleting a group that still has members fails, and
// reporting an in-use group as an orphan would send the operator after something
// they must not remove.
func scanPlacementGroupsWith(ctx context.Context, api placementGroupAPI, region string) ([]ManagedResource, error) {
	out, err := api.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:spawn:managed"), Values: []string{"true"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe placement groups in %s: %w", region, err)
	}
	if len(out.PlacementGroups) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(out.PlacementGroups))
	for _, pg := range out.PlacementGroups {
		if n := aws.ToString(pg.GroupName); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}

	occupied, err := placementGroupMembers(ctx, api, names)
	if err != nil {
		return nil, err
	}

	var resources []ManagedResource
	for _, pg := range out.PlacementGroups {
		name := aws.ToString(pg.GroupName)
		if name == "" {
			continue
		}
		tags := map[string]string{}
		for _, t := range pg.Tags {
			if k := aws.ToString(t.Key); k != "" {
				tags[k] = aws.ToString(t.Value)
			}
		}
		state := "empty"
		if occupied[name] {
			state = "in-use"
		}
		resources = append(resources, ManagedResource{
			// Synthesized: a placement group's ARN tail is its NAME, not an id,
			// and DeletePlacementGroup takes the name.
			ARN:          fmt.Sprintf("arn:aws:ec2:%s::placement-group/%s", region, name),
			Service:      "ec2",
			ResourceType: "placement-group",
			ID:           name,
			Region:       region,
			State:        state,
			Tags:         tags,
		})
	}
	return resources, nil
}

// placementGroupMembers reports which of names still has a non-terminated
// instance in it.
//
// Deliberately asked of EC2 rather than inferred from the spawn-managed instance
// list: a group with ANY member cannot be deleted, including a member spawn did
// not create. Inferring from spawn's own instances would report such a group as
// empty and then fail to delete it.
func placementGroupMembers(ctx context.Context, api placementGroupAPI, names []string) (map[string]bool, error) {
	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("placement-group-name"), Values: names},
			// "terminated" and "shutting-down" are excluded: a terminated instance
			// holds nothing. shutting-down DOES still block the delete, which is
			// what DeletePlacementGroupWithRetry waits out — but a cohort that is
			// mid-termination is not an orphan, so leave it out of the report and
			// let the next sweep pick it up.
			{Name: aws.String("instance-state-name"), Values: []string{
				"pending", "running", "stopping", "stopped",
			}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("describe placement-group members: %w", err)
	}
	occupied := map[string]bool{}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			if inst.Placement == nil {
				continue
			}
			if n := aws.ToString(inst.Placement.GroupName); n != "" {
				occupied[n] = true
			}
		}
	}
	return occupied, nil
}
