package aws

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// subnetHints puts subnets that map public IPs first, is stable by ID within
// each group, and keeps at most max (spawn#1267).
func TestSubnetHints_OrderAndLimit(t *testing.T) {
	sn := func(id string, public bool) types.Subnet {
		return types.Subnet{SubnetId: aws.String(id), VpcId: aws.String("vpc-0p"), AvailabilityZone: aws.String("us-west-2a"), MapPublicIpOnLaunch: aws.Bool(public)}
	}
	got := subnetHints([]types.Subnet{sn("subnet-c", false), sn("subnet-b", true), sn("subnet-a", false), sn("subnet-d", true)}, 3)
	want := []string{"subnet-b", "subnet-d", "subnet-a"}
	if len(got) != len(want) {
		t.Fatalf("got %d hints, want %d: %+v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].SubnetID != id {
			t.Errorf("hint %d = %s, want %s (all: %+v)", i, got[i].SubnetID, id, got)
		}
	}
	if !got[0].MapPublicIPOnLaunch || got[0].VPCID != "vpc-0p" || got[0].AvailabilityZone != "us-west-2a" {
		t.Errorf("fields not carried: %+v", got[0])
	}
}
