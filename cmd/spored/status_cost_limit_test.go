package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/agent"
	"github.com/spore-host/spawn/pkg/provider"
)

// TestStatusCostLimitUsedIsTheEnforcedSpend is #959: `spored status` must report
// cost-limit used/remaining from the same function the enforcement loop terminates
// on (agent.CostLimitSpend → accumulatedComputeCost: compute + EBS, #616), not from
// a compute-only copy. The case that matters is an instance that has been stopped:
// its EBS kept billing while compute did not move, so a compute-only "remaining"
// overstates the headroom and the cap fires before the display reaches zero.
func TestStatusCostLimitUsedIsTheEnforcedSpend(t *testing.T) {
	identity := &provider.Identity{InstanceID: "i-0123456789abcdef0", Region: "us-west-2", Provider: "stub"}
	cfg := &provider.Config{
		LaunchTime:     time.Now().Add(-10 * time.Hour),  // launched 10h ago
		ComputeSeconds: int64((2 * time.Hour).Seconds()), // ran 2h, then sat stopped
		PricePerHour:   1.00,
		EBSHourlyCost:  0.50, // 10h wall clock × $0.50 = $5 of EBS the cap counts
		CostLimit:      20.0,
	}
	ag, err := agent.NewAgent(context.Background(), &stubStatusProvider{identity: identity, config: cfg})
	if err != nil {
		t.Fatalf("agent.NewAgent: %v", err)
	}
	report := buildStatusReport(ag, cfg, identity, false)
	if report.Cost == nil {
		t.Fatal("no cost section in the report")
	}
	c := report.Cost

	enforced := math.Round(ag.CostLimitSpend()*100) / 100
	if math.Abs(c.CostLimitUsed-enforced) > 0.011 {
		t.Errorf("cost_limit_used=$%.2f, but the enforcement function says $%.2f — status and the cap disagree",
			c.CostLimitUsed, enforced)
	}
	// From the intent, not the code: 2h × $1 compute + 10h × $0.50 EBS ≈ $7.
	if c.CostLimitUsed < 6.9 || c.CostLimitUsed > 7.1 {
		t.Errorf("cost_limit_used=$%.2f, want ~$7.00 (compute $2 + EBS $5)", c.CostLimitUsed)
	}
	if math.Abs(c.CostLimitRemaining-(cfg.CostLimit-c.CostLimitUsed)) > 0.011 {
		t.Errorf("cost_limit_remaining=$%.2f does not follow from used=$%.2f and limit=$%.2f",
			c.CostLimitRemaining, c.CostLimitUsed, cfg.CostLimit)
	}

	out := captureStdout(t, func() { _ = renderStatusTable(report) })
	if strings.Contains(out, "compute-only") {
		t.Errorf("the cost-limit line still calls the cap compute-only:\n%s", out)
	}
	if !strings.Contains(out, "compute + EBS") {
		t.Errorf("the cost-limit line should say what it counts (compute + EBS):\n%s", out)
	}
}
