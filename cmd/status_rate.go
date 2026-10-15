package cmd

import (
	"context"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// statusRate is the compute rate `spawn status` charges an instance at, and
// what it is.
type statusRate struct {
	PerHour float64
	// Basis is a human label: "on-demand", "spot price at launch",
	// "current spot price", or "on-demand; spot price unknown".
	Basis string
	// SpotPriced is true when PerHour is a spot price.
	SpotPriced bool
	// OnDemand is the on-demand rate (spawn:price-per-hour), the baseline.
	OnDemand float64
}

const (
	rateBasisOnDemand    = "on-demand"
	rateBasisSpotLaunch  = "spot price at launch"
	rateBasisSpotCurrent = "current spot price"
	rateBasisSpotUnknown = "on-demand; spot price unknown"
)

// spotPriceLookup returns the spot price now in effect for a spot instance.
type spotPriceLookup func(ctx context.Context, region, instanceID string) (float64, error)

// resolveStatusRate picks the compute rate for `spawn status`'s own lines (max
// cost, billable resources). A spot instance used to be labelled "(spot)" and
// costed at the on-demand rate — about 2.7x high on the reported m8a.4xlarge
// (#896). Order: the spot price spawn recorded at launch (spawn:spot-price-per-
// hour); else, for an instance launched before spawn recorded it, the price now
// in effect (lookup, may be nil); else the on-demand rate, labelled as such.
func resolveStatusRate(ctx context.Context, instance *aws.InstanceInfo, lookup spotPriceLookup) statusRate {
	if instance == nil {
		return statusRate{}
	}
	r := statusRate{OnDemand: pricePerHour(instance)}
	if !instance.SpotInstance {
		r.PerHour, r.Basis = r.OnDemand, rateBasisOnDemand
		return r
	}
	if p := parseTagFloat(instance.Tags[aws.SpotPriceTag]); p > 0 {
		return statusRate{PerHour: p, Basis: rateBasisSpotLaunch, SpotPriced: true, OnDemand: r.OnDemand}
	}
	if lookup != nil && (instance.State == "running" || instance.State == "pending") {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if p, err := lookup(lctx, instance.Region, instance.InstanceID); err == nil && p > 0 {
			return statusRate{PerHour: p, Basis: rateBasisSpotCurrent, SpotPriced: true, OnDemand: r.OnDemand}
		}
	}
	r.PerHour, r.Basis = r.OnDemand, rateBasisSpotUnknown
	return r
}

// tagStatusRate resolves the rate from the instance's tags alone (no AWS call).
func tagStatusRate(instance *aws.InstanceInfo) statusRate {
	return resolveStatusRate(context.Background(), instance, nil)
}

// clientSpotPriceLookup adapts aws.Client.CurrentSpotPrice.
func clientSpotPriceLookup(client *aws.Client) spotPriceLookup {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, region, instanceID string) (float64, error) {
		sp, err := client.CurrentSpotPrice(ctx, region, instanceID)
		if err != nil {
			return 0, err
		}
		return sp.PricePerHour, nil
	}
}

// rateNote is the parenthetical a compute row carries to say what its rate is.
func (r statusRate) rateNote() string {
	if r.PerHour <= 0 {
		return ""
	}
	if r.Basis == rateBasisSpotUnknown {
		return "on-demand rate shown; spot price unknown, so this overstates"
	}
	return r.Basis
}
