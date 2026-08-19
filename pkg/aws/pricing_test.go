package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// fakePricer is a deterministic, offline stand-in for *truffleaws.Client's
// pricing API so these tests never touch AWS.
type fakePricer struct {
	price  float64
	source truffleaws.PriceSource
	err    error
}

func (f fakePricer) OnDemandPriceWithSource(_ context.Context, _, _ string) (float64, truffleaws.PriceSource, error) {
	return f.price, f.source, f.err
}

// TestResolvePricePerHour_Success covers #533 case (a): a successful truffle
// lookup populates cfg.PricePerHour with the real rate (not a fabricated
// static-table guess) so it gets tagged correctly downstream.
func TestResolvePricePerHour_Success(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "g6e.xlarge", Region: "us-east-1"}
	pricer := fakePricer{price: 1.861, source: truffleaws.PriceSourceLive}

	if err := resolvePricePerHour(context.Background(), pricer, cfg); err != nil {
		t.Fatalf("resolvePricePerHour: unexpected error: %v", err)
	}
	if cfg.PricePerHour != 1.861 {
		t.Errorf("PricePerHour = %v, want 1.861 (the real rate, not a fabricated guess)", cfg.PricePerHour)
	}
}

// TestResolvePricePerHour_StaticSourceStillSucceeds covers truffle's own safe
// static fallback (a real published rate, not a family-size guess): it must
// still populate PricePerHour and succeed, just like a live lookup.
func TestResolvePricePerHour_StaticSourceStillSucceeds(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "m5.large", Region: "us-east-1"}
	pricer := fakePricer{price: 0.096, source: truffleaws.PriceSourceStatic}

	if err := resolvePricePerHour(context.Background(), pricer, cfg); err != nil {
		t.Fatalf("resolvePricePerHour: unexpected error: %v", err)
	}
	if cfg.PricePerHour != 0.096 {
		t.Errorf("PricePerHour = %v, want 0.096", cfg.PricePerHour)
	}
}

// TestResolvePricePerHour_FailureWithCostLimit covers #533 case (b): when
// truffle cannot price the instance at all AND the launch requested
// --cost-limit, the launch must fail with a clear, actionable error rather
// than silently leaving PricePerHour at 0 (which would let the instance run
// with an unenforced cap) or falling back to a fabricated guess.
func TestResolvePricePerHour_FailureWithCostLimit(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "p5.48xlarge", Region: "us-east-1", CostLimit: 8}
	pricer := fakePricer{err: errors.New("no on-demand price found")}

	err := resolvePricePerHour(context.Background(), pricer, cfg)
	if err == nil {
		t.Fatal("resolvePricePerHour: expected an error when pricing fails with a cost limit requested, got nil")
	}
	if cfg.PricePerHour != 0 {
		t.Errorf("PricePerHour = %v, want 0 (must not fabricate a rate on failure)", cfg.PricePerHour)
	}
	for _, want := range []string{"p5.48xlarge", "us-east-1", "8.00"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q — the failure must be actionable", err.Error(), want)
		}
	}
}

// TestResolvePricePerHour_FailureWithoutCostLimit covers #533 case (c): when
// truffle cannot price the instance and no --cost-limit was requested, the
// launch proceeds — PricePerHour is left at 0 (spored's cap-check already
// treats 0 as "rate unknown, no cap enforced"), never a fabricated number.
func TestResolvePricePerHour_FailureWithoutCostLimit(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "p5.48xlarge", Region: "us-east-1"}
	pricer := fakePricer{err: errors.New("no on-demand price found")}

	if err := resolvePricePerHour(context.Background(), pricer, cfg); err != nil {
		t.Fatalf("resolvePricePerHour: expected no error without a cost limit, got: %v", err)
	}
	if cfg.PricePerHour != 0 {
		t.Errorf("PricePerHour = %v, want 0 (must not fabricate a rate)", cfg.PricePerHour)
	}
}

// TestResolvePricePerHour_ZeroPriceNoErrorTreatedAsFailure covers the edge
// case where the pricer returns (0, source, nil) — no error, but also no
// usable price. This must be treated the same as an error: with a cost
// limit, fail; without one, proceed with PricePerHour left at 0.
func TestResolvePricePerHour_ZeroPriceNoErrorTreatedAsFailure(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "weird.type", Region: "us-east-1", CostLimit: 5}
	pricer := fakePricer{price: 0, source: truffleaws.PriceSourceUnknown, err: nil}

	if err := resolvePricePerHour(context.Background(), pricer, cfg); err == nil {
		t.Fatal("resolvePricePerHour: expected an error for a zero price with a cost limit requested, got nil")
	}
}

// TestResolvePricePerHour_ExplicitPriceIsNoOp confirms an already-set
// PricePerHour (e.g. an SDK caller that supplied its own rate) is never
// overridden by a truffle lookup.
func TestResolvePricePerHour_ExplicitPriceIsNoOp(t *testing.T) {
	cfg := &LaunchConfig{InstanceType: "m5.large", Region: "us-east-1", PricePerHour: 3.14}
	pricer := fakePricer{price: 0.096, source: truffleaws.PriceSourceLive}

	if err := resolvePricePerHour(context.Background(), pricer, cfg); err != nil {
		t.Fatalf("resolvePricePerHour: unexpected error: %v", err)
	}
	if cfg.PricePerHour != 3.14 {
		t.Errorf("PricePerHour = %v, want 3.14 (caller-supplied price must not be overridden)", cfg.PricePerHour)
	}
}
