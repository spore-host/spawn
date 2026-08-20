package cost

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

const epsilon = 1e-6

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < epsilon
}

// fakePricer is a deterministic, offline stand-in for *truffleaws.Client's
// OnDemandPrice, matching the fake used in pkg/aws/pricing_test.go for #533.
type fakePricer struct {
	price float64
	err   error
	calls int
}

func (f *fakePricer) OnDemandPrice(_ context.Context, _, _ string) (float64, error) {
	f.calls++
	return f.price, f.err
}

func TestCostBreakdownCalculations(t *testing.T) {
	tests := []struct {
		name               string
		budget             float64
		totalCost          float64
		wantBudgetExceeded bool
		wantRemaining      float64
	}{
		{
			name:               "within budget",
			budget:             100.0,
			totalCost:          75.50,
			wantBudgetExceeded: false,
			wantRemaining:      24.50,
		},
		{
			name:               "exceeded budget",
			budget:             50.0,
			totalCost:          75.50,
			wantBudgetExceeded: true,
			wantRemaining:      -25.50,
		},
		{
			name:               "exactly at budget",
			budget:             100.0,
			totalCost:          100.0,
			wantBudgetExceeded: false,
			wantRemaining:      0.0,
		},
		{
			name:               "no budget set",
			budget:             0.0,
			totalCost:          100.0,
			wantBudgetExceeded: false,
			wantRemaining:      0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			breakdown := &CostBreakdown{
				TotalCost: tt.totalCost,
				Budget:    tt.budget,
			}

			// Calculate budget status manually (simulating what GetCostBreakdown does)
			if breakdown.Budget > 0 {
				breakdown.BudgetRemaining = breakdown.Budget - breakdown.TotalCost
				breakdown.BudgetExceeded = breakdown.TotalCost > breakdown.Budget
			}

			if breakdown.BudgetExceeded != tt.wantBudgetExceeded {
				t.Errorf("BudgetExceeded = %v, want %v", breakdown.BudgetExceeded, tt.wantBudgetExceeded)
			}

			if tt.budget > 0 {
				// Only check remaining if budget is set
				if breakdown.BudgetRemaining != tt.wantRemaining {
					t.Errorf("BudgetRemaining = %.2f, want %.2f", breakdown.BudgetRemaining, tt.wantRemaining)
				}
			}
		})
	}
}

func TestRegionalCostAggregation(t *testing.T) {
	// Create test instances
	instances := []SweepInstance{
		{
			Region:        "us-east-1",
			ActualType:    "t3.micro",
			InstanceHours: 2.5,
			EstimatedCost: 0.0125,
		},
		{
			Region:        "us-east-1",
			ActualType:    "t3.micro",
			InstanceHours: 3.0,
			EstimatedCost: 0.015,
		},
		{
			Region:        "us-west-2",
			ActualType:    "t3.small",
			InstanceHours: 1.5,
			EstimatedCost: 0.0105,
		},
	}

	// Aggregate by region
	regionMap := make(map[string]*RegionalCost)
	for _, inst := range instances {
		if regionMap[inst.Region] == nil {
			regionMap[inst.Region] = &RegionalCost{
				Region: inst.Region,
			}
		}
		regionMap[inst.Region].InstanceHours += inst.InstanceHours
		regionMap[inst.Region].EstimatedCost += inst.EstimatedCost
		regionMap[inst.Region].InstanceCount++
	}

	// Verify us-east-1
	if usEast1, ok := regionMap["us-east-1"]; ok {
		if usEast1.InstanceCount != 2 {
			t.Errorf("us-east-1 InstanceCount = %d, want 2", usEast1.InstanceCount)
		}
		if usEast1.InstanceHours != 5.5 {
			t.Errorf("us-east-1 InstanceHours = %.1f, want 5.5", usEast1.InstanceHours)
		}
		expectedCost := 0.0125 + 0.015
		if usEast1.EstimatedCost != expectedCost {
			t.Errorf("us-east-1 EstimatedCost = %.4f, want %.4f", usEast1.EstimatedCost, expectedCost)
		}
	} else {
		t.Error("us-east-1 region not found in aggregation")
	}

	// Verify us-west-2
	if usWest2, ok := regionMap["us-west-2"]; ok {
		if usWest2.InstanceCount != 1 {
			t.Errorf("us-west-2 InstanceCount = %d, want 1", usWest2.InstanceCount)
		}
		if usWest2.InstanceHours != 1.5 {
			t.Errorf("us-west-2 InstanceHours = %.1f, want 1.5", usWest2.InstanceHours)
		}
	} else {
		t.Error("us-west-2 region not found in aggregation")
	}
}

