package provider

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/spore-host/spawn/pkg/costmodel"
)

// TestCostFootprintOf_CountsEveryPublicIPv4 derives the IPv4 counts from the
// instance's actual addresses (#51). The multi-interface EFA case is the one
// that matters: EC2 auto-assigns nothing there, so the launch Elastic IP on the
// primary interface is the instance's only public address, and it keeps
// billing while the instance is stopped.
func TestCostFootprintOf_CountsEveryPublicIPv4(t *testing.T) {
	assoc := func(ip, owner string) *types.InstanceNetworkInterfaceAssociation {
		return &types.InstanceNetworkInterfaceAssociation{PublicIp: aws.String(ip), IpOwnerId: aws.String(owner)}
	}
	priv := func(a *types.InstanceNetworkInterfaceAssociation) []types.InstancePrivateIpAddress {
		return []types.InstancePrivateIpAddress{{Primary: aws.Bool(true), Association: a}}
	}
	efaPrimary := assoc("52.1.1.1", "123456789012")
	autoPrimary := assoc("3.1.1.1", "amazon")
	cases := []struct {
		name          string
		inst          types.Instance
		auto, elastic int
	}{
		{
			name: "multi-interface EFA launch: one launch EIP on card 0, none on the others",
			inst: types.Instance{NetworkInterfaces: []types.InstanceNetworkInterface{
				{Association: efaPrimary, PrivateIpAddresses: priv(efaPrimary)},
				{PrivateIpAddresses: priv(nil)},
				{PrivateIpAddresses: priv(nil)},
				{PrivateIpAddresses: priv(nil)},
			}},
			elastic: 1,
		},
		{
			name: "single interface, auto-assigned",
			inst: types.Instance{NetworkInterfaces: []types.InstanceNetworkInterface{
				{Association: autoPrimary, PrivateIpAddresses: priv(autoPrimary)},
			}},
			auto: 1,
		},
		{
			name: "user-associated EIP on a secondary private IP",
			inst: types.Instance{NetworkInterfaces: []types.InstanceNetworkInterface{
				{Association: autoPrimary, PrivateIpAddresses: append(priv(autoPrimary),
					types.InstancePrivateIpAddress{Association: assoc("54.2.2.2", "123456789012")})},
			}},
			auto: 1, elastic: 1,
		},
		{
			name: "no public address",
			inst: types.Instance{NetworkInterfaces: []types.InstanceNetworkInterface{{PrivateIpAddresses: priv(nil)}}},
		},
	}
	for _, c := range cases {
		fp := costFootprintOf(c.inst, nil)
		if fp.PublicIPv4 != c.auto || fp.ElasticIPv4 != c.elastic {
			t.Errorf("%s: (%d auto, %d elastic), want (%d, %d)", c.name, fp.PublicIPv4, fp.ElasticIPv4, c.auto, c.elastic)
		}
	}
}

// TestCostFootprintOf_PricesEveryAttachedVolume: root and data volumes alike,
// including provisioned IOPS (#51).
func TestCostFootprintOf_PricesEveryAttachedVolume(t *testing.T) {
	vols := []types.Volume{
		{VolumeType: types.VolumeTypeGp3, Size: aws.Int32(8), Iops: aws.Int32(3000), Throughput: aws.Int32(125)},
		{VolumeType: types.VolumeTypeIo2, Size: aws.Int32(100), Iops: aws.Int32(10000)},
	}
	got := costFootprintOf(types.Instance{}, vols).EBSHourlyCost
	want := costmodel.VolumeHourly(costmodel.Volume{Type: "gp3", SizeGiB: 8, IOPS: 3000, Throughput: 125}) +
		costmodel.VolumeHourly(costmodel.Volume{Type: "io2", SizeGiB: 100, IOPS: 10000})
	if got != want || got <= costmodel.VolumeHourly(costmodel.Volume{Type: "gp3", SizeGiB: 8}) {
		t.Errorf("EBS hourly = %.6f, want %.6f (root + attached)", got, want)
	}
}

// TestFootprintTagWrites_NeverLowers: a recorded rate is priced over the whole
// life, so lowering it would forgive cost already incurred and make the cap
// fire late (#51). A rise, or an absent tag, is written.
func TestFootprintTagWrites_NeverLowers(t *testing.T) {
	tag := func(k, v string) types.Tag { return types.Tag{Key: aws.String(k), Value: aws.String(v)} }
	current := []types.Tag{
		tag("spawn:ebs-hourly-cost", "0.050000"),
		tag("spawn:elastic-ipv4-count", "1"),
	}
	got := map[string]string{}
	for _, kv := range footprintTagWrites(CostFootprint{EBSHourlyCost: 0.01, PublicIPv4: 1, ElasticIPv4: 0}, current) {
		got[kv[0]] = kv[1]
	}
	if _, ok := got["spawn:ebs-hourly-cost"]; ok {
		t.Error("a lower EBS rate must not overwrite a higher recorded one")
	}
	if _, ok := got["spawn:elastic-ipv4-count"]; ok {
		t.Error("a lower Elastic IP count (a first look before the launch EIP was associated) must not overwrite a higher one")
	}
	if got["spawn:public-ipv4-count"] != "1" {
		t.Errorf("an absent count must be written; writes = %v", got)
	}
	got = map[string]string{}
	for _, kv := range footprintTagWrites(CostFootprint{EBSHourlyCost: 0.09, ElasticIPv4: 2}, current) {
		got[kv[0]] = kv[1]
	}
	if got["spawn:ebs-hourly-cost"] != "0.090000" || got["spawn:elastic-ipv4-count"] != "2" {
		t.Errorf("a rise must be written; writes = %v", got)
	}
}
