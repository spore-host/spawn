package aws

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	cwl "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	rgt "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
)

// ManagedResource is one spawn-created AWS resource discovered by tag.
type ManagedResource struct {
	ARN          string
	Service      string // ec2, iam, dynamodb, logs, …
	ResourceType string // instance, security-group, key-pair, table, address, …
	ID           string // the resource id parsed from the ARN (best-effort)
	Region       string
	Tags         map[string]string
	// State is filled for EC2 instances (running/stopped/…) and for Elastic IPs
	// (address); empty otherwise. Address states are synthesized by scanAddresses:
	// "unassociated" (billable, attached to nothing) or "assoc:<instance-state>"
	// (attached to an instance in that state — billable while that state is not
	// running).
	State string
	// PublicIP / AssociationID are filled only for ResourceType == "address".
	// spawn never allocates EIPs, so these describe a USER-owned address surfaced
	// for visibility — spawn reports it, never releases it (#262).
	PublicIP      string
	AssociationID string
}

// IsRunningInstance reports whether this is an EC2 instance still running or
// pending — cleanup must never touch these.
func (r ManagedResource) IsRunningInstance() bool {
	return r.ResourceType == "instance" && (r.State == "running" || r.State == "pending")
}

// DiscoverOptions scopes a managed-resource discovery sweep.
type DiscoverOptions struct {
	// Region to search. Empty uses the client's configured region.
	Region string
	// OnlyMine restricts results to resources tagged with the caller's
	// spawn:iam-user (the principal that created them). When false, every
	// spawn:managed resource in the account/region is returned.
	OnlyMine bool
}

