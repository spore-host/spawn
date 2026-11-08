package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/costmodel"
	"github.com/spore-host/spawn/pkg/provider"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// The reported instance (#1497): c9gd.16xlarge in us-east-1, Spot $1.09/hr,
// on-demand $3.418/hr, --cost-limit 7. spored terminated it after 2.05 h, when
// on-demand x compute reached $7.01; its Spot spend was about $2.23.
const (
	issueSpotRate     = 1.09
	issueOnDemandRate = 3.418
	issueCostLimit    = 7.0
	issueRunTime      = 2*time.Hour + 3*time.Minute // 2.05 h
)

// capAgent is an agent at the issue's point: 2.05 h of compute this boot,
// its periodic tag writes pushed into the future so checkAndAct makes no
// EC2 call, seeded as newAgent seeds it.
func capAgent(t *testing.T, cfg *provider.Config, spot bool) (*Agent, *stubProvider, *fakeTagPutter) {
	t.Helper()
	identity := &provider.Identity{InstanceID: "i-spot", Region: "us-east-1", Provider: "stub"}
	sp := &stubProvider{identity: identity, config: cfg, spot: spot}
	tags := &fakeTagPutter{}
	now := time.Now()
	a := &Agent{
		provider:              sp,
		identity:              identity,
		config:                cfg,
		startTime:             now.Add(-issueRunTime),
		lastActivityTime:      now,
		lastSessionTagWrite:   now.Add(time.Hour),
		lastComputeTagWrite:   now.Add(time.Hour),
		lastHeartbeatTagWrite: now.Add(time.Hour),
		tagger:                tags,
	}
	a.seedSpotAccrual(cfg, now)
	return a, sp, tags
}

func issueConfig() *provider.Config {
	return &provider.Config{
		PricePerHour:     issueOnDemandRate,
		SpotPricePerHour: issueSpotRate,
		CostLimit:        issueCostLimit,
		LaunchTime:       time.Now().Add(-issueRunTime),
		PriceSource:      "live",
	}
}

// A Spot instance accrues at its Spot price, and the issue's instance is NOT
// terminated at the on-demand-equivalent point.
func TestSpotInstanceAccruesAtTheSpotRate(t *testing.T) {
	a, sp, _ := capAgent(t, issueConfig(), true)

	got := a.CostLimitSpend()
	if want := issueSpotRate * issueRunTime.Hours(); got < want-0.01 || got > want+0.01 {
		t.Fatalf("CostLimitSpend = $%.4f after 2.05 h of Spot, want ~$%.2f (the Spot rate), not the on-demand $%.2f",
			got, want, issueOnDemandRate*issueRunTime.Hours())
	}
	a.checkAndAct(context.Background())
	if sp.terminated {
		t.Fatal("the issue's Spot instance was terminated at ~$2.23 of real spend against a $7 cost limit (#1497)")
	}
	if b := a.CostLimitBasis(); b.Basis != costmodel.BasisSpot || b.PerHour != issueSpotRate || b.Fallback != "" {
		t.Errorf("basis = %+v, want spot at $%.2f/hr with no fallback", b, issueSpotRate)
	}
}

// An on-demand instance is unchanged: charged at spawn:price-per-hour, and the
// issue's numbers terminate it.
func TestOnDemandInstanceIsUnchanged(t *testing.T) {
	cfg := issueConfig()
	cfg.SpotPricePerHour = 0
	a, sp, tags := capAgent(t, cfg, false)

	if f := a.costFootprint(); f.Accrual != nil || f.ComputePerHour != issueOnDemandRate {
		t.Fatalf("on-demand footprint = %+v, want the on-demand rate and no accrual", f)
	}
	if got, want := a.CostLimitSpend(), issueOnDemandRate*issueRunTime.Hours(); got < want-0.01 || got > want+0.01 {
		t.Fatalf("CostLimitSpend = $%.4f, want $%.4f (on-demand x compute)", got, want)
	}
	a.checkAndAct(context.Background())
	if !sp.terminated {
		t.Error("an on-demand instance past its cost limit must still terminate")
	}
	if b := a.CostLimitBasis(); b.Basis != "on-demand" || b.Fallback != "" {
		t.Errorf("basis = %+v, want plain on-demand", b)
	}
	for _, w := range tags.writes {
		if _, ok := w["spawn:"+costmodel.TagComputeAccrual]; ok {
			t.Errorf("an on-demand instance wrote a Spot accrual: %v", w)
		}
	}
}

