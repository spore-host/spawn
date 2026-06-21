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
	"github.com/aws/aws-sdk-go-v2/service/iam"
	rgt "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	rgttypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
)

// ManagedResource is one spawn-created AWS resource discovered by tag.
type ManagedResource struct {
	ARN          string
	Service      string // ec2, iam, dynamodb, logs, …
	ResourceType string // instance, security-group, key-pair, table, …
	ID           string // the resource id parsed from the ARN (best-effort)
	Region       string
	Tags         map[string]string
	// State is filled for EC2 instances (running/stopped/…); empty otherwise.
	State string
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
	cfg := c.cfg.Copy()
	if opts.Region != "" {
		cfg.Region = opts.Region
	}
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

	sort.Slice(resources, func(i, j int) bool { return resources[i].ARN < resources[j].ARN })
	return resources, nil
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
		// A NotFound for an already-gone instance shouldn't abort the sweep.
		if strings.Contains(err.Error(), "InvalidInstanceID.NotFound") {
			return nil
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
		if strings.Contains(err.Error(), "InvalidVolume.NotFound") {
			return nil
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
		return r.State == "available" || r.State == "" // detached/unknown
	case r.ResourceType == "security-group", r.ResourceType == "key-pair":
		return !hasRunningInstance
	case r.Service == "iam":
		return !hasRunningInstance
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
	cfg := c.cfg.Copy()
	if r.Region != "" {
		cfg.Region = r.Region
	}

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