// DiscoverManagedResources finds spawn-created resources in one region via the
// Resource Groups Tagging API (a single tag query across all taggable services),
// then enriches EC2 instances with their state so cleanup can refuse running
// ones. Results are scoped to spawn:managed=true and, when OnlyMine is set, to
// the caller's spawn:iam-user — the identity already stamped on every resource
// (#259), so cleanup acts only on what the caller owns.
func (c *Client) DiscoverManagedResources(ctx context.Context, opts DiscoverOptions) ([]ManagedResource, error) {
	cfg := c.regionalConfig(opts.Region)
	region := cfg.Region

	var callerARN string
	if opts.OnlyMine {
		_, userARN, err := c.GetCallerIdentityInfo(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve caller identity for --mine scope: %w", err)
		}
		callerARN = userARN
	}

	rgtClient := rgt.NewFromConfig(cfg)
	filters := []rgttypes.TagFilter{{Key: aws.String("spawn:managed"), Values: []string{"true"}}}

	var resources []ManagedResource
	paginator := rgt.NewGetResourcesPaginator(rgtClient, &rgt.GetResourcesInput{
		TagFilters: filters,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list tagged resources in %s: %w", region, err)
		}
		for _, m := range page.ResourceTagMappingList {
			arn := aws.ToString(m.ResourceARN)
			tags := map[string]string{}
			for _, t := range m.Tags {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			if opts.OnlyMine && tags["spawn:iam-user"] != callerARN {
				continue
			}
			svc, rtype, id := parseARN(arn)
			resources = append(resources, ManagedResource{
				ARN:          arn,
				Service:      svc,
				ResourceType: rtype,
				ID:           id,
				Region:       region,
				Tags:         tags,
			})
		}
	}

	// Enrich EC2 instances and volumes with their state — the tagging API
	// doesn't carry it, and cleanup/orphan decisions hinge on it (running vs.
	// stopped instances; available vs. in-use volumes).
	if err := c.enrichInstanceState(ctx, cfg, resources); err != nil {
		return nil, err
	}
	if err := c.enrichVolumeState(ctx, cfg, resources); err != nil {
		return nil, err
	}

	// Elastic IPs don't come back from the Resource Groups Tagging API (ec2:Address
	// isn't a taggable-in-RGT type here) and spawn never tags them anyway, so scan
	// them separately and append. Runs AFTER instance enrichment so an EIP attached
	// to a stopped instance can be classified as a billable leak (#262).
	addrs, err := c.scanAddresses(ctx, cfg, region, resources)
	if err != nil {
		return nil, err
	}
	resources = append(resources, addrs...)

	sort.Slice(resources, func(i, j int) bool { return resources[i].ARN < resources[j].ARN })
	return resources, nil
}

// describeAddressesAPI is the slice of EC2 that scanAddresses needs.
type describeAddressesAPI interface {
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
}

// scanAddresses lists Elastic IPs and returns the ones that look like billable
// leaks as synthetic address ManagedResources — for VISIBILITY only. spawn never
// allocates an EIP, so every address here is a user-owned static address; spawn
// reports it and NEVER releases it. Two leak shapes are surfaced:
//   - unassociated (attached to nothing) — always billable.
//   - associated to a spawn-managed instance that is NOT running (stopped) — an
//     EIP on a stopped instance still bills.
//
// An EIP attached to a running spawn instance is legitimate and is skipped.
// Addresses attached to instances spawn doesn't manage are ignored entirely.
func (c *Client) scanAddresses(ctx context.Context, cfg aws.Config, region string, resources []ManagedResource) ([]ManagedResource, error) {
	// Map instance-id -> state for the spawn-managed instances we just enriched.
	instState := map[string]string{}
	for _, r := range resources {
		if r.ResourceType == "instance" && r.ID != "" {
			instState[r.ID] = r.State
		}
	}

	var api describeAddressesAPI = ec2.NewFromConfig(cfg)
	out, err := api.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err != nil {
		return nil, fmt.Errorf("describe elastic IPs in %s: %w", region, err)
	}
	return classifyAddresses(out.Addresses, instState, region), nil
}

// classifyAddresses is the pure decision core of scanAddresses (testable without
// AWS): given the raw addresses and the spawn-managed instance states, it returns
// the billable-leak addresses as ManagedResources.
func classifyAddresses(addresses []ec2types.Address, instState map[string]string, region string) []ManagedResource {
	var out []ManagedResource
	for _, a := range addresses {
		instID := aws.ToString(a.InstanceId)
		assoc := aws.ToString(a.AssociationId)

		var state string
		switch {
		case instID == "" && assoc == "":
			// Attached to nothing — always billable.
			state = "unassociated"
		case instID != "":
			st, managed := instState[instID]
			if !managed {
				// Attached to an instance spawn doesn't manage — not our concern.
				continue
			}
			if st == "running" || st == "pending" {
				// Legitimately in use by a live spawn instance — not a leak.
				continue
			}
			// Attached to a stopped/other spawn instance — billable leak.
			state = "assoc:" + st
		default:
			// Associated to an ENI but no instance id — skip (rare; not a clear leak).
			continue
		}

		out = append(out, ManagedResource{
			Service:       "ec2",
			ResourceType:  "address",
			ID:            aws.ToString(a.AllocationId),
			Region:        region,
			State:         state,
			PublicIP:      aws.ToString(a.PublicIp),
			AssociationID: assoc,
			Tags:          map[string]string{},
		})
	}
	return out
}

// ElasticIP describes an Elastic IP attached to an instance (for status output).
type ElasticIP struct {
	PublicIP     string
	AllocationID string
}

// GetInstanceElasticIP returns any Elastic IP associated with the given instance,
// or nil if none. Used by `spawn status` to surface an attached EIP so the user
// sees the (billable) static address they're holding — informational when the
// instance runs, a leak warning when it's stopped. spawn never releases it.
func (c *Client) GetInstanceElasticIP(ctx context.Context, region, instanceID string) (*ElasticIP, error) {
	ec2c := c.regionalEC2(region)
	out, err := ec2c.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		Filters: []ec2types.Filter{{Name: aws.String("instance-id"), Values: []string{instanceID}}},
	})
	if err != nil {
		return nil, fmt.Errorf("describe elastic IPs for %s: %w", instanceID, err)
	}
	for _, a := range out.Addresses {
		return &ElasticIP{PublicIP: aws.ToString(a.PublicIp), AllocationID: aws.ToString(a.AllocationId)}, nil
	}
	return nil, nil
}

