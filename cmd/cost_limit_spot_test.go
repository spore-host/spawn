package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/costmodel"
)

// The CLI counts a Spot instance's cost limit exactly as spored enforces it
// (#1497): from spored's spawn:spot-accrual, not spawn:price-per-hour.
func TestCostLimitSpendUsesSporedsSpotAccrual(t *testing.T) {
	inst := &aws.InstanceInfo{SpotInstance: true, Tags: map[string]string{
		"spawn:price-per-hour":      "3.418",
		"spawn:spot-price-per-hour": "1.09",
	}}
	compute := 2*time.Hour + 3*time.Minute
	// A spored that predates #1497 records no accrual and enforces at on-demand.
	if got := costLimitSpend(inst, compute, compute).Compute; got < 7.0 || got > 7.02 {
		t.Errorf("no accrual: compute = $%.4f, want the on-demand $7.01 such a spored enforces", got)
	}
	if l := costLimitBasisLabel(inst); !strings.Contains(l, "on-demand") || !strings.Contains(l, "predates spawn#1497") {
		t.Errorf("label = %q", l)
	}

	inst.Tags["spawn:"+costmodel.TagComputeAccrual] = costmodel.EncodeComputeAccrual(costmodel.ComputeAccrual{
		PerHour: 1.09, Basis: costmodel.BasisSpot, SampledAt: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)})
	if got := costLimitSpend(inst, compute, compute).Compute; got < 2.22 || got > 2.25 {
		t.Errorf("with spored's accrual: compute = $%.4f, want the Spot $2.23", got)
	}
	if l := costLimitBasisLabel(inst); !strings.Contains(l, "Spot price") || !strings.Contains(l, "$1.0900/hr") ||
		!strings.Contains(l, "2026-11-08T10:00:00Z") {
		t.Errorf("label = %q", l)
	}
	// The display footprint (priced at a display rate) is not the accrual.
	if f := footprintAt(inst, 1.5); f.Accrual != nil || f.ComputePerHour != 1.5 {
		t.Errorf("footprintAt = %+v", f)
	}
}

// The on-demand fallback a Spot instance's cost limit uses when spored cannot
// read the Spot price is labelled a fallback; a static rate says static, with
// its collection date and reason unknown because the instance records neither
// (owner rule, truffle#184; spawn#1512).
func TestCostLimitBasisLabelSaysAStaticFallbackIsStatic(t *testing.T) {
	inst := &aws.InstanceInfo{SpotInstance: true, Tags: map[string]string{
		"spawn:price-per-hour": "3.418",
		aws.PriceSourceTag:     "static",
		"spawn:" + costmodel.TagComputeAccrual: costmodel.EncodeComputeAccrual(costmodel.ComputeAccrual{
			PerHour: 3.418, Basis: costmodel.BasisOnDemandFallback, SampledAt: time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC)}),
	}}
	l := costLimitBasisLabel(inst)
	for _, want := range []string{"FALLBACK", "could not read the Spot price", "since 2026-11-08T11:00:00Z", "$3.4180/hr",
		"static", "collection date unknown", "reason not recorded"} {
		if !strings.Contains(l, want) {
			t.Errorf("label missing %q: %s", want, l)
		}
	}
	inst.Tags[aws.PriceSourceTag] = "live"
	if l := costLimitBasisLabel(inst); strings.Contains(l, "static") || !strings.Contains(l, "live") {
		t.Errorf("a live fallback rate: %s", l)
	}
	if l := costLimitBasisLabel(&aws.InstanceInfo{Tags: map[string]string{"spawn:price-per-hour": "1"}}); l != "compute at the on-demand rate" {
		t.Errorf("on-demand instance: %q", l)
	}
}

// `spawn cost` on a Spot instance shows spored's accrual as its compute, so the
// line and the total agree with what the cap counts.
func TestSingleInstanceCostReportShowsTheSpotAccrual(t *testing.T) {
	inst := &aws.InstanceInfo{
		Name: "spot", InstanceID: "i-0spot", InstanceType: "c9gd.16xlarge", State: "running", SpotInstance: true,
		LaunchTime: costTestNow.Add(-3 * time.Hour),
		Tags: map[string]string{
			"spawn:price-per-hour":  "3.418",
			"spawn:compute-seconds": "7380",
			"spawn:" + costmodel.TagComputeAccrual: costmodel.EncodeComputeAccrual(costmodel.ComputeAccrual{
				PerHour: 1.09, Basis: costmodel.BasisSpot}),
		},
	}
	out := singleInstanceCostReport(inst, costTestNow)
	for _, want := range []string{"Est. compute:    ~$2.23", "Spot price", "--cost-limit counts"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}
