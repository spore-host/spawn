// Security-group create-or-get helpers (MPI/EFA, Windows RDP, DCV, Lustre ports).

package aws

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ensureMPIClusterRules makes a security group usable for MPI *and EFA* traffic
// between its own members, idempotently.
//
// The rules are self-referential and all-protocol (#659). The group previously
// authorized only `tcp` 0-65535, which silently excluded EFA: the Scalable
// Reliable Datagram transport is not TCP, so the fabric could not pass traffic
// at all. That is a hard failure rather than a slowdown — GCHP/MAPL aborts at
// MPI_Win_create when the one-sided transport is unavailable instead of falling
// back to TCP — so an EFA-enabled launch simply died.
//
// Egress is set explicitly even though a freshly created group already has a
// default allow-all egress rule (which is why this went unnoticed). AWS
// documents EFA as requiring the self-referential egress rule, and anyone who
// tightens the permissive default otherwise loses the fabric with no indication
// why.
//
// Idempotent because it now runs on every launch, including against groups that
// already have the rules: a duplicate-rule error is success, not a launch
// failure. Follows the EnsureLustrePorts pattern below.
func ensureMPIClusterRules(ctx context.Context, ec2Client *ec2.Client, sgID string) error {
	// IpProtocol "-1" means every protocol. FromPort/ToPort must be omitted —
	// AWS rejects a port range on an all-protocols rule.
	selfAll := []types.IpPermission{{
		IpProtocol: aws.String("-1"),
		UserIdGroupPairs: []types.UserIdGroupPair{{
			GroupId:     aws.String(sgID),
			Description: aws.String("All traffic between MPI/EFA cluster nodes (self)"),
		}},
	}}

	isBenign := func(err error) bool {
		s := err.Error()
		return strings.Contains(s, "InvalidPermission.Duplicate") ||
			strings.Contains(s, "already exists")
	}

	if _, err := ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: selfAll,
	}); err != nil && !isBenign(err) {
		return fmt.Errorf("authorize all-traffic MPI ingress on %s: %w", sgID, err)
	}

	if _, err := ec2Client.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: selfAll,
	}); err != nil && !isBenign(err) {
		return fmt.Errorf("authorize all-traffic MPI egress on %s: %w", sgID, err)
	}

	return nil
}

// CreateOrGetMPISecurityGroup creates or gets a security group configured for
// MPI clusters. The group allows ALL traffic between its own members, which EFA
// requires (#659), plus SSH from outside for user access.
func (c *Client) CreateOrGetMPISecurityGroup(ctx context.Context, region, vpcID, groupName string) (string, error) {
	sgID, created, err := c.createOrGetClusterGroup(ctx, region, vpcID, groupName,
		"Security group for MPI cluster inter-node communication", "mpi-cluster")
	if err != nil || !created {
		return sgID, err
	}

	// Add ingress rule: allow SSH from anywhere (for user access)
	_, err = c.regionalEC2(region).AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges: []types.IpRange{
					{
						CidrIp:      aws.String("0.0.0.0/0"),
						Description: aws.String("SSH access"),
					},
				},
			},
		},
	})
	if err != nil {
		// Non-fatal if SSH rule fails (might already exist from default)
		fmt.Printf("Warning: failed to add SSH rule: %v\n", err)
	}

	return sgID, nil
}

// EFAGroupPrefix prefixes the security group `spawn launch --efa-group NAME`
// creates or joins (#1168).
const EFAGroupPrefix = "spawn-efa-"

// EFAGroupPurpose is the spawn:purpose tag on an --efa-group security group.
const EFAGroupPurpose = "efa-group"

