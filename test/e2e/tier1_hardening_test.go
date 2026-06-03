//go:build e2e_tier1

package e2e

// Tier 1 hardening (gap pass): assert on real AWS *values*, not just non-empty
// output, and cover the truffle/spawn commands whose whole purpose is real-API
// data that Substrate can't fake (capacity reservations, AZ offerings, live
// Spot pricing, availability stats). All API-only — launches nothing, ~$0.

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// spotPriceResult mirrors truffle's `spot --output json` element.
type spotPriceResult struct {
	InstanceType     string  `json:"instance_type"`
	Region           string  `json:"region"`
	AvailabilityZone string  `json:"availability_zone"`
	SpotPrice        float64 `json:"spot_price"`
	OnDemandPrice    float64 `json:"on_demand_price"`
}

// TestTier1_SpotPricingValues asserts the *values* from live DescribeSpotPrice
// History are sane — not merely that output is non-empty. This is the real
// reason Tier 1 hits AWS instead of Substrate (the plan's named follow-on).
func TestTier1_SpotPricingValues(t *testing.T) {
	t.Parallel()
	bin := truffleBin(t)
	out, err := exec.Command(bin, "spot", "t3.small", //nolint:gosec // nosemgrep
		"--regions", testRegion, "--show-savings", "--output", "json").CombinedOutput()
	if err != nil {
		t.Fatalf("truffle spot --output json: %v\n%s", err, out)
	}

	var results []spotPriceResult
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("parse spot json: %v\n%s", err, out)
	}
	if len(results) == 0 {
		t.Skip("no spot prices for t3.small in region (capacity/availability) — nothing to assert")
	}

	for _, r := range results {
		if r.SpotPrice <= 0 {
			t.Errorf("%s/%s: spot_price = %v, want > 0 (real price)", r.Region, r.AvailabilityZone, r.SpotPrice)
		}
		// Spot is essentially always cheaper than on-demand; if on-demand was
		// resolved (--show-savings), spot must be below it. Allow a tiny margin.
		if r.OnDemandPrice > 0 && r.SpotPrice >= r.OnDemandPrice {
			t.Errorf("%s/%s: spot_price %v >= on_demand_price %v — savings model wrong",
				r.Region, r.AvailabilityZone, r.SpotPrice, r.OnDemandPrice)
		}
		// Sanity ceiling: a t3.small spot price over $1/hr would be absurd.
		if r.SpotPrice > 1.0 {
			t.Errorf("%s/%s: t3.small spot_price = %v/hr is implausibly high", r.Region, r.AvailabilityZone, r.SpotPrice)
		}
	}
	t.Logf("validated %d real spot prices", len(results))
}

// TestTier1_TruffleAZ exercises `truffle az` — real DescribeInstanceTypeOfferings
// per availability zone. Substrate can't fake which AZs actually offer a type.
func TestTier1_TruffleAZ(t *testing.T) {
	t.Parallel()
	bin := truffleBin(t)
	out, err := exec.Command(bin, "az", "t3.small", //nolint:gosec // nosemgrep
		"--regions", testRegion, "--output", "json").CombinedOutput()
	if err != nil {
		t.Fatalf("truffle az: %v\n%s", err, out)
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		t.Fatal("truffle az produced no output")
	}
	// JSON mode emits an array; if t3.small is offered, at least one AZ entry
	// with the region should appear. Empty array is acceptable (skip).
	var results []map[string]any
	if err := json.Unmarshal([]byte(body), &results); err != nil {
		// az may print a non-JSON summary to stderr even in json mode; only fail
		// if it's neither valid JSON nor mentions the region.
		if !strings.Contains(body, testRegion) {
			t.Fatalf("truffle az: unparseable and no region reference:\n%s", body)
		}
		return
	}
	if len(results) == 0 {
		t.Skip("t3.small not offered in any queried AZ (acceptable)")
	}
	t.Logf("truffle az: %d AZ-availability entries", len(results))
}

// TestTier1_TruffleCapacity exercises `truffle capacity` — real
// DescribeCapacityReservations. A dev account usually has none; the command
// must succeed and report that cleanly (not error).
func TestTier1_TruffleCapacity(t *testing.T) {
	t.Parallel()
	bin := truffleBin(t)
	out, err := exec.Command(bin, "capacity", "--regions", testRegion).CombinedOutput() //nolint:gosec // nosemgrep
	if err != nil {
		// AccessDenied (no ec2:DescribeCapacityReservations) is an environment
		// limitation, not a product bug — skip rather than fail.
		if strings.Contains(string(out), "AccessDenied") || strings.Contains(string(out), "UnauthorizedOperation") {
			t.Skipf("capacity reservations not permitted in this role: %s", out)
		}
		t.Fatalf("truffle capacity: %v\n%s", err, out)
	}
	t.Logf("truffle capacity OK: %d bytes", len(out))
}

// TestTier1_SpawnAvailability exercises `spawn availability` — real DynamoDB
// historical stats in the infra account. Tolerates "No data" (empty table is a
// valid state); only fails on an error.
func TestTier1_SpawnAvailability(t *testing.T) {
	out, err := spawnMayFail(t, "availability", "--instance-type", testInstanceType, "--regions", testRegion)
	if err != nil {
		if strings.Contains(out, "AccessDenied") || strings.Contains(out, "ResourceNotFoundException") {
			t.Skipf("availability stats table not accessible in this account: %s", out)
		}
		t.Fatalf("spawn availability: %v\n%s", err, out)
	}
	if !strings.Contains(out, testInstanceType) && !strings.Contains(strings.ToLower(out), "availability") {
		t.Errorf("expected availability output for %s, got:\n%s", testInstanceType, out)
	}
	t.Logf("spawn availability OK")
}
