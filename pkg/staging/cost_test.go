package staging

import (
	"strings"
	"testing"
)

func TestEstimateStagingCost(t *testing.T) {
	tests := []struct {
		name                 string
		dataSizeGB           int
		numRegions           int
		instancesPerRegion   int
		expectRecommendation string
		expectSavings        bool
	}{
		{
			name:                 "Large dataset, multiple regions",
			dataSizeGB:           100,
			numRegions:           3,
			instancesPerRegion:   10,
			expectRecommendation: "Regional replication",
			expectSavings:        true,
		},
		{
			name:                 "Small dataset, few instances",
			dataSizeGB:           10,
			numRegions:           2,
			instancesPerRegion:   2,
			expectRecommendation: "Regional replication",
			expectSavings:        true, // Even with small workloads, replication is often cheaper
		},
		{
			name:                 "Medium dataset, many instances",
			dataSizeGB:           50,
			numRegions:           2,
			instancesPerRegion:   20,
			expectRecommendation: "Regional replication",
			expectSavings:        true,
		},
		{
			name:                 "Large dataset, single instance per region",
			dataSizeGB:           100,
			numRegions:           4,
			instancesPerRegion:   1,
			expectRecommendation: "Regional replication",
			expectSavings:        true, // Replication cost ($0.02/GB) < cross-region transfer ($0.09/GB)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate := EstimateStagingCost(tt.dataSizeGB, tt.numRegions, tt.instancesPerRegion)

			// Verify basic fields
			if estimate.DataSizeGB != tt.dataSizeGB {
				t.Errorf("DataSizeGB = %d, want %d", estimate.DataSizeGB, tt.dataSizeGB)
			}
			if estimate.NumRegions != tt.numRegions {
				t.Errorf("NumRegions = %d, want %d", estimate.NumRegions, tt.numRegions)
			}
			if estimate.InstancesPerRegion != tt.instancesPerRegion {
				t.Errorf("InstancesPerRegion = %d, want %d", estimate.InstancesPerRegion, tt.instancesPerRegion)
			}

			// Verify recommendation
			if estimate.Recommendation != tt.expectRecommendation {
				t.Errorf("Recommendation = %q, want %q", estimate.Recommendation, tt.expectRecommendation)
			}

			// Verify savings expectation
			hasSavings := estimate.Savings > 0
			if hasSavings != tt.expectSavings {
				t.Errorf("Savings = %.2f (has savings: %v), want savings: %v",
					estimate.Savings, hasSavings, tt.expectSavings)
			}

			// Verify cost calculations are positive
			if estimate.SingleRegionStorageCost < 0 {
				t.Errorf("SingleRegionStorageCost is negative: %.2f", estimate.SingleRegionStorageCost)
			}
			if estimate.CrossRegionTransferCost < 0 {
				t.Errorf("CrossRegionTransferCost is negative: %.2f", estimate.CrossRegionTransferCost)
			}
			if estimate.MultiRegionStorageCost < 0 {
				t.Errorf("MultiRegionStorageCost is negative: %.2f", estimate.MultiRegionStorageCost)
			}
			if estimate.ReplicationCost < 0 {
				t.Errorf("ReplicationCost is negative: %.2f", estimate.ReplicationCost)
			}
		})
	}
}

func TestFormatCostEstimate(t *testing.T) {
	estimate := EstimateStagingCost(100, 2, 10)
	formatted := estimate.FormatCostEstimate()

	// Verify output contains key sections
	expectedSections := []string{
		"Data Staging Cost Comparison",
		"Option A: Single-Region Storage",
		"Option B: Regional Replication",
		"Dataset: 100 GB",
		"Regions: 2",
		"Instances per region: 10",
	}

	for _, section := range expectedSections {
		if !strings.Contains(formatted, section) {
			t.Errorf("FormatCostEstimate() missing section: %s", section)
		}
	}

	// Verify numbers are formatted as currency
	if !strings.Contains(formatted, "$") {
		t.Error("FormatCostEstimate() should contain currency symbols")
	}
}

func TestBreakEvenAnalysis(t *testing.T) {
	output := BreakEvenAnalysis(100, 3)

	expectedSections := []string{
		"Break-Even Analysis",
		"Replication cost:",
		"Cross-region cost per instance:",
		"Break-even point:",
	}

	for _, section := range expectedSections {
		if !strings.Contains(output, section) {
			t.Errorf("BreakEvenAnalysis() missing section: %s", section)
		}
	}
}