// CreateOrGetEFASecurityGroup creates or gets the security group several
// `spawn launch --efa --efa-group NAME` calls share (#1168): a fleet of
// different instance types (a GPU client plus EFA proxies, say) cannot be one
// MPI job array, so it has no spawn-mpi-* group, and EFA needs a group that
// admits all traffic from itself in both directions.
//
// The rules are exactly the MPI group's (ensureMPIClusterRules), and an existing
// group is brought up to date the same way. Unlike the MPI group it opens no
// SSH: a plain launch gets no outside SSH from the VPC default group either, so
// joining an EFA group leaves reachability as it was, and --security-group-ids
// still adds a group that opens it.
//
// The group is tagged spawn:managed=true with a spawn:created stamp
// (LifecycleTags). Its deleter is the reaper's managed-network sweep
// (lambda/ttl-reaper netresources.go, reapSecurityGroups), which removes it once
// no instance has used it for the grace period.
func (c *Client) CreateOrGetEFASecurityGroup(ctx context.Context, region, vpcID, groupName string) (string, error) {
	sgID, _, err := c.createOrGetClusterGroup(ctx, region, vpcID, groupName,
		"spawn EFA group: all traffic between its members", EFAGroupPurpose)
	return sgID, err
}

// createOrGetClusterGroup finds groupName in vpcID or creates it, and makes
// sure it carries the all-traffic self rules either way. created reports a new
// group.
func (c *Client) createOrGetClusterGroup(ctx context.Context, region, vpcID, groupName, description, purpose string) (sgID string, created bool, err error) {
	// Refuse a name with no cluster-specific suffix (#685).
	//
	// Callers build this as a prefix + a cluster name ("spawn-mpi-" +
	// jobArrayName). With an empty name the result was the bare prefix, so EVERY
	// such launch shared ONE group — and since #659 that group allows ALL
	// protocols between its members, making accidental sharing worse than it was
	// before. The caller-side ordering fix makes this unreachable from the CLI;
	// this is the guard that keeps it unreachable from anywhere else.
	if groupName == "" || strings.HasSuffix(groupName, "-") {
		return "", false, fmt.Errorf("refusing to create cluster security group %q: the name has no "+
			"cluster-specific suffix, so unrelated clusters would share one all-traffic "+
			"group (#685)", groupName)
	}

	ec2Client := c.regionalEC2(region)

	// Try to find existing security group
	describeResult, err := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("group-name"),
				Values: []string{groupName},
			},
			{
				Name:   aws.String("vpc-id"),
				Values: []string{vpcID},
			},
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("failed to describe security groups: %w", err)
	}

	// If security group exists, reuse it — but bring its rules up to date first
	// (#659). Returning it unexamined meant every spawn-mpi-* group created
	// before the all-traffic fix kept only the old tcp rule, so upgrading spawn
	// fixed nothing for anyone who had already run an MPI launch. The only
	// workaround was deleting the group by hand, which fails while any instance
	// still references it.
	if len(describeResult.SecurityGroups) > 0 {
		existingID := *describeResult.SecurityGroups[0].GroupId
		if err := ensureMPIClusterRules(ctx, ec2Client, existingID); err != nil {
			return "", false, err
		}
		return existingID, false, nil
	}

	// Create new security group
	createResult, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(groupName),
		Description: aws.String(description),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSecurityGroup,
				// LifecycleTags adds spawn:created, which the reaper's sweep needs
				// and which nothing used to write — see the #685 follow-up.
				Tags: LifecycleTags(groupName, purpose, time.Now()),
			},
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("failed to create security group: %w", err)
	}

	sgID = *createResult.GroupId

	// Allow ALL traffic between cluster nodes, not just TCP (#659).
	if err := ensureMPIClusterRules(ctx, ec2Client, sgID); err != nil {
		return "", true, err
	}
	return sgID, true, nil
}

// CreateOrGetWindowsSecurityGroup creates or gets a security group for Windows
// instances, opening 22 (SSH-over-SSM fallback / OpenSSH) and 3389 (RDP) to the
// given CIDR. Without this, spawn-launched Windows instances fall back to the
// default SG (which typically opens only 22, if anything), so RDP is impossible
// (#95). allowCIDR defaults to 0.0.0.0/0 when empty (caller should warn).
func (c *Client) CreateOrGetWindowsSecurityGroup(ctx context.Context, region, vpcID, groupName, allowCIDR string) (string, error) {
	if allowCIDR == "" {
		allowCIDR = "0.0.0.0/0"
	}
	ec2Client := c.regionalEC2(region)

	// Reuse an existing group of this name in the VPC (idempotent).
	describeResult, err := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{Name: aws.String("group-name"), Values: []string{groupName}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe security groups: %w", err)
	}
	if len(describeResult.SecurityGroups) > 0 {
		return *describeResult.SecurityGroups[0].GroupId, nil
	}

	createResult, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(groupName),
		Description: aws.String("Security group for spawn Windows instances (RDP + SSH)"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags:         LifecycleTags(groupName, "windows", time.Now()),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create security group: %w", err)
	}
	sgID := *createResult.GroupId

	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges:   []types.IpRange{{CidrIp: aws.String(allowCIDR), Description: aws.String("SSH access")}},
			},
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(3389),
				ToPort:     aws.Int32(3389),
				IpRanges:   []types.IpRange{{CidrIp: aws.String(allowCIDR), Description: aws.String("RDP access")}},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to authorize Windows security group ingress: %w", err)
	}
	return sgID, nil
}

