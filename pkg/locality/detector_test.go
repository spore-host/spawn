package locality

import (
	"testing"
)

func TestGetCrossRegionCost(t *testing.T) {
	tests := []struct {
		name         string
		sourceRegion string
		destRegion   string
		expectedCost float64
	}{
		{
			name:         "Same continent - US East to US West",
			sourceRegion: "us-east-1",
			destRegion:   "us-west-2",
			expectedCost: 0.02,
		},
		{
			name:         "Same continent - EU West to EU Central",
			sourceRegion: "eu-west-1",
			destRegion:   "eu-central-1",
			expectedCost: 0.02,
		},
		{
			name:         "Cross-continent - US to EU",
			sourceRegion: "us-east-1",
			destRegion:   "eu-west-1",
			expectedCost: 0.08,
		},
		{
			name:         "Cross-continent - EU to AP",
			sourceRegion: "eu-west-1",
			destRegion:   "ap-southeast-1",
			expectedCost: 0.08,
		},
		{
			name:         "Unknown region defaults to cross-continent",
			sourceRegion: "unknown-region-1",
			destRegion:   "us-east-1",
			expectedCost: 0.08,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cost := getCrossRegionCost(tt.sourceRegion, tt.destRegion)
			if cost != tt.expectedCost {
				t.Errorf("getCrossRegionCost(%q, %q) = %v, want %v",
					tt.sourceRegion, tt.destRegion, cost, tt.expectedCost)
			}
		})
	}
}

func TestEstimateLatency(t *testing.T) {
	tests := []struct {
		name            string
		sourceRegion    string
		destRegion      string
		expectedLatency int
	}{
		{
			name:            "Same region",
			sourceRegion:    "us-east-1",
			destRegion:      "us-east-1",
			expectedLatency: 0,
		},
		{
			name:            "Same continent - US",
			sourceRegion:    "us-east-1",
			destRegion:      "us-west-2",
			expectedLatency: 50,
		},
		{
			name:            "Cross-continent",
			sourceRegion:    "us-east-1",
			destRegion:      "eu-west-1",
			expectedLatency: 150,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latency := estimateLatency(tt.sourceRegion, tt.destRegion)
			if latency != tt.expectedLatency {
				t.Errorf("estimateLatency(%q, %q) = %v, want %v",
					tt.sourceRegion, tt.destRegion, latency, tt.expectedLatency)
			}
		})
	}
}

func TestRegionMismatchFormatWarning(t *testing.T) {
	warning := &DataLocalityWarning{
		Mismatches: []RegionMismatch{
			{
				ResourceType:   "EFS",
				ResourceID:     "fs-12345678",
				ResourceRegion: "us-west-2",
				LaunchRegion:   "us-east-1",
				EstimatedCost:  0.02,
			},
		},
		HasMismatches:  true,
		TotalCostPerGB: 0.02,
		AvgLatencyMs:   50,
		Recommendation: "Launch instances in us-west-2 for best performance and lowest cost",
	}

	formatted := warning.FormatWarning()

	// Check that formatted output contains key information
	if formatted == "" {
		t.Error("FormatWarning() returned empty string")
	}

	// Verify it contains the filesystem ID
	if !contains(formatted, "fs-12345678") {
		t.Error("FormatWarning() should contain filesystem ID")
	}

	// Verify it contains regions
	if !contains(formatted, "us-west-2") || !contains(formatted, "us-east-1") {
		t.Error("FormatWarning() should contain both regions")
	}

	// Verify it contains cost information
	if !contains(formatted, "$0.02") {
		t.Error("FormatWarning() should contain cost estimate")
	}

	// Verify it contains recommendation
	if !contains(formatted, "Recommendation") {
		t.Error("FormatWarning() should contain recommendation")
	}
}

func TestRegionMismatchFormatWarningEmpty(t *testing.T) {
	warning := &DataLocalityWarning{
		HasMismatches: false,
	}

	formatted := warning.FormatWarning()
	if formatted != "" {
		t.Error("FormatWarning() should return empty string when no mismatches")
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
