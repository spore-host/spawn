package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// #1596: the DCV, web and MPI groups spawn creates open no SSH. They opened
// tcp/22 to 0.0.0.0/0 "so users can debug", which `spawn connect` never
// needed: it uses SSM when no group admits tcp/22.

func describeSG(t *testing.T, c *Client, id string) types.SecurityGroup {
	t.Helper()
	out, err := c.regionalEC2("us-east-1").DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{GroupIds: []string{id}})
	if err != nil || len(out.SecurityGroups) != 1 {
		t.Fatalf("describe %s: %v", id, err)
	}
	return out.SecurityGroups[0]
}

func sshIngress(sg types.SecurityGroup) []string {
	var cidrs []string
	for _, p := range sg.IpPermissions {
		proto := aws.ToString(p.IpProtocol)
		if proto != "-1" && (proto != "tcp" || aws.ToInt32(p.FromPort) > 22 || aws.ToInt32(p.ToPort) < 22) {
			continue
		}
		for _, r := range p.IpRanges {
			cidrs = append(cidrs, aws.ToString(r.CidrIp))
		}
		for _, r := range p.Ipv6Ranges {
			cidrs = append(cidrs, aws.ToString(r.CidrIpv6))
		}
	}
	return cidrs
}

// substrateHasNoSGTags stands in for the adoption check in a test whose groups
// are spawn's: the emulator keeps no security-group tags
// (scttfrdmn/substrate#1440). TestRequireSpawnManagedGroup holds the real
// check; TestUnmanagedGroupOfTheSameNameIsRefused runs it on the emulator.
func substrateHasNoSGTags(t *testing.T) {
	t.Helper()
	old := adoptManagedGroup
	adoptManagedGroup = func(types.SecurityGroup) error { return nil }
	t.Cleanup(func() { adoptManagedGroup = old })
}

func managedGroupCreators(c *Client, vpc string) map[string]func() (string, error) {
	ctx := context.Background()
	return map[string]func() (string, error){
		"spawn-dcv": func() (string, error) { return c.CreateOrGetDCVSecurityGroup(ctx, "us-east-1", vpc) },
		"spawn-web": func() (string, error) { return c.CreateOrGetWebSecurityGroup(ctx, "us-east-1", vpc) },
		"spawn-mpi-test-1596": func() (string, error) {
			return c.CreateOrGetMPISecurityGroup(ctx, "us-east-1", vpc, "spawn-mpi-test-1596")
		},
	}
}

func TestManagedGroupsOpenNoSSH(t *testing.T) {
	c, vpc := mpiTestClient(t)
	for name, create := range managedGroupCreators(c, vpc) {
		id, err := create()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cidrs := sshIngress(describeSG(t, c, id)); len(cidrs) > 0 {
			t.Errorf("%s admits SSH from %v: no managed app or MPI group may open tcp/22 to the outside (#1596)", name, cidrs)
		}
	}
}

// An existing spawn-managed group made by an older spawn loses its world-open
// SSH rule when reused; a narrower SSH rule someone added stays.
func TestReusedManagedGroupLosesWorldSSH(t *testing.T) {
	c, vpc := mpiTestClient(t)
	substrateHasNoSGTags(t)
	ec2c := c.regionalEC2("us-east-1")
	ctx := context.Background()
	for name, create := range managedGroupCreators(c, vpc) {
		out, err := ec2c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName: aws.String(name), Description: aws.String("old spawn"), VpcId: aws.String(vpc),
			TagSpecifications: []types.TagSpecification{{ResourceType: types.ResourceTypeSecurityGroup,
				Tags: []types.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}}}},
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// One call per range: on EC2 each CIDR is its own rule either way, but
		// the emulator revokes every range of a permission authorized together
		// (scttfrdmn/substrate#1453).
		for _, cidr := range []string{"0.0.0.0/0", "10.1.0.0/16"} {
			if _, err := ec2c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
				GroupId: out.GroupId,
				IpPermissions: []types.IpPermission{{
					IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22),
					IpRanges: []types.IpRange{{CidrIp: aws.String(cidr)}},
				}},
			}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		id, err := create()
		if err != nil {
			t.Fatalf("%s: reuse: %v", name, err)
		}
		if id != aws.ToString(out.GroupId) {
			t.Fatalf("%s: got group %s, want the existing %s", name, id, aws.ToString(out.GroupId))
		}
		got := sshIngress(describeSG(t, c, id))
		if len(got) != 1 || got[0] != "10.1.0.0/16" {
			t.Errorf("%s: SSH ingress after reuse is %v, want only the narrower 10.1.0.0/16", name, got)
		}
	}
}

// A spawn-dcv or spawn-web group that spawn did not create is refused, not
// joined: anyone able to create a group in the account could otherwise
// pre-create one with rules of their own (#1596).
func TestUnmanagedGroupOfTheSameNameIsRefused(t *testing.T) {
	c, vpc := mpiTestClient(t)
	ctx := context.Background()
	for name, create := range managedGroupCreators(c, vpc) {
		if strings.HasPrefix(name, "spawn-mpi-") {
			continue // a caller-named cluster group, joined by name on purpose
		}
		if _, err := c.regionalEC2("us-east-1").CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName: aws.String(name), Description: aws.String("not spawn's"), VpcId: aws.String(vpc),
		}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := create(); err == nil || !strings.Contains(err.Error(), "spawn:managed") {
			t.Errorf("%s: an untagged group of that name was adopted (err %v)", name, err)
		}
	}
}

func TestWorldSSHPermissions(t *testing.T) {
	sg := types.SecurityGroup{IpPermissions: []types.IpPermission{
		{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22),
			IpRanges:   []types.IpRange{{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("SSH")}, {CidrIp: aws.String("192.0.2.1/32")}},
			Ipv6Ranges: []types.Ipv6Range{{CidrIpv6: aws.String("::/0")}}},
		{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(8443), ToPort: aws.Int32(8443),
			IpRanges: []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}},
	}}
	got := worldSSHPermissions(sg)
	if len(got) != 1 || len(got[0].IpRanges) != 1 || aws.ToString(got[0].IpRanges[0].CidrIp) != "0.0.0.0/0" ||
		len(got[0].Ipv6Ranges) != 1 || got[0].IpRanges[0].Description != nil {
		t.Errorf("worldSSHPermissions = %+v, want only tcp/22 from 0.0.0.0/0 and ::/0, without descriptions", got)
	}
}

func TestRequireSpawnManagedGroup(t *testing.T) {
	tagged := types.SecurityGroup{GroupId: aws.String("sg-1"), Tags: []types.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}}}
	if err := requireSpawnManagedGroup(tagged); err != nil {
		t.Errorf("a spawn:managed=true group was refused: %v", err)
	}
	for _, sg := range []types.SecurityGroup{
		{GroupId: aws.String("sg-2")},
		{GroupId: aws.String("sg-3"), Tags: []types.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("false")}}},
		{GroupId: aws.String("sg-4"), Tags: []types.Tag{{Key: aws.String("Name"), Value: aws.String("spawn-dcv")}}},
	} {
		if err := requireSpawnManagedGroup(sg); err == nil {
			t.Errorf("group %s with tags %v was adopted", aws.ToString(sg.GroupId), sg.Tags)
		}
	}
}
