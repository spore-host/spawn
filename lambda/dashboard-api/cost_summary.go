package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2Types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Cost summary for the dashboard.
//
// Moved here when #772 removed autoscale: these symbols shared a file with the
// autoscale handlers but have nothing to do with autoscale, and deleting that
// file wholesale silently removed a live route (/api/cost-summary). The build
// caught it.
//
// Moved AS-IS on purpose. The price table is a real problem — see its note — but
// changing cost arithmetic inside a removal commit would mix an unreviewable
// behaviour change into a mechanical one.

// instancePricing is a HARDCODED hourly on-demand price table, us-east-1 only,
// with no record of when it was collected.
//
// It is the source for /api/cost-summary's "EstimatedMonthlyCost", and it
// violates the project rule that a static price reaching output must state that
// it is static, why, and when it was collected. Three specific problems:
//
//   - no collection date, so staleness is unknowable to the reader;
//   - us-east-1 rates applied to instances in every region the scan covers;
//   - an unknown instance type silently defaults to $0.10/hr, a FABRICATED
//     price. truffle#114 established that a fabricated price is worse than no
//     price because nothing downstream can tell the difference — and this one is
//     then multiplied by 730 and displayed as a monthly figure.
//
// truffle already solves this: libs/pricing carries a dated table (PricesAsOf)
// behind the live AWS Price List API with a documented fallback. This should
// delegate to truffle, as spawn#533 did when it moved on-demand pricing off a
// fabricated rate. Tracked in truffle#184.
var instancePricing = map[string]float64{
	"t3.micro":    0.0104,
	"t3.small":    0.0208,
	"t3.medium":   0.0416,
	"t3.large":    0.0832,
	"t3.xlarge":   0.1664,
	"t3.2xlarge":  0.3328,
	"t3a.micro":   0.0094,
	"t3a.small":   0.0188,
	"t3a.medium":  0.0376,
	"t3a.large":   0.0752,
	"t3a.xlarge":  0.1504,
	"t3a.2xlarge": 0.3008,
	"m5.large":    0.096,
	"m5.xlarge":   0.192,
	"m5.2xlarge":  0.384,
	"m5.4xlarge":  0.768,
	"c5.large":    0.085,
	"c5.xlarge":   0.17,
	"c5.2xlarge":  0.34,
	"c5.4xlarge":  0.68,
	"r5.large":    0.126,
	"r5.xlarge":   0.252,
	"r5.2xlarge":  0.504,
}

// handleGetCostSummary handles GET /api/cost-summary.
func handleGetCostSummary(ctx context.Context, cfg aws.Config, cliIamArn string) (events.APIGatewayProxyResponse, error) {
	cost, err := calculateInstanceCosts(ctx, cfg, cliIamArn)
	if err != nil {
		return errorResponse(500, fmt.Sprintf("Failed to calculate costs: %v", err)), nil
	}

	return successResponse(CostSummaryAPIResponse{Success: true, Cost: *cost})
}

func calculateInstanceCosts(ctx context.Context, cfg aws.Config, cliIamArn string) (*CostSummary, error) {
	breakdown := make(map[string]TypeCost)
	totalHourly := 0.0
	totalCount := 0

	for _, region := range awsRegions {
		ec2Client, err := getEC2ClientForRegion(ctx, cfg, region)
		if err != nil {
			continue
		}

		filters := []ec2Types.Filter{
			{
				Name:   aws.String("tag:spawn:iam-user"),
				Values: []string{cliIamArn},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running"},
			},
		}

		result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: filters,
		})
		if err != nil {
			continue
		}

		for _, reservation := range result.Reservations {
			for _, instance := range reservation.Instances {
				instanceType := string(instance.InstanceType)
				isSpot := instance.InstanceLifecycle == ec2Types.InstanceLifecycleTypeSpot

				// Get base price
				price, exists := instancePricing[instanceType]
				if !exists {
					// Default estimate for unknown types
					price = 0.10
				}

				// Apply spot discount (70% savings estimate)
				if isSpot {
					price = price * 0.30
				}

				// Update breakdown
				entry := breakdown[instanceType]
				entry.Count++
				entry.HourlyCost += price
				breakdown[instanceType] = entry

				totalHourly += price
				totalCount++
			}
		}
	}

	// Calculate monthly (730 hours/month average)
	totalMonthly := totalHourly * 730

	return &CostSummary{
		TotalHourlyCost:      totalHourly,
		EstimatedMonthlyCost: totalMonthly,
		InstanceCount:        totalCount,
		BreakdownByType:      breakdown,
	}, nil
}
