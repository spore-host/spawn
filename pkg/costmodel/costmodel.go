// Package costmodel is the one cost accounting for a spawn instance (#51):
// what spored's --cost-limit enforcement terminates on, what `spored status`
// reports as cost-limit used, and what the CLI shows and sizes a raised cap
// with (`spawn status`, `spawn cost`, `spawn extend`). Every one of them calls
// Accrue, so a component cannot be counted in one and missing in another.
//
// # What is counted, in which states
//
//	component                     running  stopped  hibernated  how it is timed
//	compute (instance rate)       yes      no       no          compute time (spawn:compute-seconds)
//	EBS, root + attached volumes  yes      yes      yes         wall clock since first launch
//	  (size, plus gp3/io1/io2 IOPS and gp3 throughput above the free baseline)
//	auto-assigned public IPv4     yes      no       no          compute time (released on stop)
//	Elastic IP (incl. the launch  yes      yes      yes         wall clock since first launch
//	  EIP of a multi-interface EFA launch)                      (an EIP bills whether associated or idle)
//
// Hibernation is a stop: compute and the auto-assigned address stop, EBS and
// Elastic IPs keep billing. The RAM image lives on the root volume, whose
// provisioned size is what the EBS figure measures, so it is already counted.
// After termination nothing here bills: spawn's volumes are DeleteOnTermination
// and its launch Elastic IP is released by spored, `spawn terminate` or the TTL
// reaper.
//
// # What is not counted, deliberately
//
//   - ENIs and EFA interfaces: free.
//   - FSx and EFS: they outlive the instance, so terminating it would not stop
//     them billing; they are refused up front instead (cmd costLimitPreflight)
//     and listed separately by `spawn status`.
//   - Data transfer, snapshots, NAT: out of scope; spawn cannot see them from
//     the instance.
//
// # Static rates
//
// The public IPv4 and EBS rates below are STATIC: they are not read from the
// Price List API, because spored enforces from inside the instance with no
// pricing permission and must work offline. They are AWS's published rates as
// of RatesAsOf; regional rates differ and AWS may change them. Where a rate is
// uncertain the higher figure is used, so the cap errs early, never late.
package costmodel

import (
	"strconv"
	"strings"
	"time"
)

// RatesAsOf is when the static rates in this package were taken from AWS's
// published pricing pages (https://aws.amazon.com/vpc/pricing/ and
// https://aws.amazon.com/ebs/pricing/, fetched 2026-10-26).
const RatesAsOf = "2026-10-26"

// PublicIPv4PerHour is AWS's charge per public IPv4 address per hour, in use or
// idle, auto-assigned or Elastic (VPC pricing page, "Hourly charge for In-use
// Public IPv4 Address $0.005", in effect since 2024-02-01; fetched RatesAsOf).
// BYOIP addresses are exempt; spawn cannot tell one from an account's own
// Elastic IP, so it charges them too (an over-count).
const PublicIPv4PerHour = 0.005

// hoursPerMonth is AWS's billing month for per-month EBS rates.
const hoursPerMonth = 730.0

// Tag suffixes (after the "spawn:" prefix) the rates are cached in. spored
// measures them from the instance's own volumes and addresses and writes them;
// every reader reads them from here.
const (
	TagEBSHourlyCost  = "ebs-hourly-cost"
	TagPublicIPv4     = "public-ipv4-count"  // auto-assigned (Amazon-owned) addresses
	TagElasticIPv4    = "elastic-ipv4-count" // Elastic IPs associated to the instance
	TagPricePerHour   = "price-per-hour"
	TagComputeSeconds = "compute-seconds"
)

// FootprintTags are the suffixes of the footprint tags spored writes on every
// instance, which a launch keeps a slot free for (#1355).
var FootprintTags = []string{TagEBSHourlyCost, TagPublicIPv4, TagElasticIPv4}

// Footprint is what an instance pays for by the hour, split by how each part
// is timed.
type Footprint struct {
	// ComputePerHour is the instance rate. Enforcement uses the on-demand rate
	// (spawn:price-per-hour), which can only overstate a spot instance.
	ComputePerHour float64
	// EBSPerHour is every attached volume (spawn:ebs-hourly-cost).
	EBSPerHour float64
	// PublicIPv4 is the number of auto-assigned public IPv4 addresses; they are
	// released on stop, so they bill with compute.
	PublicIPv4 int
	// ElasticIPv4 is the number of Elastic IPs; they bill on the wall clock.
	ElasticIPv4 int
}

// Spend is an accrued cost, by component.
type Spend struct {
	Compute    float64
	EBS        float64
	PublicIPv4 float64 // auto-assigned and Elastic together
	Total      float64
}

// Accrue is THE cost accounting: f accrued over compute time (the instance was
// running) and wall time (since its first launch, stopped and hibernated time
// included). Negative durations count as zero.
func Accrue(f Footprint, compute, wall time.Duration) Spend {
	ch, wh := hours(compute), hours(wall)
	s := Spend{
		Compute:    f.ComputePerHour * ch,
		EBS:        f.EBSPerHour * wh,
		PublicIPv4: PublicIPv4PerHour * (float64(f.PublicIPv4)*ch + float64(f.ElasticIPv4)*wh),
	}
	s.Total = s.Compute + s.EBS + s.PublicIPv4
	return s
}

