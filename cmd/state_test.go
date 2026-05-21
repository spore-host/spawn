package cmd

import (
	"fmt"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/testutil"
)

// TestParseDuration tests TTL duration parsing with various formats
func TestParseDuration(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{
			name:  "standard hours",
			input: "8h",
			want:  8 * time.Hour,
		},
		{
			name:  "standard minutes",
			input: "30m",
			want:  30 * time.Minute,
		},
		{
			name:  "standard seconds",
			input: "300s",
			want:  300 * time.Second,
		},
		{
			name:  "combined hours and minutes",
			input: "2h30m",
			want:  2*time.Hour + 30*time.Minute,
		},
		{
			name:  "custom format days",
			input: "7d",
			want:  7 * 24 * time.Hour,
		},
		{
			name:  "custom format days and hours",
			input: "2d12h",
			want:  2*24*time.Hour + 12*time.Hour,
		},
		{
			name:  "large value",
			input: "168h",
			want:  168 * time.Hour,
		},
		{
			name:    "invalid format",
			input:   "invalid",
			wantErr: true,
		},
		{
			name:  "empty string",
			input: "",
			want:  0,
		},
		{
			name:    "missing unit",
			input:   "42",
			wantErr: true,
		},
		{
			name:    "invalid unit",
			input:   "5x",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDuration(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseDuration() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFormatDurationForTTL tests duration formatting for TTL tags
func TestFormatDurationForTTL(t *testing.T) {
	tests := []struct {
		name  string
		input time.Duration
		want  string
	}{
		{
			name:  "seconds",
			input: 45 * time.Second,
			want:  "45s",
		},
		{
			name:  "minutes",
			input: 30 * time.Minute,
			want:  "30m",
		},
		{
			name:  "hours",
			input: 8 * time.Hour,
			want:  "8h",
		},
		{
			name:  "days",
			input: 3 * 24 * time.Hour,
			want:  "3d",
		},
		{
			name:  "mixed days and hours",
			input: 2*24*time.Hour + 12*time.Hour,
			want:  "2d12h",
		},
		{
			name:  "mixed hours and minutes",
			input: 2*time.Hour + 30*time.Minute,
			want:  "2h30m",
		},
		{
			name:  "one week",
			input: 7 * 24 * time.Hour,
			want:  "7d",
		},
		{
			name:  "zero duration",
			input: 0,
			want:  "0s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDurationForTTL(tt.input)
			if got != tt.want {
				t.Errorf("formatDurationForTTL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseDurationRoundTrip tests that parsing and formatting are consistent
func TestParseDurationRoundTrip(t *testing.T) {
	durations := []string{
		"30m",
		"8h",
		"2d",
		"7d",
		"2h30m",
	}

	for _, ttl := range durations {
		t.Run(ttl, func(t *testing.T) {
			// Parse the TTL
			parsed, err := parseDuration(ttl)
			if err != nil {
				t.Fatalf("parseDuration(%q) error = %v", ttl, err)
			}

			// Format it back
			formatted := formatDurationForTTL(parsed)

			// Parse again to ensure equivalence
			parsed2, err := parseDuration(formatted)
			if err != nil {
				t.Fatalf("parseDuration(%q) error = %v", formatted, err)
			}

			if parsed != parsed2 {
				t.Errorf("round trip failed: %q -> %v -> %q -> %v", ttl, parsed, formatted, parsed2)
			}
		})
	}
}

// TestStateValidation tests state transition validation
func TestStateValidation(t *testing.T) {
	tests := []struct {
		name          string
		currentState  string
		targetAction  string
		shouldSucceed bool
	}{
		{
			name:          "stop running instance",
			currentState:  "running",
			targetAction:  "stop",
			shouldSucceed: true,
		},
		{
			name:          "hibernate running instance",
			currentState:  "running",
			targetAction:  "hibernate",
			shouldSucceed: true,
		},
		{
			name:          "start stopped instance",
			currentState:  "stopped",
			targetAction:  "start",
			shouldSucceed: true,
		},
		{
			name:          "cannot stop already stopped",
			currentState:  "stopped",
			targetAction:  "stop",
			shouldSucceed: false,
		},
		{
			name:          "cannot stop pending instance",
			currentState:  "pending",
			targetAction:  "stop",
			shouldSucceed: false,
		},
		{
			name:          "cannot start running instance",
			currentState:  "running",
			targetAction:  "start",
			shouldSucceed: false,
		},
		{
			name:          "cannot start terminating instance",
			currentState:  "terminating",
			targetAction:  "start",
			shouldSucceed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Validate state transition rules
			valid := isValidStateTransition(tt.currentState, tt.targetAction)
			if valid != tt.shouldSucceed {
				t.Errorf("isValidStateTransition(%q, %q) = %v, want %v",
					tt.currentState, tt.targetAction, valid, tt.shouldSucceed)
			}
		})
	}
}

// Helper function to validate state transitions
func isValidStateTransition(currentState, action string) bool {
	switch action {
	case "stop", "hibernate":
		return currentState == "running"
	case "start":
		return currentState == "stopped"
	default:
		return false
	}
}

// TestTTLCalculation tests remaining TTL calculation
func TestTTLCalculation(t *testing.T) {
	tests := []struct {
		name       string
		ttl        string
		uptime     time.Duration
		wantRemain string
		shouldCalc bool
	}{
		{
			name:       "half time remaining",
			ttl:        "8h",
			uptime:     4 * time.Hour,
			wantRemain: "4h",
			shouldCalc: true,
		},
		{
			name:       "most time remaining",
			ttl:        "24h",
			uptime:     2 * time.Hour,
			wantRemain: "22h",
			shouldCalc: true,
		},
		{
			name:       "minutes remaining",
			ttl:        "2h",
			uptime:     90 * time.Minute,
			wantRemain: "30m",
			shouldCalc: true,
		},
		{
			name:       "no time remaining",
			ttl:        "4h",
			uptime:     5 * time.Hour,
			wantRemain: "",
			shouldCalc: false,
		},
		{
			name:       "exact time",
			ttl:        "8h",
			uptime:     8 * time.Hour,
			wantRemain: "",
			shouldCalc: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse TTL
			ttlDuration, err := parseDuration(tt.ttl)
			if err != nil {
				t.Fatalf("parseDuration() error = %v", err)
			}

			// Calculate remaining
			remaining := ttlDuration - tt.uptime

			if remaining <= 0 {
				if tt.shouldCalc {
					t.Errorf("expected remaining time, but got none (remaining=%v)", remaining)
				}
				return
			}

			if !tt.shouldCalc {
				t.Errorf("expected no remaining time, but got %v", remaining)
				return
			}

			// Format remaining
			formatted := formatDurationForTTL(remaining)

			// Parse the expected and formatted to compare durations
			wantDuration, err := parseDuration(tt.wantRemain)
			if err != nil {
				t.Fatalf("parseDuration(want) error = %v", err)
			}

			gotDuration, err := parseDuration(formatted)
			if err != nil {
				t.Fatalf("parseDuration(formatted) error = %v", err)
			}

			// Allow small difference due to rounding
			diff := wantDuration - gotDuration
			if diff < 0 {
				diff = -diff
			}
			if diff > time.Minute {
				t.Errorf("remaining TTL = %q (%v), want %q (%v)", formatted, gotDuration, tt.wantRemain, wantDuration)
			}
		})
	}
}

// TestJobArrayStateManagement tests job array state operations
func TestJobArrayStateManagement(t *testing.T) {
	tests := []struct {
		name          string
		jobArrayID    string
		jobArrayName  string
		valid         bool
		errorContains string
	}{
		{
			name:         "valid job array ID",
			jobArrayID:   "compute-20260122-abc123",
			jobArrayName: "",
			valid:        true,
		},
		{
			name:         "valid job array name",
			jobArrayID:   "",
			jobArrayName: "compute",
			valid:        true,
		},
		{
			name:          "both ID and name provided",
			jobArrayID:    "compute-20260122-abc123",
			jobArrayName:  "compute",
			valid:         false,
			errorContains: "both",
		},
		{
			name:          "neither ID nor name provided",
			jobArrayID:    "",
			jobArrayName:  "",
			valid:         false,
			errorContains: "either",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateJobArrayInput(tt.jobArrayID, tt.jobArrayName)
			if tt.valid && err != nil {
				t.Errorf("validateJobArrayInput() unexpected error = %v", err)
			}
			if !tt.valid {
				if err == nil {
					t.Error("validateJobArrayInput() expected error, got nil")
				} else if tt.errorContains != "" && !testutil.Contains(err.Error(), tt.errorContains) {
					t.Errorf("validateJobArrayInput() error = %q, should contain %q", err.Error(), tt.errorContains)
				}
			}
		})
	}
}

// Helper function to validate job array input
func validateJobArrayInput(jobArrayID, jobArrayName string) error {
	if jobArrayID != "" && jobArrayName != "" {
		return fmt.Errorf("cannot specify both job array ID and name")
	}
	if jobArrayID == "" && jobArrayName == "" {
		return fmt.Errorf("must specify either job array ID or name")
	}
	return nil
}
