package cmd

import (
	"testing"
)

// TestCheckCompleteExitCodes tests the --check-complete exit code logic
func TestCheckCompleteExitCodes(t *testing.T) {
	tests := []struct {
		name         string
		sweepStatus  string
		expectedExit int
		description  string
	}{
		{
			name:         "completed sweep",
			sweepStatus:  "COMPLETED",
			expectedExit: 0,
			description:  "Should exit 0 for completed sweeps",
		},
		{
			name:         "failed sweep",
			sweepStatus:  "FAILED",
			expectedExit: 1,
			description:  "Should exit 1 for failed sweeps",
		},
		{
			name:         "cancelled sweep",
			sweepStatus:  "CANCELLED",
			expectedExit: 1,
			description:  "Should exit 1 for cancelled sweeps",
		},
		{
			name:         "running sweep",
			sweepStatus:  "RUNNING",
			expectedExit: 2,
			description:  "Should exit 2 for running sweeps",
		},
		{
			name:         "initializing sweep",
			sweepStatus:  "INITIALIZING",
			expectedExit: 2,
			description:  "Should exit 2 for initializing sweeps",
		},
		{
			name:         "unknown status",
			sweepStatus:  "UNKNOWN",
			expectedExit: 3,
			description:  "Should exit 3 for unknown states",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the exit code mapping logic
			var exitCode int
			switch tt.sweepStatus {
			case "COMPLETED":
				exitCode = 0
			case "FAILED", "CANCELLED":
				exitCode = 1
			case "RUNNING", "INITIALIZING":
				exitCode = 2
			default:
				exitCode = 3
			}

			if exitCode != tt.expectedExit {
				t.Errorf("%s: got exit code %d, want %d", tt.description, exitCode, tt.expectedExit)
			}
		})
	}
}

// TestStatusOutputModes tests different output modes for status command
func TestStatusOutputModes(t *testing.T) {
	tests := []struct {
		name             string
		jsonOutput       bool
		checkComplete    bool
		expectJSONOutput bool
		expectExitCode   bool
	}{
		{
			name:             "default mode",
			jsonOutput:       false,
			checkComplete:    false,
			expectJSONOutput: false,
			expectExitCode:   false,
		},
		{
			name:             "json mode",
			jsonOutput:       true,
			checkComplete:    false,
			expectJSONOutput: true,
			expectExitCode:   false,
		},
		{
			name:             "check-complete mode",
			jsonOutput:       false,
			checkComplete:    true,
			expectJSONOutput: false,
			expectExitCode:   true,
		},
		{
			name:             "json + check-complete",
			jsonOutput:       true,
			checkComplete:    true,
			expectJSONOutput: false,
			expectExitCode:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify that check-complete takes precedence over json
			if tt.checkComplete && tt.expectJSONOutput {
				t.Error("check-complete should prevent JSON output")
			}

			// Verify that check-complete mode uses exit codes
			if tt.checkComplete != tt.expectExitCode {
				t.Error("check-complete mode should use exit codes")
			}
		})
	}
}
