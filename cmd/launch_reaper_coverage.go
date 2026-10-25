package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spore-host/spawn/pkg/aws"
)

// detectReaperCoverage is a package-level seam (like resolveLaunchDryRunPrice and
// resolveSweepRowPrice) so tests can supply a verdict without touching AWS.
//
// region is the LAUNCH region (--region), which need not be the client's: the
// coverage question is whether a reaper scans the region the instance will run
// in (#1298 — the instance that ran 74 hours was in a region its account's
// reaper did not scan).
var detectReaperCoverage = func(ctx context.Context, client *aws.Client, region string) aws.ReaperCoverage {
	cfg := client.Config().Copy()
	if region != "" {
		cfg.Region = region
	}
	return aws.DetectReaperCoverage(ctx, cfg)
}

// launchLeansOnReaper reports whether this launch's flags make the out-of-band
// reaper load-bearing, i.e. whether its absence changes what the user ends up
// paying for.
//
// Only these two cases qualify:
//
//   - --on-complete stop/hibernate: the workload finishes, the instance stops, and
//     spored — which runs INSIDE the instance — can no longer act. EBS and any
//     Elastic IP keep billing with nothing left to reclaim them. This is the exact
//     shape of the instance that sat stopped for 13 days past its TTL.
//   - --fsx-lifecycle ephemeral: the filesystem outlives its instance by design, so
//     the reaper is the ONLY thing that deletes it (spawn#613). No reaper means a
//     1200 GiB minimum filesystem, ~$174/month, persists.
//
// A plain `--on-complete terminate` launch does NOT qualify: spored terminates it
// from inside, and the reaper is a backstop for spored dying rather than the
// primary mechanism. Warning there would be the noise that made the old
// always-on check worthless.
func launchLeansOnReaper() bool {
	switch {
	case onComplete == "stop" || onComplete == "hibernate":
		return true
	case fsxLifecycle == "ephemeral":
		return true
	default:
		return false
	}
}

// warnIfReaperWontCover prints a warning when this launch depends on the reaper and
// no reaper covers the account. It is best-effort and never blocks a launch: the
// user asked for an instance, and a coverage probe failing is not a reason to
// refuse one.
//
// Every other launch — --on-complete terminate, or none — leans on the reaper
// too, but only if spored never starts or dies (#1298): TTL and --cost-limit are
// enforced by spored alone, from inside the instance. That is warned about only
// on positive evidence that no reaper scans the launch region, never on an
// undetermined probe, which would print on every launch by a caller without the
// lambda/iam read permissions — the noise that made the old always-on check
// worthless.
func warnIfReaperWontCover(ctx context.Context, client *aws.Client, region string, out io.Writer) {
	if client == nil {
		return
	}
	coverage := detectReaperCoverage(ctx, client, region)
	if msg := launchReaperAdvice(coverage, launchLeansOnReaper(), onComplete, fsxLifecycle); msg != "" {
		fmt.Fprintf(out, "\n%s\n\n", msg)
	}
}

// launchReaperAdvice is the warning for a launch given the reaper verdict, or
// "" for none. leans is launchLeansOnReaper. Pure, so the decision is testable.
func launchReaperAdvice(c aws.ReaperCoverage, leans bool, onComplete, fsxLifecycle string) string {
	if leans {
		return aws.ReaperCoverageAdvice(c, onComplete, fsxLifecycle)
	}
	if c.Covered || !c.Determined {
		return ""
	}
	return "⚠️  No out-of-band TTL reaper scans this launch's region. This instance's TTL and any\n" +
		"   --cost-limit are enforced only by spored, from INSIDE the instance: if spored never starts, or\n" +
		"   dies, nothing terminates it. (A launch whose spored is not confirmed is terminated unless\n" +
		"   --keep-on-error.) Deploy a reaper covering the region: spawn reaper deploy."
}