// EnsureLustrePorts adds self-referencing inbound rules for the Lustre protocol
// to the specified security group if they are not already present.
// Lustre requires port 988 (MGS/MDS/OSS) and 1018–1023 (dynamic OST traffic)
// to be open between all nodes that share a filesystem (fixes #316).
func (c *Client) EnsureLustrePorts(ctx context.Context, region, sgID string) error {
	ec2Client := c.regionalEC2(region)

	selfRef := []types.UserIdGroupPair{{GroupId: aws.String(sgID), Description: aws.String("Lustre inter-node (self)")}}

	perms := []types.IpPermission{
		{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(988), ToPort: aws.Int32(988), UserIdGroupPairs: selfRef},
		{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(1018), ToPort: aws.Int32(1023), UserIdGroupPairs: selfRef},
	}

	_, err := ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: perms,
	})
	if err != nil {
		// Duplicate rule error is fine — rules already exist
		errStr := err.Error()
		if strings.Contains(errStr, "InvalidPermission.Duplicate") || strings.Contains(errStr, "already exists") {
			return nil
		}
		return fmt.Errorf("ensure lustre ports on %s: %w", sgID, err)
	}
	return nil
}

// GetDefaultVPC returns the default VPC ID for the region
func (c *Client) GetDefaultVPC(ctx context.Context, region string) (string, error) {
	ec2Client := c.regionalEC2(region)

	result, err := ec2Client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("is-default"),
				Values: []string{"true"},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe VPCs: %w", err)
	}

	if len(result.Vpcs) == 0 {
		return "", fmt.Errorf("no default VPC found in region %s", region)
	}

	return *result.Vpcs[0].VpcId, nil
}

// SubnetHint is a subnet worth naming to someone who must pick one: the
// region has no default VPC, so a launch needs an explicit subnet.
type SubnetHint struct {
	SubnetID         string
	VPCID            string
	AvailabilityZone string
	// MapPublicIPOnLaunch is the subnet's own setting. spawn requests a public
	// IP on every launch regardless (#308); this only orders the hints, since a
	// subnet that maps public IPs is the likelier one to route to the internet.
	MapPublicIPOnLaunch bool
}

// DefaultVPCOrSubnets returns the region's default VPC, or, when it has none,
// up to max subnets to suggest instead, those that map public IPs first
// (spawn#1267). It errors only when EC2 could not answer, so a caller can tell
// "there is no default VPC" (vpcID "", err nil) from "could not find out".
func (c *Client) DefaultVPCOrSubnets(ctx context.Context, region string, max int) (string, []SubnetHint, error) {
	ec2Client := c.regionalEC2(region)
	vpcs, err := ec2Client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []types.Filter{{Name: aws.String("is-default"), Values: []string{"true"}}},
	})
	if err != nil {
		return "", nil, fmt.Errorf("describe VPCs in %s: %w", region, err)
	}
	if len(vpcs.Vpcs) > 0 {
		return aws.ToString(vpcs.Vpcs[0].VpcId), nil, nil
	}
	subnets, err := ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{})
	if err != nil {
		return "", nil, fmt.Errorf("describe subnets in %s: %w", region, err)
	}
	return "", subnetHints(subnets.Subnets, max), nil
}

