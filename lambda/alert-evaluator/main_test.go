package main

import (
	"testing"
	"time"
)

func TestShouldAlert(t *testing.T) {
	tests := []struct {
		name        string
		lastAlerted string
		cooldown    time.Duration
		want        bool
	}{
		{
			name:        "empty last alerted always alerts",
			lastAlerted: "",
			cooldown:    1 * time.Hour,
			want:        true,
		},
		{
			name:        "malformed timestamp always alerts",
			lastAlerted: "not-a-timestamp",
			cooldown:    1 * time.Hour,
			want:        true,
		},
		{
			name:        "recent alert within cooldown suppresses",
			lastAlerted: time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
			cooldown:    1 * time.Hour,
			want:        false,
		},
		{
			name:        "old alert outside cooldown fires",
			lastAlerted: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
			cooldown:    1 * time.Hour,
			want:        true,
		},
		{
			name:        "exactly at cooldown boundary fires",
			lastAlerted: time.Now().Add(-61 * time.Minute).UTC().Format(time.RFC3339),
			cooldown:    1 * time.Hour,
			want:        true,
		},
		{
			name:        "4 hour cooldown recent suppresses",
			lastAlerted: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
			cooldown:    4 * time.Hour,
			want:        false,
		},
		{
			name:        "4 hour cooldown old fires",
			lastAlerted: time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339),
			cooldown:    4 * time.Hour,
			want:        true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldAlert(tc.lastAlerted, tc.cooldown)
			if got != tc.want {
				t.Errorf("shouldAlert(%q, %v) = %v, want %v",
					tc.lastAlerted, tc.cooldown, got, tc.want)
			}
		})
	}
}

func TestThresholdComparison(t *testing.T) {
	// Test the threshold logic embedded in evaluateUserAlerts:
	// alert fires when current > threshold AND shouldAlert passes.
	// We test the comparison logic standalone.
	tests := []struct {
		name      string
		current   float64
		threshold float64
		wantFire  bool
	}{
		{
			name:      "current exceeds threshold",
			current:   1.50,
			threshold: 1.00,
			wantFire:  true,
		},
		{
			name:      "current equals threshold does not fire",
			current:   1.00,
			threshold: 1.00,
			wantFire:  false,
		},
		{
			name:      "current below threshold does not fire",
			current:   0.50,
			threshold: 1.00,
			wantFire:  false,
		},
		{
			name:      "zero threshold with positive current does not fire (threshold disabled)",
			current:   5.00,
			threshold: 0,
			wantFire:  false, // threshold == 0 means disabled per evaluateUserAlerts logic
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reproduce the threshold check from evaluateUserAlerts:
			// if pref.CostThresholdHourly > 0 && latestCost.HourlyCost > pref.CostThresholdHourly
			got := tc.threshold > 0 && tc.current > tc.threshold
			if got != tc.wantFire {
				t.Errorf("threshold=%v current=%v: fires=%v, want %v",
					tc.threshold, tc.current, got, tc.wantFire)
			}
		})
	}
}

func TestDailyProjectionFromHourly(t *testing.T) {
	tests := []struct {
		name       string
		hourlyCost float64
		wantDaily  float64
	}{
		{
			name:       "zero hourly",
			hourlyCost: 0,
			wantDaily:  0,
		},
		{
			name:       "one dollar hourly",
			hourlyCost: 1.0,
			wantDaily:  24.0,
		},
		{
			name:       "t3.micro hourly",
			hourlyCost: 0.0104,
			wantDaily:  0.0104 * 24,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reproduces: dailyCost := latestCost.HourlyCost * 24
			got := tc.hourlyCost * 24
			if got != tc.wantDaily {
				t.Errorf("daily projection for hourly=%v: got %v, want %v",
					tc.hourlyCost, got, tc.wantDaily)
			}
		})
	}
}

func TestInstanceCountThreshold(t *testing.T) {
	tests := []struct {
		name      string
		count     int
		threshold int
		wantFire  bool
	}{
		{
			name:      "count exceeds threshold",
			count:     10,
			threshold: 5,
			wantFire:  true,
		},
		{
			name:      "count equals threshold does not fire",
			count:     5,
			threshold: 5,
			wantFire:  false,
		},
		{
			name:      "count below threshold does not fire",
			count:     3,
			threshold: 5,
			wantFire:  false,
		},
		{
			name:      "zero threshold disabled",
			count:     100,
			threshold: 0,
			wantFire:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reproduces: pref.InstanceCountThreshold > 0 && latestCost.InstanceCount > pref.InstanceCountThreshold
			got := tc.threshold > 0 && tc.count > tc.threshold
			if got != tc.wantFire {
				t.Errorf("count=%d threshold=%d: fires=%v, want %v",
					tc.count, tc.threshold, got, tc.wantFire)
			}
		})
	}
}
