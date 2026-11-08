package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spore-host/spawn/pkg/costmodel"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/tagprefix"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// Spot compute accounting for --cost-limit (#1497).
//
// Before #1497 a Spot instance's compute was charged at the on-demand rate
// (spawn:price-per-hour), about 3x its Spot price, so a Spot instance was
// terminated after about a third of the budget the user set. Now spored
// charges it at the Spot price:
//
//   - seeded from spawn:spot-accrual (an earlier spored's running sum), else
//     from spawn:spot-price-per-hour (what spawn read at launch);
//   - re-read every spotPriceSampleInterval from DescribeSpotPriceHistory for
//     the instance's type, zone and platform, and integrated over compute time
//     (costmodel.ComputeAccrual.Advance charges each interval at the higher of
//     the two prices bounding it, so the cap errs early);
//   - if the price cannot be read, or has not been read for
//     spotPriceStaleAfter, charged at the on-demand rate from then on, said in
//     the log, the spawn:spot-accrual tag, `spored status` and the task's
//     terminal completion record.
//
// The running sum is persisted in spawn:spot-accrual, which every CLI reader
// (pkg/costmodel.FromTags) charges compute from, so `spawn status`, `spawn
// cost` and `spawn extend` show the figure that terminates the instance.

// spotPriceSampleInterval is how often spored re-reads the Spot price. Spot
// prices move slowly (hours to days); one DescribeSpotPriceHistory call per 15
// minutes keeps the integration error to one interval at the higher price.
const spotPriceSampleInterval = 15 * time.Minute

// spotPriceStaleAfter is how long the last Spot price read stays in use while
// reads fail, before compute is charged at the on-demand rate instead.
const spotPriceStaleAfter = time.Hour

// spotSample is one Spot price read, handed from the sampler goroutine to the
// monitor loop, which owns the compute clock.
type spotSample struct {
	perHour   float64
	sampledAt time.Time // the price's own timestamp in the Spot price history
	readAt    time.Time // when spored read it
	err       error
}

// spotAccrual is the agent's Spot compute accounting. mu guards everything;
// the monitor loop writes it and CostLimitSpend may read it from another
// goroutine.
type spotAccrual struct {
	mu     sync.Mutex
	active bool // compute is charged through acc, not spawn:price-per-hour
	acc    costmodel.ComputeAccrual
	// lastRead is when spored last read the Spot price (or, before its first
	// read, when it started), which staleness is measured from.
	lastRead time.Time
	// fallback is why acc is on the on-demand fallback; "" when it is not.
	fallback string
	// failures counts consecutive failed reads, to log the first and then every
	// fourth rather than every 15 minutes.
	failures int
	// pending is the sampler's latest read, not yet applied.
	pending atomic.Pointer[spotSample]
}

// seedSpotAccrual starts the Spot accounting from what the instance's tags
// record. An instance with neither tag is costed at on-demand until the
// sampler, finding it is a Spot instance, activates it (activateSpotAccrual).
func (a *Agent) seedSpotAccrual(c *provider.Config, now time.Time) {
	s := &a.spot
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case c.SpotAccrual != nil:
		s.active, s.acc = true, *c.SpotAccrual
		if s.acc.Basis == costmodel.BasisOnDemandFallback {
			s.fallback = "an earlier spored run could not read the Spot price"
		}
	case c.SpotPricePerHour > 0:
		s.active = true
		s.acc = costmodel.ComputeAccrual{PerHour: c.SpotPricePerHour, Basis: costmodel.BasisSpot, SampledAt: c.LaunchTime}
	}
	// Staleness counts from this spored's start, not from the seed's
	// timestamp: the sampler reads at once, and gets spotPriceStaleAfter to
	// succeed before the fallback. (A Spot price's own timestamp is when it
	// last changed, which can be days ago for a price still in effect.)
	s.lastRead = now
}

// spotComputeAccrual is the accrual compute is charged through, if any.
func (a *Agent) spotComputeAccrual() (costmodel.ComputeAccrual, bool) {
	a.spot.mu.Lock()
	defer a.spot.mu.Unlock()
	return a.spot.acc, a.spot.active
}

// monitorSpotPrice reads the Spot price every interval in its own goroutine and
// leaves each read for the monitor loop to apply (applySpotSample), which owns
// the compute clock. It never blocks the lifecycle loop (#65). An on-demand
// instance, or a provider that cannot read Spot prices, returns at once.
func (a *Agent) monitorSpotPrice(ctx context.Context) {
	reader, ok := a.provider.(provider.SpotPriceReader)
	if !ok {
		return
	}
	if _, active := a.spotComputeAccrual(); !active && !a.provider.IsSpotInstance(ctx) {
		return
	}
	interval := a.spotSampleInterval
	if interval <= 0 {
		interval = spotPriceSampleInterval
	}
	read := func() {
		price, sampledAt, err := reader.CurrentSpotPrice(ctx)
		a.spot.pending.Store(&spotSample{perHour: price, sampledAt: sampledAt, readAt: a.spotClock(), err: err})
	}
	read()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			read()
		}
	}
}