// subnetHints orders subnets for DefaultVPCOrSubnets: those that map public IPs
// first, then by ID so the suggestion is stable, keeping at most max.
func subnetHints(subnets []types.Subnet, max int) []SubnetHint {
	hints := make([]SubnetHint, 0, len(subnets))
	for _, sn := range subnets {
		hints = append(hints, SubnetHint{
			SubnetID:            aws.ToString(sn.SubnetId),
			VPCID:               aws.ToString(sn.VpcId),
			AvailabilityZone:    aws.ToString(sn.AvailabilityZone),
			MapPublicIPOnLaunch: aws.ToBool(sn.MapPublicIpOnLaunch),
		})
	}
	sort.SliceStable(hints, func(i, j int) bool {
		if hints[i].MapPublicIPOnLaunch != hints[j].MapPublicIPOnLaunch {
			return hints[i].MapPublicIPOnLaunch
		}
		return hints[i].SubnetID < hints[j].SubnetID
	})
	if max >= 0 && len(hints) > max {
		hints = hints[:max]
	}
	return hints
}

// sgHasUDP8443 reports whether a security group already has an ingress rule
// covering UDP port 8443 (NICE DCV QUIC), so the ensure-rule path is idempotent.
func sgHasUDP8443(sg types.SecurityGroup) bool {
	for _, p := range sg.IpPermissions {
		if aws.ToString(p.IpProtocol) != "udp" {
			continue
		}
		from, to := aws.ToInt32(p.FromPort), aws.ToInt32(p.ToPort)
		if from <= 8443 && 8443 <= to {
			return true
		}
	}
	return false
}

// CreateOrGetDCVSecurityGroup creates or retrieves a security group named "spawn-dcv"
// that allows inbound TCP+UDP 8443 (NICE DCV, incl. QUIC) from anywhere. Returns
// the security group ID.
func (c *Client) CreateOrGetDCVSecurityGroup(ctx context.Context, region, vpcID string) (string, error) {
	ec2Client := c.regionalEC2(region)

	const sgName = "spawn-dcv"

	// Check if it already exists
	describeResult, err := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{Name: aws.String("group-name"), Values: []string{sgName}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("describe security groups: %w", err)
	}
	if len(describeResult.SecurityGroups) > 0 {
		sg := describeResult.SecurityGroups[0]
		sgID := *sg.GroupId
		// Ensure the UDP 8443 (QUIC) rule exists on a pre-existing spawn-dcv SG
		// created before #282 added it. Idempotent: only authorize if absent.
		if !sgHasUDP8443(sg) {
			_, aerr := ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
				GroupId: aws.String(sgID),
				IpPermissions: []types.IpPermission{{
					IpProtocol: aws.String("udp"),
					FromPort:   aws.Int32(8443),
					ToPort:     aws.Int32(8443),
					IpRanges:   []types.IpRange{{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("NICE DCV QUIC (IPv4)")}},
					Ipv6Ranges: []types.Ipv6Range{{CidrIpv6: aws.String("::/0"), Description: aws.String("NICE DCV QUIC (IPv6)")}},
				}},
			})
			if aerr != nil && !contains(aerr.Error(), "InvalidPermission.Duplicate") {
				fmt.Fprintf(os.Stderr, "⚠️  DCV SG %s: could not add UDP 8443 (QUIC) rule: %v — TCP transport still works\n", sgID, aerr)
			}
		}
		return sgID, nil
	}

	// Create it
	createResult, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("spawn-managed: NICE DCV application streaming (TCP+UDP 8443)"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags:         LifecycleTags(sgName, "", time.Now()),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create security group: %w", err)
	}
	sgID := *createResult.GroupId

	// Authorize DCV ports. TCP 8443 is the HTTPS/WebSocket transport; UDP 8443 is
	// DCV's QUIC datagram transport for lower-latency streaming (#282) — without
	// it DCV silently falls back to TCP and feels laggy on high-RTT links.
	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(8443),
				ToPort:     aws.Int32(8443),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("NICE DCV (IPv4)")},
				},
				Ipv6Ranges: []types.Ipv6Range{
					{CidrIpv6: aws.String("::/0"), Description: aws.String("NICE DCV (IPv6)")},
				},
			},
			{
				IpProtocol: aws.String("udp"),
				FromPort:   aws.Int32(8443),
				ToPort:     aws.Int32(8443),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("NICE DCV QUIC (IPv4)")},
				},
				Ipv6Ranges: []types.Ipv6Range{
					{CidrIpv6: aws.String("::/0"), Description: aws.String("NICE DCV QUIC (IPv6)")},
				},
			},
			// Also allow SSH so users can debug if needed
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("SSH")},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("authorize DCV ingress: %w", err)
	}

	return sgID, nil
}

