package aws

import (
	"context"
	"encoding/json"
	"log"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	awspricing "github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// regionToLocationName maps AWS region codes to the location name used by the Pricing API.
var regionToLocationName = map[string]string{
	"us-east-1":      "US East (N. Virginia)",
	"us-east-2":      "US East (Ohio)",
	"us-west-1":      "US West (N. California)",
	"us-west-2":      "US West (Oregon)",
	"eu-west-1":      "Europe (Ireland)",
	"eu-west-2":      "Europe (London)",
	"eu-west-3":      "Europe (Paris)",
	"eu-central-1":   "Europe (Frankfurt)",
	"eu-north-1":     "Europe (Stockholm)",
	"eu-south-1":     "Europe (Milan)",
	"ap-northeast-1": "Asia Pacific (Tokyo)",
	"ap-northeast-2": "Asia Pacific (Seoul)",
	"ap-northeast-3": "Asia Pacific (Osaka)",
	"ap-southeast-1": "Asia Pacific (Singapore)",
	"ap-southeast-2": "Asia Pacific (Sydney)",
	"ap-south-1":     "Asia Pacific (Mumbai)",
	"ap-east-1":      "Asia Pacific (Hong Kong)",
	"ca-central-1":   "Canada (Central)",
	"sa-east-1":      "South America (Sao Paulo)",
	"me-south-1":     "Middle East (Bahrain)",
	"af-south-1":     "Africa (Cape Town)",
}

// LookupEC2OnDemandPrice queries the AWS Pricing API for the current on-demand price
// of an instance type in a region. Returns 0 and logs if the lookup fails.
// The Pricing API is only available in us-east-1 and ap-south-1.
func LookupEC2OnDemandPrice(ctx context.Context, region, instanceType string) float64 {
	location, ok := regionToLocationName[region]
	if !ok {
		log.Printf("pricing: unknown region %q, cannot look up price", region)
		return 0
	}

	// Pricing API is only in us-east-1 and ap-south-1 regardless of where the instance is
	pricingCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		log.Printf("pricing: failed to load config: %v", err)
		return 0
	}

	pricingClient := awspricing.NewFromConfig(pricingCfg)
	out, err := pricingClient.GetProducts(ctx, &awspricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []pricingtypes.Filter{
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("instanceType"), Value: aws.String(instanceType)},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("location"), Value: aws.String(location)},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("operatingSystem"), Value: aws.String("Linux")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("tenancy"), Value: aws.String("Shared")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("preInstalledSw"), Value: aws.String("NA")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("capacitystatus"), Value: aws.String("Used")},
		},
		MaxResults: aws.Int32(1),
	})
	if err != nil {
		log.Printf("pricing: GetProducts failed for %s in %s: %v", instanceType, region, err)
		return 0
	}
	if len(out.PriceList) == 0 {
		log.Printf("pricing: no price found for %s in %s", instanceType, region)
		return 0
	}

	// Parse the nested pricing JSON: terms → OnDemand → priceDimensions → pricePerUnit USD
	var priceDoc struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(out.PriceList[0]), &priceDoc); err != nil {
		log.Printf("pricing: parse error: %v", err)
		return 0
	}
	for _, term := range priceDoc.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			if usd, ok := dim.PricePerUnit["USD"]; ok {
				if price, err := strconv.ParseFloat(usd, 64); err == nil && price > 0 {
					return price
				}
			}
		}
	}
	return 0
}
