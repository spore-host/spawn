package aws

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// SpotPriceTag records the spot market price spawn observed for an instance's
// availability zone, type and product at launch (#896). It is display-only:
// --cost-limit stays enforced against the on-demand spawn:price-per-hour, which
// can only overstate a spot instance's spend.
const SpotPriceTag = "spawn:spot-price-per-hour"

// PriceSourceTag records where spawn:price-per-hour came from: "live" or
// "static" (#1076). See LaunchConfig.PriceSource.
const PriceSourceTag = "spawn:price-source"

// SpotPrice is one spot market price observation for an instance.
type SpotPrice struct {
	PricePerHour       float64
	AvailabilityZone   string
	InstanceType       string
	ProductDescription string
	Timestamp          time.Time
}

// spotPriceAPI is the slice of EC2 a spot price lookup needs.
type spotPriceAPI interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeSpotPriceHistory(ctx context.Context, in *ec2.DescribeSpotPriceHistoryInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error)
}

// spotProductDescription maps an instance's billing platform to the spot price
// history product it is billed under. Only the four platforms spot prices are
// published for are mapped; anything else (Windows with SQL Server, BYOL, Ubuntu
// Pro, ...) is reported as unknown rather than priced as plain Linux, which would
// understate it.
func spotProductDescription(inst types.Instance) (string, bool) {
	switch aws.ToString(inst.PlatformDetails) {
	case "Linux/UNIX", "Red Hat Enterprise Linux", "SUSE Linux", "Windows":
		return aws.ToString(inst.PlatformDetails), true
	case "":
		if inst.Platform == types.PlatformValuesWindows {
			return "Windows", true
		}
		return "", false
	default:
		return "", false
	}
}

// spotPriceForInstance returns the current spot price for inst's zone, type and
// product. If the RunInstances response lacks the placement or platform it
// re-describes the instance once.
func spotPriceForInstance(ctx context.Context, api spotPriceAPI, inst types.Instance) (SpotPrice, error) {
	product, ok := spotProductDescription(inst)
	if !ok || inst.Placement == nil || aws.ToString(inst.Placement.AvailabilityZone) == "" {
		out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{aws.ToString(inst.InstanceId)}})
		if err != nil {
			return SpotPrice{}, fmt.Errorf("describe instance: %w", err)
		}
		if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
			return SpotPrice{}, fmt.Errorf("instance %s not found", aws.ToString(inst.InstanceId))
		}
		inst = out.Reservations[0].Instances[0]
		if product, ok = spotProductDescription(inst); !ok {
			return SpotPrice{}, fmt.Errorf("no spot price is published for platform %q", aws.ToString(inst.PlatformDetails))
		}
	}
	if inst.Placement == nil || aws.ToString(inst.Placement.AvailabilityZone) == "" {
		return SpotPrice{}, errors.New("instance has no availability zone yet")
	}
	return lookupSpotPrice(ctx, api, string(inst.InstanceType), aws.ToString(inst.Placement.AvailabilityZone), product)
}

// lookupSpotPrice returns the most recent spot price for one zone, type and
// product. A StartTime of now asks EC2 for the price in effect now.
func lookupSpotPrice(ctx context.Context, api spotPriceAPI, instanceType, az, product string) (SpotPrice, error) {
	out, err := api.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []types.InstanceType{types.InstanceType(instanceType)},
		ProductDescriptions: []string{product},
		AvailabilityZone:    aws.String(az),
		StartTime:           aws.Time(time.Now()),
	})
	if err != nil {
		return SpotPrice{}, fmt.Errorf("describe spot price history: %w", err)
	}
	var best SpotPrice
	for _, h := range out.SpotPriceHistory {
		if aws.ToString(h.AvailabilityZone) != az {
			continue
		}
		p, err := strconv.ParseFloat(aws.ToString(h.SpotPrice), 64)
		if err != nil || p <= 0 {
			continue
		}
		ts := aws.ToTime(h.Timestamp)
		if best.PricePerHour == 0 || ts.After(best.Timestamp) {
			best = SpotPrice{PricePerHour: p, AvailabilityZone: az, InstanceType: instanceType, ProductDescription: product, Timestamp: ts}
		}
	}
	if best.PricePerHour == 0 {
		return SpotPrice{}, fmt.Errorf("no spot price published for %s %s in %s", product, instanceType, az)
	}
	return best, nil
}

// CurrentSpotPrice returns the spot price now in effect for a running spot
// instance's zone, type and product. `spawn status` uses it for instances
// launched before spawn recorded SpotPriceTag. Needs ec2:DescribeInstances and
// ec2:DescribeSpotPriceHistory.
func (c *Client) CurrentSpotPrice(ctx context.Context, region, instanceID string) (SpotPrice, error) {
	return spotPriceForInstance(ctx, c.regionalEC2(region), types.Instance{InstanceId: aws.String(instanceID)})
}

// spotPriceTagAPI adds the tag write recordLaunchSpotPrice needs.
type spotPriceTagAPI interface {
	spotPriceAPI
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

// recordLaunchSpotPrice looks up a just-launched spot instance's price and
// records it in SpotPriceTag, so spored can cost the instance at what it
// actually pays rather than the on-demand rate (#896). Best-effort: a failure is
// logged and the launch proceeds — status then labels its figures as on-demand.
func recordLaunchSpotPrice(ctx context.Context, api spotPriceTagAPI, inst types.Instance) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sp, err := spotPriceForInstance(ctx, api, inst)
	if err != nil {
		log.Printf("note: could not record the spot price for %s (%v); status will show the on-demand rate for it",
			aws.ToString(inst.InstanceId), err)
		return
	}
	if _, err := api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{aws.ToString(inst.InstanceId)},
		Tags:      []types.Tag{{Key: aws.String(SpotPriceTag), Value: aws.String(strconv.FormatFloat(sp.PricePerHour, 'f', 6, 64))}},
	}); err != nil {
		log.Printf("note: could not tag the spot price for %s: %v", aws.ToString(inst.InstanceId), err)
	}
}
