// Package pricelist fetches a whole region's EC2 On-Demand rate table from the
// AWS Price List API in one paginated query, for callers that rank many
// instance types at once (spawn task sizing, spawn#1181).
package pricelist

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// Sizing prices every candidate it ranks, and a `cpu: 2` request matches more
// than a thousand types in us-east-1. Pricing them one Price List request at a
// time is what made `spawn task run` spend 149-256 s before RunInstances
// (spawn#1181, measured in experiment E1, spawn#1171): 1325 serial GetProducts
// calls through truffle's per-type pricer, each one retried with backoff when the
// Price List API throttles (truffle#182).
//
// So sizing fetches the region's whole Linux On-Demand table instead: one
// paginated GetProducts query filtered by region but not by instance type. Its
// cost is ceil(types offered in the region / 100) requests, which depends on the
// region's catalogue and not on how many candidates the spec matched. The
// filters are the ones truffle's per-type lookup uses (pkg/aws/pricing_batch.go
// onDemandFilters at truffle v0.59.0, minus instanceType), so each rate is the
// one a per-type request would have returned.
//
// This belongs in truffle, as a region-wide sibling of Client.OnDemandPrices;
// until truffle has one, spawn carries this copy.

// API is the Price List call RegionOnDemandPrices makes; *pricing.Client
// satisfies it, and tests inject a fake.
type API = pricing.GetProductsAPIClient

// PageSize is the GetProducts page size; 100 is the API maximum.
const PageSize = 100

// maxPages bounds one region's table fetch. us-east-1 offers
// roughly 1,000-1,400 types, i.e. 10-14 pages, so 40 leaves room for growth
// while still stopping a paginator that never ends. A truncated table is not an
// error the caller must fail on: the types it lacks fall through to the static
// table, exactly as an unpriced type does.
const maxPages = 40

// maxAttempts mirrors truffle's pricingMaxAttempts (truffle#175):
// the Price List API throttles readily, and a retried page is far cheaper than a
// launch ranked without prices.
const maxAttempts = 8

// NewClient builds a Price List client with a throttling-tolerant retryer. The API is
// served only from us-east-1 and ap-south-1, and us-east-1 prices every region.
func NewClient(cfg awssdk.Config) *pricing.Client {
	if cfg.Region != "us-east-1" && cfg.Region != "ap-south-1" {
		cfg.Region = "us-east-1"
	}
	cfg.Retryer = func() awssdk.Retryer {
		return retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = maxAttempts
		})
	}
	return pricing.NewFromConfig(cfg)
}

// regionOnDemandFilters selects the Linux, shared-tenancy, no-preinstalled-
// software On-Demand rate of every instance type in region.
func regionOnDemandFilters(region string) []pricingtypes.Filter {
	term := func(field, value string) pricingtypes.Filter {
		return pricingtypes.Filter{
			Type:  pricingtypes.FilterTypeTermMatch,
			Field: awssdk.String(field),
			Value: awssdk.String(value),
		}
	}
	return []pricingtypes.Filter{
		term("regionCode", region),
		term("operatingSystem", "Linux"),
		term("tenancy", "Shared"),
		term("preInstalledSw", "NA"),
		term("capacitystatus", "Used"),
		term("marketoption", "OnDemand"),
	}
}

// RegionOnDemandPrices returns the live On-Demand $/hr of every instance type
// the Price List publishes for region, and how many GetProducts requests that
// took. On an error partway through it returns what it had read so far along
// with the error.
func RegionOnDemandPrices(ctx context.Context, api API, region string) (map[string]float64, int, error) {
	pager := pricing.NewGetProductsPaginator(api, &pricing.GetProductsInput{
		ServiceCode:   awssdk.String("AmazonEC2"),
		FormatVersion: awssdk.String("aws_v1"),
		Filters:       regionOnDemandFilters(region),
		MaxResults:    awssdk.Int32(PageSize),
	})
	out := make(map[string]float64)
	requests := 0
	for pager.HasMorePages() {
		if requests >= maxPages {
			return out, requests, fmt.Errorf("price list for %s: stopped after %d pages", region, requests)
		}
		requests++
		page, err := pager.NextPage(ctx)
		if err != nil {
			return out, requests, fmt.Errorf("price list GetProducts for %s: %w", region, err)
		}
		for _, item := range page.PriceList {
			instanceType, price, ok := parseItem(item)
			if !ok {
				continue
			}
			// First rate wins, as in truffle's per-type fetch, which returns the
			// first parseable item of its response.
			if _, seen := out[instanceType]; !seen {
				out[instanceType] = price
			}
		}
	}
	return out, requests, nil
}

// parseItem extracts the instance type and the USD hourly On-Demand
// rate from one Price List product document:
//
//	product.attributes.instanceType
//	terms.OnDemand.<offerCode>.priceDimensions.<rateCode>.pricePerUnit.USD
//
// $0.00 placeholders, which AWS sometimes emits, are skipped.
func parseItem(item string) (string, float64, bool) {
	var doc struct {
		Product struct {
			Attributes struct {
				InstanceType string `json:"instanceType"`
			} `json:"attributes"`
		} `json:"product"`
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(item), &doc); err != nil {
		return "", 0, false
	}
	instanceType := doc.Product.Attributes.InstanceType
	if instanceType == "" {
		return "", 0, false
	}
	for _, offer := range doc.Terms.OnDemand {
		for _, dim := range offer.PriceDimensions {
			usd, ok := dim.PricePerUnit["USD"]
			if !ok {
				continue
			}
			price, err := strconv.ParseFloat(usd, 64)
			if err != nil || price <= 0 {
				continue
			}
			return instanceType, price, true
		}
	}
	return "", 0, false
}
