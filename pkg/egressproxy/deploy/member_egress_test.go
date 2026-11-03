package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// A member's own groups must let it reach the proxy on tcp 3128 (#1515 phase
// 4): the proxy's ingress admits the clients group, but egress is the member's.
func TestCheckMemberEgress(t *testing.T) {
	g := Groups{ClientsID: "sg-clients", ProxyID: "sg-proxy"}
	const ip = "10.0.1.5"
	tcp := func(from, to int32) types.IpPermission {
		return types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(from), ToPort: aws.Int32(to)}
	}
	all := types.IpPermission{IpProtocol: aws.String("-1")}
	cidr := func(p types.IpPermission, c string) types.IpPermission {
		p.IpRanges = []types.IpRange{{CidrIp: aws.String(c)}}
		return p
	}
	group := func(p types.IpPermission, id string) types.IpPermission {
		p.UserIdGroupPairs = []types.UserIdGroupPair{{GroupId: aws.String(id)}}
		return p
	}
	for name, tc := range map[string]struct {
		egress []types.IpPermission
		ok     bool
	}{
		"EC2's default allow-all (spawn's own groups, the VPC default)": {[]types.IpPermission{cidr(all, "0.0.0.0/0")}, true},
		"tcp 3128 to the clients group":                                 {[]types.IpPermission{group(tcp(3128, 3128), "sg-clients")}, true},
		"tcp 3128 to the proxy group":                                   {[]types.IpPermission{group(tcp(3128, 3128), "sg-proxy")}, true},
		"tcp 3128 to the proxy's /32":                                   {[]types.IpPermission{cidr(tcp(3128, 3128), ip+"/32")}, true},
		"a tcp range over 3128 to the VPC CIDR":                         {[]types.IpPermission{cidr(tcp(1024, 65535), "10.0.0.0/16")}, true},
		"all traffic to the proxy group":                                {[]types.IpPermission{group(all, "sg-proxy")}, true},
		"only 443 out":                                                  {[]types.IpPermission{cidr(tcp(443, 443), "0.0.0.0/0")}, false},
		"3128 to another subnet":                                        {[]types.IpPermission{cidr(tcp(3128, 3128), "10.0.2.0/24")}, false},
		"3128 to another group":                                         {[]types.IpPermission{group(tcp(3128, 3128), "sg-other")}, false},
		"udp 3128":                                                      {[]types.IpPermission{cidr(types.IpPermission{IpProtocol: aws.String("udp"), FromPort: aws.Int32(3128), ToPort: aws.Int32(3128)}, "0.0.0.0/0")}, false},
		"no egress at all":                                              {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeEC2{groups: []types.SecurityGroup{
				{GroupId: aws.String("sg-theirs"), GroupName: aws.String("locked-down"), IpPermissionsEgress: tc.egress},
				{GroupId: aws.String("sg-clients"), GroupName: aws.String(ClientsGroupName(testVPC))},
			}}
			err := CheckMemberEgress(context.Background(), f, []string{"sg-theirs", "sg-clients"}, g, ip)
			if tc.ok != (err == nil) {
				t.Fatalf("CheckMemberEgress = %v, want ok %v", err, tc.ok)
			}
			if err != nil {
				if !errors.Is(err, ErrMemberEgress) {
					t.Errorf("%v does not wrap ErrMemberEgress", err)
				}
				for _, w := range []string{"sg-theirs (locked-down)", "tcp 3128", ip} {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("the refusal %q does not name %q", err, w)
					}
				}
				if strings.Contains(err.Error(), "sg-clients (") {
					t.Errorf("the refusal blames the clients group, which has no rules by design: %v", err)
				}
			}
		})
	}
	// One permissive group among restrictive ones is enough: groups are a union.
	f := &fakeEC2{groups: []types.SecurityGroup{
		{GroupId: aws.String("sg-a"), IpPermissionsEgress: []types.IpPermission{cidr(tcp(443, 443), "0.0.0.0/0")}},
		{GroupId: aws.String("sg-b"), IpPermissionsEgress: []types.IpPermission{group(tcp(3128, 3128), "sg-clients")}},
	}}
	if err := CheckMemberEgress(context.Background(), f, []string{"sg-a", "sg-b"}, g, ip); err != nil {
		t.Errorf("a member with one group reaching the proxy was refused: %v", err)
	}
}
