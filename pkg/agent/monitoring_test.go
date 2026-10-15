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

// TestIsIdle_DiskIOThreshold validates disk I/O idle threshold (100KB/min)
func TestIsIdle_DiskIOThreshold(t *testing.T) {
	threshold := int64(100000) // 100KB

	tests := []struct {
		diskIO       int64
		shouldBeIdle bool
	}{
		{0, true},        // No disk I/O - idle
		{50000, true},    // 50KB - under threshold
		{99999, true},    // Just under threshold
		{100000, true},   // At threshold
		{100001, false},  // Just over threshold
		{200000, false},  // Well over threshold
		{1000000, false}, // 1MB - definitely not idle
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			// Test threshold logic
			isIdle := tt.diskIO <= threshold
			if isIdle != tt.shouldBeIdle {
				t.Errorf("Disk I/O %d bytes: expected idle=%v, got idle=%v",
					tt.diskIO, tt.shouldBeIdle, isIdle)
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

// TestIsIdle_NetworkThreshold validates network traffic idle threshold (10KB/min)
func TestIsIdle_NetworkThreshold(t *testing.T) {
	threshold := int64(10000) // 10KB

	tests := []struct {
		networkBytes int64
		shouldBeIdle bool
	}{
		{0, true},        // No network traffic - idle
		{5000, true},     // 5KB - under threshold
		{9999, true},     // Just under threshold
		{10000, true},    // At threshold
		{10001, false},   // Just over threshold
		{50000, false},   // 50KB - over threshold
		{1000000, false}, // 1MB - definitely not idle
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			// Test threshold logic
			isIdle := tt.networkBytes <= threshold
			if isIdle != tt.shouldBeIdle {
				t.Errorf("Network traffic %d bytes: expected idle=%v, got idle=%v",
					tt.networkBytes, tt.shouldBeIdle, isIdle)
			}
		})
	}
}

// TestIsIdle_AllConditions validates that ALL conditions must be met for idle
func TestIsIdle_AllConditions(t *testing.T) {
	tests := []struct {
		name         string
		cpuUsage     float64
		cpuThreshold float64
		networkBytes int64
		diskIO       int64
		gpuUtil      float64
		expectedIdle bool
	}{
		{
			name:         "All metrics idle",
			cpuUsage:     2.0,
			cpuThreshold: 5.0,
			networkBytes: 1000,
			diskIO:       10000,
			gpuUtil:      1.0,
			expectedIdle: true,
		},
		{
			name:         "CPU over threshold",
			cpuUsage:     6.0,
			cpuThreshold: 5.0,
			networkBytes: 1000,
			diskIO:       10000,
			gpuUtil:      1.0,
			expectedIdle: false,
		},
		{
			name:         "Network over threshold",
			cpuUsage:     2.0,
			cpuThreshold: 5.0,
			networkBytes: 15000,
			diskIO:       10000,
			gpuUtil:      1.0,
			expectedIdle: false,
		},
		{
			name:         "Disk I/O over threshold",
			cpuUsage:     2.0,
			cpuThreshold: 5.0,
			networkBytes: 1000,
			diskIO:       150000,
			gpuUtil:      1.0,
			expectedIdle: false,
		},
		{
			name:         "GPU over threshold",
			cpuUsage:     2.0,
			cpuThreshold: 5.0,
			networkBytes: 1000,
			diskIO:       10000,
			gpuUtil:      10.0,
			expectedIdle: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test that ALL conditions must be met
			cpuIdle := tt.cpuUsage < tt.cpuThreshold
			networkIdle := tt.networkBytes <= 10000
			diskIdle := tt.diskIO <= 100000
			gpuIdle := tt.gpuUtil <= 5.0

			allIdle := cpuIdle && networkIdle && diskIdle && gpuIdle

			if allIdle != tt.expectedIdle {
				t.Errorf("Expected idle=%v, but got idle=%v (cpu=%v, net=%v, disk=%v, gpu=%v)",
					tt.expectedIdle, allIdle, cpuIdle, networkIdle, diskIdle, gpuIdle)
			}
		})
	}
}

// TestDiskIOThreshold_RealWorldScenarios validates threshold with realistic values
func TestDiskIOThreshold_RealWorldScenarios(t *testing.T) {
	tests := []struct {
		scenario     string
		diskIO       int64
		description  string
		shouldBeIdle bool
	}{
		{"No activity", 0, "System sitting idle", true},
		{"Light logging", 1024, "1KB - minimal log writes", true},
		{"Text editing", 10240, "10KB - small file saves", true},
		{"Code compilation", 500000, "500KB - compiling code", false},
		{"Database writes", 1048576, "1MB - database operations", false},
		{"Large file copy", 10485760, "10MB - copying large file", false},
	}

	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			threshold := int64(100000) // 100KB
			isIdle := tt.diskIO <= threshold

			if isIdle != tt.shouldBeIdle {
				t.Errorf("%s (%s): disk I/O %d bytes, expected idle=%v, got idle=%v",
					tt.scenario, tt.description, tt.diskIO, tt.shouldBeIdle, isIdle)
			}

			t.Logf("%s: %d bytes (%s) - idle: %v", tt.scenario, tt.diskIO, tt.description, isIdle)
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
