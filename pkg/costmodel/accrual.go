package costmodel

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// The bases a ComputeAccrual charges compute at (#1497).
const (
	// BasisSpot: the Spot price spored read from DescribeSpotPriceHistory for
	// the instance's type, zone and platform (or that spawn recorded at
	// launch, spawn:spot-price-per-hour, until spored's first read).
	BasisSpot = "spot"
	// BasisOnDemandFallback: spored could not read the Spot price, so compute
	// is charged at the on-demand rate spawn recorded at launch
	// (spawn:price-per-hour). A Spot instance never pays more than that, so the
	// cap fires early, never late.
	BasisOnDemandFallback = "on-demand"
)

// computeAccrualVersion prefixes the spawn:spot-accrual value. A later version
// may only APPEND fields.
const computeAccrualVersion = "v1"

// ComputeAccrual is a Spot instance's compute cost, integrated over its compute
// time from the prices spored has sampled (#1497). Spot prices move, so a
// single rate x compute time cannot be right for a long-lived instance; this is
// a running sum that a reader extends at the current rate:
//
//	cost(compute) = Dollars + PerHour x (compute - AtSeconds)
//
// Before #1497 --cost-limit charged a Spot instance's compute at the on-demand
// rate, about 3x its Spot price, and terminated it at about a third of the
// budget the user set.
type ComputeAccrual struct {
	// Dollars is the compute cost accrued up to AtSeconds of compute time.
	Dollars float64
	// AtSeconds is the compute time (spawn:compute-seconds) Dollars runs to.
	AtSeconds int64
	// PerHour is the rate compute after AtSeconds is charged at, and Basis
	// says which rate it is: BasisSpot or BasisOnDemandFallback.
	PerHour float64
	Basis   string
	// SampledAt is when PerHour was read: the Spot price's sample time, or,
	// for the fallback, when spored fell back to it. Zero when unknown.
	SampledAt time.Time
}

// At is the compute cost at the given total compute time. Compute before
// AtSeconds is already in Dollars; a compute time below it (a lagging
// spawn:compute-seconds) adds nothing.
func (c ComputeAccrual) At(compute time.Duration) float64 {
	extra := compute.Seconds() - float64(c.AtSeconds)
	if extra <= 0 {
		return c.Dollars
	}
	return c.Dollars + c.PerHour*extra/3600
}

// Advance rolls the accrual forward to computeSeconds and starts charging
// perHour on basis from there. The compute since the last sample is charged at
// the HIGHER of the old and the new rate: the price moved somewhere in that
// interval and spored cannot tell where, so it errs toward firing early. A
// computeSeconds at or below AtSeconds only replaces the rate.
func (c ComputeAccrual) Advance(computeSeconds int64, perHour float64, basis string, sampledAt time.Time) ComputeAccrual {
	if computeSeconds > c.AtSeconds {
		c.Dollars += max(c.PerHour, perHour) * float64(computeSeconds-c.AtSeconds) / 3600
		c.AtSeconds = computeSeconds
	}
	c.PerHour, c.Basis, c.SampledAt = perHour, basis, sampledAt
	return c
}

// EncodeComputeAccrual is the spawn:spot-accrual value:
// "v1:<dollars>/<at compute seconds>/<rate $/hr>/<basis>/<sampled at, unix>",
// separated by "/" as spawn:cost-footprint is. Well inside EC2's 256-character
// tag limit.
func EncodeComputeAccrual(c ComputeAccrual) string {
	var sampled int64
	if !c.SampledAt.IsZero() {
		sampled = c.SampledAt.Unix()
	}
	return computeAccrualVersion + ":" +
		strconv.FormatFloat(finiteNonNeg(c.Dollars), 'f', 6, 64) + "/" +
		strconv.FormatInt(max(c.AtSeconds, 0), 10) + "/" +
		strconv.FormatFloat(finiteNonNeg(c.PerHour), 'f', 6, 64) + "/" +
		c.Basis + "/" + strconv.FormatInt(sampled, 10)
}

// ParseComputeAccrual reads a spawn:spot-accrual value. ok is false for
// anything this reader does not understand, and the reader then charges the
// on-demand rate, as a spored before #1497 enforced.
func ParseComputeAccrual(v string) (c ComputeAccrual, ok bool) {
	ver, body, found := strings.Cut(strings.TrimSpace(v), ":")
	if !found || len(ver) < 2 || ver[0] != 'v' {
		return ComputeAccrual{}, false
	}
	if n, err := strconv.Atoi(ver[1:]); err != nil || n < 1 {
		return ComputeAccrual{}, false
	}
	f := strings.Split(body, "/")
	if len(f) < 5 {
		return ComputeAccrual{}, false
	}
	dollars, err1 := strconv.ParseFloat(f[0], 64)
	at, err2 := strconv.ParseInt(f[1], 10, 64)
	rate, err3 := strconv.ParseFloat(f[2], 64)
	sampled, err4 := strconv.ParseInt(f[4], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return ComputeAccrual{}, false
	}
	if !(dollars >= 0) || math.IsInf(dollars, 0) || at < 0 || !(rate > 0) || math.IsInf(rate, 0) {
		return ComputeAccrual{}, false
	}
	if f[3] != BasisSpot && f[3] != BasisOnDemandFallback {
		return ComputeAccrual{}, false
	}
	c = ComputeAccrual{Dollars: dollars, AtSeconds: at, PerHour: rate, Basis: f[3]}
	if sampled > 0 {
		c.SampledAt = time.Unix(sampled, 0).UTC()
	}
	return c, true
}

func finiteNonNeg(v float64) float64 {
	if !(v >= 0) || math.IsInf(v, 0) {
		return 0
	}
	return v
}
