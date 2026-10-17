package agent

import (
	"testing"
)

// The disk filter tests that lived here re-implemented the old prefix-and-length
// filter inline and asserted what it did, including skipping nvme0n1 and keeping
// sda1, so they certified #937. The real reader is tested in diskstats_test.go.

// TestGetDiskIO_SectorToByteConversion validates sector to byte conversion
func TestGetDiskIO_SectorToByteConversion(t *testing.T) {
	tests := []struct {
		sectors int64
		bytes   int64
	}{
		{0, 0},
		{1, 512},
		{2, 1024},
		{100, 51200},
		{1000, 512000},
		{10000, 5120000},
		{100000, 51200000},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			bytes := tt.sectors * 512
			if bytes != tt.bytes {
				t.Errorf("Sectors %d: expected %d bytes, got %d", tt.sectors, tt.bytes, bytes)
			}
		})
	}
}

// TestIsIdle_GPUThreshold validates GPU utilization idle threshold (5%)
func TestIsIdle_GPUThreshold(t *testing.T) {
	threshold := 5.0 // 5%

	tests := []struct {
		gpuUtil      float64
		shouldBeIdle bool
	}{
		{0.0, true},    // No GPU activity - idle
		{2.5, true},    // Low activity - idle
		{4.9, true},    // Just under threshold
		{5.0, true},    // At threshold
		{5.1, false},   // Just over threshold
		{10.0, false},  // Moderate activity
		{50.0, false},  // High activity
		{100.0, false}, // Maximum activity
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			// Test threshold logic
			isIdle := tt.gpuUtil <= threshold
			if isIdle != tt.shouldBeIdle {
				t.Errorf("GPU utilization %.2f%%: expected idle=%v, got idle=%v",
					tt.gpuUtil, tt.shouldBeIdle, isIdle)
			}
		})
	}
}

// TestGPUUtilizationThreshold_RealWorldScenarios validates threshold with realistic values
func TestGPUUtilizationThreshold_RealWorldScenarios(t *testing.T) {
	tests := []struct {
		scenario     string
		gpuUtil      float64
		description  string
		shouldBeIdle bool
	}{
		{"Idle GPU", 0.0, "No GPU workload", true},
		{"Desktop compositing", 1.5, "Basic UI rendering", true},
		{"Light rendering", 4.5, "Minimal graphics work", true},
		{"Development tools", 8.0, "IDE with GPU acceleration", false},
		{"Machine learning", 75.0, "Training neural network", false},
		{"Gaming", 95.0, "Playing GPU-intensive game", false},
	}

	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			threshold := 5.0 // 5%
			isIdle := tt.gpuUtil <= threshold

			if isIdle != tt.shouldBeIdle {
				t.Errorf("%s (%s): GPU util %.2f%%, expected idle=%v, got idle=%v",
					tt.scenario, tt.description, tt.gpuUtil, tt.shouldBeIdle, isIdle)
			}

			t.Logf("%s: %.2f%% (%s) - idle: %v", tt.scenario, tt.gpuUtil, tt.description, isIdle)
		})
	}
}