// A price read is integrated and persisted in spawn:spot-accrual.
func TestSpotSampleIsIntegratedAndRecorded(t *testing.T) {
	a, _, tags := capAgent(t, issueConfig(), true)
	at := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	a.spot.pending.Store(&spotSample{perHour: 1.20, sampledAt: at, readAt: time.Now()})
	a.applySpotSample(context.Background())

	acc, ok := a.spotComputeAccrual()
	if !ok || acc.PerHour != 1.20 || acc.Basis != costmodel.BasisSpot || !acc.SampledAt.Equal(at) {
		t.Fatalf("accrual = %+v, want spot at $1.20 as of %s", acc, at)
	}
	// The 2.05 h before the read are charged at the higher of the two prices.
	if want := 1.20 * issueRunTime.Hours(); acc.Dollars < want-0.01 || acc.Dollars > want+0.01 {
		t.Errorf("accrued = $%.4f, want ~$%.4f", acc.Dollars, want)
	}
	v := tags.tags["spawn:"+costmodel.TagComputeAccrual]
	if got, ok := costmodel.ParseComputeAccrual(v); !ok || got.PerHour != 1.20 {
		t.Errorf("spawn:spot-accrual = %q, want the accrual at $1.20", v)
	}
}

// A failed read keeps the last price while it is fresh, then falls back to the
// on-demand rate, labelled with why, the rate's source and when it was
// collected; a later good read restores the Spot price.
func TestSpotPriceUnreadableFallsBackToOnDemandLabelled(t *testing.T) {
	cfg := issueConfig()
	s := &spotAccrual{}
	now := time.Now()
	s.active, s.acc, s.lastRead = true, costmodel.ComputeAccrual{PerHour: issueSpotRate, Basis: costmodel.BasisSpot}, now
	denied := errors.New("UnauthorizedOperation: ec2:DescribeSpotPriceHistory")

	if changed, _ := s.apply(&spotSample{err: denied, readAt: now}, now.Add(10*time.Minute), 3600, cfg); changed || s.fallback != "" {
		t.Fatalf("a failed read 10 min after a good one changed the basis: fallback %q", s.fallback)
	}
	changed, msg := s.apply(&spotSample{err: denied}, now.Add(spotPriceStaleAfter), 7200, cfg)
	if !changed || s.acc.Basis != costmodel.BasisOnDemandFallback || s.acc.PerHour != issueOnDemandRate {
		t.Fatalf("after %v of failed reads: changed=%v acc=%+v, want the on-demand fallback", spotPriceStaleAfter, changed, s.acc)
	}
	for _, want := range []string{"could not read the Spot price", "UnauthorizedOperation", "$3.4180/hr", "spawn:price-per-hour", "AWS Price List", "at launch, "} {
		if !strings.Contains(msg, want) {
			t.Errorf("fallback log line missing %q:\n%s", want, msg)
		}
	}
	// Everything since the last good read (here, the 2 h from the start) is
	// charged at on-demand, the higher price: where in that span the Spot price
	// moved is unknown.
	if want := 2 * issueOnDemandRate; s.acc.Dollars < want-0.001 || s.acc.Dollars > want+0.001 {
		t.Errorf("accrued = $%.4f at the fallback, want $%.4f", s.acc.Dollars, want)
	}

	if changed, msg := s.apply(&spotSample{perHour: 1.0, sampledAt: now, readAt: now}, now.Add(2*time.Hour), 9000, cfg); !changed ||
		s.acc.Basis != costmodel.BasisSpot || s.fallback != "" || !strings.Contains(msg, "read again") {
		t.Errorf("a good read after the fallback: changed=%v acc=%+v fallback=%q msg=%q", changed, s.acc, s.fallback, msg)
	}
}