// enrichInstanceState fills the State field for any EC2 instance resources.
func (c *Client) enrichInstanceState(ctx context.Context, cfg aws.Config, resources []ManagedResource) error {
	var ids []string
	idx := map[string]int{}
	for i, r := range resources {
		if r.ResourceType == "instance" && r.ID != "" {
			ids = append(ids, r.ID)
			idx[r.ID] = i
		}
	}
	if len(ids) == 0 {
		return nil
	}

	ec2Client := ec2.NewFromConfig(cfg)
	out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		// EC2 fails the WHOLE batch with InvalidInstanceID.NotFound if ANY id is
		// already gone. Don't lose state for the survivors — fall back to a per-id
		// sweep so live instances still get their real state and only the truly
		// missing ones are marked deleted.
		if isInstanceNotFound(err) {
			return c.enrichInstanceStatePerID(ctx, ec2Client, resources, idx)
		}
		return fmt.Errorf("describe instance state: %w", err)
	}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			id := aws.ToString(inst.InstanceId)
			if i, ok := idx[id]; ok {
				resources[i].State = string(inst.State.Name)
			}
		}
	}
	return nil
}

// describeInstancesAPI is the slice of EC2 that the per-id instance fallback
// needs — narrow enough to fake in tests (the substrate emulator doesn't
// reproduce the batch-NotFound failure this path exists to handle).
type describeInstancesAPI interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// enrichInstanceStatePerID is the fallback when a batched DescribeInstances hits
// a NotFound: it queries each id individually, filling State for the ones that
// still exist and marking the rest "deleted" so a gone instance is never
// mistaken for one whose state is merely unknown.
func (c *Client) enrichInstanceStatePerID(ctx context.Context, ec2Client describeInstancesAPI, resources []ManagedResource, idx map[string]int) error {
	for id, i := range idx {
		out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
		if err != nil {
			if isInstanceNotFound(err) {
				resources[i].State = "deleted"
				continue
			}
			return fmt.Errorf("describe instance state %s: %w", id, err)
		}
		for _, res := range out.Reservations {
			for _, inst := range res.Instances {
				resources[i].State = string(inst.State.Name)
			}
		}
	}
	return nil
}

// enrichVolumeState fills the State field for any EBS volume resources.
func (c *Client) enrichVolumeState(ctx context.Context, cfg aws.Config, resources []ManagedResource) error {
	var ids []string
	idx := map[string]int{}
	for i, r := range resources {
		if r.ResourceType == "volume" && r.ID != "" {
			ids = append(ids, r.ID)
			idx[r.ID] = i
		}
	}
	if len(ids) == 0 {
		return nil
	}

	ec2Client := ec2.NewFromConfig(cfg)
	out, err := ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: ids})
	if err != nil {
		// As with instances, one already-deleted volume 400s the whole batch. Fall
		// back to per-id so surviving volumes keep their real state — otherwise
		// every volume's State stays blank and IsLikelyOrphan mis-flags them all.
		if isVolumeNotFound(err) {
			return c.enrichVolumeStatePerID(ctx, ec2Client, resources, idx)
		}
		return fmt.Errorf("describe volume state: %w", err)
	}
	for _, v := range out.Volumes {
		if i, ok := idx[aws.ToString(v.VolumeId)]; ok {
			resources[i].State = string(v.State)
		}
	}
	return nil
}

// describeVolumesAPI is the slice of EC2 the per-id volume fallback needs.
type describeVolumesAPI interface {
	DescribeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
}

// enrichVolumeStatePerID is the per-id fallback for a NotFound batch (mirrors
// enrichInstanceStatePerID): live volumes get their real state, genuinely gone
// ones are marked "deleted" rather than left blank.
func (c *Client) enrichVolumeStatePerID(ctx context.Context, ec2Client describeVolumesAPI, resources []ManagedResource, idx map[string]int) error {
	for id, i := range idx {
		out, err := ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}})
		if err != nil {
			if isVolumeNotFound(err) {
				resources[i].State = "deleted"
				continue
			}
			return fmt.Errorf("describe volume state %s: %w", id, err)
		}
		for _, v := range out.Volumes {
			resources[i].State = string(v.State)
		}
	}
	return nil
}

