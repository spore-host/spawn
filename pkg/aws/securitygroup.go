// Security-group create-or-get helpers (MPI/EFA, Windows RDP, DCV, Lustre ports).

package aws

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// CreateOrGetMPISecurityGroup creates or gets a security group configured for MPI clusters
// The security group allows all TCP traffic from instances in the same security group
func (c *Client) CreateOrGetMPISecurityGroup(ctx context.Context, region, vpcID, groupName string) (string, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

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
		return "", fmt.Errorf("failed to describe security groups: %w", err)
	}

	// If security group exists, return it
	if len(describeResult.SecurityGroups) > 0 {
		return *describeResult.SecurityGroups[0].GroupId, nil
	}

	// Create new security group
	createResult, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(groupName),
		Description: aws.String("Security group for MPI cluster inter-node communication"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags: []types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(groupName),
					},
					{
						Key:   aws.String("spawn:managed"),
						Value: aws.String("true"),
					},
					{
						Key:   aws.String("spawn:purpose"),
						Value: aws.String("mpi-cluster"),
					},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create security group: %w", err)
	}

	sgID := *createResult.GroupId

	// Add ingress rule: allow all TCP from same security group
	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(0),
				ToPort:     aws.Int32(65535),
				UserIdGroupPairs: []types.UserIdGroupPair{
					{
						GroupId:     aws.String(sgID),
						Description: aws.String("Allow all TCP from MPI cluster nodes"),
					},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to authorize security group ingress: %w", err)
	}

	// Add ingress rule: allow SSH from anywhere (for user access)
	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
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

// CreateOrGetWindowsSecurityGroup creates or gets a security group for Windows
// instances, opening 22 (SSH-over-SSM fallback / OpenSSH) and 3389 (RDP) to the
// given CIDR. Without this, spawn-launched Windows instances fall back to the
// default SG (which typically opens only 22, if anything), so RDP is impossible
// (#95). allowCIDR defaults to 0.0.0.0/0 when empty (caller should warn).
func (c *Client) CreateOrGetWindowsSecurityGroup(ctx context.Context, region, vpcID, groupName, allowCIDR string) (string, error) {
	if allowCIDR == "" {
		allowCIDR = "0.0.0.0/0"
	}
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

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
				Tags: []types.Tag{
					{Key: aws.String("Name"), Value: aws.String(groupName)},
					{Key: aws.String("spawn:managed"), Value: aws.String("true")},
					{Key: aws.String("spawn:purpose"), Value: aws.String("windows")},
				},
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
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

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
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

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
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

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
				log.Printf("DCV SG %s: could not add UDP 8443 (QUIC) rule: %v — TCP transport still works", sgID, aerr)
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
				Tags: []types.Tag{
					{Key: aws.String("spawn:managed"), Value: aws.String("true")},
					{Key: aws.String("Name"), Value: aws.String(sgName)},
				},
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
