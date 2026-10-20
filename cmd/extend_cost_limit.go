package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
)

// costLimitDecision is what `spawn extend` should do to spawn:cost-limit, and why.
type costLimitDecision struct {
	// Current is the cap on the instance now; 0 means none is set.
	Current float64
	// Needed is the cap required for the new deadline to actually be reached.
	Needed float64
	// NewValue is what to write; 0 means leave the tag alone.
	NewValue float64
	// Reason explains the decision in the user's terms.
	Reason string
}

// Changed reports whether the tag should be written.
func (d costLimitDecision) Changed() bool { return d.NewValue > 0 && d.NewValue != d.Current }

// decideCostLimit works out whether extending the TTL also requires raising the cost
// limit, and to what (spawn#639).
//
// The bug this fixes: `spawn extend` moved spawn:ttl and spawn:ttl-deadline and
// nothing else, while spored enforces the cost cap INDEPENDENTLY —
// "first-to-fire wins" (pkg/agent/agent.go). So for anyone who sized their cap to
// their TTL, which is what the cost guidance tells you to do, extend was a no-op by
// construction: the limit that was just left alone is always the tighter one. The
// instance died at almost exactly the old deadline and the natural reading was
// "extend didn't work", not "a second limit fired" — nothing in the output mentioned
// the cap.
//
// `needed` is the cap the new deadline requires, computed the way the agent's own
// arithmetic works (pkg/agent accumulatedComputeCost), assuming the instance runs
// continuously from now to the new deadline — the worst case, so the cap cannot
// fire first:
//
//	compute: spawn:price-per-hour x (compute so far + now..deadline)
//	EBS:     spawn:ebs-hourly-cost x (original launch..deadline)
//
// Compute so far is the cumulative spawn:compute-seconds spored keeps across every
// boot (#928), and EBS bills on wall clock from the ORIGINAL launch
// (spawn:launch-time), stopped time included. This used to be rate x (deadline -
// EC2 LaunchTime) — but EC2 resets LaunchTime on every start, so after a
// stop/start it left out the compute of every earlier boot and the raised cap
// could still fire before the new deadline: #639 again (#992). Both rates come
// from the instance's own tags, so this cannot disagree with what is enforced. The
// EBS rate is included because the cap counts EBS too (#616).
//
// It raises, never lowers: an extension is a decision to allow more spend, not less,
// and silently tightening someone's cap would be its own surprise.
func decideCostLimit(instance *aws.InstanceInfo, newDeadline, now time.Time, explicit float64, keep bool) costLimitDecision {
	d := costLimitDecision{Current: parseTagFloat(instance.Tags["spawn:cost-limit"])}

	if explicit > 0 {
		d.NewValue = explicit
		d.Reason = "set explicitly with --cost-limit"
		return d
	}
	if keep {
		d.Reason = "left unchanged (--keep-cost-limit)"
		return d
	}
	// No cap means nothing binds early; extending the TTL is the whole story.
	if d.Current <= 0 {
		return d
	}

	rate := parseTagFloat(instance.Tags["spawn:price-per-hour"])
	if rate <= 0 {
		// Without a rate we cannot compute what the new deadline costs. Say so rather
		// than guess — an invented cap is how #533's cap stopped being enforceable.
		d.Reason = "left unchanged: no spawn:price-per-hour tag, so the required cap cannot be computed"
		return d
	}
	ebsRate := parseTagFloat(instance.Tags["spawn:ebs-hourly-cost"])

	launch := originalLaunchTime(instance)
	if launch.IsZero() {
		d.Reason = "left unchanged: no launch time on the instance, so the required cap cannot be computed"
		return d
	}
	if !newDeadline.After(launch) {
		return d
	}
	remaining := newDeadline.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	compute := computeSoFar(instance, launch, now) + remaining
	d.Needed = rate*compute.Hours() + ebsRate*newDeadline.Sub(launch).Hours()

	if d.Current >= d.Needed {
		// The existing cap already reaches the new deadline; nothing to do.
		return d
	}
	d.NewValue = d.Needed
	d.Reason = fmt.Sprintf("raised: $%.4f would have stopped this instance before the new deadline, which was unreachable",
		d.Current)
	return d
}

