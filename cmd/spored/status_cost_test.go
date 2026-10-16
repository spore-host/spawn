package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/provider"
)

// The reported instance (#896): an m8a.4xlarge launched with --spot in
// us-west-2, on-demand $0.9738/hr, spot about $0.36/hr.
const (
	issueOnDemand = 0.9738
	issueSpot     = 0.36
	issueEBS      = 0.0022
)

func statusIdentity() *provider.Identity {
	return &provider.Identity{
		InstanceID: "i-028c23f816d2eb68e", Name: "demo", Region: "us-west-2",
		AccountID: "123456789012", Provider: "stub",
	}
}

// queryAgent builds the agent the way `spored status` does: a fresh process
// whose own start time is now.
func queryAgent(t *testing.T, cfg *provider.Config) *agent.Agent {
	t.Helper()
	ag, err := agent.NewAgentForQuery(context.Background(), &stubStatusProvider{identity: statusIdentity(), config: cfg})
	if err != nil {
		t.Fatalf("NewAgentForQuery: %v", err)
	}
	return ag
}

// publishAndAdopt writes the daemon's clock the way runDaemon does and has the
// status agent read it back, through the real file round trip.
func publishAndAdopt(t *testing.T, ag *agent.Agent, daemonStart time.Time, base int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compute-clock.json")
	if err := agent.WriteComputeClock(path, agent.ComputeClock{
		InstanceID: statusIdentity().InstanceID, StartedAt: daemonStart, BaseSeconds: base,
	}); err != nil {
		t.Fatalf("WriteComputeClock: %v", err)
	}
	c, err := agent.ReadComputeClock(path)
	if err != nil {
		t.Fatalf("ReadComputeClock: %v", err)
	}
	if err := ag.AdoptComputeClock(c, time.Time{}); err != nil {
		t.Fatalf("AdoptComputeClock: %v", err)
	}
}

func renderTable(t *testing.T, r *statusReport) string {
	t.Helper()
	return captureStdout(t, func() {
		if err := renderStatusTable(r); err != nil {
			t.Fatalf("renderStatusTable: %v", err)
		}
	})
}

// A spot instance is costed at its spot price, not the on-demand rate, and the
// on-demand rate stays on screen as the comparison baseline (#896).
func TestStatusReport_SpotInstanceIsCostedAtItsSpotPrice(t *testing.T) {
	cfg := &provider.Config{
		LaunchTime:       time.Now().Add(-2 * time.Hour),
		PricePerHour:     issueOnDemand,
		SpotPricePerHour: issueSpot,
		EBSHourlyCost:    issueEBS,
		CostLimit:        10,
	}
	ag := queryAgent(t, cfg)
	publishAndAdopt(t, ag, time.Now().Add(-2*time.Hour+time.Minute), 0)
	r := buildStatusReport(ag, cfg, statusIdentity(), true)

	if r.Cost == nil {
		t.Fatal("no cost section for a priced instance")
	}
	if r.Cost.RatePerHour != issueSpot || r.Cost.RateBasis != rateBasisSpot {
		t.Fatalf("rate = $%.4f (%s), want the spot price $%.4f", r.Cost.RatePerHour, r.Cost.RateBasis, issueSpot)
	}
	// Two hours at the spot price, not at on-demand ($1.95).
	if r.Cost.ComputeCost < 0.70 || r.Cost.ComputeCost > 0.74 {
		t.Errorf("compute cost = $%.2f for 2h of spot at $%.2f/hr, want ~$0.72", r.Cost.ComputeCost, issueSpot)
	}
	if r.Cost.PricePerHour != issueOnDemand {
		t.Errorf("price_per_hour = %.4f, want the on-demand baseline %.4f", r.Cost.PricePerHour, issueOnDemand)
	}
	// Two hours at spot is ~63% below continuous on-demand — a real saving.
	if r.Cost.EffectiveRate == nil || r.Cost.SavingsPercent < 55 {
		t.Errorf("effective rate/saving = %v/%.0f%%, want the spot discount to show", r.Cost.EffectiveRate, r.Cost.SavingsPercent)
	}
	// --cost-limit is enforced at the on-demand rate; status must not report it
	// as further from the limit than enforcement measures.
	if r.Cost.CostLimitUsed < 1.8 {
		t.Errorf("cost-limit used = $%.2f, want the on-demand measure (~$1.9) that enforcement uses", r.Cost.CostLimitUsed)
	}

	out := renderTable(t, r)
	for _, want := range []string{"$0.3600/hr spot", "On-demand rate:   $0.9738/hr", "Spot rate:"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "× $0.9738/hr") {
		t.Errorf("compute is still charged at the on-demand rate:\n%s", out)
	}
}