// applySpotSample folds the sampler's latest read into the accrual, at the
// current compute time, and falls back to the on-demand rate when the price
// has been unreadable for spotPriceStaleAfter. Called by the monitor loop
// before the cost-limit check. Writes spawn:spot-accrual when the accrual
// changed.
func (a *Agent) applySpotSample(ctx context.Context) {
	sample := a.spot.pending.Swap(nil)
	cfg := a.cfg()
	now := a.spotClock()
	compute := a.TotalComputeSeconds()

	s := &a.spot
	s.mu.Lock()
	changed, msg := s.apply(sample, now, compute, cfg)
	acc := s.acc
	s.mu.Unlock()

	if msg != "" {
		log.Print(msg)
	}
	if changed {
		a.writeSpotAccrualTag(ctx, acc)
	}
}

// apply is applySpotSample's state change, separated for tests. s.mu is held.
func (s *spotAccrual) apply(sample *spotSample, now time.Time, compute int64, cfg *provider.Config) (changed bool, msg string) {
	onDemand := cfg.PricePerHour
	switch {
	case sample != nil && sample.err == nil && sample.perHour > 0:
		if !s.active {
			// The sampler found a Spot instance with no launch-recorded price:
			// its compute so far was enforced at on-demand, and stays charged so.
			s.active = true
			s.acc = costmodel.ComputeAccrual{PerHour: onDemand, Basis: costmodel.BasisOnDemandFallback}
		}
		prev := s.acc
		s.acc = s.acc.Advance(compute, sample.perHour, costmodel.BasisSpot, sample.sampledAt)
		s.lastRead = sample.readAt
		if s.fallback != "" || prev.Basis != costmodel.BasisSpot {
			msg = fmt.Sprintf("Cost limit: Spot price read again, $%.4f/hr (as of %s); compute is charged at the Spot price from now on",
				sample.perHour, sample.sampledAt.UTC().Format(time.RFC3339))
		} else if prev.PerHour != sample.perHour {
			msg = fmt.Sprintf("Cost limit: Spot price changed $%.4f/hr -> $%.4f/hr (as of %s)",
				prev.PerHour, sample.perHour, sample.sampledAt.UTC().Format(time.RFC3339))
		}
		s.fallback, s.failures = "", 0
		return true, msg

	case sample != nil:
		s.failures++
		err := sample.err
		if err == nil {
			err = fmt.Errorf("no positive Spot price returned")
		}
		if !s.active {
			// Not yet costed at Spot, so already at on-demand: activate on the
			// labelled fallback, so status and the record say why.
			s.active = true
			s.acc = costmodel.ComputeAccrual{PerHour: onDemand, Basis: costmodel.BasisOnDemandFallback}.
				Advance(compute, onDemand, costmodel.BasisOnDemandFallback, now)
			s.fallback = fmt.Sprintf("could not read the Spot price: %v", err)
			return true, "Warning: " + fallbackLogLine(s.fallback, onDemand, cfg)
		}
		if s.fallback == "" && now.Sub(s.lastRead) < spotPriceStaleAfter {
			if s.failures == 1 || s.failures%4 == 0 {
				msg = fmt.Sprintf("Warning: could not read the Spot price (%v); still charging compute at the last one read, $%.4f/hr, "+
					"until it is %v old, then at the on-demand rate", err, s.acc.PerHour, spotPriceStaleAfter)
			}
			return false, msg
		}
		if s.fallback == "" {
			s.fallback = fmt.Sprintf("could not read the Spot price since %s: %v", s.lastRead.UTC().Format(time.RFC3339), err)
			s.acc = s.acc.Advance(compute, onDemand, costmodel.BasisOnDemandFallback, now)
			return true, "Warning: " + fallbackLogLine(s.fallback, onDemand, cfg)
		}
		if s.failures%4 == 0 {
			msg = "Warning: " + fallbackLogLine(s.fallback, onDemand, cfg)
		}
		return false, msg

	case s.active && s.fallback == "" && s.acc.Basis == costmodel.BasisSpot && now.Sub(s.lastRead) >= spotPriceStaleAfter:
		// No read at all for too long (the sampler is stuck): the same fallback.
		s.fallback = fmt.Sprintf("no Spot price read since %s", s.lastRead.UTC().Format(time.RFC3339))
		s.acc = s.acc.Advance(compute, onDemand, costmodel.BasisOnDemandFallback, now)
		return true, "Warning: " + fallbackLogLine(s.fallback, onDemand, cfg)
	}
	return false, ""
}

