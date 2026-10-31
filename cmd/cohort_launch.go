package cmd

import (
	"fmt"
	"time"

	"github.com/spore-host/cohort"
	"github.com/spore-host/spawn/pkg/aws"
)

// launchPreworkAllowance is the part of a cohort member's launch budget for
// what aws.Client.Launch does around its RunInstances: caller identity, the
// account name, the price through truffle, the launch config object in S3, the
// AMI's root device, and afterwards the instance tags, the spot price record
// and any Elastic IP. Generous: the budget is a backstop, not a pace.
const launchPreworkAllowance = time.Minute

// cohortLaunch is what one member's cohort LaunchAcked context has to hold, for
// sizing that budget (#1448, #1454). A deadline shorter than this, not the
// retry schedule, decides when a launch gives up, and the member is reported
// failed for an error that was about to clear.
type cohortLaunch struct {
	// attempts is how many Launch calls share one LaunchAcked context. A
	// member that places on its own walks its whole rung chain in one context
	// (cohort's doLaunch advances on capacity without a new deadline), so it is
	// the chain's length.
	attempts int
	// beforeEach is what the Actuator does before each Launch: the wait for a
	// per-AZ placement group to become available (mpicohort.Actuator).
	beforeEach time.Duration
}

// budget is the LaunchAcked budget for l: the #572 instance-profile retry
// window once, plus every attempt's own calls. The retry window is spent at
// most once per context: the profile propagates once, so attempts after the
// one that saw it propagate are not rejected for it, and a retry that runs out
// is a terminal error that ends the walk.
func (l cohortLaunch) budget() time.Duration {
	n := l.attempts
	if n < 1 {
		n = 1
	}
	return aws.InstanceProfilePropagationRetryWindow() + time.Duration(n)*(launchPreworkAllowance+l.beforeEach)
}

// cohortMemberReason is cohort's Record.Summary with the fault's AWS error code
// and message, which Summary leaves out ("terminal/ at phase=launch-acked",
// #1448), for the failed member and for the members cancelled around it.
func cohortMemberReason(rec cohort.Record) string {
	switch {
	case rec.Terminal != nil:
		return fmt.Sprintf("%s at phase=%s after %d rung(s)", cohortFaultText(*rec.Terminal), rec.ReachedPhase, len(rec.Attempts))
	case rec.CohortCancelled != nil:
		cc := rec.CohortCancelled
		return fmt.Sprintf("cohort-cancelled at phase=%s — culprit=%s %s at phase=%s",
			cc.SurvivorPhase, cc.CulpritID, cohortFaultText(cc.CulpritFault), cc.CulpritPhase)
	default:
		return rec.Summary()
	}
}

func cohortFaultText(f cohort.Fault) string {
	code := f.Code
	if code == "" {
		code = "unknown"
	}
	s := fmt.Sprintf("%s/%s", f.Class, code)
	if f.Message != "" {
		s += ": " + f.Message
	}
	return s
}
