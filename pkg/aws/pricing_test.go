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

// TestResolveDisplayPrice_Success is the #543 regression for the DISPLAY-side
// pricing call sites (--estimate-only, `spawn service --dry-run`, `spawn
// cost`): a successful truffle lookup returns the real rate and its source,
// not libs/pricing's static per-family-size guess (which under-quoted
// c8g.metal-48xl in us-east-1 by 38x — $0.20/hr against a real $7.657/hr).
func TestResolveDisplayPrice_Success(t *testing.T) {
	pricer := fakePricer{price: 7.6570, source: truffleaws.PriceSourceLive}

	dp, err := resolveDisplayPrice(context.Background(), pricer, "c8g.metal-48xl", "us-east-1")
	if err != nil {
		t.Fatalf("resolveDisplayPrice: unexpected error: %v", err)
	}
	if dp.PricePerHour != 7.6570 {
		t.Errorf("PricePerHour = %v, want 7.6570 (the real rate, not the $0.20 fabricated guess)", dp.PricePerHour)
	}
	if dp.Source != string(truffleaws.PriceSourceLive) {
		t.Errorf("Source = %q, want %q", dp.Source, truffleaws.PriceSourceLive)
	}
	if got := dp.SourceLabel(); got != "live" {
		t.Errorf("SourceLabel() = %q, want %q", got, "live")
	}
}

// TestResolveDisplayPrice_StaticSourceIsLabeled covers truffle's own safe
// static fallback: it must still return a usable price, labeled distinctly
// from a live quote so a caller can tell a degraded-but-real rate from the
// authoritative one.
func TestResolveDisplayPrice_StaticSourceIsLabeled(t *testing.T) {
	pricer := fakePricer{price: 0.096, source: truffleaws.PriceSourceStatic}

	dp, err := resolveDisplayPrice(context.Background(), pricer, "m5.large", "us-east-1")
	if err != nil {
		t.Fatalf("resolveDisplayPrice: unexpected error: %v", err)
	}
	if dp.PricePerHour != 0.096 {
		t.Errorf("PricePerHour = %v, want 0.096", dp.PricePerHour)
	}
	if got := dp.SourceLabel(); got != "static fallback" {
		t.Errorf("SourceLabel() = %q, want %q", got, "static fallback")
	}
}

// TestResolveDisplayPrice_FailureIsAnErrorNotAGuess covers a truffle pricing
// miss: the display path must return an explicit error rather than falling
// back to a fabricated rate. Unlike resolvePricePerHour there is no
// --cost-limit gate to make conditional — a display quote that can't be
// priced is always worth surfacing as an error, since there is nothing else
// useful to show.
func TestResolveDisplayPrice_FailureIsAnErrorNotAGuess(t *testing.T) {
	pricer := fakePricer{err: errors.New("no on-demand price found")}

	dp, err := resolveDisplayPrice(context.Background(), pricer, "p5.48xlarge", "us-east-1")
	if err == nil {
		t.Fatal("resolveDisplayPrice: expected an error when truffle cannot price the type, got nil")
	}
	if dp.PricePerHour != 0 {
		t.Errorf("PricePerHour = %v, want 0 (must not fabricate a rate on failure)", dp.PricePerHour)
	}
	for _, want := range []string{"p5.48xlarge", "us-east-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q — the failure must be actionable", err.Error(), want)
		}
	}
}

// TestResolveDisplayPrice_ZeroPriceNoErrorIsTreatedAsFailure mirrors
// TestResolvePricePerHour_ZeroPriceNoErrorTreatedAsFailure: a pricer that
// returns (0, source, nil) has not actually priced the instance, and must not
// be reported as a success.
func TestResolveDisplayPrice_ZeroPriceNoErrorIsTreatedAsFailure(t *testing.T) {
	pricer := fakePricer{price: 0, source: truffleaws.PriceSourceUnknown, err: nil}

	if _, err := resolveDisplayPrice(context.Background(), pricer, "weird.type", "us-east-1"); err == nil {
		t.Fatal("resolveDisplayPrice: expected an error for a zero price, got nil")
	}
}
