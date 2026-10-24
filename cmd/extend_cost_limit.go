package cmd

import (
	"fmt"
	"math"
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
	// Binds is set when the cap is kept and will terminate the instance before
	// the new deadline. FiresAt is about when, assuming it runs continuously.
	Binds   bool
	FiresAt time.Time
	// InstanceID names the instance in the command the warning suggests, and
	// PerHour is its compute plus EBS rate, which prices that command's extra
	// minute.
	InstanceID string
	PerHour    float64
}

// Changed reports whether the tag should be written.
func (d costLimitDecision) Changed() bool { return d.NewValue > 0 && d.NewValue != d.Current }

// checkExtendCostLimitFlags rejects contradictory cost-limit flags.
// --keep-cost-limit is the default now and only conflicts with asking for a
// change.
func checkExtendCostLimitFlags(explicit float64, raise, keep bool) error {
	if explicit < 0 {
		return fmt.Errorf("--cost-limit must be positive, got %.2f", explicit)
	}
	if explicit > 0 && raise {
		return fmt.Errorf("--cost-limit and --raise-cost-limit both set the cost limit; pass one")
	}
	if keep && (explicit > 0 || raise) {
		return fmt.Errorf("--keep-cost-limit contradicts --cost-limit/--raise-cost-limit; pass one")
	}
	return nil
}

// decideCostLimit works out whether the instance's cost limit will stop it
// before the new deadline, and what, if anything, to write.
//
// The cost limit is a ceiling the user chose, and spored enforces it
// independently of the TTL ("first-to-fire wins", pkg/agent). Extending a TTL
// is not consent to spend past it, so by default the cap is KEPT (#1262). That
// reverses #639, which raised it automatically to whatever the new deadline
// needed: in #1262 eight c8gn.48xlarge launched with --cost-limit 25 were each
// extended twice, the cap was raised to about $46 each time without anyone
// asking, and they ran to about $29 each with no cost-limit termination. What
// #639 actually lacked was being told, so when the kept cap will fire before
// the new deadline the decision says so (Binds, FiresAt) and how to raise it.
// raise restores #639's raise for a user who asks.
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
// It never lowers: a cap that already covers the new deadline is left alone.
func decideCostLimit(instance *aws.InstanceInfo, newDeadline, now time.Time, explicit float64, raise bool) costLimitDecision {
	d := costLimitDecision{Current: parseTagFloat(instance.Tags["spawn:cost-limit"]), InstanceID: instance.InstanceID}

	if explicit > 0 {
		d.NewValue = explicit
		d.Reason = "set explicitly with --cost-limit"
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
		d.Reason = "left unchanged: no spawn:price-per-hour tag, so whether it stops this instance before the new deadline cannot be computed"
		return d
	}
	ebsRate := parseTagFloat(instance.Tags["spawn:ebs-hourly-cost"])

	launch := originalLaunchTime(instance)
	if launch.IsZero() {
		d.Reason = "left unchanged: no launch time on the instance, so whether it stops this instance before the new deadline cannot be computed"
		return d
	}
	if !newDeadline.After(launch) {
		return d
	}
	remaining := newDeadline.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	soFar := computeSoFar(instance, launch, now)
	d.Needed = rate*(soFar+remaining).Hours() + ebsRate*newDeadline.Sub(launch).Hours()

	if d.Current >= d.Needed {
		// The existing cap already reaches the new deadline; nothing to do.
		return d
	}
	if raise {
		d.NewValue = d.Needed
		d.Reason = fmt.Sprintf("raised with --raise-cost-limit: $%.4f would have terminated this instance before the new deadline", d.Current)
		return d
	}
	d.Binds = true
	d.PerHour = rate + ebsRate
	spent := rate*soFar.Hours() + ebsRate*now.Sub(launch).Hours()
	d.FiresAt = now
	if left := d.Current - spent; left > 0 {
		d.FiresAt = now.Add(time.Duration(left / (rate + ebsRate) * float64(time.Hour)))
	}
	d.Reason = "kept"
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
	case d.Binds:
		// The case #1262 and #639 are both about: say it loudly, with the time and
		// the way out, so an instance that dies early is never a mystery and a
		// cap is never moved without being asked.
		when := "now: it has already reached it"
		if d.FiresAt.After(time.Now()) {
			when = "at about " + d.FiresAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		fmt.Fprintf(&b, "⚠️  Cost limit:  $%.4f is kept, and spored will terminate this instance %s,\n", d.Current, when)
		fmt.Fprintf(&b, "                 before the new deadline. Running to the deadline needs about $%.2f.\n", d.Needed)
		// extend has no "change only the cap" form, so the suggestion extends by
		// one more minute; --cost-limit on the original command avoids that.
		fmt.Fprintf(&b, "                 To allow it: spawn extend %s 1m --cost-limit %.2f (or --raise-cost-limit),\n", d.InstanceID, math.Ceil((d.Needed+d.PerHour/60)*100)/100)
		fmt.Fprintf(&b, "                 or pass either flag with the extension next time.\n")
	case d.Current > 0 && d.Reason != "":
		// A cap exists and we could not tell whether it binds — say so, so a user
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
