package costmodel

import (
	"math"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestAccrue_EachComponentInEachState is the package doc's state table, row by
// row (#51). The expectations come from AWS's billing rules as the doc states
// them, not from Accrue: compute and auto-assigned IPv4 bill only while
// running; EBS (root and attached alike) and Elastic IPs bill in every state
// until termination; hibernation is a stop. Each case is one hour spent in the
// state, so the clocks it advances are derived from the state, and the cost of
// each component is checked to be present exactly when the table says it bills.
func TestAccrue_EachComponentInEachState(t *testing.T) {
	// Clocks each state advances over one hour, per the doc table. terminated
	// advances none: nothing bills after termination.
	clocks := map[string]struct{ compute, wall time.Duration }{
		"running":    {time.Hour, time.Hour},
		"stopped":    {0, time.Hour},
		"hibernated": {0, time.Hour},
		"terminated": {0, 0},
	}
	type component struct {
		name  string
		fp    Footprint
		bills map[string]bool // state -> bills in it
		want  func(Spend) float64
	}
	running := map[string]bool{"running": true}
	allButTerminated := map[string]bool{"running": true, "stopped": true, "hibernated": true}
	rootAndData := VolumeHourly(Volume{Type: "gp3", SizeGiB: 8}) + VolumeHourly(Volume{Type: "io2", SizeGiB: 100, IOPS: 10000})
	components := []component{
		{"compute", Footprint{ComputePerHour: 1.0}, running, func(s Spend) float64 { return s.Compute }},
		{"EBS root + attached", Footprint{EBSPerHour: rootAndData}, allButTerminated, func(s Spend) float64 { return s.EBS }},
		{"auto-assigned public IPv4", Footprint{PublicIPv4: 1}, running, func(s Spend) float64 { return s.PublicIPv4 }},
		{"Elastic IP (launch EIP)", Footprint{ElasticIPv4: 1}, allButTerminated, func(s Spend) float64 { return s.PublicIPv4 }},
	}
	for _, c := range components {
		for state, clk := range clocks {
			t.Run(c.name+"/"+state, func(t *testing.T) {
				s := Accrue(c.fp, clk.compute, clk.wall)
				got := c.want(s)
				if c.bills[state] && got <= 0 {
					t.Errorf("%s bills while %s, but Accrue counted $%.6f — the cap would fire late", c.name, state, got)
				}
				if !c.bills[state] && got != 0 {
					t.Errorf("%s does not bill while %s, but Accrue counted $%.6f", c.name, state, got)
				}
				if !near(s.Total, s.Compute+s.EBS+s.PublicIPv4) {
					t.Errorf("Total %.6f is not the sum of its components", s.Total)
				}
			})
		}
	}
}

// TestAccrue_IPv4RateIsPerAddress: $0.005/hr for every public IPv4 address,
// in use or idle (VPC pricing page, RatesAsOf).
func TestAccrue_IPv4RateIsPerAddress(t *testing.T) {
	s := Accrue(Footprint{PublicIPv4: 1, ElasticIPv4: 2}, 10*time.Hour, 10*time.Hour)
	if want := 3 * 0.005 * 10; !near(s.PublicIPv4, want) {
		t.Errorf("3 addresses for 10h = $%.4f, want $%.4f", s.PublicIPv4, want)
	}
	if PublicIPv4PerHour != 0.005 {
		t.Errorf("PublicIPv4PerHour = %v, want AWS's published $0.005", PublicIPv4PerHour)
	}
}

// TestVolumeHourly_CountsProvisionedPerformance: an io2 volume's IOPS and a
// gp3 volume's IOPS/throughput above the free baseline bill on top of its size
// (#51); before, only size was priced.
func TestVolumeHourly_CountsProvisionedPerformance(t *testing.T) {
	cases := []struct {
		name string
		v    Volume
		want float64 // per month
	}{
		{"gp3 at baseline is size only", Volume{Type: "gp3", SizeGiB: 100, IOPS: 3000, Throughput: 125}, 100 * 0.08},
		{"gp3 above baseline", Volume{Type: "gp3", SizeGiB: 100, IOPS: 6000, Throughput: 250}, 100*0.08 + 3000*0.005 + 125*0.06},
		{"io1 IOPS", Volume{Type: "io1", SizeGiB: 100, IOPS: 5000}, 100*0.125 + 5000*0.065},
		{"io2 IOPS tiered", Volume{Type: "io2", SizeGiB: 100, IOPS: 40000}, 100*0.125 + 32000*0.065 + 8000*0.046},
		{"gp2 size only", Volume{Type: "gp2", SizeGiB: 50, IOPS: 150}, 50 * 0.10},
		{"st1", Volume{Type: "st1", SizeGiB: 500}, 500 * 0.045},
		{"sc1", Volume{Type: "sc1", SizeGiB: 500}, 500 * 0.015},
		{"unknown priced as gp3", Volume{Type: "new-type", SizeGiB: 10}, 10 * 0.08},
	}
	for _, c := range cases {
		if got := VolumeHourly(c.v) * hoursPerMonth; math.Abs(got-c.want) > 1e-6 {
			t.Errorf("%s: $%.4f/month, want $%.4f", c.name, got, c.want)
		}
	}
}

// TestCountIPv4 derives the counts from the addresses themselves: Amazon-owned
// is auto-assigned, anything else is an Elastic IP; each address once.
func TestCountIPv4(t *testing.T) {
	auto, eip := CountIPv4([]Address{
		{PublicIP: "3.0.0.1", IPOwnerID: "amazon"},
		{PublicIP: "3.0.0.1", IPOwnerID: "amazon"}, // same address via primary private IP
		{PublicIP: "52.0.0.2", IPOwnerID: "123456789012"},
		{PublicIP: "", IPOwnerID: "amazon"},
	})
	if auto != 1 || eip != 1 {
		t.Errorf("CountIPv4 = (%d auto, %d elastic), want (1, 1)", auto, eip)
	}
}

func TestFromTags(t *testing.T) {
	tags := map[string]string{
		"spawn:price-per-hour":     "1.5",
		"spawn:ebs-hourly-cost":    "0.02",
		"spawn:public-ipv4-count":  "1",
		"spawn:elastic-ipv4-count": "2",
	}
	got := FromTags(tags, "spawn")
	want := Footprint{ComputePerHour: 1.5, EBSPerHour: 0.02, PublicIPv4: 1, ElasticIPv4: 2}
	if got != want {
		t.Errorf("FromTags = %+v, want %+v", got, want)
	}
	if !IPv4Measured(tags, "spawn") || IPv4Measured(map[string]string{}, "spawn") {
		t.Error("IPv4Measured must be true exactly when a count tag is present")
	}
}

// The footprint in one tag (#1428): what EncodeFootprint writes,
// MeasuredFromTags reads back, and a reader gets the same footprint from it as
// from the three tags a spored before #1428 writes.
func TestFootprintTagReadsLikeTheLegacyThree(t *testing.T) {
	m := Measured{EBSPerHour: 0.021918, PublicIPv4: 1, ElasticIPv4: 2, IPv4Known: true}
	v := EncodeFootprint(m)
	if v != "v1:0.021918/1/2" {
		t.Errorf("EncodeFootprint = %q, want v1:0.021918/1/2", v)
	}
	one := map[string]string{"spawn:price-per-hour": "1.5", "spawn:cost-footprint": v}
	legacy := map[string]string{
		"spawn:price-per-hour":     "1.5",
		"spawn:ebs-hourly-cost":    "0.021918",
		"spawn:public-ipv4-count":  "1",
		"spawn:elastic-ipv4-count": "2",
	}
	if a, b := MeasuredFromTags(one, "spawn"), MeasuredFromTags(legacy, "spawn"); a != m || b != m {
		t.Errorf("MeasuredFromTags: one tag %+v, legacy %+v, want %+v", a, b, m)
	}
	if a, b := FromTags(one, "spawn"), FromTags(legacy, "spawn"); a != b || a.EBSPerHour != m.EBSPerHour {
		t.Errorf("FromTags: one tag %+v, legacy %+v", a, b)
	}
	if !IPv4Measured(one, "spawn") || !IPv4Measured(legacy, "spawn") {
		t.Error("IPv4Measured must be true from either form")
	}
	// A footprint of zero addresses is still a measurement.
	if !IPv4Measured(map[string]string{"spawn:cost-footprint": "v1:0.000000/0/0"}, "spawn") {
		t.Error("a recorded zero count is not a measurement")
	}
	// Both present (spored upgraded past #1428): each figure the larger.
	both := map[string]string{"spawn:cost-footprint": "v1:0.010000/3/0", "spawn:ebs-hourly-cost": "0.02", "spawn:elastic-ipv4-count": "1"}
	if got := MeasuredFromTags(both, "spawn"); got != (Measured{EBSPerHour: 0.02, PublicIPv4: 3, ElasticIPv4: 1, IPv4Known: true}) {
		t.Errorf("both forms: %+v, want the larger of each", got)
	}
	if n := len(EncodeFootprint(Measured{EBSPerHour: 1e15, PublicIPv4: 1 << 30, ElasticIPv4: 1 << 30})); n > 256 {
		t.Errorf("a huge footprint encodes to %d characters, over EC2's 256", n)
	}
}

func TestParseFootprint(t *testing.T) {
	for v, want := range map[string]bool{
		"v1:0.5/1/2":       true,
		"v2:0.5/1/2/extra": true, // a later version may only append
		"v1:0.5/1":         false,
		"0.5/1/2":          false,
		"v0:0.5/1/2":       false,
		"vx:0.5/1/2":       false,
		"v1:-1/1/2":        false,
		"v1:0.5/-1/2":      false,
		"v1:NaN/1/2":       false,
		"v1:Inf/1/2":       false,
		"":                 false,
	} {
		if _, ok := ParseFootprint(v); ok != want {
			t.Errorf("ParseFootprint(%q) ok = %v, want %v", v, ok, want)
		}
	}
	if m, _ := ParseFootprint("v2:0.5/1/2/9"); m != (Measured{EBSPerHour: 0.5, PublicIPv4: 1, ElasticIPv4: 2, IPv4Known: true}) {
		t.Errorf("a v2 value's first three fields: %+v", m)
	}
	if got := EncodeFootprint(Measured{EBSPerHour: -1, PublicIPv4: -2}); got != "v1:0.000000/0/0" {
		t.Errorf("negative figures encode as %q", got)
	}
}