// A sampler that produces nothing at all is the same as one that fails.
func TestSpotPriceNeverReadFallsBack(t *testing.T) {
	s := &spotAccrual{}
	now := time.Now()
	s.active, s.acc, s.lastRead = true, costmodel.ComputeAccrual{PerHour: issueSpotRate, Basis: costmodel.BasisSpot}, now
	if changed, _ := s.apply(nil, now.Add(time.Minute), 60, issueConfig()); changed {
		t.Fatal("no read for a minute changed the basis")
	}
	if changed, msg := s.apply(nil, now.Add(spotPriceStaleAfter), 3600, issueConfig()); !changed ||
		s.acc.Basis != costmodel.BasisOnDemandFallback || !strings.Contains(msg, "no Spot price read since") {
		t.Errorf("no read for %v: changed=%v acc=%+v msg=%q", spotPriceStaleAfter, changed, s.acc, msg)
	}
}

// A Spot instance with no launch-recorded price is costed at on-demand until a
// read: the compute before it stays charged at on-demand, and after it at Spot.
func TestSpotWithoutLaunchPriceActivatesOnFirstRead(t *testing.T) {
	cfg := issueConfig()
	cfg.SpotPricePerHour = 0
	s := &spotAccrual{}
	if changed, _ := s.apply(&spotSample{perHour: issueSpotRate, readAt: time.Now()}, time.Now(), 3600, cfg); !changed || !s.active {
		t.Fatal("the first good read did not activate the Spot accounting")
	}
	if s.acc.Dollars < issueOnDemandRate-0.001 || s.acc.Dollars > issueOnDemandRate+0.001 || s.acc.PerHour != issueSpotRate {
		t.Errorf("accrual = %+v, want the first hour at on-demand and Spot from then on", s.acc)
	}

	// And a first read that fails activates the labelled fallback.
	s2 := &spotAccrual{}
	if changed, _ := s2.apply(&spotSample{err: errors.New("throttled")}, time.Now(), 60, cfg); !changed ||
		s2.acc.Basis != costmodel.BasisOnDemandFallback || !strings.Contains(s2.fallback, "throttled") {
		t.Errorf("a failing first read: acc=%+v fallback=%q, want the labelled on-demand fallback", s2.acc, s2.fallback)
	}
}

// spored restarted (stop/start, upgrade): the accrual carries over from its tag.
func TestSpotAccrualSeedsFromItsTag(t *testing.T) {
	cfg := issueConfig()
	cfg.SpotAccrual = &costmodel.ComputeAccrual{Dollars: 1.5, AtSeconds: 3600, PerHour: 1.25, Basis: costmodel.BasisSpot}
	a, _, _ := capAgent(t, cfg, true)
	a.computeSecondsBase = 3600
	a.startTime = time.Now().Add(-time.Hour) // 2 h total
	if got, want := a.CostLimitSpend(), 1.5+1.25; got < want-0.01 || got > want+0.01 {
		t.Errorf("CostLimitSpend = $%.4f, want $%.2f (the carried $1.50 + 1 h at $1.25)", got, want)
	}

	cfg2 := issueConfig()
	cfg2.SpotAccrual = &costmodel.ComputeAccrual{PerHour: issueOnDemandRate, Basis: costmodel.BasisOnDemandFallback}
	b, _, _ := capAgent(t, cfg2, true)
	if lb := b.CostLimitBasis(); lb.Fallback == "" || !strings.Contains(lb.Label, "FALLBACK") {
		t.Errorf("a carried-over fallback is not labelled: %+v", lb)
	}
}

