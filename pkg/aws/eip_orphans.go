package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// LaunchInstanceGone is the LaunchInstanceState of a spawn-allocated Elastic IP
// whose tagged instance DescribeInstances no longer returns at all.
const LaunchInstanceGone = "gone"

// IsOrphanedSpawnAddress is THE definition of an orphaned launch Elastic IP on
// the CLI side (#1232): an address spawn allocated for a multi-interface EFA
// launch (#661, IsSpawnAllocatedAddress), associated with nothing, whose tagged
// instance is terminated or no longer exists. `spawn cleanup` releases exactly
// these, and `spawn list` / `spawn status` warn about exactly these.
//
// The instance check matters because an unassociated launch address is not
// always an orphan: Launch allocates it after the instance is running and
// associates it a moment later, so in that window it is unassociated with a
// live instance. Requiring a looked-up "terminated" or "gone" also fails closed:
// an address whose instance was never resolved (LaunchInstanceState "") is not
// an orphan. It matches the TTL reaper's sweep, which treats pending, running,
// stopping, stopped and shutting-down as live.
func IsOrphanedSpawnAddress(r ManagedResource) bool {
	return IsSpawnAllocatedAddress(r) && r.State == "unassociated" &&
		(r.LaunchInstanceState == "terminated" || r.LaunchInstanceState == LaunchInstanceGone)
}

// OwnedBy reports whether r is the caller's under cleanup's default scope: it
// carries callerARN as its spawn:iam-user, or no owner at all.
func (r ManagedResource) OwnedBy(callerARN string) bool {
	return ownedByCaller(r.Tags, callerARN)
}

// resolveLaunchAddressInstances fills LaunchInstanceState for every
// spawn-allocated, unassociated address in rs, with ONE paginated
// DescribeInstances per call however many addresses there are (an instance-id
// filter, not InstanceIds, so an ID EC2 has forgotten is simply absent rather
// than failing the whole call). Addresses that are associated, or not spawn's,
// are left alone. On error nothing is filled, so nothing qualifies as orphaned.
func resolveLaunchAddressInstances(ctx context.Context, api describeInstancesAPI, rs []ManagedResource) error {
	var ids []string
	seen := map[string]bool{}
	for _, r := range rs {
		if !IsSpawnAllocatedAddress(r) || r.State != "unassociated" {
			continue
		}
		if id := r.Tags[ElasticIPInstanceTagKey]; !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	states := map[string]string{}
	// EC2 accepts at most 200 values per filter.
	for start := 0; start < len(ids); start += 200 {
		end := min(start+200, len(ids))
		in := &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: ids[start:end]}},
		}
		p := ec2.NewDescribeInstancesPaginator(api, in)
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return fmt.Errorf("look up the instances of spawn Elastic IPs: %w", err)
			}
			for _, res := range page.Reservations {
				for _, inst := range res.Instances {
					if inst.State != nil {
						states[aws.ToString(inst.InstanceId)] = string(inst.State.Name)
					}
				}
			}
		}
	}
	for i := range rs {
		if !IsSpawnAllocatedAddress(rs[i]) || rs[i].State != "unassociated" {
			continue
		}
		st, ok := states[rs[i].Tags[ElasticIPInstanceTagKey]]
		if !ok {
			st = LaunchInstanceGone
		}
		rs[i].LaunchInstanceState = st
	}
	return nil
}

// orphanAddressAPI is the slice of EC2 FindOrphanedSpawnAddresses needs.
type orphanAddressAPI interface {
	describeAddressesAPI
	describeInstancesAPI
}

// FindOrphanedSpawnAddresses returns the region's orphaned launch Elastic IPs
// (IsOrphanedSpawnAddress), whoever launched them. It costs one
// DescribeAddresses, plus one DescribeInstances only when a spawn-allocated
// address is unassociated. Read-only; `spawn cleanup --elastic-ips` releases
// what it returns.
func (c *Client) FindOrphanedSpawnAddresses(ctx context.Context, region string) ([]ManagedResource, error) {
	return findOrphanedSpawnAddresses(ctx, c.regionalEC2(region), c.regionalConfig(region).Region)
}

func findOrphanedSpawnAddresses(ctx context.Context, api orphanAddressAPI, region string) ([]ManagedResource, error) {
	out, err := api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: []ec2types.Filter{{Name: aws.String("tag:spawn:managed"), Values: []string{"true"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("describe Elastic IPs in %s: %w", region, err)
	}
	eipTags := addressTags(out.Addresses)
	// No instance states: every associated address is skipped as "not a spawn
	// instance's", which is what this wants — only unassociated ones remain.
	candidates := classifyAddresses(out.Addresses, eipTags, nil, region)
	if err := resolveLaunchAddressInstances(ctx, api, candidates); err != nil {
		return nil, err
	}
	var orphans []ManagedResource
	for _, r := range candidates {
		if IsOrphanedSpawnAddress(r) {
			orphans = append(orphans, r)
		}
	}
	return orphans, nil
}

// addressTags flattens each address's tags into a map, index-aligned with
// addresses, as classifyAddresses expects.
func addressTags(addresses []ec2types.Address) []map[string]string {
	eipTags := make([]map[string]string, len(addresses))
	for i, a := range addresses {
		eipTags[i] = map[string]string{}
		for _, t := range a.Tags {
			if k := aws.ToString(t.Key); k != "" {
				eipTags[i][k] = aws.ToString(t.Value)
			}
		}
	}
	return eipTags
}
