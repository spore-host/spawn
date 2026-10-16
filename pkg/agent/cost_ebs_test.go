package agent

import (
	"context"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/provider"
)

// TestAccumulatedCostIncludesEBS is spawn#616's in-flight half. --cost-limit became a
// total, so the running tally has to include the storage that dies with the instance —
// which is the storage terminating actually stops.
func TestAccumulatedCostIncludesEBS(t *testing.T) {
	launch := time.Now().Add(-2 * time.Hour)

	computeOnly := &Agent{config: &provider.Config{PricePerHour: 1.0, LaunchTime: launch}}
	computeOnly.computeSecondsBase = int64((1 * time.Hour).Seconds())
	computeOnly.startTime = time.Now()

	withEBS := &Agent{config: &provider.Config{PricePerHour: 1.0, EBSHourlyCost: 0.5, LaunchTime: launch}}
	withEBS.computeSecondsBase = int64((1 * time.Hour).Seconds())
	withEBS.startTime = time.Now()

	base := computeOnly.accumulatedComputeCost()
	total := withEBS.accumulatedComputeCost()

	if total <= base {
		t.Errorf("EBS must raise the accumulated cost: compute-only=%.4f with-EBS=%.4f", base, total)
	}

	// EBS bills on WALL CLOCK, not compute time. The instance ran 1h but launched 2h
	// ago, so EBS must contribute ~2 x 0.5 = ~1.0, not 1 x 0.5 = 0.5. Pricing it over
	// compute hours would undercount exactly the case that matters: an instance
	// stopped "to save money" still paying for its volumes.
	ebsPart := total - base
	if ebsPart < 0.9 || ebsPart > 1.1 {
		t.Errorf("EBS contribution = %.4f, want ~1.00 (2h wall clock x $0.50/hr). "+
			"If it is ~0.50 this is being priced over compute hours and undercounts a stopped instance.", ebsPart)
	}
}

// TestAccumulatedCostWithoutEBSRateIsUnchanged: an instance whose EBS rate was never
// resolved must not have its cap arithmetic altered, or the change would silently
// shift the cap for every pre-existing instance.
func TestAccumulatedCostWithoutEBSRateIsUnchanged(t *testing.T) {
	a := &Agent{config: &provider.Config{PricePerHour: 2.0, LaunchTime: time.Now().Add(-3 * time.Hour)}}
	a.computeSecondsBase = int64((1 * time.Hour).Seconds())
	a.startTime = time.Now()

	got := a.accumulatedComputeCost()
	if got < 1.9 || got > 2.3 {
		t.Errorf("with no EBS rate the cost must be compute only (~$2.00), got %.4f", got)
	}
}

// TestCheckAndAct_CostLimitTerminatesOnEBS is the enforcement half of #616 that the
// help text and `spored status` describe (#959): crossing --cost-limit TERMINATES the
// instance (never stop or hibernate — a stopped instance keeps billing EBS with no
// daemon left to count it), and the spend that crosses it includes EBS. Compute here
// is well under the cap; only the EBS accrued since launch pushes it over.
func TestCheckAndAct_CostLimitTerminatesOnEBS(t *testing.T) {
	identity := &provider.Identity{InstanceID: "i-cap", Region: "us-east-1", Provider: "stub"}
	cfg := &provider.Config{
		PricePerHour:    1.0, // 1h compute → $1
		EBSHourlyCost:   1.0, // 10h since launch → $10
		LaunchTime:      time.Now().Add(-10 * time.Hour),
		CostLimit:       5.0,  // compute alone ($1) is under it; compute + EBS ($11) is over
		HibernateOnIdle: true, // proves the cap path ignores the idle action
	}
	sp := &stubProvider{identity: identity, config: cfg}
	a := &Agent{
		provider:              sp,
		identity:              identity,
		config:                cfg,
		startTime:             time.Now().Add(-1 * time.Hour),
		lastActivityTime:      time.Now(),
		lastSessionTagWrite:   time.Now().Add(time.Hour),
		lastComputeTagWrite:   time.Now().Add(time.Hour),
		lastHeartbeatTagWrite: time.Now().Add(time.Hour),
	}
	if got := a.CostLimitSpend(); got < 10.9 || got > 11.1 {
		t.Fatalf("CostLimitSpend=%.4f, want ~11 (compute $1 + EBS $10)", got)
	}

	a.checkAndAct(context.Background())

	if !sp.terminated {
		t.Error("a cost limit crossed by compute + EBS must Terminate (#616)")
	}
	if sp.stopped || sp.hibernated {
		t.Errorf("the cost limit must terminate, not stop/hibernate: stopped=%v hibernated=%v", sp.stopped, sp.hibernated)
	}
}

// TestCostLimitSpendIsTheEnforcedFigure: the exported accessor `spored status`
// reads must be the enforcement arithmetic itself, not a parallel copy (#959).
func TestCostLimitSpendIsTheEnforcedFigure(t *testing.T) {
	a := &Agent{config: &provider.Config{PricePerHour: 2.0, EBSHourlyCost: 0.5, LaunchTime: time.Now().Add(-4 * time.Hour)}}
	a.computeSecondsBase = int64((1 * time.Hour).Seconds())
	a.startTime = time.Now()
	if got, want := a.CostLimitSpend(), a.accumulatedComputeCost(); got-want > 0.001 || want-got > 0.001 {
		t.Errorf("CostLimitSpend=%.4f, accumulatedComputeCost=%.4f — status and enforcement disagree", got, want)
	}
}
