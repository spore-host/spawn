package aws

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// subnetsInTwoVPCs is three subnets of vpc-a in us-west-2a, two tied on free
// IPs above the third, plus decoys in another AZ and another VPC with more
// free IPs than any of them. Listed so neither "first" nor "last" is the
// right answer.
func subnetsInTwoVPCs() []fakeSubnet {
	return []fakeSubnet{
		{"subnet-0ccc", "vpc-a", "us-west-2a", 200},
		{"subnet-0zzz", "vpc-a", "us-west-2a", 50},
		{"subnet-0bbb", "vpc-a", "us-west-2a", 200},
		{"subnet-0aaa", "vpc-a", "us-west-2b", 4000},
		{"subnet-0000", "vpc-b", "us-west-2a", 4000},
	}
}

// SubnetInAZ is deterministic (#1389): of the subnets in that VPC and AZ, the
// most free IPs, ties to the lowest ID, whatever order EC2 lists them in. It
// was Subnets[0].
func TestSubnetInAZPicksMostFreeThenLowestID(t *testing.T) {
	all := subnetsInTwoVPCs()
	for rot := 0; rot < len(all); rot++ {
		order := append(append([]fakeSubnet{}, all[rot:]...), all[:rot]...)
		f := &fakeEC2{subnets: order}
		got, err := newFakeEC2Client(t, f).SubnetInAZ(context.Background(), "us-west-2", "us-west-2a", "vpc-a")
		if err != nil {
			t.Fatalf("rotation %d: %v", rot, err)
		}
		want := SubnetChoice{SubnetID: "subnet-0bbb", AvailabilityZone: "us-west-2a", VPCID: "vpc-a", FreeIPs: 200}
		if got != want {
			t.Errorf("rotation %d: chose %+v, want %+v", rot, got, want)
		}
	}
	// A clear winner on free IPs beats a lower ID.
	f := &fakeEC2{subnets: []fakeSubnet{{"subnet-01", "vpc-a", "us-west-2a", 10}, {"subnet-09", "vpc-a", "us-west-2a", 11}}}
	if got, err := newFakeEC2Client(t, f).GetSubnetForAZ(context.Background(), "us-west-2", "us-west-2a", "vpc-a"); err != nil || got != "subnet-09" {
		t.Errorf("GetSubnetForAZ = %q, %v; want subnet-09 (more free IPs)", got, err)
	}
}

// The request filters by both the VPC and the AZ.
func TestSubnetInAZFiltersByVPCAndAZ(t *testing.T) {
	f := &fakeEC2{subnets: subnetsInTwoVPCs()}
	if _, err := newFakeEC2Client(t, f).SubnetInAZ(context.Background(), "us-west-2", "us-west-2a", "vpc-a"); err != nil {
		t.Fatal(err)
	}
	req := f.request("DescribeSubnets")
	got := map[string]string{}
	for i := 1; req.Get(fmt.Sprintf("Filter.%d.Name", i)) != ""; i++ {
		got[req.Get(fmt.Sprintf("Filter.%d.Name", i))] = req.Get(fmt.Sprintf("Filter.%d.Value.1", i))
	}
	if got["vpc-id"] != "vpc-a" || got["availability-zone"] != "us-west-2a" {
		t.Errorf("DescribeSubnets filters = %v; want vpc-id=vpc-a and availability-zone=us-west-2a", got)
	}
}

// No subnet in that VPC and AZ is an error naming both.
func TestSubnetInAZNoneIsAnError(t *testing.T) {
	f := &fakeEC2{subnets: subnetsInTwoVPCs()}
	_, err := newFakeEC2Client(t, f).SubnetInAZ(context.Background(), "us-west-2", "us-west-2c", "vpc-a")
	if err == nil || !strings.Contains(err.Error(), "us-west-2c") || !strings.Contains(err.Error(), "vpc-a") {
		t.Errorf("SubnetInAZ with no subnet in the AZ = %v; want an error naming the AZ and the VPC", err)
	}
}

// A multi-card EFA launch with a VPC and an AZ and no subnet puts every
// interface in SubnetInAZ's choice (#661, #1389).
func TestLaunch_MultiEFAVPCAndAZUsesTheChosenSubnet(t *testing.T) {
	fastEIPPolling(t)
	f := &fakeEC2{efaCards: 2, subnets: subnetsInTwoVPCs()}
	c := newFakeEC2Client(t, f)
	if _, err := c.Launch(context.Background(), LaunchConfig{
		InstanceType: "c8gn.48xlarge", Region: "us-west-2", AMI: "ami-0123456789abcdef0", PricePerHour: 1,
		EFAEnabled: true, EFAInterfaces: 2, VPCID: "vpc-a", AvailabilityZone: "us-west-2a",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	run := f.request("RunInstances")
	for _, k := range []string{"NetworkInterface.1.SubnetId", "NetworkInterface.2.SubnetId"} {
		if got := run.Get(k); got != "subnet-0bbb" {
			t.Errorf("%s = %q, want subnet-0bbb", k, got)
		}
	}
}

// A plain launch with the chosen subnet in its config sends it to RunInstances.
func TestLaunch_SubnetIDReachesRunInstances(t *testing.T) {
	run := launchWith(t, &fakeEC2{}, LaunchConfig{TTL: "1h", SubnetID: "subnet-0bbb", VPCID: "vpc-a", AvailabilityZone: "us-west-2a"})
	if got := run.Get("NetworkInterface.1.SubnetId"); got != "subnet-0bbb" {
		t.Errorf("RunInstances NetworkInterface.1.SubnetId = %q, want subnet-0bbb", got)
	}
}
