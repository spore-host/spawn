package cmd

import (
	"context"
	"fmt"
	"io"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// reportAutoscaleCoverage prints a warning when nothing in this account will act
// on an autoscale group. Silent when coverage is confirmed.
//
// This is spawn#772. `autoscale launch` and `add-schedule` write into the
// group's DynamoDB record; a SCHEDULED Lambda reads those records and acts.
// Nothing in the CLI referenced that schedule, so the halves could come apart
// and every command would still report success.
//
// It is a WARNING, never an error. A group recorded without an orchestrator is
// still recorded, and failing the command would make a reporting gap into an
// outage. The same choice reaper_skew.go makes: make the gap visible, do not
// gate on it.
//
// Best-effort by construction — a coverage probe that cannot complete must not
// turn a successful launch into a failure, so every error path here degrades to
// either a hedged note or silence.
func reportAutoscaleCoverage(ctx context.Context, w io.Writer) spawnaws.AutoscaleCoverage {
	// The SAME resolver the rest of the command uses (spawn#774), so coverage
	// cannot report on a different region than the one being operated on. It
	// previously called config.LoadDefaultConfig directly and reported "no
	// autoscale orchestrator runs in this account" against an account that has
	// one, because the ambient region was us-west-2.
	cfg, err := autoscaleConfig(ctx)
	if err != nil {
		// Nothing useful to say, and the caller's real work already succeeded.
		return spawnaws.AutoscaleCoverage{Why: err.Error()}
	}
	// Scoped to the env being operated on (--env), not "any orchestrator in the
	// account": matching a prefix warned a production user about staging.
	c := spawnaws.DetectAutoscaleCoverage(ctx, cfg, autoscaleEnv)
	if advice := spawnaws.AutoscaleCoverageAdvice(c); advice != "" {
		fmt.Fprintf(w, "\n⚠️  %s\n", advice)
		return c
	}
	if spawnVerbose {
		fmt.Fprintf(w, "\nautoscale orchestrator: %s\n", c.How)
	}
	return c
}

// autoscaleScheduleClaim is the sentence `autoscale launch` prints about what
// happens next, chosen by whether anything is actually scheduled.
//
// The old wording was unconditional: "Instances will launch on next scheduled
// run (within 1 minute)." With the schedule disabled that is simply false, and
// it is worse than a generic message because the user has been told a specific
// interval to wait for.
//
// Note why the immediate trigger does not rescue this: triggerLambda INVOKES the
// function directly, which succeeds whether or not a rule is enabled. So a
// disabled schedule reconciles exactly once, at creation, and then never again —
// which looks like success and decays silently.
func autoscaleScheduleClaim(c spawnaws.AutoscaleCoverage) string {
	switch {
	case c.Covered:
		return "Instances will launch on the next scheduled run."
	case !c.Determined:
		return "Could not confirm the orchestrator is scheduled; if nothing launches, run " +
			"`spawn autoscale status` to check coverage."
	default:
		return "NOTHING IS SCHEDULED to reconcile this group — see the warning above. " +
			"The immediate trigger below reconciles once; after that the group is inert."
	}
}