// originalLaunchTime is when the instance was first launched, and the ONE
// place in cmd/ that reads EC2's LaunchTime for a duration: every age, TTL
// fallback and wall-clock cost figure is measured from it (#1029).
//
// It is pkg/ttl.LaunchAnchor — the earlier of the spawn:launch-time tag (written
// once at launch) and EC2's LaunchTime — which is the same rule the TTL reaper
// enforces with (GHSA-c93q-4jh7-6pvj), so what spawn shows is what is enforced.
// EC2's LaunchTime alone would be wrong: EC2 resets it on every start of a
// stopped instance (#992). TestNoDisplayDurationFromRawLaunchTime keeps every
// other cmd/ reader off the raw field.
func originalLaunchTime(instance *aws.InstanceInfo) time.Time {
	return ttlpkg.LaunchAnchor(instance.Tags["spawn:launch-time"], instance.LaunchTime)
}

// computeTagLag bounds how far spawn:compute-seconds can trail the agent's own
// running total: spored rewrites it every minute for the first ten minutes of
// a boot and every five minutes after (pkg/agent writeComputeSecondsTag), and
// the flush on shutdown can be lost.
const computeTagLag = 5 * time.Minute

// computeSoFar is the compute time the cost limit has already counted, across
// every boot: spawn:compute-seconds plus computeTagLag, never more than the
// wall-clock time since the original launch (compute cannot exceed it). With no
// usable tag it is that wall-clock bound — the most the instance can have run —
// so a raised cap errs high, never low.
func computeSoFar(instance *aws.InstanceInfo, launch, now time.Time) time.Duration {
	wall := now.Sub(launch)
	if wall < 0 {
		wall = 0
	}
	secs, ok := taggedComputeSeconds(instance)
	if !ok {
		return wall
	}
	if c := time.Duration(secs)*time.Second + computeTagLag; c < wall {
		return c
	}
	return wall
}

// taggedComputeSeconds is spawn:compute-seconds, spored's cumulative compute
// across every boot (#928), and whether the tag holds a usable value. It is the
// one reader of that tag for both `spawn extend`'s cost limit (#992) and the
// stopped-instance `spawn status`.
func taggedComputeSeconds(instance *aws.InstanceInfo) (int64, bool) {
	secs, err := strconv.ParseInt(strings.TrimSpace(instance.Tags["spawn:compute-seconds"]), 10, 64)
	if err != nil || secs < 0 {
		return 0, false
	}
	return secs, true
}

// renderCostLimitDecision is the line(s) `spawn extend` prints about the cap. Empty
// when there is nothing worth saying (no cap, and none requested).
func renderCostLimitDecision(d costLimitDecision) string {
	var b strings.Builder
	switch {
	case d.Changed():
		fmt.Fprintf(&b, "   Cost limit:   $%.4f → $%.4f\n", d.Current, d.NewValue)
		if d.Reason != "" {
			fmt.Fprintf(&b, "                 (%s)\n", d.Reason)
		}
	case d.Current > 0 && d.Reason != "":
		// A cap exists and we deliberately did not change it — say which, so a user
		// whose instance then dies early is not left guessing again.
		fmt.Fprintf(&b, "   Cost limit:   $%.4f, %s\n", d.Current, d.Reason)
	case d.Current > 0:
		fmt.Fprintf(&b, "   Cost limit:   $%.4f (already covers the new deadline)\n", d.Current)
	}
	return b.String()
}

// costLimitTagValue formats a cap for the spawn:cost-limit tag, matching the 4-decimal
// form spawn writes at launch so spored parses it identically.
func costLimitTagValue(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}