func TestInstanceTypeCostAggregation(t *testing.T) {
	instances := []SweepInstance{
		{
			ActualType:    "t3.micro",
			InstanceHours: 2.5,
			EstimatedCost: 0.0125,
		},
		{
			ActualType:    "t3.micro",
			InstanceHours: 3.0,
			EstimatedCost: 0.015,
		},
		{
			ActualType:    "t3.small",
			InstanceHours: 1.5,
			EstimatedCost: 0.0105,
		},
	}

	// Aggregate by instance type
	typeMap := make(map[string]*InstanceTypeCost)
	for _, inst := range instances {
		if typeMap[inst.ActualType] == nil {
			typeMap[inst.ActualType] = &InstanceTypeCost{
				InstanceType: inst.ActualType,
			}
		}
		typeMap[inst.ActualType].InstanceHours += inst.InstanceHours
		typeMap[inst.ActualType].EstimatedCost += inst.EstimatedCost
		typeMap[inst.ActualType].InstanceCount++
	}

	// Verify t3.micro
	if t3Micro, ok := typeMap["t3.micro"]; ok {
		if t3Micro.InstanceCount != 2 {
			t.Errorf("t3.micro InstanceCount = %d, want 2", t3Micro.InstanceCount)
		}
		if t3Micro.InstanceHours != 5.5 {
			t.Errorf("t3.micro InstanceHours = %.1f, want 5.5", t3Micro.InstanceHours)
		}
	} else {
		t.Error("t3.micro instance type not found in aggregation")
	}

	// Verify t3.small
	if t3Small, ok := typeMap["t3.small"]; ok {
		if t3Small.InstanceCount != 1 {
			t.Errorf("t3.small InstanceCount = %d, want 1", t3Small.InstanceCount)
		}
	} else {
		t.Error("t3.small instance type not found in aggregation")
	}
}

// --- pure function tests ---

func TestTotalHours(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		launched  string
		end       time.Time
		wantHours float64
	}{
		{"2 hours", "2025-01-01T10:00:00Z", base, 2.0},
		{"30 minutes", "2025-01-01T11:30:00Z", base, 0.5},
		{"invalid timestamp", "not-a-time", base, 0},
		{"end before launch", "2025-01-01T13:00:00Z", base, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := totalHours(tt.launched, tt.end)
			if !approxEqual(got, tt.wantHours) {
				t.Errorf("totalHours(%q) = %f, want %f", tt.launched, got, tt.wantHours)
			}
		})
	}
}

func TestCalculateRunningHours_NoHistory(t *testing.T) {
	end := time.Date(2025, 1, 1, 14, 0, 0, 0, time.UTC)
	got := calculateRunningHours(nil, "2025-01-01T12:00:00Z", end)
	if !approxEqual(got, 2.0) {
		t.Errorf("calculateRunningHours(nil) = %f, want 2.0", got)
	}
}

func TestCalculateRunningHours_WithHistory(t *testing.T) {
	end := time.Date(2025, 1, 1, 16, 0, 0, 0, time.UTC)
	history := []StateTransition{
		{State: "pending", Timestamp: "2025-01-01T12:00:00Z"},
		{State: "running", Timestamp: "2025-01-01T12:05:00Z"},
		{State: "stopped", Timestamp: "2025-01-01T14:05:00Z"},
		{State: "running", Timestamp: "2025-01-01T14:30:00Z"},
	}
	// running: 12:05→14:05 = 2h, 14:30→16:00 = 1.5h → total 3.5h
	got := calculateRunningHours(history, "2025-01-01T12:00:00Z", end)
	if !approxEqual(got, 3.5) {
		t.Errorf("calculateRunningHours = %f, want 3.5", got)
	}
}

func TestCalculateRunningHours_InvalidTimestamp(t *testing.T) {
	end := time.Date(2025, 1, 1, 14, 0, 0, 0, time.UTC)
	history := []StateTransition{{State: "running", Timestamp: "bad-ts"}}
	got := calculateRunningHours(history, "2025-01-01T12:00:00Z", end)
	if got != 0 {
		t.Errorf("expected 0 for invalid timestamp, got %f", got)
	}
}

