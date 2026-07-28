package slurm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Tests for the live-priced cost estimate (#447).
//
// The whole point of the Pricer seam is that no rate lives in this repo, so these
// tests supply rates through a fake and assert on how the estimate uses them —
// never on what a real rate is. A test that pinned "$55.04" here would be the
// same stale-table failure wearing a different hat.

// fakePricer returns fixed rates and records what it was asked for, so a test can
// verify the region and type actually reached the pricer.
type fakePricer struct {
	onDemand    float64
	spot        float64
	spotErr     error
	onDemandErr error

	calls []string // "instanceType/region/model"
}

func (f *fakePricer) HourlyRate(_ context.Context, instanceType, region, model string) (float64, error) {
	f.calls = append(f.calls, instanceType+"/"+region+"/"+model)
	if model == "spot" {
		return f.spot, f.spotErr
	}
	return f.onDemand, f.onDemandErr
}

// TestEstimateCostUsesLiveRates verifies both totals are rate × hours from the
// pricer, with no residual table price or fixed spot ratio anywhere in the math.
func TestEstimateCostUsesLiveRates(t *testing.T) {
	job := &SlurmJob{
		SpawnInstanceType: "p5.48xlarge",
		TimeLimit:         3 * time.Hour,
	}
	p := &fakePricer{onDemand: 10, spot: 4}

	est, err := EstimateCostWithPricer(context.Background(), job, p, "us-west-2")
	if err != nil {
		t.Fatalf("EstimateCostWithPricer: %v", err)
	}
	if est.InstanceHours != 3 {
		t.Errorf("InstanceHours = %v, want 3", est.InstanceHours)
	}
	if est.OnDemandCost != 30 {
		t.Errorf("OnDemandCost = %v, want 30 (3h × $10)", est.OnDemandCost)
	}
	if est.SpotCost != 12 {
		t.Errorf("SpotCost = %v, want 12 (3h × $4 — the live spot rate, not a fraction of on-demand)", est.SpotCost)
	}
	// The old code derived spot as on-demand × 0.3. With these rates that would be
	// 9, so this pins the actual defect #447 flagged in the cost path.
	if est.SpotCost == est.OnDemandCost*0.3 {
		t.Error("SpotCost still looks like a fixed 70% discount off on-demand")
	}
	if est.OnDemandRate != 10 || est.SpotRate != 4 {
		t.Errorf("rates not reported: on-demand %v, spot %v", est.OnDemandRate, est.SpotRate)
	}
}

// TestEstimateCostPricesTheSelectedTypeInTheAskedRegion verifies the type and
// region reach the pricer. Region matters materially: GPU On-Demand rates differ
// by up to 70% across regions, so pricing us-east-1 for a job that will run in
// eu-central-1 is simply a wrong number.
func TestEstimateCostPricesTheSelectedTypeInTheAskedRegion(t *testing.T) {
	job := &SlurmJob{SpawnInstanceType: "g6e.4xlarge", TimeLimit: time.Hour}
	p := &fakePricer{onDemand: 3, spot: 1}

	est, err := EstimateCostWithPricer(context.Background(), job, p, "eu-central-1")
	if err != nil {
		t.Fatalf("EstimateCostWithPricer: %v", err)
	}
	if est.Region != "eu-central-1" {
		t.Errorf("Region = %q, want eu-central-1", est.Region)
	}
	for _, want := range []string{"g6e.4xlarge/eu-central-1/on-demand", "g6e.4xlarge/eu-central-1/spot"} {
		found := false
		for _, got := range p.calls {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("pricer was never asked for %q; calls = %v", want, p.calls)
		}
	}
}

// TestEstimateCostSurvivesMissingSpotPrice verifies a type with no published spot
// price still produces an on-demand answer. Newly-launched accelerator types
// routinely have no spot market, and "what would this cost" is still answerable —
// but the gap must be visible, not rendered as $0.00.
func TestEstimateCostSurvivesMissingSpotPrice(t *testing.T) {
	job := &SlurmJob{SpawnInstanceType: "p6-b300.48xlarge", TimeLimit: 2 * time.Hour}

	for _, tc := range []struct {
		name string
		p    *fakePricer
	}{
		{"spot query errors", &fakePricer{onDemand: 100, spotErr: errors.New("no spot price available")}},
		{"spot rate is zero", &fakePricer{onDemand: 100, spot: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			est, err := EstimateCostWithPricer(context.Background(), job, tc.p, "us-east-1")
			if err != nil {
				t.Fatalf("a missing spot price must not fail the estimate: %v", err)
			}
			if !est.SpotUnavailable {
				t.Error("SpotUnavailable not set — an absent spot price would render as $0.00")
			}
			if est.SpotCost != 0 {
				t.Errorf("SpotCost = %v, want 0 when no spot rate is known", est.SpotCost)
			}
			if est.OnDemandCost != 200 {
				t.Errorf("OnDemandCost = %v, want 200 — the on-demand answer must survive", est.OnDemandCost)
			}
		})
	}
}

// TestEstimateCostFailsWithoutAnOnDemandRate verifies a missing on-demand rate is
// an error rather than a $0.00 estimate. This figure gates `slurm submit`, which
// launches billable instances — quoting zero because pricing was unreachable is
// the worst available outcome.
func TestEstimateCostFailsWithoutAnOnDemandRate(t *testing.T) {
	job := &SlurmJob{SpawnInstanceType: "p5.48xlarge", TimeLimit: time.Hour}
	p := &fakePricer{onDemandErr: errors.New("AccessDenied")}

	if _, err := EstimateCostWithPricer(context.Background(), job, p, "us-east-1"); err == nil {
		t.Fatal("an unavailable on-demand rate returned no error — the estimate would read $0.00")
	}
}

// TestTotalInstanceHours verifies the hour arithmetic for each job shape, which is
// the half of the estimate that needs no credentials.
func TestTotalInstanceHours(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  *SlurmJob
		want float64
	}{
		{
			"single task",
			&SlurmJob{TimeLimit: 4 * time.Hour},
			4,
		},
		{
			"array job, unlimited concurrency",
			&SlurmJob{Array: &ArraySpec{Start: 1, End: 10, Step: 1}, TimeLimit: 2 * time.Hour},
			20, // 10 tasks × 2h
		},
		{
			"array job, capped concurrency bills the same total",
			&SlurmJob{Array: &ArraySpec{Start: 1, End: 10, Step: 1, MaxRunning: 5}, TimeLimit: 2 * time.Hour},
			20, // 10 tasks × 2h, just spread over more wall time
		},
		{
			"MPI job",
			&SlurmJob{Nodes: 8, TasksPerNode: 4, TimeLimit: 90 * time.Minute},
			12, // 8 nodes × 1.5h
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TotalInstanceHours(tc.job); got != tc.want {
				t.Errorf("TotalInstanceHours = %v, want %v", got, tc.want)
			}
		})
	}
}
