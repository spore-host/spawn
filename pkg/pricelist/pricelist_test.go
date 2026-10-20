package pricelist

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

func TestParseItem(t *testing.T) {
	cases := []struct {
		name, item string
		wantType   string
		wantPrice  float64
		wantOK     bool
	}{
		{"priced", `{"product":{"attributes":{"instanceType":"m7i.large"}},"terms":{"OnDemand":{"a":{"priceDimensions":{"b":{"pricePerUnit":{"USD":"0.1008"}}}}}}}`, "m7i.large", 0.1008, true},
		{"zero placeholder", `{"product":{"attributes":{"instanceType":"m7i.large"}},"terms":{"OnDemand":{"a":{"priceDimensions":{"b":{"pricePerUnit":{"USD":"0.0000000000"}}}}}}}`, "", 0, false},
		{"no type", `{"terms":{"OnDemand":{"a":{"priceDimensions":{"b":{"pricePerUnit":{"USD":"0.1"}}}}}}}`, "", 0, false},
		{"garbage", `not json`, "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it, p, ok := parseItem(tc.item)
			if it != tc.wantType || p != tc.wantPrice || ok != tc.wantOK {
				t.Errorf("parseItem = (%q, %v, %v), want (%q, %v, %v)", it, p, ok, tc.wantType, tc.wantPrice, tc.wantOK)
			}
		})
	}
}

// endlessPages always returns one item and another NextToken.
type endlessPages struct{ calls int }

func (e *endlessPages) GetProducts(_ context.Context, in *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	e.calls++
	next := "more"
	return &pricing.GetProductsOutput{
		PriceList: []string{`{"product":{"attributes":{"instanceType":"m7i.large"}},"terms":{"OnDemand":{"a":{"priceDimensions":{"b":{"pricePerUnit":{"USD":"0.1"}}}}}}}`},
		NextToken: &next,
	}, nil
}

// TestRegionOnDemandPricesStopsARunawayPaginator: a paginator that never ends
// is cut off at maxPages with an error, and what was read is still returned.
func TestRegionOnDemandPricesStopsARunawayPaginator(t *testing.T) {
	api := &endlessPages{}
	got, requests, err := RegionOnDemandPrices(context.Background(), api, "us-east-1")
	if err == nil {
		t.Fatal("want an error from a paginator that never ends")
	}
	if requests != maxPages || api.calls != maxPages {
		t.Errorf("requests = %d, calls = %d, want %d", requests, api.calls, maxPages)
	}
	if got["m7i.large"] != 0.1 {
		t.Errorf("partial table lost: %v", got)
	}
}
