package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func piSG(id, name string, perms ...types.IpPermission) types.SecurityGroup {
	return types.SecurityGroup{GroupId: aws.String(id), GroupName: aws.String(name), IpPermissions: perms}
}

func piTCP(from, to int32) types.IpPermission {
	return types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(from), ToPort: aws.Int32(to)}
}

func piCIDR(p types.IpPermission, cidr string) types.IpPermission {
	p.IpRanges = []types.IpRange{{CidrIp: aws.String(cidr)}}
	return p
}

func piGroup(p types.IpPermission, id string) types.IpPermission {
	p.UserIdGroupPairs = []types.UserIdGroupPair{{GroupId: aws.String(id)}}
	return p
}

var piAllTraffic = types.IpPermission{IpProtocol: aws.String("-1")}

// The rule that decides "definitely closed" (#1296). Only !Open is acted on, so
// every case where SSH might work must be Open; the cases that are closed are
// the stock default-VPC group and groups with no rule for the port at all.
func TestEvaluatePortIngress(t *testing.T) {
	cases := []struct {
		name   string
		groups []types.SecurityGroup
		open   bool
	}{
		// The case the issue is about: AWS's stock default group, all traffic
		// from itself. A self-reference never admits traffic to a public IP.
		{"stock default VPC group", []types.SecurityGroup{piSG("sg-def", "default", piGroup(piAllTraffic, "sg-def"))}, false},
		{"no rules", []types.SecurityGroup{piSG("sg-a", "a")}, false},
		{"only 443", []types.SecurityGroup{piSG("sg-a", "a", piCIDR(piTCP(443, 443), "0.0.0.0/0"))}, false},
		{"udp 22", []types.SecurityGroup{piSG("sg-a", "a", piCIDR(types.IpPermission{IpProtocol: aws.String("udp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22)}, "0.0.0.0/0"))}, false},

		{"22 from anywhere", []types.SecurityGroup{piSG("sg-a", "a", piCIDR(piTCP(22, 22), "0.0.0.0/0"))}, true},
		{"22 from a /32 (not the caller's: still possibly)", []types.SecurityGroup{piSG("sg-a", "a", piCIDR(piTCP(22, 22), "198.51.100.7/32"))}, true},
		{"a range covering 22", []types.SecurityGroup{piSG("sg-a", "a", piCIDR(piTCP(0, 1024), "10.0.0.0/8"))}, true},
		{"all traffic from a CIDR", []types.SecurityGroup{piSG("sg-a", "a", piCIDR(piAllTraffic, "10.0.0.0/8"))}, true},
		{"protocol number 6", []types.SecurityGroup{piSG("sg-a", "a", piCIDR(types.IpPermission{IpProtocol: aws.String("6"), FromPort: aws.Int32(22), ToPort: aws.Int32(22)}, "0.0.0.0/0"))}, true},
		{"22 from IPv6", []types.SecurityGroup{piSG("sg-a", "a", func() types.IpPermission {
			p := piTCP(22, 22)
			p.Ipv6Ranges = []types.Ipv6Range{{CidrIpv6: aws.String("::/0")}}
			return p
		}())}, true},
		{"22 from a prefix list", []types.SecurityGroup{piSG("sg-a", "a", func() types.IpPermission {
			p := piTCP(22, 22)
			p.PrefixListIds = []types.PrefixListId{{PrefixListId: aws.String("pl-1")}}
			return p
		}())}, true},
		{"22 from ANOTHER group", []types.SecurityGroup{piSG("sg-a", "a", piGroup(piTCP(22, 22), "sg-bastion"))}, true},
		{"22 naming no source", []types.SecurityGroup{piSG("sg-a", "a", piTCP(22, 22))}, true},
		{"default plus a group that opens 22", []types.SecurityGroup{
			piSG("sg-def", "default", piGroup(piAllTraffic, "sg-def")),
			piSG("sg-ssh", "ssh", piCIDR(piTCP(22, 22), "0.0.0.0/0")),
		}, true},
		{"no groups read: no evidence", nil, true},
	}
	for _, c := range cases {
		if got := EvaluatePortIngress(c.groups, 22); got.Open != c.open {
			t.Errorf("%s: Open = %v, want %v", c.name, got.Open, c.open)
		}
	}
}

func TestEvaluatePortIngressNamesTheGroups(t *testing.T) {
	got := EvaluatePortIngress([]types.SecurityGroup{piSG("sg-b", "web"), piSG("sg-a", "default")}, 22)
	if len(got.Groups) != 2 || got.Groups[0] != "sg-a (default)" || got.Groups[1] != "sg-b (web)" {
		t.Errorf("Groups = %v, want both groups named, sorted", got.Groups)
	}
}

func TestInstanceGroupIDsCoversEveryInterface(t *testing.T) {
	inst := &types.Instance{
		SecurityGroups: []types.GroupIdentifier{{GroupId: aws.String("sg-1")}},
		NetworkInterfaces: []types.InstanceNetworkInterface{
			{Groups: []types.GroupIdentifier{{GroupId: aws.String("sg-1")}}},
			{Groups: []types.GroupIdentifier{{GroupId: aws.String("sg-2")}}},
		},
	}
	got := instanceGroupIDs(inst)
	if len(got) != 2 || got[0] != "sg-1" || got[1] != "sg-2" {
		t.Errorf("instanceGroupIDs = %v, want [sg-1 sg-2]", got)
	}
}
