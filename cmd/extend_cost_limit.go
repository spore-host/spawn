package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
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
// arithmetic works: rate x hours from LAUNCH to the new deadline. It includes the EBS
// rate because the cap counts EBS too (#616) and because a cap that only covers
// compute would bind early again — the same bug one layer down. Both rates come from
// the instance's own tags (spawn:price-per-hour, spawn:ebs-hourly-cost), so this
// cannot disagree with what is actually enforced.
//
// It raises, never lowers: an extension is a decision to allow more spend, not less,
// and silently tightening someone's cap would be its own surprise.
func decideCostLimit(instance *aws.InstanceInfo, newDeadline time.Time, explicit float64, keep bool) costLimitDecision {
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
	rate += parseTagFloat(instance.Tags["spawn:ebs-hourly-cost"])

	if instance.LaunchTime.IsZero() {
		d.Reason = "left unchanged: no launch time on the instance, so the required cap cannot be computed"
		return d
	}
	hours := newDeadline.Sub(instance.LaunchTime).Hours()
	if hours <= 0 {
		return d
	}
	d.Needed = rate * hours

	if d.Current >= d.Needed {
		// The existing cap already reaches the new deadline; nothing to do.
		return d
	}
	d.NewValue = d.Needed
	d.Reason = fmt.Sprintf("raised: $%.4f capped this instance at %s, so the new deadline was unreachable",
		d.Current, formatDuration(time.Duration(d.Current/rate*float64(time.Hour))))
	return d
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