// parseARN extracts (service, resourceType, id) from an AWS ARN, best-effort.
// arn:partition:service:region:account:resourceType/id  — or  …:resourceType:id
func parseARN(arn string) (service, resourceType, id string) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return "", "", ""
	}
	service = parts[2]
	tail := parts[5]
	// tail is "type/id", "type/id/sub", or "type:id".
	sep := strings.IndexAny(tail, "/:")
	if sep < 0 {
		return service, tail, ""
	}
	resourceType = tail[:sep]
	id = tail[sep+1:]
	return service, resourceType, id
}

// IsLikelyOrphan reports whether a managed resource looks abandoned.
// hasRunningInstance is whether any spawn-managed instance is running/pending in
// the same region: shared infra (security group, key pair, IAM role/profile) is
// only an orphan once nothing is running. EBS volumes in 'available' state are
// orphans regardless (nothing is attached to them).
func IsLikelyOrphan(r ManagedResource, hasRunningInstance bool) bool {
	switch {
	case r.ResourceType == "instance":
		return false // instances aren't orphans; they're the thing infra serves
	case r.ResourceType == "volume":
		// Only a genuinely detached ('available') volume is an orphan. A blank or
		// 'deleted' state means the volume is gone or its state couldn't be
		// resolved — NOT an orphan (nothing to clean, and treating unknown as
		// orphan is what made 'orphans' report already-deleted volumes, #262).
		return r.State == "available"
	case r.ResourceType == "security-group", r.ResourceType == "key-pair":
		return !hasRunningInstance
	case r.Service == "iam":
		return !hasRunningInstance
	case r.ResourceType == "address":
		// scanAddresses only ever emits billable-leak EIPs (unassociated, or
		// attached to a non-running spawn instance), so any address that made it
		// this far is a leak worth surfacing. spawn reports it but never releases
		// it — the user owns the address (#262).
		return true
	default:
		// Tables/log groups are long-lived control-plane state, not per-run
		// orphans — don't flag them here.
		return false
	}
}

