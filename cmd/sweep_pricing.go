package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spore-host/libs/pricing"
	"github.com/spore-host/spawn/pkg/aws"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// resolveSweepRowPrice looks up the on-demand rate for one sweep row. It is a
// package-level seam (like [resolveServiceDryRunPrice] in cmd/service.go and
// [resolveLaunchDryRunPrice] in cmd/launch_dryrun.go) so tests can price a sweep
// without touching AWS or requiring credentials.
//
// The default loads a region-pinned config and delegates to
// [aws.ResolveDisplayPrice] — truffle, the suite's pricing authority (#533).
// truffle already degrades from the live Price List to its own exact-match static
// table internally and reports which one answered, so there is no fallback to
// build here: a result is either a real quote (live or static) or an error.
var resolveSweepRowPrice = func(ctx context.Context, region, instanceType string) (aws.DisplayPrice, error) {
	cfg, err := loadAWSConfig(ctx, region)
	if err != nil {
		return aws.DisplayPrice{}, fmt.Errorf("load AWS config for %s: %w", region, err)
	}
	return aws.ResolveDisplayPrice(ctx, cfg, instanceType, region)
}

// sweepPricer adapts [resolveSweepRowPrice] to [pricing.RateResolver], memoizing
// per (region, instanceType) and recording where each quote came from.
//
// Memoization is the point: a sweep is usually many rows over few distinct
// shapes, so a 500-row sweep makes one pricing call per distinct pair rather than
// 500. It also keeps the "N live, M static" summary counting distinct shapes
// instead of double-counting repeated rows.
type sweepPricer struct {
	ctx   context.Context
	cache map[string]sweepPriceEntry
}

type sweepPriceEntry struct {
	price float64
	quote aws.DisplayPrice // the rate and its origin, for SourceSummary
	err   error
}

func newSweepPricer(ctx context.Context) *sweepPricer {
	return &sweepPricer{ctx: ctx, cache: map[string]sweepPriceEntry{}}
}

// Resolve satisfies [pricing.RateResolver]. An error here is not fatal: the
// estimator records the row as unpriced and the preview names it, which is the
// honest alternative to the guessed rate that used to fill the gap (#29).
func (p *sweepPricer) Resolve(region, instanceType string) (float64, error) {
	key := region + "\x00" + instanceType
	if hit, ok := p.cache[key]; ok {
		return hit.price, hit.err
	}

	dp, err := resolveSweepRowPrice(p.ctx, region, instanceType)
	entry := sweepPriceEntry{price: dp.PricePerHour, quote: dp, err: err}
	if err == nil && dp.PricePerHour <= 0 {
		entry.err = fmt.Errorf("no on-demand price for %s in %s", instanceType, region)
	}
	p.cache[key] = entry
	return entry.price, entry.err
}

// SourceSummary describes where the successful quotes came from, e.g.
// "2 instance shape(s) priced: 1 live, 1 static (static, collected 2026-10-07:
// live AWS Price List unavailable: <err>)". Per #543 an estimate that doesn't
// say whether it was quoted or guessed cannot be audited, and the single-launch
// path already labels its rate this way — this is the sweep path's equivalent.
// A static rate must also say why and when (#1502), so each distinct static
// origin is spelled out once in truffle's wording rather than per row.
//
// It returns "" when nothing priced, since the unpriced-row list already says so.
func (p *sweepPricer) SourceSummary() string {
	counts := map[string]int{}
	var staticOrigins []truffleaws.PriceOrigin
	total := 0
	for _, e := range p.cache {
		if e.err != nil {
			continue
		}
		o := e.quote.Origin()
		switch {
		case o.IsStatic():
			counts["static"]++
			staticOrigins = append(staticOrigins, o)
		default:
			counts[e.quote.SourceLabel()]++
		}
		total++
	}
	if total == 0 {
		return ""
	}

	labels := make([]string, 0, len(counts))
	for label := range counts {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		part := fmt.Sprintf("%d %s", counts[label], label)
		if label == "static" {
			part += " (" + strings.Join(truffleaws.StaticPriceNotes(staticOrigins), "; ") + ")"
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("%d instance shape(s) priced: %s", total, strings.Join(parts, ", "))
}

// estimateAndReportSweep prices a sweep through truffle, prints the estimate with
// where the rates came from and which rows could not be priced, and reports
// --budget.
//
// Both the --estimate-only path ([estimateSweepOnly]) and the real detached
// launch go through here so the two cannot drift — the same reason
// [reportSweepBudget] is shared.
//
// Rates used to come from libs/pricing's static per-family-size guess, which
// returned a fabricated number for any instance family absent from its table —
// every GPU family newer than p4d — with no error, so a g6e.12xlarge row was
// quoted at $2.40/hr against a real $10.49 and the sweep total was understated
// 4-12x for GPU work (spore-host/libs#29). Now an unpriceable row is named and
// excluded, and the total is labelled a FLOOR.
func estimateAndReportSweep(ctx context.Context, paramFormat *ParamFileFormat) (*pricing.CostEstimate, error) {
	fmt.Fprintf(os.Stderr, "💰 Estimating cost...\n")

	pricer := newSweepPricer(ctx)
	costEstimate, err := pricing.EstimateSweepCost(&pricing.ParamFileFormat{
		Defaults: paramFormat.Defaults,
		Params:   paramFormat.Params,
	}, pricer.Resolve)
	if err != nil {
		return nil, fmt.Errorf("failed to estimate cost: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n%s\n", costEstimate.Display())
	if summary := pricer.SourceSummary(); summary != "" {
		fmt.Fprintf(os.Stderr, "\n  %s\n", summary)
	}
	fmt.Fprintf(os.Stderr, "\n")

	reportSweepBudget(costEstimate.TotalCost, costEstimate.Partial())
	return costEstimate, nil
}