// onDemandRateLabel describes the on-demand rate as a fallback must (owner
// rule, truffle#184): where it came from and when it was collected.
func onDemandRateLabel(perHour float64, cfg *provider.Config) string {
	when := "at a launch time not recorded"
	if !cfg.LaunchTime.IsZero() {
		when = "at launch, " + cfg.LaunchTime.UTC().Format(time.RFC3339)
	}
	var src string
	switch cfg.PriceSource {
	case "live":
		src = "read live from the AWS Price List " + when
	case "static":
		// The instance records only that the launch's rate was static, not
		// when that table was collected or why the live Price List was not
		// used (spawn#1512).
		src = "STATIC, from a static price table " + when + "; the table's collection date and why the live " +
			"Price List was not used are not recorded on the instance"
	default:
		src = "recorded " + when + ", source not recorded"
	}
	return fmt.Sprintf("on-demand rate $%.4f/hr (spawn:price-per-hour, %s)", perHour, src)
}

func fallbackLogLine(reason string, onDemand float64, cfg *provider.Config) string {
	return fmt.Sprintf("Cost limit: %s; charging compute at the %s instead, a conservative cap: a Spot instance never pays more than on-demand",
		reason, onDemandRateLabel(onDemand, cfg))
}

// writeSpotAccrualTag persists the accrual in spawn:spot-accrual. Best-effort:
// a failed write only means the next spored run, or a CLI reader, extends an
// older sum at its rate.
func (a *Agent) writeSpotAccrualTag(ctx context.Context, acc costmodel.ComputeAccrual) {
	if a.tagger == nil || a.identity == nil || a.identity.InstanceID == "" {
		return
	}
	tags := map[string]string{tagprefix.Tag(costmodel.TagComputeAccrual): costmodel.EncodeComputeAccrual(acc)}
	if err := a.tagger.putTags(ctx, a.identity.InstanceID, tags); err != nil {
		log.Printf("Warning: could not write the Spot compute accrual tag: %v", err)
	}
}

// CostLimitBasis describes the compute rate --cost-limit is charging now: for
// `spored status` and the terminal completion record. basis is
// costmodel.BasisSpot or BasisOnDemandFallback for a Spot instance, and
// "on-demand" for an on-demand one (fallback then empty).
type CostLimitBasis struct {
	Basis       string
	PerHour     float64
	CollectedAt time.Time
	Fallback    string
	Label       string
}

// CostLimitBasis reports how compute is being charged against --cost-limit.
func (a *Agent) CostLimitBasis() CostLimitBasis {
	cfg := a.cfg()
	acc, active := a.spotComputeAccrual()
	a.spot.mu.Lock()
	fallback := a.spot.fallback
	a.spot.mu.Unlock()
	switch {
	case !active:
		return CostLimitBasis{Basis: "on-demand", PerHour: cfg.PricePerHour, CollectedAt: cfg.LaunchTime,
			Label: "compute at the " + onDemandRateLabel(cfg.PricePerHour, cfg)}
	case acc.Basis == costmodel.BasisSpot:
		return CostLimitBasis{Basis: costmodel.BasisSpot, PerHour: acc.PerHour, CollectedAt: acc.SampledAt,
			Label: fmt.Sprintf("compute at the Spot price, now $%.4f/hr (DescribeSpotPriceHistory, as of %s; re-read every %v)",
				acc.PerHour, formatWhen(acc.SampledAt), spotPriceSampleInterval)}
	default:
		if fallback == "" {
			fallback = "spored could not read the Spot price"
		}
		return CostLimitBasis{Basis: costmodel.BasisOnDemandFallback, PerHour: acc.PerHour, CollectedAt: cfg.LaunchTime,
			Fallback: fallback,
			Label:    fmt.Sprintf("compute at the %s, FALLBACK for a Spot instance: %s", onDemandRateLabel(acc.PerHour, cfg), fallback)}
	}
}

func formatWhen(t time.Time) string {
	if t.IsZero() {
		return "time unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

// costBasisJSON is the CostBasis object the terminal-flush hook records for a
// cost-limit termination (spawn#1497), passed in taskproto.CostBasisEnv.
func (a *Agent) costBasisJSON() string {
	cfg := a.cfg()
	if cfg == nil {
		return ""
	}
	b := a.CostLimitBasis()
	rec := taskproto.CostBasis{
		Limit:          cfg.CostLimit,
		Accrued:        a.accumulatedComputeCost(),
		ComputeBasis:   b.Basis,
		ComputePerHour: b.PerHour,
		Fallback:       b.Fallback,
	}
	if !b.CollectedAt.IsZero() {
		rec.RateCollectedAt = b.CollectedAt.UTC().Format(time.RFC3339)
	}
	if b.Basis != costmodel.BasisSpot {
		rec.RateSource = cfg.PriceSource
	}
	out, err := json.Marshal(rec)
	if err != nil {
		return ""
	}
	return string(out)
}
