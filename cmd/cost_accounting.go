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
