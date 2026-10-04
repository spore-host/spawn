package cmd

import (
	"strings"
	"testing"
)

// TestEstimateMultipliesByCount is spawn#662.
//
// --estimate-only printed the per-instance rate multiplied by the TTL and
// nothing else, so a 2-node cohort quoted $7.66/hr when the real ceiling was
// $15.31. The entire point of the flag is a bound on spend before committing to
// it, so under-reporting by a factor of --count defeats it — and it understates
// in the expensive direction, on the path a user takes precisely because they
// are being careful.
func TestEstimateMultipliesByCount(t *testing.T) {
	var b strings.Builder
	renderInstanceCostEstimate(&b, "c8g.48xlarge", "us-east-1", 7.656, "on-demand", "4h", 2)
	out := b.String()

	// 7.656 * 2 = 15.312/hr; over 4h = 61.248
	if !strings.Contains(out, "15.31") {
		t.Errorf("no total hourly rate for 2 instances (want ~15.31):\n%s", out)
	}
	if !strings.Contains(out, "61.2") {
		t.Errorf("no total TTL cost for 2 instances (want ~61.25):\n%s", out)
	}
	// The per-instance figure must still be visible — it is what a user compares
	// against a price list.
	if !strings.Contains(out, "7.656") && !strings.Contains(out, "7.66") {
		t.Errorf("per-instance rate must still be shown:\n%s", out)
	}
	// And the count must be stated, so the total is attributable.
	if !strings.Contains(out, "2") {
		t.Errorf("the instance count must appear:\n%s", out)
	}
}

// TestEstimateSingleInstanceUnchanged: --count defaults to 1, which is the
// overwhelmingly common case. It must not grow noisy "× 1 instances" phrasing or
// a redundant total.
func TestEstimateSingleInstanceUnchanged(t *testing.T) {
	var b strings.Builder
	renderInstanceCostEstimate(&b, "m7i.large", "us-east-1", 0.1008, "on-demand", "2h", 1)
	out := b.String()

	if !strings.Contains(out, "0.1008") {
		t.Errorf("per-instance rate missing:\n%s", out)
	}
	if !strings.Contains(out, "0.20") {
		t.Errorf("TTL cost for 2h missing:\n%s", out)
	}
	for _, unwanted := range []string{"each", "total", "instances"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("single-instance output should not mention %q:\n%s", unwanted, out)
		}
	}
}

// TestEstimateWithoutTTL: no TTL means no bounded cost to quote. The hourly rate
// must still appear, and the absence of a bound should not silently read as
// "cheap" — an instance with no TTL is the unbounded case.
func TestEstimateWithoutTTL(t *testing.T) {
	var b strings.Builder
	renderInstanceCostEstimate(&b, "m7i.large", "us-east-1", 0.1008, "on-demand", "", 3)
	out := b.String()

	if !strings.Contains(out, "0.3024") && !strings.Contains(out, "0.30") {
		t.Errorf("total hourly rate for 3 instances missing:\n%s", out)
	}
	if strings.Contains(out, "TTL cost") {
		t.Errorf("there is no TTL, so no TTL cost should be quoted:\n%s", out)
	}
}

// TestEstimateCountIsClamped: count is an int flag, so 0 or a negative value is
// reachable. Treating 0 as 0 cost would print "$0.00" for a launch that still
// costs money, which is the worst possible rounding of this bug.
func TestEstimateCountIsClamped(t *testing.T) {
	for _, n := range []int{0, -5} {
		var b strings.Builder
		renderInstanceCostEstimate(&b, "m7i.large", "us-east-1", 0.1008, "on-demand", "1h", n)
		out := b.String()
		if strings.Contains(out, "0.0000/hr") || strings.Contains(out, "$0.00 ") {
			t.Errorf("count=%d produced a zero estimate; a nonsensical count must not read "+
				"as a free launch:\n%s", n, out)
		}
	}
}
