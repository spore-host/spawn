package cmd

import (
	"time"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/costmodel"
)

// enforcedFootprint is what spored enforces --cost-limit with, read from the
// tags it keeps current: the on-demand rate (spawn:price-per-hour), every
// attached EBS volume (spawn:ebs-hourly-cost) and every public IPv4 address
// (spawn:public-ipv4-count, spawn:elastic-ipv4-count) (#51).
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
	f.ComputePerHour = perHour
	return f
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
