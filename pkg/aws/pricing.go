package aws

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// onDemandPricer is the subset of *truffleaws.Client's pricing API that
// resolvePricePerHour needs. Defining it as a spawn-local interface (rather
// than taking *truffleaws.Client directly) lets tests inject a fake that
// never touches AWS, since *truffleaws.Client satisfies it structurally.
type onDemandPricer interface {
	OnDemandPriceWithSource(ctx context.Context, instanceType, region string) (float64, truffleaws.PriceSource, error)
}

// resolvePricePerHour fills in cfg.PricePerHour by asking truffle — the
// suite's pricing authority (#533) — for the current on-demand rate. spawn no
// longer looks up or fabricates a price itself: its own hand-rolled AWS
// Pricing API call had no cache and, on failure, fell back to a static
// per-family-size guess table that was wrong by 5-9x for any current-gen GPU
// family (g6e, p5, ...) — silently undermining --cost-limit, which is
// enforced against whatever ends up in the spawn:price-per-hour tag.
//
// If cfg.PricePerHour is already set (non-zero), this is a no-op — an
// explicit caller-supplied price always wins.
//
// If truffle cannot determine a price (neither a live Price List lookup nor
// its own static-table fallback succeeds) and the launch requested a
// --cost-limit, the launch fails outright: a cost limit that cannot be priced
// cannot be enforced, and silently leaving it unenforced is worse than
// refusing to launch. If no cost limit was requested, the failure is
// non-fatal: PricePerHour stays 0, which spored's cap-check already treats as
// "rate unknown, no cap enforced" — never a fabricated number.
func resolvePricePerHour(ctx context.Context, pricer onDemandPricer, cfg *LaunchConfig) error {
	if cfg.PricePerHour != 0 {
		return nil
	}

	price, source, err := pricer.OnDemandPriceWithSource(ctx, cfg.InstanceType, cfg.Region)
	if err == nil && price <= 0 {
		err = fmt.Errorf("no on-demand price returned for %s in %s", cfg.InstanceType, cfg.Region)
	}
	if err != nil {
		if cfg.CostLimit > 0 {
			return fmt.Errorf("cannot determine the on-demand price for %s in %s, required to enforce --cost-limit %.2f: %w",
				cfg.InstanceType, cfg.Region, cfg.CostLimit, err)
		}
		log.Printf("pricing: no on-demand price available for %s in %s (%v) — launching with no cost-limit enforcement possible", cfg.InstanceType, cfg.Region, err)
		return nil
	}

	cfg.PricePerHour = price
	switch source {
	case truffleaws.PriceSourceLive:
		log.Printf("pricing: %s in %s = $%.4f/hr (truffle, live AWS Price List)", cfg.InstanceType, cfg.Region, price)
	case truffleaws.PriceSourceStatic:
		log.Printf("pricing: %s in %s = $%.4f/hr (truffle, static fallback table — Price List API unavailable)", cfg.InstanceType, cfg.Region, price)
	default:
		log.Printf("pricing: %s in %s = $%.4f/hr (truffle)", cfg.InstanceType, cfg.Region, price)
	}
	return nil
}

// DisplayPrice is an on-demand rate quoted to a human (an estimate, a dry-run
// preview, a cost breakdown) rather than one enforced against a spend cap.
// Source records where it came from, matching [truffleaws.PriceSource]'s two
// values ("live" or "static") as a string so display call sites don't need to
// import truffle's aws package just to print it.
type DisplayPrice struct {
	PricePerHour float64
	Source       string // "live", "static", or "" (unknown)
}

// SourceLabel renders Source as the short parenthetical shown next to a
// quoted rate, e.g. "(live)" or "(static fallback)". Per #543, an estimate
// that doesn't say whether it was quoted or guessed cannot be audited, so
// every display call site shows this alongside the number.
func (d DisplayPrice) SourceLabel() string {
	switch d.Source {
	case string(truffleaws.PriceSourceLive):
		return "live"
	case string(truffleaws.PriceSourceStatic):
		return "static fallback"
	default:
		return "unknown source"
	}
}

// ResolveDisplayPrice asks truffle — the suite's pricing authority (#533) —
// for instanceType's current on-demand rate in region, for DISPLAY purposes
// (an --estimate-only quote, a dry-run preview, a `spawn cost` breakdown).
//
// This is the display-side counterpart to [resolvePricePerHour], which fills
// LaunchConfig.PricePerHour for --cost-limit enforcement. There is no cost
// limit to fail here, so a pricing miss is reported as an error rather than
// silently substituted: a quote that doesn't say whether it is real or
// fabricated cannot be audited (#543), and libs/pricing's old per-family-size
// guess table was wrong by up to 38x (metal-Nxl variants fell through to a
// "default to xlarge" multiplier meant for a 4-vCPU size, not a 96-192 vCPU
// bare-metal box).
func ResolveDisplayPrice(ctx context.Context, cfg aws.Config, instanceType, region string) (DisplayPrice, error) {
	return resolveDisplayPrice(ctx, truffleaws.NewClientFromConfig(cfg), instanceType, region)
}

// resolveDisplayPrice is [ResolveDisplayPrice]'s testable core: it takes the
// same spawn-local onDemandPricer seam as [resolvePricePerHour], so tests can
// inject a fake and never touch AWS or the network.
func resolveDisplayPrice(ctx context.Context, pricer onDemandPricer, instanceType, region string) (DisplayPrice, error) {
	price, source, err := pricer.OnDemandPriceWithSource(ctx, instanceType, region)
	if err == nil && price <= 0 {
		err = fmt.Errorf("no on-demand price returned for %s in %s", instanceType, region)
	}
	if err != nil {
		return DisplayPrice{}, fmt.Errorf("cannot determine the on-demand price for %s in %s: %w", instanceType, region, err)
	}
	return DisplayPrice{PricePerHour: price, Source: string(source)}, nil
}
