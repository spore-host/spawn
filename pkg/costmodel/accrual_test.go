package costmodel

import (
	"math"
	"testing"
	"time"
)

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// The issue's numbers (#1497): c9gd.16xlarge Spot at $1.09/hr, on-demand
// $3.418/hr, --cost-limit 7. After 2.05 h of compute the on-demand measure was
// $7.01 and the instance was terminated; the Spot spend was about $2.23.
func TestComputeAccrual_SpotAccruesAtTheSpotRate(t *testing.T) {
	acc := ComputeAccrual{PerHour: 1.09, Basis: BasisSpot}
	compute := 2*time.Hour + 3*time.Minute // 2.05 h
	f := Footprint{ComputePerHour: 3.418, Accrual: &acc}
	got := Accrue(f, compute, compute).Compute
	if !nearly(got, 1.09*2.05) {
		t.Errorf("Spot compute after 2.05 h = $%.4f, want $%.4f (the Spot rate)", got, 1.09*2.05)
	}
	if od := Accrue(Footprint{ComputePerHour: 3.418}, compute, compute).Compute; !nearly(od, 3.418*2.05) {
		t.Errorf("on-demand compute = $%.4f, want $%.4f (unchanged)", od, 3.418*2.05)
	}
}

// A price change is integrated, and the interval it happened in is charged at
// the higher of the two prices (the cap errs early).
func TestComputeAccrual_AdvanceIntegratesAtTheHigherPrice(t *testing.T) {
	acc := ComputeAccrual{PerHour: 1.0, Basis: BasisSpot}
	acc = acc.Advance(3600, 2.0, BasisSpot, time.Time{}) // first hour: max(1,2) = $2
	if !nearly(acc.Dollars, 2.0) || acc.AtSeconds != 3600 || acc.PerHour != 2.0 {
		t.Fatalf("after a rise: %+v, want $2.00 at 3600 s, now $2/hr", acc)
	}
	acc = acc.Advance(7200, 0.5, BasisSpot, time.Time{}) // second hour: max(2,0.5) = $2
	if !nearly(acc.Dollars, 4.0) {
		t.Fatalf("after a fall: $%.4f, want $4.00", acc.Dollars)
	}
	// Extended at the current rate past the last sample.
	if got := acc.At(9000 * time.Second); !nearly(got, 4.0+0.5*1800/3600) {
		t.Errorf("At(9000s) = $%.4f, want $4.25", got)
	}
	// A lagging compute clock adds nothing and never subtracts.
	if got := acc.At(100 * time.Second); !nearly(got, 4.0) {
		t.Errorf("At(before AtSeconds) = $%.4f, want the $4.00 already accrued", got)
	}
	// Advancing to an earlier time only replaces the rate.
	if back := acc.Advance(10, 9, BasisOnDemandFallback, time.Time{}); !nearly(back.Dollars, 4.0) || back.AtSeconds != 7200 || back.PerHour != 9 {
		t.Errorf("Advance backwards = %+v, want only the rate replaced", back)
	}
}

func TestComputeAccrual_EncodeParseRoundTrip(t *testing.T) {
	in := ComputeAccrual{Dollars: 2.234567, AtSeconds: 7380, PerHour: 1.09, Basis: BasisSpot,
		SampledAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	v := EncodeComputeAccrual(in)
	if v != "v1:2.234567/7380/1.090000/spot/1794139200" {
		t.Errorf("encoded = %q", v)
	}
	if len(v) > 256 {
		t.Errorf("encoded is %d characters, over EC2's 256", len(v))
	}
	out, ok := ParseComputeAccrual(v)
	if !ok || out != in {
		t.Errorf("round trip = %+v, %v; want %+v", out, ok, in)
	}
	// A later version may append fields.
	if _, ok := ParseComputeAccrual("v2:" + v[3:] + "/extra"); !ok {
		t.Error("a v2 value with an appended field must still parse")
	}
	for _, bad := range []string{"", "1.09", "v1:1/2/3", "v1:x/1/1/spot/0", "v1:1/1/0/spot/0", "v1:1/1/1/guess/0", "v1:-1/1/1/spot/0", "vx:1/1/1/spot/0"} {
		if _, ok := ParseComputeAccrual(bad); ok {
			t.Errorf("ParseComputeAccrual(%q) accepted", bad)
		}
	}
}

// FromTags (every CLI reader) charges compute exactly as spored does: from the
// accrual when spored recorded one, at on-demand otherwise — the launch's
// spot-price tag alone does not switch it, because a spored before #1497
// enforced at on-demand.
func TestFromTags_ChargesTheRecordedAccrual(t *testing.T) {
	base := map[string]string{"spawn:price-per-hour": "3.418", "spawn:spot-price-per-hour": "1.09"}
	if f := FromTags(base, "spawn"); f.Accrual != nil || f.ComputePerHour != 3.418 {
		t.Errorf("no accrual tag: %+v, want on-demand with no accrual", f)
	}
	base["spawn:"+TagComputeAccrual] = "v1:1.000000/3600/1.200000/spot/0"
	f := FromTags(base, "spawn")
	if f.Accrual == nil || f.ComputePerHour != 1.2 {
		t.Fatalf("with an accrual: %+v, want the accrual at $1.20/hr", f)
	}
	if got := Accrue(f, 2*time.Hour, 2*time.Hour).Compute; !nearly(got, 2.2) {
		t.Errorf("compute = $%.4f, want $1.00 + 1 h x $1.20 = $2.20", got)
	}
}