// DeletionOrder sorts resources so dependents are removed before their
// dependencies: instances → security groups / key pairs → IAM → tables/logs.
func DeletionOrder(resources []ManagedResource) []ManagedResource {
	rank := func(r ManagedResource) int {
		switch {
		case r.ResourceType == "instance":
			return 0
		case r.ResourceType == "security-group":
			return 1
		case r.ResourceType == "key-pair":
			return 2
		case r.ResourceType == "volume":
			return 3
		case r.Service == "iam" && r.ResourceType == "instance-profile":
			return 4
		case r.Service == "iam" && r.ResourceType == "role":
			return 5
		case r.Service == "logs":
			return 6
		case r.Service == "dynamodb":
			return 7
		default:
			return 8
		}
	}
	out := make([]ManagedResource, len(resources))
	copy(out, resources)
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// RemoveResource deletes one discovered resource. It is best-effort and
// idempotent: an already-deleted resource is treated as success. It refuses to
// remove a running/pending EC2 instance — cleanup never terminates live compute
// (#259); callers must stop/terminate those explicitly.
func (c *Client) RemoveResource(ctx context.Context, r ManagedResource) error {
	cfg := c.regionalConfig(r.Region)

	switch {
	case r.ResourceType == "instance":
		if r.IsRunningInstance() {
			return fmt.Errorf("refusing to remove running instance %s — stop or terminate it first", r.ID)
		}
		// stopped/stopping instances: terminate.
		return c.Terminate(ctx, cfg.Region, r.ID)

	case r.ResourceType == "security-group":
		ec2c := ec2.NewFromConfig(cfg)
		_, err := ec2c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(r.ID)})
		return ignoreNotFound(err, "InvalidGroup.NotFound")

	case r.ResourceType == "key-pair":
		ec2c := ec2.NewFromConfig(cfg)
		// r.ID is the key-pair id (key-xxxx); delete by id.
		_, err := ec2c.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{KeyPairId: aws.String(r.ID)})
		return ignoreNotFound(err, "InvalidKeyPair.NotFound")

	case r.ResourceType == "volume":
		ec2c := ec2.NewFromConfig(cfg)
		_, err := ec2c.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(r.ID)})
		return ignoreNotFound(err, "InvalidVolume.NotFound")

	case r.ResourceType == "address":
		// spawn never allocates an Elastic IP, so it never releases one — the
		// address is the user's. cleanup reports it (via orphans) but must not
		// destroy it: releasing an EIP is irreversible and it isn't ours (#262).
		return fmt.Errorf("refusing to release Elastic IP %s (%s) — spawn does not own it; "+
			"release it yourself with 'aws ec2 release-address --allocation-id %s' if unneeded", r.ID, r.PublicIP, r.ID)

	case r.Service == "iam" && r.ResourceType == "instance-profile":
		return c.deleteInstanceProfile(ctx, cfg, r.ID)

	case r.Service == "iam" && r.ResourceType == "role":
		return c.deleteRole(ctx, cfg, r.ID)

	case r.Service == "logs":
		// r.ID for a log-group ARN tail is "log-group:<name>[:*]"; normalize.
		name := strings.TrimSuffix(strings.TrimPrefix(r.ID, "log-group:"), ":*")
		logc := cwl.NewFromConfig(cfg)
		_, err := logc.DeleteLogGroup(ctx, &cwl.DeleteLogGroupInput{LogGroupName: aws.String(name)})
		return ignoreNotFound(err, "ResourceNotFoundException")

	case r.Service == "dynamodb":
		ddb := dynamodb.NewFromConfig(cfg)
		_, err := ddb.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(r.ID)})
		return ignoreNotFound(err, "ResourceNotFoundException")

	default:
		return fmt.Errorf("don't know how to remove %s resource %q", r.Service, r.ResourceType)
	}
}

// deleteRole detaches managed policies, deletes inline policies, removes the
// role from any instance profiles, then deletes the role.
func (c *Client) deleteRole(ctx context.Context, cfg aws.Config, roleName string) error {
	iamc := iam.NewFromConfig(cfg)

	// Inline policies
	if lp, err := iamc.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(roleName)}); err == nil {
		for _, p := range lp.PolicyNames {
			_, _ = iamc.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(roleName), PolicyName: aws.String(p)})
		}
	}
	// Attached managed policies
	if ap, err := iamc.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(roleName)}); err == nil {
		for _, p := range ap.AttachedPolicies {
			_, _ = iamc.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(roleName), PolicyArn: p.PolicyArn})
		}
	}
	// Remove from instance profiles
	if ipl, err := iamc.ListInstanceProfilesForRole(ctx, &iam.ListInstanceProfilesForRoleInput{RoleName: aws.String(roleName)}); err == nil {
		for _, ip := range ipl.InstanceProfiles {
			_, _ = iamc.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
				InstanceProfileName: ip.InstanceProfileName, RoleName: aws.String(roleName),
			})
		}
	}
	_, err := iamc.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	return ignoreNotFound(err, "NoSuchEntity")
}

// deleteInstanceProfile removes any roles from the profile, then deletes it.
func (c *Client) deleteInstanceProfile(ctx context.Context, cfg aws.Config, profileName string) error {
	iamc := iam.NewFromConfig(cfg)
	if got, err := iamc.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(profileName)}); err == nil {
		for _, role := range got.InstanceProfile.Roles {
			_, _ = iamc.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
				InstanceProfileName: aws.String(profileName), RoleName: role.RoleName,
			})
		}
	}
	_, err := iamc.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(profileName)})
	return ignoreNotFound(err, "NoSuchEntity")
}

// ignoreNotFound treats an already-deleted resource as success.
func ignoreNotFound(err error, notFoundCode string) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), notFoundCode) {
		return nil
	}
	return err
}