// The terminal completion record says how the cap priced the instance, and a
// fallback says so, why, and when and where its rate was collected.
func TestCostBasisJSONLabelsTheFallback(t *testing.T) {
	cfg := issueConfig()
	cfg.PriceSource = "static"
	a, _, _ := capAgent(t, cfg, true)
	a.spot.acc = costmodel.ComputeAccrual{PerHour: issueOnDemandRate, Basis: costmodel.BasisOnDemandFallback}
	a.spot.fallback = "could not read the Spot price: AccessDenied"

	var rec taskproto.CostBasis
	if err := json.Unmarshal([]byte(a.costBasisJSON()), &rec); err != nil {
		t.Fatalf("cost basis JSON: %v", err)
	}
	if rec.ComputeBasis != costmodel.BasisOnDemandFallback || rec.ComputePerHour != issueOnDemandRate ||
		!strings.Contains(rec.Fallback, "AccessDenied") || rec.RateSource != "static" || rec.RateCollectedAt == "" ||
		rec.Limit != issueCostLimit || rec.Accrued <= 0 {
		t.Errorf("cost basis = %+v", rec)
	}
	if lb := a.CostLimitBasis(); !strings.Contains(lb.Label, "STATIC") || !strings.Contains(lb.Label, "collection date") {
		t.Errorf("a static fallback rate is not labelled static with its collection date: %s", lb.Label)
	}
}

// The hook gets the cost basis only for a cost-limit termination, and never
// inherits one from spored's environment.
func TestFlushHookEnvCarriesTheCostBasisOnlyForTheCostLimit(t *testing.T) {
	inherited := []string{"PATH=/bin", taskproto.CostBasisEnv + "=stale"}
	basis := func() string { return `{"compute_basis":"spot"}` }

	got := flushHookEnv(inherited, taskproto.ExitCostLimitExceeded, basis)
	if strings.Join(got, "\n") != "PATH=/bin\n"+taskproto.CostBasisEnv+`={"compute_basis":"spot"}` {
		t.Errorf("cost limit env = %v", got)
	}
	got = flushHookEnv(inherited, taskproto.ExitTTLExpired, basis)
	if strings.Join(got, "\n") != "PATH=/bin" {
		t.Errorf("TTL env = %v, want the inherited value removed and none added", got)
	}
}

// spotReadingProvider is a Spot instance's provider that reads a Spot price.
type spotReadingProvider struct {
	*stubProvider
	price float64
	err   error
	reads chan struct{}
}

func (p *spotReadingProvider) CurrentSpotPrice(context.Context) (float64, time.Time, error) {
	defer func() { p.reads <- struct{}{} }()
	return p.price, time.Now(), p.err
}

// The sampler reads at once and leaves the read for the monitor loop; an
// on-demand instance never reads.
func TestMonitorSpotPriceReadsOffTheLoop(t *testing.T) {
	identity := &provider.Identity{InstanceID: "i-spot", Region: "us-east-1", Provider: "stub"}
	p := &spotReadingProvider{stubProvider: &stubProvider{identity: identity, config: issueConfig(), spot: true},
		price: 1.5, reads: make(chan struct{}, 4)}
	a := &Agent{provider: p, identity: identity, config: issueConfig(), spotSampleInterval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.monitorSpotPrice(ctx); close(done) }()
	select {
	case <-p.reads:
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler never read the Spot price")
	}
	cancel()
	<-done
	if s := a.spot.pending.Load(); s == nil || s.perHour != 1.5 {
		t.Errorf("pending read = %+v, want $1.50", s)
	}

	od := &spotReadingProvider{stubProvider: &stubProvider{identity: identity, config: &provider.Config{PricePerHour: 1}},
		reads: make(chan struct{}, 1)}
	b := &Agent{provider: od, identity: identity, config: &provider.Config{PricePerHour: 1}}
	b.monitorSpotPrice(context.Background()) // returns at once: not Spot
	if len(od.reads) != 0 {
		t.Error("an on-demand instance read a Spot price")
	}
}
