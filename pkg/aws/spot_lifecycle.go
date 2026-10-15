// Spot request shape for RunInstances (#922).

package aws

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/spotrequest"
)

// spotIsPersistent reports whether this launch needs a persistent Spot request:
// its lifecycle asks EC2 to stop or hibernate the instance, which EC2 refuses for
// an instance from a one-time request ("You can only stop Spot Instances
// associated with persistent Spot Instance requests").
func spotIsPersistent(lc LaunchConfig) bool {
	return spotrequest.NeedsStoppable(lc.IdleTimeout, lc.OnComplete, lc.Hibernate, lc.HibernateOnIdle)
}

// spotRequestType is the spawn:spot-request-type value for a Spot launch.
func spotRequestType(lc LaunchConfig) string {
	if spotIsPersistent(lc) {
		return spotrequest.TypePersistent
	}
	return spotrequest.TypeOneTime
}

// applySpotOptions sets the RunInstances Spot market options for a Spot launch.
//
// Before #922 every --spot launch was a one-time request, whose interruption
// behaviour is terminate, so the idle timeout's default stop, --on-idle,
// --on-complete stop|hibernate, `spawn stop` and `spawn hibernate` could never
// work on one. A launch whose lifecycle stops or hibernates now gets a PERSISTENT
// request that is interrupted by stopping — or by hibernating, when the instance
// is configured for hibernation — so EC2 accepts the stop and a Spot reclaim
// parks the instance instead of destroying it.
//
// A persistent request outlives its instance: terminate the instance without
// cancelling it and EC2 launches a replacement. Every spawn terminate path
// cancels it first (pkg/spotrequest.Terminate), and the request carries the
// instance's tags, spawn:managed=true included, so it can be found and so the
// tag-conditioned ec2:CancelSpotInstanceRequests grants match it.
//
// A terminate-only lifecycle stays one-time and untagged, exactly as before.
func applySpotOptions(input *ec2.RunInstancesInput, lc LaunchConfig, tags []types.Tag) {
	// A Capacity Block replaces the market options below (the CLI rejects
	// --spot with --capacity-block; an SDK caller combining them got the block).
	if !lc.Spot || (lc.CapacityBlock && lc.ReservationID != "") {
		return
	}

	opts := &types.SpotMarketOptions{SpotInstanceType: types.SpotInstanceTypeOneTime}
	if spotIsPersistent(lc) {
		opts.SpotInstanceType = types.SpotInstanceTypePersistent
		opts.InstanceInterruptionBehavior = types.InstanceInterruptionBehaviorStop
		if lc.Hibernate {
			opts.InstanceInterruptionBehavior = types.InstanceInterruptionBehaviorHibernate
		}
		input.TagSpecifications = append(input.TagSpecifications, types.TagSpecification{
			ResourceType: types.ResourceTypeSpotInstancesRequest,
			Tags:         tags,
		})
	}
	if lc.SpotMaxPrice != "" {
		opts.MaxPrice = aws.String(lc.SpotMaxPrice)
	}

	input.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
		MarketType:  types.MarketTypeSpot,
		SpotOptions: opts,
	}
}
