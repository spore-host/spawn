package aws

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

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
//
// windows is whether the instance runs Windows. truffle prices Linux only
// (spore-host/truffle#185), so for Windows its figure omits the licence EC2
// bills on top — m7i.xlarge in us-east-1 is $0.2016/hr Linux against $0.3856/hr
// Windows. spored enforces --cost-limit as PricePerHour × compute hours, so a
// Linux rate made a Windows cap fire after about twice the spend it promised
// (#1058). A Windows rate cannot be priced honestly yet, so it is treated as
// unpriceable: refused with a cost limit, left at 0 without one — the same two
// outcomes as any other pricing failure, and never a figure known to be low.
func resolvePricePerHour(ctx context.Context, pricer onDemandPricer, cfg *LaunchConfig, windows bool) error {
	if cfg.PricePerHour != 0 {
		return nil
	}
	if windows {
		if err := CheckCostLimitEnforceable(*cfg, windows); err != nil {
			return err
		}
		log.Printf("pricing: %s in %s not priced — Windows rates are not available (truffle#185); "+
			"no spawn:price-per-hour is recorded rather than the lower Linux rate", cfg.InstanceType, cfg.Region)
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
		// A warning the user must see, so stderr directly rather than the `log`
		// diagnostics a CLI may route to a file (#895).
		fmt.Fprintf(os.Stderr, "⚠️  pricing: no on-demand price available for %s in %s (%v) — launching with no cost-limit enforcement possible\n", cfg.InstanceType, cfg.Region, err)
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

// launchIsWindowsForPricing reports whether a launch must be priced as Windows
// (#1058). TargetOS answers when set; "" (which callers other than the CLI leave
// it as) is resolved from the AMI, but only when a cost limit makes the answer
// matter and no rate was supplied, so an ordinary launch pays no extra
// DescribeImages. isWindowsAMI is [Client.IsWindowsAMI] in production.
func launchIsWindowsForPricing(ctx context.Context, cfg LaunchConfig, isWindowsAMI func(ctx context.Context, region, ami string) bool) bool {
	if strings.EqualFold(strings.TrimSpace(cfg.TargetOS), "windows") {
		return true
	}
	if cfg.TargetOS != "" || cfg.CostLimit <= 0 || cfg.PricePerHour != 0 {
		return false
	}
	return isWindowsAMI(ctx, cfg.Region, cfg.AMI)
}

// CheckCostLimitEnforceable refuses a --cost-limit that spored could not enforce
// honestly: one on a Windows instance with no caller-supplied PricePerHour, since
// the only rate available is the Linux one, below what Windows bills (#1058).
// Launch applies it; the CLI also calls it in preflight, so the refusal comes
// before any resource is created rather than at RunInstances.
func CheckCostLimitEnforceable(cfg LaunchConfig, windows bool) error {
	if !windows || cfg.CostLimit <= 0 || cfg.PricePerHour != 0 {
		return nil
	}
	return fmt.Errorf("--cost-limit %.2f cannot be enforced on a Windows instance: on-demand pricing "+
		"covers Linux only (spore-host/truffle#185), and enforcing the cap against the Linux rate would let "+
		"%s spend well past it. Bound the instance with --ttl instead", cfg.CostLimit, cfg.InstanceType)
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