// A spot instance with no recorded spot price falls back to on-demand, and the
// output says the figures overstate rather than presenting them as the spot cost.
func TestStatusReport_SpotWithoutRecordedPriceIsLabelledAsOnDemand(t *testing.T) {
	cfg := &provider.Config{LaunchTime: time.Now().Add(-time.Hour), PricePerHour: issueOnDemand, EBSHourlyCost: issueEBS}
	ag := queryAgent(t, cfg)
	publishAndAdopt(t, ag, time.Now().Add(-59*time.Minute), 0)
	r := buildStatusReport(ag, cfg, statusIdentity(), true)

	if r.Cost.RateBasis != rateBasisSpotUnknown {
		t.Errorf("rate basis = %q, want %q", r.Cost.RateBasis, rateBasisSpotUnknown)
	}
	out := renderTable(t, r)
	if !strings.Contains(out, "spot price unknown") || !strings.Contains(out, "overstate") {
		t.Errorf("a spot instance costed at on-demand is not labelled as such:\n%s", out)
	}
}

// The reported shape: 47s after launch, `spored status` — a fresh process —
// said "0s" of compute. With the daemon's clock it counts the running time.
func TestStatusReport_QueryCountsTheDaemonsRunningTime(t *testing.T) {
	cfg := &provider.Config{LaunchTime: time.Now().Add(-47 * time.Second), PricePerHour: issueOnDemand,
		SpotPricePerHour: issueSpot, EBSHourlyCost: issueEBS}
	ag := queryAgent(t, cfg)
	daemonStart := time.Now().Add(-30 * time.Second) // daemon up 17s after launch
	publishAndAdopt(t, ag, daemonStart, 0)
	// Sampling inside buildStatusReport is not instant (seconds on Windows, where
	// each sample is a PowerShell process), so "now" is a window, not an instant.
	before := time.Now()
	r := buildStatusReport(ag, cfg, statusIdentity(), true)
	after := time.Now()
	within := func(got float64, from time.Time) bool {
		return got >= before.Sub(from).Seconds()-1 && got <= after.Sub(from).Seconds()+1
	}

	if r.ComputeClock != computeClockLive {
		t.Errorf("compute clock = %q, want live", r.ComputeClock)
	}
	if !within(r.ComputeSeconds, daemonStart) {
		t.Errorf("compute = %.0fs, want %.0f–%.0fs (the daemon's running time)", r.ComputeSeconds,
			before.Sub(daemonStart).Seconds(), after.Sub(daemonStart).Seconds())
	}
	// Billing started at launch, so the boot gap is billed too.
	if !within(r.BilledSeconds, cfg.LaunchTime) {
		t.Errorf("billed = %.0fs, want %.0f–%.0fs (launch to now)", r.BilledSeconds,
			before.Sub(cfg.LaunchTime).Seconds(), after.Sub(cfg.LaunchTime).Seconds())
	}
	out := renderTable(t, r)
	if strings.Contains(out, "(0s ×") {
		t.Errorf("compute still reads 0s at 47s elapsed:\n%s", out)
	}
}

// Never claim a saving from a ratio of zeros. Built directly on the live path
// (launch == daemon start == now) as well as the checkpoint path, because the
// guard must hold for both, not just the one the reported case took.
func TestStatusReport_NoRunningTimeClaimsNoSaving(t *testing.T) {
	cases := map[string]func() (*statusReport, *provider.Config){
		"checkpoint, nothing written yet": func() (*statusReport, *provider.Config) {
			cfg := &provider.Config{LaunchTime: time.Now().Add(-47 * time.Second), PricePerHour: issueOnDemand, EBSHourlyCost: issueEBS}
			return buildStatusReport(queryAgent(t, cfg), cfg, statusIdentity(), true), cfg
		},
		"live clock, zero running time": func() (*statusReport, *provider.Config) {
			now := time.Now()
			cfg := &provider.Config{LaunchTime: now, PricePerHour: issueOnDemand, EBSHourlyCost: issueEBS}
			ag := queryAgent(t, cfg)
			publishAndAdopt(t, ag, now, 0)
			time.Sleep(20 * time.Millisecond) // elapsed > 0, running time still 0
			return buildStatusReport(ag, cfg, statusIdentity(), false), cfg
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := build()
			if r.Cost == nil {
				t.Fatal("no cost section")
			}
			if r.Cost.SavingsPercent != 0 {
				t.Errorf("savings = %.0f%% with no running time recorded", r.Cost.SavingsPercent)
			}
			if r.Cost.EffectiveRate != nil && *r.Cost.EffectiveRate == 0 {
				t.Errorf("effective rate $0/hr reported from no data")
			}
			out := renderTable(t, r)
			for _, bad := range []string{"lower than continuous", "$0.0000/hr", "(0s ×"} {
				if strings.Contains(out, bad) {
					t.Errorf("output contains %q with no running time recorded:\n%s", bad, out)
				}
			}
			// Sampling takes seconds on Windows, so by the time the live clock is
			// read it may genuinely have run a second or more; "unknown" is owed
			// exactly when the report has under a second of billed time. On Linux
			// that is always, so the gate bites there.
			if r.BilledSeconds < 1 {
				if !strings.Contains(out, "Compute cost:     —") {
					t.Errorf("compute cost is not shown as unknown:\n%s", out)
				}
			} else if r.ComputeClock != computeClockLive {
				t.Errorf("billed = %.0fs on the checkpoint path with no checkpoint written:\n%s", r.BilledSeconds, out)
			}
		})
	}
}
