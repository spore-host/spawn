// Spot request shape for RunInstances (#922).

package aws

import (
	"context"
	"log"
	"time"

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
		// ValidUntil (#950): EC2 gives a persistent request no end date of its
		// own, so a request orphaned by a terminate outside spawn would keep
		// launching replacements indefinitely. Bound it at the TTL deadline (or
		// the reaper's max-age without one), the value buildTags recorded.
		if vu, ok := spotValidUntilFromTags(tags); ok {
			opts.ValidUntil = aws.Time(vu)
		}
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

// spotValidUntilFromTags reads the end date buildTags recorded for a persistent
// request (spotrequest.ValidUntilTag), so the request and the instance tag carry
// the same instant.
func spotValidUntilFromTags(tags []types.Tag) (time.Time, bool) {
	for _, t := range tags {
		if aws.ToString(t.Key) == spotrequest.ValidUntilTag {
			v, err := time.Parse(time.RFC3339, aws.ToString(t.Value))
			return v, err == nil
		}
	}
	return time.Time{}, false
}

// tagOriginalInstance records, on a persistent Spot request, the instance spawn
// launched from it (spotrequest.OriginalInstanceTag, #950). The TTL reaper reads
// it to tell a replacement instance apart: one EC2 launched after the original
// was terminated outside spawn. The instance ID only exists after RunInstances,
// so this is a second call. It is best-effort. Without the tag the reaper still
// spots a replacement by its missing spawn:managed tag. It can't spot one that
// inherited the tags.
func tagOriginalInstance(ctx context.Context, api createTagsAPI, lc LaunchConfig, inst types.Instance) {
	reqID := aws.ToString(inst.SpotInstanceRequestId)
	if reqID == "" || !lc.Spot || !spotIsPersistent(lc) {
		return
	}
	if _, err := api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{reqID},
		Tags:      []types.Tag{{Key: aws.String(spotrequest.OriginalInstanceTag), Value: inst.InstanceId}},
	}); err != nil {
		log.Printf("warning: could not tag Spot request %s with its instance id: %v", reqID, err)
	}
}

// createTagsAPI is the slice of the EC2 client tagOriginalInstance uses.
type createTagsAPI interface {
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}
