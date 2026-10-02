package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spore-host/spawn/pkg/aws"
)

// detectReaperCoverage is a package-level seam (like resolveLaunchDryRunPrice and
// resolveSweepRowPrice) so tests can supply a verdict without touching AWS.
var detectReaperCoverage = func(ctx context.Context, client *aws.Client) aws.ReaperCoverage {
	return aws.DetectReaperCoverage(ctx, client.Config())
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
func warnIfReaperWontCover(ctx context.Context, client *aws.Client, out io.Writer) {
	if !launchLeansOnReaper() || client == nil {
		return
	}
	coverage := detectReaperCoverage(ctx, client)
	if msg := aws.ReaperCoverageAdvice(coverage, onComplete, fsxLifecycle); msg != "" {
		fmt.Fprintf(out, "\n%s\n\n", msg)
	}
}