func TestCostCalculationAccuracy(t *testing.T) {
	// Test with known values to verify calculation accuracy
	dataSizeGB := 100
	numRegions := 2
	instancesPerRegion := 10

	estimate := EstimateStagingCost(dataSizeGB, numRegions, instancesPerRegion)

	// Manual calculations for verification
	expectedSingleRegionStorage := float64(dataSizeGB) * s3StorageCostPerGB
	if estimate.SingleRegionStorageCost != expectedSingleRegionStorage {
		t.Errorf("SingleRegionStorageCost = %.2f, want %.2f",
			estimate.SingleRegionStorageCost, expectedSingleRegionStorage)
	}

	// Cross-region transfer: only instances in non-primary regions pay
	expectedCrossRegionTransfer := float64(dataSizeGB) * float64(instancesPerRegion*(numRegions-1)) * crossRegionTransferCost
	if estimate.CrossRegionTransferCost != expectedCrossRegionTransfer {
		t.Errorf("CrossRegionTransferCost = %.2f, want %.2f",
			estimate.CrossRegionTransferCost, expectedCrossRegionTransfer)
	}

	// Multi-region storage: all regions
	expectedMultiRegionStorage := float64(dataSizeGB) * float64(numRegions) * s3StorageCostPerGB
	if estimate.MultiRegionStorageCost != expectedMultiRegionStorage {
		t.Errorf("MultiRegionStorageCost = %.2f, want %.2f",
			estimate.MultiRegionStorageCost, expectedMultiRegionStorage)
	}

	// Replication cost: to non-primary regions only
	expectedReplicationCost := float64(dataSizeGB) * float64(numRegions-1) * s3ReplicationCost
	if estimate.ReplicationCost != expectedReplicationCost {
		t.Errorf("ReplicationCost = %.2f, want %.2f",
			estimate.ReplicationCost, expectedReplicationCost)
	}

	// Total costs should match sum of components
	expectedSingleTotal := expectedSingleRegionStorage + expectedCrossRegionTransfer
	if estimate.SingleRegionTotalCost != expectedSingleTotal {
		t.Errorf("SingleRegionTotalCost = %.2f, want %.2f",
			estimate.SingleRegionTotalCost, expectedSingleTotal)
	}

	expectedMultiTotal := expectedMultiRegionStorage + expectedReplicationCost
	if estimate.MultiRegionTotalCost != expectedMultiTotal {
		t.Errorf("MultiRegionTotalCost = %.2f, want %.2f",
			estimate.MultiRegionTotalCost, expectedMultiTotal)
	}

	// Savings calculation
	expectedSavings := expectedSingleTotal - expectedMultiTotal
	if estimate.Savings != expectedSavings {
		t.Errorf("Savings = %.2f, want %.2f", estimate.Savings, expectedSavings)
	}

	// Savings percent
	expectedSavingsPercent := (expectedSavings / expectedSingleTotal) * 100
	if estimate.SavingsPercent != expectedSavingsPercent {
		t.Errorf("SavingsPercent = %.2f, want %.2f",
			estimate.SavingsPercent, expectedSavingsPercent)
	}
}

func TestEdgeCases(t *testing.T) {
	t.Run("Zero data size", func(t *testing.T) {
		estimate := EstimateStagingCost(0, 2, 10)
		if estimate.SingleRegionTotalCost != 0 {
			t.Errorf("Expected zero cost for zero data, got %.2f", estimate.SingleRegionTotalCost)
		}
	})

	t.Run("Single region", func(t *testing.T) {
		estimate := EstimateStagingCost(100, 1, 10)
		// With 1 region, no cross-region transfer needed
		if estimate.CrossRegionTransferCost != 0 {
			t.Errorf("Expected zero cross-region cost with 1 region, got %.2f",
				estimate.CrossRegionTransferCost)
		}
		// No replication needed
		if estimate.ReplicationCost != 0 {
			t.Errorf("Expected zero replication cost with 1 region, got %.2f",
				estimate.ReplicationCost)
		}
	})

	t.Run("Zero instances", func(t *testing.T) {
		estimate := EstimateStagingCost(100, 2, 0)
		// No instances = no transfer costs
		if estimate.CrossRegionTransferCost != 0 {
			t.Errorf("Expected zero transfer cost with 0 instances, got %.2f",
				estimate.CrossRegionTransferCost)
		}
	})

	t.Run("Many regions", func(t *testing.T) {
		estimate := EstimateStagingCost(100, 10, 5)
		// Storage cost should scale with regions
		expectedStorage := float64(100) * float64(10) * s3StorageCostPerGB
		if estimate.MultiRegionStorageCost != expectedStorage {
			t.Errorf("MultiRegionStorageCost = %.2f, want %.2f",
				estimate.MultiRegionStorageCost, expectedStorage)
		}
	})
}

func TestSavingsScenarios(t *testing.T) {
	tests := []struct {
		name               string
		dataSizeGB         int
		numRegions         int
		instancesPerRegion int
		wantSavings        bool
	}{
		{
			name:               "High instance count - should save",
			dataSizeGB:         100,
			numRegions:         2,
			instancesPerRegion: 50,
			wantSavings:        true,
		},
		{
			name:               "Low instance count - still saves",
			dataSizeGB:         100,
			numRegions:         2,
			instancesPerRegion: 1,
			wantSavings:        true, // Even 1 instance benefits from replication
		},
		{
			name:               "Many regions - should save",
			dataSizeGB:         50,
			numRegions:         5,
			instancesPerRegion: 10,
			wantSavings:        true,
		},
		{
			name:               "Large dataset - should save",
			dataSizeGB:         500,
			numRegions:         2,
			instancesPerRegion: 5,
			wantSavings:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate := EstimateStagingCost(tt.dataSizeGB, tt.numRegions, tt.instancesPerRegion)
			hasSavings := estimate.Savings > 0

			if hasSavings != tt.wantSavings {
				t.Errorf("Expected savings=%v, got savings=%.2f (has savings: %v)",
					tt.wantSavings, estimate.Savings, hasSavings)
				t.Logf("  Single-region cost: $%.2f", estimate.SingleRegionTotalCost)
				t.Logf("  Multi-region cost: $%.2f", estimate.MultiRegionTotalCost)
				t.Logf("  Recommendation: %s", estimate.Recommendation)
			}
		})
	}
}
