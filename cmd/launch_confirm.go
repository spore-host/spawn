package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spore-host/spawn/pkg/aws"
)

// confirmLaunchCost is the cost check before a launch (#1115). --yes used to
// promise "Auto-approve cost estimate (skip confirmation)" on a launch that
// showed no estimate and asked nothing: `echo n | spawn launch …` launched.
//
// On an interactive terminal it now shows what is about to be launched and
// what it can cost at most, and asks [y/N]; --yes skips the question. A
// non-interactive run (a script, CI, a pipe) launches as before, so nothing
// that runs today breaks, but every launch prints its maximum cost to stderr.
//
// It returns false only when the user answered no; the caller returns
// newAborted, so the exit code says the launch was declined. A price that can't be
// resolved never blocks a launch: the summary says the rate is unknown.
func confirmLaunchCost(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig) bool {
	plan := describeLaunchPlan(config)
	if dp, err := resolveLaunchDryRunPrice(ctx, awsClient, config.InstanceType, config.Region); err == nil {
		plan.OnDemandPricePerHour = dp.PricePerHour
		plan.PriceSource = dp.SourceLabel()
		plan.PriceIsLowerBound = isWindowsTarget(config.TargetOS)
		if d, derr := parseDuration(plan.TTL); derr == nil {
			plan.MaxCostUSD = dp.PricePerHour * d.Hours() * float64(max(plan.Count, 1))
		}
	}
	interactive := launchStdinIsInteractive()
	if autoYes || !interactive {
		fmt.Fprintln(os.Stderr, maxCostLine(plan))
		return true
	}
	renderLaunchConfirmation(os.Stderr, plan)
	// A filesystem --fsx-create would make costs far more than the instance
	// (spawn#613); the question must not be asked without it.
	if fp, ok := fsxCreatePlanFromFlags(config.FSxLustreCreate, fsxStorageCapacity, fsxThroughput, config.FSxLifecycle, config.FSxTTL, config.FSxMountPoint); ok {
		renderFSxCostEstimate(os.Stderr, fp)
	}
	return confirmYes(false, "Launch?")
}

// launchStdinIsInteractive is a seam so tests decide whether stdin is a
// terminal, rather than depending on how `go test` wires it.
var launchStdinIsInteractive = stdinIsInteractive

// renderLaunchConfirmation is the summary shown before the question: the same
// facts as --dry-run's instance, purchase, lifetime, rate and maximum cost.
func renderLaunchConfirmation(out io.Writer, p launchPlan) {
	fmt.Fprintf(out, "About to launch:\n")
	if p.Count > 1 {
		fmt.Fprintf(out, "  Instances:    %d × %s in %s (job array %s)\n", p.Count, orDash(p.InstanceType), orDash(p.Region), orDash(p.JobArrayName))
	} else {
		fmt.Fprintf(out, "  Instance:     %s in %s\n", orDash(p.InstanceType), orDash(p.Region))
	}
	purchase := "on-demand"
	switch {
	case p.Spot:
		purchase = "spot"
	case p.CapacityBlock:
		purchase = "capacity block " + p.ReservationID
	case p.ReservationID != "":
		purchase = "on-demand into reservation " + p.ReservationID
	}
	fmt.Fprintf(out, "  Purchase:     %s\n", purchase)
	fmt.Fprintf(out, "  Lifetime:     ttl=%s idle=%s on-complete=%s\n", orDash(p.TTL), orDash(p.IdleTimeout), orDash(p.OnComplete))
	if p.OnDemandPricePerHour > 0 {
		fmt.Fprintf(out, "  Rate:         $%.4f/hr on-demand (%s)\n", p.OnDemandPricePerHour, p.PriceSource)
		if p.PriceIsLowerBound {
			fmt.Fprintf(out, "                ⚠️  %s\n", windowsLicenceNote)
		}
	}
	fmt.Fprintf(out, "  %s\n", maxCostLine(p))
}

// maxCostLine states the most the launch can cost: the on-demand rate for the
// whole TTL, for every instance. A Spot launch usually costs less, and a
// launch that finishes early much less; it is a ceiling, not an estimate.
func maxCostLine(p launchPlan) string {
	switch {
	case p.MaxCostUSD > 0 && p.Count > 1:
		return fmt.Sprintf("Max cost:     ~$%.2f (on-demand rate × ttl × %d instances, compute only)", p.MaxCostUSD, p.Count)
	case p.MaxCostUSD > 0:
		return fmt.Sprintf("Max cost:     ~$%.2f (on-demand rate × ttl, compute only)", p.MaxCostUSD)
	case p.TTL == "":
		return "Max cost:     unbounded: no TTL, so nothing stops the instance on a schedule"
	default:
		return "Max cost:     unknown (no on-demand rate for this type and region)"
	}
}
