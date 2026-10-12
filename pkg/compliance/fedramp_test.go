package compliance

import (
	"strings"
	"testing"
)

func TestFedRAMPControlSet(t *testing.T) {
	tests := []struct {
		name             string
		level            FedRAMPLevel
		expectedBaseline Baseline
	}{
		{
			name:             "FedRAMP Low",
			level:            FedRAMPLow,
			expectedBaseline: BaselineLow,
		},
		{
			name:             "FedRAMP Moderate",
			level:            FedRAMPModerate,
			expectedBaseline: BaselineModerate,
		},
		{
			name:             "FedRAMP High",
			level:            FedRAMPHigh,
			expectedBaseline: BaselineHigh,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controlSet := FedRAMPControlSet(tt.level)

			if controlSet == nil {
				t.Fatal("Expected control set, got nil")
			}

			if controlSet.Name == "" {
				t.Error("Expected control set name to be set")
			}

			if !strings.Contains(controlSet.Name, "FedRAMP") {
				t.Errorf("Expected FedRAMP branding in name, got: %s", controlSet.Name)
			}

			if controlSet.Description == "" {
				t.Error("Expected control set description to be set")
			}

			if len(controlSet.Controls) == 0 {
				t.Error("Expected controls to be defined")
			}

			// Verify control count matches the baseline
			expectedCount := GetBaselineControlCount(tt.expectedBaseline)
			if len(controlSet.Controls) != expectedCount {
				t.Errorf("Expected %d controls (from %s baseline), got %d",
					expectedCount, tt.expectedBaseline, len(controlSet.Controls))
			}
		})
	}
}

func TestMapFedRAMPToBaseline(t *testing.T) {
	tests := []struct {
		level    FedRAMPLevel
		expected Baseline
	}{
		{FedRAMPLow, BaselineLow},
		{FedRAMPModerate, BaselineModerate},
		{FedRAMPHigh, BaselineHigh},
	}

	for _, tt := range tests {
		result := MapFedRAMPToBaseline(tt.level)
		if result != tt.expected {
			t.Errorf("MapFedRAMPToBaseline(%s) = %s, want %s", tt.level, result, tt.expected)
		}
	}
}
