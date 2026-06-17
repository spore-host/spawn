package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// CapacityBlockOffering is a purchasable EC2 Capacity Block for ML offering, as
// returned by DescribeCapacityBlockOfferings. The up-front fee is what the user
// is asked to confirm before PurchaseCapacityBlock charges it (#217).
type CapacityBlockOffering struct {
	OfferingID       string
	InstanceType     string
	InstanceCount    int32
	AvailabilityZone string
	StartDate        string // RFC3339, empty if unset
	EndDate          string // RFC3339, empty if unset
	DurationHours    int32
	UpfrontFee       string // total up-front price (AWS returns a string)
	CurrencyCode     string
	Tenancy          string
}

// FindCapacityBlockOffering looks up a single Capacity Block offering by id, so
// the purchase command can (re-)validate the exact terms and price immediately
// before charging (#217). It queries DescribeCapacityBlockOfferings with the
// given instance type / count / duration (the API's required filters) and returns
// the offering whose CapacityBlockOfferingId matches offeringID, or an error if
// no such offering is currently available (terms/price may have changed).
func (c *Client) FindCapacityBlockOffering(ctx context.Context, region, offeringID, instanceType string, instanceCount, durationHours int32) (*CapacityBlockOffering, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	if instanceCount <= 0 {
		instanceCount = 1
	}
	input := &ec2.DescribeCapacityBlockOfferingsInput{
		CapacityDurationHours: aws.Int32(durationHours),
		InstanceType:          aws.String(instanceType),
		InstanceCount:         aws.Int32(instanceCount),
	}

	paginator := ec2.NewDescribeCapacityBlockOfferingsPaginator(ec2Client, input)
	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe capacity block offerings: %w", err)
		}
		for _, o := range out.CapacityBlockOfferings {
			if aws.ToString(o.CapacityBlockOfferingId) != offeringID {
				continue
			}
			off := &CapacityBlockOffering{
				OfferingID:       aws.ToString(o.CapacityBlockOfferingId),
				InstanceType:     aws.ToString(o.InstanceType),
				InstanceCount:    aws.ToInt32(o.InstanceCount),
				AvailabilityZone: aws.ToString(o.AvailabilityZone),
				DurationHours:    aws.ToInt32(o.CapacityBlockDurationHours),
				UpfrontFee:       aws.ToString(o.UpfrontFee),
				CurrencyCode:     aws.ToString(o.CurrencyCode),
				Tenancy:          string(o.Tenancy),
			}
			if o.StartDate != nil {
				off.StartDate = o.StartDate.UTC().Format("2006-01-02T15:04:05Z07:00")
			}
			if o.EndDate != nil {
				off.EndDate = o.EndDate.UTC().Format("2006-01-02T15:04:05Z07:00")
			}
			return off, nil
		}
	}
	return nil, fmt.Errorf("capacity block offering %q is no longer available for %s ×%d (%dh) in %s — its terms or price may have changed", offeringID, instanceType, instanceCount, durationHours, region)
}

// PurchaseCapacityBlock purchases a Capacity Block for ML from an offering id —
// a paid, NON-REFUNDABLE write operation (#217). The platform defaults to
// Linux/UNIX when empty. Returns the resulting Capacity Reservation id, which
// feeds `spawn launch --reservation-id <id> --capacity-block`. Callers MUST gate
// this behind the typed-confirmation flow; this method performs no confirmation
// itself. When dryRun is true it relies on the API's DryRun (no charge, returns
// a DryRunOperation error on success).
func (c *Client) PurchaseCapacityBlock(ctx context.Context, region, offeringID, platform string, dryRun bool, tags map[string]string) (string, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	plat := types.CapacityReservationInstancePlatform(platform)
	if platform == "" {
		plat = types.CapacityReservationInstancePlatformLinuxUnix
	}

	input := &ec2.PurchaseCapacityBlockInput{
		CapacityBlockOfferingId: aws.String(offeringID),
		InstancePlatform:        plat,
	}
	if dryRun {
		input.DryRun = aws.Bool(true)
	}
	if len(tags) > 0 {
		ts := make([]types.Tag, 0, len(tags))
		for k, v := range tags {
			ts = append(ts, types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		input.TagSpecifications = []types.TagSpecification{{
			ResourceType: types.ResourceTypeCapacityReservation,
			Tags:         ts,
		}}
	}

	out, err := ec2Client.PurchaseCapacityBlock(ctx, input)
	if err != nil {
		return "", fmt.Errorf("purchase capacity block: %w", err)
	}
	if out.CapacityReservation == nil || out.CapacityReservation.CapacityReservationId == nil {
		return "", fmt.Errorf("purchase capacity block: no reservation id returned")
	}
	return aws.ToString(out.CapacityReservation.CapacityReservationId), nil
}
