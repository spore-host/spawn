package aws

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// PortIngress is what an instance's security groups say about one inbound TCP
// port (spawn#1296).
//
// Open is deliberately generous: it is true when ANY attached group has an
// ingress rule covering the port, from any source — a CIDR, an IPv6 range, a
// prefix list or another security group. Whether that source includes the
// caller is not checked, so Open means "possibly reachable" and only !Open
// means "definitely unreachable". A caller acting on !Open (skipping an SSH
// wait, going straight to SSM) therefore never refuses a reachable instance.
// Network ACLs and routing are not considered; they can only make an Open port
// unreachable, never the reverse.
//
// The one source not counted is a group referencing ITSELF. That rule admits
// only the group's own members, and only over private addresses (a group
// reference never matches traffic to a public IP), so it cannot admit a
// workstation reaching the instance's public IP. It is also the whole of the
// stock default-VPC group — "all traffic from itself" — which is the group a
// plain Linux launch gets (#1296); counting it would make that case Open.
type PortIngress struct {
	Port   int32
	Groups []string // the attached groups, as "sg-… (name)", sorted
	Open   bool
}

// InstancePortIngress reads the security groups attached to an instance (on
// every network interface) and reports whether any of them could admit inbound
// TCP on port. Read-only: ec2:DescribeInstances and ec2:DescribeSecurityGroups.
//
// An instance with no groups to read is reported Open: it is not evidence the
// port is closed, and !Open must only ever be said on evidence.
func (c *Client) InstancePortIngress(ctx context.Context, region, instanceID string, port int32) (PortIngress, error) {
	ec2Client := c.regionalEC2(region)
	inst, err := c.describeInstance(ctx, ec2Client, instanceID)
	if err != nil {
		return PortIngress{}, err
	}
	ids := instanceGroupIDs(inst)
	if len(ids) == 0 {
		return PortIngress{Port: port, Open: true}, nil
	}
	out, err := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: ids})
	if err != nil {
		return PortIngress{}, fmt.Errorf("describe security groups %v: %w", ids, err)
	}
	return EvaluatePortIngress(out.SecurityGroups, port), nil
}

// instanceGroupIDs is the union of the instance-level groups and those on each
// network interface (multi-card EFA instances carry several ENIs).
func instanceGroupIDs(inst *types.Instance) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(g types.GroupIdentifier) {
		if id := aws.ToString(g.GroupId); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, g := range inst.SecurityGroups {
		add(g)
	}
	for _, ni := range inst.NetworkInterfaces {
		for _, g := range ni.Groups {
			add(g)
		}
	}
	sort.Strings(ids)
	return ids
}

// EvaluatePortIngress decides PortIngress from security groups already read.
// Pure, so the rule that decides "definitely closed" is testable without EC2.
// No groups at all is reported Open (no evidence), as in InstancePortIngress.
func EvaluatePortIngress(groups []types.SecurityGroup, port int32) PortIngress {
	pi := PortIngress{Port: port, Open: len(groups) == 0}
	for _, sg := range groups {
		label := aws.ToString(sg.GroupId)
		if name := aws.ToString(sg.GroupName); name != "" {
			label += " (" + name + ")"
		}
		pi.Groups = append(pi.Groups, label)
		for _, p := range sg.IpPermissions {
			if permissionCoversTCPPort(p, port) && permissionHasOutsideSource(p, aws.ToString(sg.GroupId)) {
				pi.Open = true
			}
		}
	}
	sort.Strings(pi.Groups)
	return pi
}

// permissionCoversTCPPort reports whether one ingress permission covers TCP
// port: all traffic ("-1"), or tcp/6 with the port inside FromPort..ToPort.
// The source is not inspected (see PortIngress).
func permissionCoversTCPPort(p types.IpPermission, port int32) bool {
	switch aws.ToString(p.IpProtocol) {
	case "-1", "all":
		return true
	case "tcp", "6":
		return aws.ToInt32(p.FromPort) <= port && port <= aws.ToInt32(p.ToPort)
	}
	return false
}

// permissionHasOutsideSource reports whether a permission names any source
// other than its own group (selfID). A permission naming no source at all is
// counted, since !Open must only be said on evidence.
func permissionHasOutsideSource(p types.IpPermission, selfID string) bool {
	if len(p.IpRanges) > 0 || len(p.Ipv6Ranges) > 0 || len(p.PrefixListIds) > 0 {
		return true
	}
	if len(p.UserIdGroupPairs) == 0 {
		return true
	}
	for _, pair := range p.UserIdGroupPairs {
		if aws.ToString(pair.GroupId) != selfID {
			return true
		}
	}
	return false
}
