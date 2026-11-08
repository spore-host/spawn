package cmd

import (
	"fmt"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/costmodel"
)

// enforcedFootprint is what spored enforces --cost-limit with, read from the
// tags it keeps current: the on-demand rate (spawn:price-per-hour), or for a
// Spot instance the Spot compute accrual spored records (spawn:spot-accrual,
// #1497), every
// attached EBS volume and every public IPv4 address (#51), in spawn:cost-footprint
// or, from a spored before #1428, the three tags it replaced
// (costmodel.MeasuredFromTags).
func enforcedFootprint(instance *aws.InstanceInfo) costmodel.Footprint {
	if instance == nil {
		return costmodel.Footprint{}
	}
	return costmodel.FromTags(instance.Tags, "spawn")
}

// footprintAt is enforcedFootprint with the compute rate replaced by perHour,
// for a display that prices a spot instance at its spot price. Every other
// component is the enforced one.
func footprintAt(instance *aws.InstanceInfo, perHour float64) costmodel.Footprint {
	f := enforcedFootprint(instance)
	f.ComputePerHour, f.Accrual = perHour, nil
	return f
}

// costLimitBasisLabel says which compute rate --cost-limit counts for
// instance, as spored records it (#1497). A fallback is labelled with why and
// when its rate was collected (truffle#184).
func costLimitBasisLabel(instance *aws.InstanceInfo) string {
	f := enforcedFootprint(instance)
	if f.Accrual == nil {
		if instance != nil && instance.SpotInstance {
			return "compute at the on-demand rate: this instance's spored records no Spot price " +
				"(it predates spawn#1497, or has not read one yet)"
		}
		return "compute at the on-demand rate"
	}
	when := "time unknown"
	if !f.Accrual.SampledAt.IsZero() {
		when = f.Accrual.SampledAt.UTC().Format(time.RFC3339)
	}
	if f.Accrual.Basis == costmodel.BasisSpot {
		return fmt.Sprintf("compute at the Spot price spored samples, last $%.4f/hr as of %s", f.Accrual.PerHour, when)
	}
	src := "its price source was not recorded"
	switch instance.Tags[aws.PriceSourceTag] {
	case "live":
		src = "read live from the AWS Price List at launch"
	case "static":
		// The instance records only that its rate was static, not when that
		// table was collected or why the live Price List was not used
		// (spawn#1512), so the label says both are unknown.
		src = aws.DisplayPrice{Source: "static"}.SourceLabel() + " (the instance records only that its rate was static)"
	}
	return fmt.Sprintf("compute at the on-demand rate $%.4f/hr, a FALLBACK: spored could not read the Spot price "+
		"(since %s); that rate is spawn:price-per-hour, %s", f.Accrual.PerHour, when, src)
}

// costLimitSpend is the CLI's view of the spend --cost-limit has counted:
// pkg/costmodel.Accrue — the function spored's enforcement loop terminates on
// (pkg/agent accumulatedComputeCost) — over the instance's enforced footprint.
// `spawn extend`'s decideCostLimit, `spawn status` and `spawn cost` all go
// through here or footprintAt+Accrue, so a component cannot be shown and not
// enforced or the reverse (#51).
func costLimitSpend(instance *aws.InstanceInfo, compute, wall time.Duration) costmodel.Spend {
	return costmodel.Accrue(enforcedFootprint(instance), compute, wall)
}

// accrualWindow is where a display's wall-clock cost window ends (#1341).
//
// EBS and Elastic IPs bill on the wall clock, so a display multiplies their
// rates by "time since the original launch". That is right while an instance
// exists, stopped included, and wrong once it is gone: spawn's volumes are
// DeleteOnTermination and its launch Elastic IP is released, so nothing bills
// after termination, and a window that keeps running to now made `spawn cost`
// report more storage spend every time it was run.
type accrualWindow struct {
	// End is when wall-clock billing stopped: now for an instance that still
	// exists, the termination time for one that does not. Zero when !Known.
	End time.Time
	// Ended reports that the instance is terminated or shutting down.
	Ended bool
	// Known is false for an ended instance whose termination time cannot be
	// found. A display must then not accrue wall-clock cost at all, and say so,
	// rather than run the window to now.
	Known bool
}

// accrualEnd resolves the window end for instance at now.
//
// The termination time comes from EC2's StateTransitionReason, "User initiated
// (2026-10-08 16:48:02 GMT)" (parseTransitionTime). It is the only source: no
// spawn tag records termination (spored dies with the instance, and `spawn
// terminate` writes none), and EC2 keeps a terminated instance, with that
// reason, for only about an hour. A shutting-down instance carries the time
// termination began, which bounds it the same way.
//
// Only the CLI displays use this. spored's enforcement accrues against its own
// clock while it runs, and it does not run once the instance is gone.
func accrualEnd(instance *aws.InstanceInfo, now time.Time) accrualWindow {
	if instance == nil || (instance.State != "terminated" && instance.State != "shutting-down") {
		return accrualWindow{End: now, Known: true}
	}
	t := parseTransitionTime(instance.StateTransitionReason)
	if t.IsZero() {
		return accrualWindow{Ended: true}
	}
	if t.After(now) {
		t = now
	}
	return accrualWindow{End: t, Ended: true, Known: true}
}

// accrualUnknownReason is the wording every display uses for !Known.
const accrualUnknownReason = "terminated, and EC2 no longer reports when " +
	"(it clears StateTransitionReason about an hour after termination)"