func TestCalculateComputeCost(t *testing.T) {
	t.Run("uses actual type over requested", func(t *testing.T) {
		inst := SweepInstance{Region: "us-east-1", ActualType: "t3.micro", RequestedType: "m5.large"}
		p := &fakePricer{price: 0.0104}
		cache := map[string]float64{}
		cost, priced := calculateComputeCost(context.Background(), p, cache, inst, 1.0)
		if !priced || cost <= 0 {
			t.Errorf("expected positive priced cost, got cost=%f priced=%v", cost, priced)
		}
	})
	t.Run("falls back to requested type", func(t *testing.T) {
		inst := SweepInstance{Region: "us-east-1", RequestedType: "t3.micro"}
		p := &fakePricer{price: 0.0104}
		cache := map[string]float64{}
		single, _ := calculateComputeCost(context.Background(), p, cache, inst, 1.0)
		double, _ := calculateComputeCost(context.Background(), p, cache, inst, 2.0)
		if !approxEqual(double, 2*single) {
			t.Errorf("2h cost %f ≠ 2× 1h cost %f", double, single)
		}
	})
	t.Run("zero hours returns zero without calling the pricer", func(t *testing.T) {
		inst := SweepInstance{Region: "us-east-1", ActualType: "t3.micro"}
		p := &fakePricer{price: 0.0104}
		cache := map[string]float64{}
		cost, priced := calculateComputeCost(context.Background(), p, cache, inst, 0)
		if cost != 0 || !priced {
			t.Errorf("expected (0, true) for 0 hours, got (%f, %v)", cost, priced)
		}
		if p.calls != 0 {
			t.Errorf("pricer was called %d times for a 0-hour instance, want 0", p.calls)
		}
	})
	// #543 regression: the real truffle rate is used, not libs/pricing's static
	// per-family-size guess table, which under-quoted c8g.metal-48xl in
	// us-east-1 by 38x ($0.20/hr against a real $7.657/hr).
	t.Run("uses the real truffle rate, not a static guess", func(t *testing.T) {
		inst := SweepInstance{Region: "us-east-1", ActualType: "c8g.metal-48xl"}
		p := &fakePricer{price: 7.6570}
		cache := map[string]float64{}
		cost, priced := calculateComputeCost(context.Background(), p, cache, inst, 1.0)
		if !priced {
			t.Fatal("expected the instance to be priced")
		}
		if !approxEqual(cost, 7.6570) {
			t.Errorf("cost = %f, want 7.6570 (the real rate)", cost)
		}
		if approxEqual(cost, 0.20) {
			t.Error("cost matches the old fabricated static-guess rate ($0.20/hr)")
		}
	})
	t.Run("a pricer failure is unpriced, not a fabricated zero-rate success", func(t *testing.T) {
		inst := SweepInstance{Region: "us-east-1", ActualType: "brand-new.type"}
		p := &fakePricer{err: errors.New("no on-demand price found")}
		cache := map[string]float64{}
		cost, priced := calculateComputeCost(context.Background(), p, cache, inst, 1.0)
		if priced {
			t.Error("expected priced=false when the pricer fails")
		}
		if cost != 0 {
			t.Errorf("cost = %f, want 0 when unpriced", cost)
		}
	})
	t.Run("a nil pricer (no AWS config) is unpriced, never a guess", func(t *testing.T) {
		inst := SweepInstance{Region: "us-east-1", ActualType: "t3.micro"}
		cache := map[string]float64{}
		cost, priced := calculateComputeCost(context.Background(), nil, cache, inst, 1.0)
		if priced || cost != 0 {
			t.Errorf("cost=%f priced=%v, want (0, false) with no pricer available", cost, priced)
		}
	})
	t.Run("caches the rate across calls for the same type and region", func(t *testing.T) {
		inst := SweepInstance{Region: "us-east-1", ActualType: "m5.large"}
		p := &fakePricer{price: 0.096}
		cache := map[string]float64{}
		if _, priced := calculateComputeCost(context.Background(), p, cache, inst, 1.0); !priced {
			t.Fatal("expected the first call to price successfully")
		}
		if _, priced := calculateComputeCost(context.Background(), p, cache, inst, 3.0); !priced {
			t.Fatal("expected the second call to price successfully")
		}
		if p.calls != 1 {
			t.Errorf("pricer was called %d times for the same (type, region), want 1 (cached)", p.calls)
		}
	})
}

func TestCalculateStorageCost(t *testing.T) {
	t.Run("no resources returns 0", func(t *testing.T) {
		inst := SweepInstance{Region: "us-east-1"}
		if cost := calculateStorageCost(inst, 24); cost != 0 {
			t.Errorf("expected 0, got %f", cost)
		}
	})
	t.Run("gp3 volume", func(t *testing.T) {
		inst := SweepInstance{
			Region:    "us-east-1",
			Resources: &InstanceResources{EBSVolumes: []EBSVolume{{SizeGB: 100, VolumeType: "gp3"}}},
		}
		if cost := calculateStorageCost(inst, 24); cost <= 0 {
			t.Errorf("expected positive storage cost, got %f", cost)
		}
	})
	t.Run("zero-size volume skipped", func(t *testing.T) {
		inst := SweepInstance{
			Region:    "us-east-1",
			Resources: &InstanceResources{EBSVolumes: []EBSVolume{{SizeGB: 0, VolumeType: "gp3"}}},
		}
		if cost := calculateStorageCost(inst, 24); cost != 0 {
			t.Errorf("expected 0 for zero-size volume, got %f", cost)
		}
	})
}

func TestCalculateNetworkCost(t *testing.T) {
	t.Run("no resources returns 0", func(t *testing.T) {
		inst := SweepInstance{Region: "us-east-1"}
		if cost := calculateNetworkCost(inst, 10); cost != 0 {
			t.Errorf("expected 0, got %f", cost)
		}
	})
	t.Run("scales linearly with hours", func(t *testing.T) {
		inst := SweepInstance{
			Region:    "us-east-1",
			Resources: &InstanceResources{IPv4Count: 1},
		}
		one := calculateNetworkCost(inst, 1.0)
		two := calculateNetworkCost(inst, 2.0)
		if one <= 0 {
			t.Errorf("expected positive cost, got %f", one)
		}
		if !approxEqual(two, 2*one) {
			t.Errorf("2h cost %f ≠ 2× 1h cost %f", two, one)
		}
	})
}
