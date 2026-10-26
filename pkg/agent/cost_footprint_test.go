package agent

import (
	"context"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/costmodel"
	"github.com/spore-host/spawn/pkg/provider"
)

// TestAccumulatedCost_IsCostmodelAccrue: what --cost-limit terminates on is
// pkg/costmodel.Accrue over the agent's footprint — the function the CLI's
// displays and `spawn extend` use (#51) — and it counts public IPv4: the
// auto-assigned address over compute time, the Elastic IP over the wall clock.
func TestAccumulatedCost_IsCostmodelAccrue(t *testing.T) {
	launch := time.Now().Add(-10 * time.Hour)
	cfg := &provider.Config{PricePerHour: 1.0, EBSHourlyCost: 0.1, PublicIPv4Count: 1, ElasticIPv4Count: 1, LaunchTime: launch}
	a := &Agent{config: cfg, startTime: time.Now()}
	a.computeSecondsBase = int64((2 * time.Hour).Seconds())

	got := a.accumulatedComputeCost()
	want := costmodel.Accrue(costmodel.Footprint{ComputePerHour: 1.0, EBSPerHour: 0.1, PublicIPv4: 1, ElasticIPv4: 1},
		2*time.Hour, 10*time.Hour).Total
	if math.Abs(got-want) > 0.001 {
		t.Errorf("accumulated = %.4f, costmodel.Accrue = %.4f — enforcement and the shared model disagree", got, want)
	}

	noIPv4 := &Agent{config: &provider.Config{PricePerHour: 1.0, EBSHourlyCost: 0.1, LaunchTime: launch}, startTime: time.Now()}
	noIPv4.computeSecondsBase = a.computeSecondsBase
	// 1 auto-assigned x 2h + 1 Elastic x 10h, at $0.005/hr.
	if d := got - noIPv4.accumulatedComputeCost(); math.Abs(d-0.005*12) > 0.001 {
		t.Errorf("public IPv4 adds $%.4f, want $%.4f (auto over compute, Elastic over wall clock)", d, 0.005*12)
	}
}

// TestAccumulatedCost_NoLaunchTagFallsBackToPendingTime: without
// spawn:launch-time EBS used to drop out of the cap entirely.
func TestAccumulatedCost_NoLaunchTagFallsBackToPendingTime(t *testing.T) {
	a := &Agent{
		config:    &provider.Config{PricePerHour: 1.0, EBSHourlyCost: 1.0},
		identity:  &provider.Identity{PendingTime: time.Now().Add(-5 * time.Hour)},
		startTime: time.Now(),
	}
	if got := a.accumulatedComputeCost(); got < 4.9 {
		t.Errorf("accumulated = %.4f, want ~5 (EBS over the 5h since IMDS pendingTime)", got)
	}
}

// measuringProvider counts MeasureCostFootprint calls and can hold one open.
type measuringProvider struct {
	*stubProvider
	calls   atomic.Int32
	release chan struct{}
	entered chan struct{}
}

func (p *measuringProvider) MeasureCostFootprint(context.Context) (provider.CostFootprint, bool) {
	p.calls.Add(1)
	if p.entered != nil {
		p.entered <- struct{}{}
	}
	if p.release != nil {
		<-p.release
	}
	return provider.CostFootprint{}, false
}

// TestCheckAndAct_RemeasuresCostFootprint: the footprint is re-measured from
// the monitor loop, not once for the instance's life (#51) — a volume attached
// or a launch Elastic IP associated after spored's first look is otherwise
// never counted.
func TestCheckAndAct_RemeasuresCostFootprint(t *testing.T) {
	identity := &provider.Identity{InstanceID: "i-fp", Region: "us-east-1", Provider: "ec2"}
	cfg := &provider.Config{}
	sp := &measuringProvider{stubProvider: &stubProvider{identity: identity, config: cfg}, release: make(chan struct{})}
	a := &Agent{
		provider:              sp,
		identity:              identity,
		config:                cfg,
		startTime:             time.Now(),
		lastActivityTime:      time.Now(),
		lastSessionTagWrite:   time.Now().Add(time.Hour),
		lastComputeTagWrite:   time.Now().Add(time.Hour),
		lastHeartbeatTagWrite: time.Now().Add(time.Hour),
	}
	a.configRefreshTick = footprintMeasureTicks - 1
	a.checkAndAct(context.Background())
	// Set synchronously before the measuring goroutine starts, and held while
	// the provider call blocks on release: no timing involved.
	started := a.footprintMeasuring.Load()
	close(sp.release)
	if !started {
		t.Fatalf("tick %d did not start a cost-footprint measurement", footprintMeasureTicks)
	}
}

// TestMaybeMeasureCostFootprint_NeverStacks: a slow measurement never starts a
// second one, and never blocks the caller (#65).
func TestMaybeMeasureCostFootprint_NeverStacks(t *testing.T) {
	sp := &measuringProvider{stubProvider: &stubProvider{}, release: make(chan struct{}), entered: make(chan struct{}, 4)}
	a := &Agent{provider: sp}
	if !a.maybeMeasureCostFootprint(context.Background()) {
		t.Fatal("the first measurement did not start")
	}
	<-sp.entered // the first is now inside the provider call, held by release
	if a.maybeMeasureCostFootprint(context.Background()) {
		t.Error("a second measurement started while the first was still running")
	}
	close(sp.release)
}
