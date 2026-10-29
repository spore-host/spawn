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
// fire late (#51). A rise, or an absent tag, is written. What is recorded is
// read from spawn:cost-footprint or, on an instance whose spored predates #1428,
// the three tags before it.
func TestFootprintTagWrites_NeverLowers(t *testing.T) {
	tag := func(k, v string) types.Tag { return types.Tag{Key: aws.String(k), Value: aws.String(v)} }
	for name, current := range map[string][]types.Tag{
		"legacy tags": {tag("spawn:ebs-hourly-cost", "0.050000"), tag("spawn:elastic-ipv4-count", "1")},
		"one tag":     {tag("spawn:cost-footprint", "v1:0.050000/0/1")},
	} {
		t.Run(name, func(t *testing.T) {
			got := map[string]string{}
			for _, kv := range footprintTagWrites(CostFootprint{EBSHourlyCost: 0.01, PublicIPv4: 1, ElasticIPv4: 0}, current) {
				got[kv[0]] = kv[1]
			}
			// The lower EBS rate and Elastic count keep the recorded ones;
			// the auto-assigned count rises from none to 1.
			if got["spawn:cost-footprint"] != "v1:0.050000/1/1" {
				t.Errorf("writes = %v, want spawn:cost-footprint=v1:0.050000/1/1", got)
			}
			got = map[string]string{}
			for _, kv := range footprintTagWrites(CostFootprint{EBSHourlyCost: 0.09, ElasticIPv4: 2}, current) {
				got[kv[0]] = kv[1]
			}
			if got["spawn:cost-footprint"] != "v1:0.090000/0/2" {
				t.Errorf("a rise must be written; writes = %v", got)
			}
		})
	}
	same := []types.Tag{tag("spawn:cost-footprint", "v1:0.050000/1/1")}
	if w := footprintTagWrites(CostFootprint{EBSHourlyCost: 0.05, PublicIPv4: 1, ElasticIPv4: 1}, same); len(w) != 0 {
		t.Errorf("an unchanged footprint is rewritten: %v", w)
	}
	if w := footprintTagWrites(CostFootprint{EBSHourlyCost: 0.01}, same); len(w) != 0 {
		t.Errorf("a lower footprint is written: %v", w)
	}
}

// The gate for #1428: spored writes the footprint as ONE tag, spawn:cost-footprint,
// whatever the instance already carries, so a launch keeps one slot for it
// instead of three. It never writes the three tags a spored before #1428 did;
// readers fall back to those for older instances.
func TestFootprintIsWrittenAsOneTag(t *testing.T) {
	tag := func(k, v string) types.Tag { return types.Tag{Key: aws.String(k), Value: aws.String(v)} }
	fp := CostFootprint{EBSHourlyCost: 0.2, PublicIPv4: 2, ElasticIPv4: 3}
	for name, current := range map[string][]types.Tag{
		"first measurement": nil,
		"legacy tags":       {tag("spawn:ebs-hourly-cost", "0.1"), tag("spawn:public-ipv4-count", "1"), tag("spawn:elastic-ipv4-count", "1")},
		"older one tag":     {tag("spawn:cost-footprint", "v1:0.100000/1/1")},
		"unparseable":       {tag("spawn:cost-footprint", "garbage")},
	} {
		w := footprintTagWrites(fp, current)
		if len(w) != 1 || w[0][0] != "spawn:cost-footprint" || w[0][1] != "v1:0.200000/2/3" {
			t.Errorf("%s: writes %v, want exactly spawn:cost-footprint=v1:0.200000/2/3", name, w)
		}
		have := map[string]string{}
		for _, kv := range w {
			have[kv[0]] = kv[1]
		}
		m := costmodel.MeasuredFromTags(have, "spawn")
		if m.EBSPerHour != 0.2 || m.PublicIPv4 != 2 || m.ElasticIPv4 != 3 || !m.IPv4Known {
			t.Errorf("%s: what spored writes reads back as %+v", name, m)
		}
	}
}

// spored's own reader (configFromTags, what --cost-limit is enforced with)
// gets the same footprint from spawn:cost-footprint as from the three tags an
// older spored wrote (#1428).
func TestConfigFromTagsReadsTheFootprintEitherWay(t *testing.T) {
	td := func(kv ...string) []types.TagDescription {
		var out []types.TagDescription
		for i := 0; i < len(kv); i += 2 {
			out = append(out, types.TagDescription{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
		}
		return out
	}
	legacy, _ := configFromTags(td("spawn:ebs-hourly-cost", "0.125000", "spawn:public-ipv4-count", "1", "spawn:elastic-ipv4-count", "2"))
	one, _ := configFromTags(td("spawn:cost-footprint", "v1:0.125000/1/2"))
	for _, c := range []*Config{legacy, one} {
		if c.EBSHourlyCost != 0.125 || c.PublicIPv4Count != 1 || c.ElasticIPv4Count != 2 {
			t.Errorf("legacy %v/%d/%d, one tag %v/%d/%d; want 0.125/1/2 from both",
				legacy.EBSHourlyCost, legacy.PublicIPv4Count, legacy.ElasticIPv4Count,
				one.EBSHourlyCost, one.PublicIPv4Count, one.ElasticIPv4Count)
			break
		}
	}
}