func hours(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return d.Hours()
}

// RunningPerHour is the rate while the instance runs: every component.
func (f Footprint) RunningPerHour() float64 {
	return f.ComputePerHour + f.EBSPerHour + PublicIPv4PerHour*float64(f.PublicIPv4+f.ElasticIPv4)
}

// IPv4PerHour is the public IPv4 charge while running.
func (f Footprint) IPv4PerHour() float64 {
	return PublicIPv4PerHour * float64(f.PublicIPv4+f.ElasticIPv4)
}

// FromTags reads a Footprint from an instance's tags, keyed by full tag name
// with the given prefix ("spawn"). The compute rate is spawn:price-per-hour.
func FromTags(tags map[string]string, prefix string) Footprint {
	key := func(s string) string { return prefix + ":" + s }
	return Footprint{
		ComputePerHour: parseFloat(tags[key(TagPricePerHour)]),
		EBSPerHour:     parseFloat(tags[key(TagEBSHourlyCost)]),
		PublicIPv4:     parseCount(tags[key(TagPublicIPv4)]),
		ElasticIPv4:    parseCount(tags[key(TagElasticIPv4)]),
	}
}

// IPv4Measured reports whether spored has recorded the instance's public IPv4
// addresses (either count tag present). An instance whose spored predates #51
// has neither, and its enforcement does not count IPv4.
func IPv4Measured(tags map[string]string, prefix string) bool {
	_, a := tags[prefix+":"+TagPublicIPv4]
	_, e := tags[prefix+":"+TagElasticIPv4]
	return a || e
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func parseCount(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// Volume is one EBS volume as DescribeVolumes reports it.
type Volume struct {
	Type       string // gp3, gp2, io1, io2, st1, sc1, standard
	SizeGiB    int32
	IOPS       int32 // provisioned IOPS (gp3/io1/io2)
	Throughput int32 // provisioned MB/s (gp3)
}

// Static EBS rates (EBS pricing page, fetched RatesAsOf). Per GB-month unless
// noted.
const (
	gp3GBMonth         = 0.08
	gp3FreeIOPS        = 3000
	gp3IOPSMonth       = 0.005 // per provisioned IOPS-month above 3,000
	gp3FreeThroughput  = 125
	gp3ThroughputMonth = 0.06 // per MB/s-month above 125 (the page's example rate; us-east-1 is lower)
	gp2GBMonth         = 0.10
	ioGBMonth          = 0.125 // io1 and io2
	ioIOPSMonth        = 0.065 // io1 flat; io2 for the first 32,000 IOPS
	io2IOPSMonthTier2  = 0.046 // io2 above 32,000 (also used above 64,000, an over-count)
	io2Tier1IOPS       = 32000
	st1GBMonth         = 0.045
	sc1GBMonth         = 0.015
	standardGBMonth    = 0.05 // previous-generation magnetic
)

// VolumeHourly is a volume's hourly cost: its size, plus the provisioned IOPS
// and throughput billed above the free baseline. Before #51 only size was
// priced, so an io2 volume's IOPS (often most of its cost) went uncounted. An
// unknown type is priced as gp3.
func VolumeHourly(v Volume) float64 {
	size := float64(max(v.SizeGiB, 0))
	iops := float64(max(v.IOPS, 0))
	var month float64
	switch strings.ToLower(v.Type) {
	case "gp2":
		month = size * gp2GBMonth
	case "io1":
		month = size*ioGBMonth + iops*ioIOPSMonth
	case "io2":
		t1 := min(iops, io2Tier1IOPS)
		month = size*ioGBMonth + t1*ioIOPSMonth + (iops-t1)*io2IOPSMonthTier2
	case "st1":
		month = size * st1GBMonth
	case "sc1":
		month = size * sc1GBMonth
	case "standard":
		month = size * standardGBMonth
	default: // gp3, and anything unknown
		month = size * gp3GBMonth
		if iops > gp3FreeIOPS {
			month += (iops - gp3FreeIOPS) * gp3IOPSMonth
		}
		if tp := float64(v.Throughput); tp > gp3FreeThroughput {
			month += (tp - gp3FreeThroughput) * gp3ThroughputMonth
		}
	}
	return month / hoursPerMonth
}

// Address is one public IPv4 address on the instance, as EC2 reports it on a
// network interface's association.
type Address struct {
	PublicIP string
	// IPOwnerID is "amazon" for an auto-assigned address and the account (or
	// BYOIP pool owner) for an Elastic IP.
	IPOwnerID string
}

// CountIPv4 splits the instance's public IPv4 addresses into auto-assigned and
// Elastic, counting each distinct address once. Every interface counts: a
// multi-interface EFA launch has its launch Elastic IP on the primary
// interface, and a user may associate more.
func CountIPv4(addrs []Address) (auto, elastic int) {
	seen := map[string]bool{}
	for _, a := range addrs {
		ip := strings.TrimSpace(a.PublicIP)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		if a.IPOwnerID == "amazon" {
			auto++
		} else {
			elastic++
		}
	}
	return auto, elastic
}
