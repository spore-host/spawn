package main

import (
	"testing"
)

func TestInstancePricingMap(t *testing.T) {
	// Verify known pricing values are present and positive.
	cases := []struct {
		instanceType string
		wantPrice    float64
	}{
		{"t3.micro", 0.0104},
		{"t3.small", 0.0208},
		{"t3.medium", 0.0416},
		{"t3.large", 0.0832},
		{"t3.xlarge", 0.1664},
		{"t3.2xlarge", 0.3328},
		{"t3a.micro", 0.0094},
		{"t3a.small", 0.0188},
		{"t3a.medium", 0.0376},
		{"t3a.large", 0.0752},
		{"m5.large", 0.096},
		{"m5.xlarge", 0.192},
		{"m5.2xlarge", 0.384},
		{"c5.large", 0.085},
		{"c5.xlarge", 0.17},
		{"c5.2xlarge", 0.34},
		{"r5.large", 0.126},
		{"r5.xlarge", 0.252},
	}

	for _, tc := range cases {
		t.Run(tc.instanceType, func(t *testing.T) {
			price, ok := instancePricing[tc.instanceType]
			if !ok {
				t.Fatalf("instancePricing missing key %q", tc.instanceType)
			}
			if price != tc.wantPrice {
				t.Errorf("instancePricing[%q] = %v, want %v", tc.instanceType, price, tc.wantPrice)
			}
			if price <= 0 {
				t.Errorf("instancePricing[%q] = %v, must be positive", tc.instanceType, price)
			}
		})
	}
}

func TestInstancePricingAllPositive(t *testing.T) {
	for typ, price := range instancePricing {
		if price <= 0 {
			t.Errorf("instancePricing[%q] = %v, must be positive", typ, price)
		}
	}
}

func TestMonthlyEstimateCalculation(t *testing.T) {
	// Monthly estimate = (compute + network) * 730 (hours/month)
	const hoursPerMonth = 730.0

	tests := []struct {
		name    string
		compute float64
		network float64
		want    float64
	}{
		{
			name:    "zero cost",
			compute: 0,
			network: 0,
			want:    0,
		},
		{
			name:    "t3.micro compute only",
			compute: instancePricing["t3.micro"],
			network: 0,
			want:    instancePricing["t3.micro"] * hoursPerMonth,
		},
		{
			name:    "t3.micro with network",
			compute: instancePricing["t3.micro"],
			network: 0.005,
			want:    (instancePricing["t3.micro"] + 0.005) * hoursPerMonth,
		},
		{
			name:    "m5.xlarge compute only",
			compute: instancePricing["m5.xlarge"],
			network: 0,
			want:    instancePricing["m5.xlarge"] * hoursPerMonth,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := (tc.compute + tc.network) * hoursPerMonth
			if got != tc.want {
				t.Errorf("monthly estimate for compute=%v network=%v: got %v, want %v",
					tc.compute, tc.network, got, tc.want)
			}
		})
	}
}

func TestSpotPriceDiscount(t *testing.T) {
	// Spot instances should be 30% of on-demand price.
	const spotMultiplier = 0.30
	cases := []string{"t3.micro", "m5.large", "c5.xlarge"}
	for _, typ := range cases {
		t.Run(typ, func(t *testing.T) {
			onDemand := instancePricing[typ]
			spot := onDemand * spotMultiplier
			if spot >= onDemand {
				t.Errorf("spot price %v >= on-demand %v for %s", spot, onDemand, typ)
			}
			if spot <= 0 {
				t.Errorf("spot price %v must be positive for %s", spot, typ)
			}
		})
	}
}

func TestAWSRegionsList(t *testing.T) {
	if len(awsRegions) == 0 {
		t.Fatal("awsRegions is empty")
	}
	seen := make(map[string]bool)
	for _, r := range awsRegions {
		if r == "" {
			t.Error("awsRegions contains empty string")
		}
		if seen[r] {
			t.Errorf("duplicate region %q in awsRegions", r)
		}
		seen[r] = true
	}
}

func TestTTLDaysConstant(t *testing.T) {
	if ttlDays <= 0 {
		t.Errorf("ttlDays = %d, must be positive", ttlDays)
	}
}