// CreateOrGetWebSecurityGroup creates or retrieves a security group named
// "spawn-web" that allows inbound TCP 443 (the spored TLS reverse proxy that
// fronts a web-UI app, #590) plus SSH. Returns the security group ID.
func (c *Client) CreateOrGetWebSecurityGroup(ctx context.Context, region, vpcID string) (string, error) {
	ec2Client := c.regionalEC2(region)

	const sgName = "spawn-web"

	describeResult, err := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{Name: aws.String("group-name"), Values: []string{sgName}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("describe security groups: %w", err)
	}
	if len(describeResult.SecurityGroups) > 0 {
		return *describeResult.SecurityGroups[0].GroupId, nil
	}

	createResult, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("spawn-managed: web-UI app streaming (TCP 443 via spored TLS proxy)"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags:         LifecycleTags(sgName, "", time.Now()),
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create security group: %w", err)
	}
	sgID := *createResult.GroupId

	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(443),
				ToPort:     aws.Int32(443),
				IpRanges:   []types.IpRange{{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("HTTPS (spored web proxy, IPv4)")}},
				Ipv6Ranges: []types.Ipv6Range{{CidrIpv6: aws.String("::/0"), Description: aws.String("HTTPS (spored web proxy, IPv6)")}},
			},
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges:   []types.IpRange{{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("SSH")}},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("authorize web ingress: %w", err)
	}

	return sgID, nil
}

// ResolveVPC returns the VPC to place managed resources in: the caller's
// explicit choice when set, otherwise the region's default VPC.
//
// One seam for every call site (#673). `--vpc` was parsed into a package global
// that nothing read — there was no VPCID field anywhere — and every consumer
// called GetDefaultVPC unconditionally, so `spawn launch --vpc vpc-0abc` was
// accepted, exited 0, and launched into the DEFAULT VPC, with its security group
// created there too.
//
// The failure is silent and its blast radius is network placement: wrong subnet,
// wrong route table, no route to an EFS or FSx mount target, and SG rules written
// into a VPC the instance is not in. An account whose research VPC is not the
// default could not target a VPC at all.
//
// Shared rather than inlined because the duplicated-condition shape is exactly
// how #539 and #667 each came to be fixed on one path and left broken on
// another.
func (c *Client) ResolveVPC(ctx context.Context, region, explicitVPC string) (string, error) {
	if explicitVPC != "" {
		return explicitVPC, nil
	}
	vpcID, err := c.GetDefaultVPC(ctx, region)
	if err != nil {
		return "", fmt.Errorf("no --vpc given and no default VPC in %s: %w", region, err)
	}
	return vpcID, nil
}

// ValidateSubnetInVPC fails closed when an explicit --subnet-id is not in the
// explicit --vpc.
//
// Passing both is the natural way to use --vpc, and EC2's own error for the
// mismatch arrives only at RunInstances and names neither flag. Checking up
// front costs one DescribeSubnets and produces a message that says which of the
// two to change.
func (c *Client) ValidateSubnetInVPC(ctx context.Context, region, subnetID, vpcID string) error {
	if subnetID == "" || vpcID == "" {
		return nil
	}
	out, err := c.regionalEC2(region).DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: []string{subnetID},
	})
	if err != nil {
		// Don't fail the launch on a describe hiccup; EC2 still rejects a real
		// mismatch at RunInstances, so this is a better-error path, not a gate.
		return nil
	}
	if len(out.Subnets) == 0 || out.Subnets[0].VpcId == nil {
		return nil
	}
	if got := *out.Subnets[0].VpcId; got != vpcID {
		return fmt.Errorf("--subnet-id %s is in VPC %s, but --vpc says %s; pass one or make them agree",
			subnetID, got, vpcID)
	}
	return nil
}
