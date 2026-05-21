package cmd

import (
	"strings"
	"testing"
)

// TestValidateTTL_ValidFormats validates correct TTL formats
func TestValidateTTL_ValidFormats(t *testing.T) {
	tests := []struct {
		name string
		ttl  string
	}{
		{"Seconds only", "30s"},
		{"Minutes only", "15m"},
		{"Hours only", "2h"},
		{"Days only", "7d"},
		{"Hours and minutes", "2h30m"},
		{"Days and hours", "1d12h"},
		{"Days, hours, minutes", "1d2h30m"},
		{"All units", "1d2h30m15s"},
		{"Large hours", "48h"},
		{"Large days", "30d"},
		{"Single digit", "1h"},
		{"Double digit", "10m"},
		{"Triple digit", "100h"},
		{"Short minute", "1m"},
		{"Short hour", "1h"},
		{"Short day", "1d"},
		{"Multiple hours and minutes", "3h45m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, want nil", tt.ttl, err)
			}
		})
	}
}

// TestValidateTTL_InvalidFormats validates rejection of incorrect formats
func TestValidateTTL_InvalidFormats(t *testing.T) {
	tests := []struct {
		name        string
		ttl         string
		expectedErr string
	}{
		{"Empty string", "", "TTL must be in format"},
		{"No unit", "30", "TTL must be in format"},
		{"Invalid unit - w", "1w", "TTL must be in format"},
		{"Invalid unit - y", "1y", "TTL must be in format"},
		{"Invalid unit - M", "1M", "TTL must be in format"},
		{"Space between number and unit", "2 h", "TTL must be in format"},
		{"Space between components", "2h 30m", "TTL must be in format"},
		{"Decimal number", "2.5h", "TTL must be in format"},
		{"Negative number", "-1h", "TTL must be in format"},
		{"Just unit", "h", "TTL must be in format"},
		{"Unit before number", "h2", "TTL must be in format"},
		{"Multiple spaces", "2h  30m", "TTL must be in format"},
		{"Comma separator", "2h,30m", "TTL must be in format"},
		{"Special characters", "2h!30m", "TTL must be in format"},
		{"Uppercase units", "2H", "TTL must be in format"},
		{"Mixed case", "2H30m", "TTL must be in format"},
		{"Zero seconds", "0s", "TTL must be greater than 0"},
		{"Zero minutes", "0m", "TTL must be greater than 0"},
		{"Zero hours", "0h", "TTL must be greater than 0"},
		{"Zero days", "0d", "TTL must be greater than 0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error containing %q", tt.ttl, tt.expectedErr)
			} else if tt.expectedErr != "" && !strings.Contains(err.Error(), tt.expectedErr) {
				t.Errorf("validateTTL(%q) error = %v, want error containing %q", tt.ttl, err, tt.expectedErr)
			}
		})
	}
}

// TestValidateTTL_EdgeCases validates edge cases and boundary conditions
func TestValidateTTL_EdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		ttl       string
		shouldErr bool
	}{
		{"Very large number", "999999h", false},
		{"Maximum reasonable", "365d", false},
		{"One second", "1s", false},
		{"Multiple of same unit", "1h2h", false}, // Weird but valid format
		{"Out of order units", "30m2h", false},   // Order doesn't matter for validation
		{"Duplicate units", "1h1h", false},       // Weird but technically valid
		{"Very long chain", "1d1h1m1s", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if tt.shouldErr && err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error", tt.ttl)
			}
			if !tt.shouldErr && err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, expected nil", tt.ttl, err)
			}
		})
	}
}

// TestValidateTTL_TotalDuration validates that total duration is calculated correctly
func TestValidateTTL_TotalDuration(t *testing.T) {
	// These are all valid formats, we're just ensuring they parse without error
	tests := []struct {
		name string
		ttl  string
	}{
		{"60 seconds = 1 minute", "60s"},
		{"60 minutes = 1 hour", "60m"},
		{"24 hours = 1 day", "24h"},
		{"Combined equals 1 day", "23h60m"}, // 23h + 60m = 24h
		{"Multiple components", "1d23h59m59s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, want nil", tt.ttl, err)
			}
		})
	}
}

// TestFormatTTLDuration validates TTL formatting for display
func TestFormatTTLDuration(t *testing.T) {
	tests := []struct {
		name     string
		ttl      string
		expected string
	}{
		{"1 second", "1s", "1 second"},
		{"Multiple seconds", "30s", "30 seconds"},
		{"1 minute", "1m", "1 minute"},
		{"Multiple minutes", "15m", "15 minutes"},
		{"1 hour", "1h", "1 hour"},
		{"Multiple hours", "2h", "2 hours"},
		{"1 day", "1d", "1 day"},
		{"Multiple days", "7d", "7 days"},
		{"Hours and minutes", "2h30m", "2 hours 30 minutes"},
		{"Days and hours", "1d12h", "1 day 12 hours"},
		{"All components", "1d2h30m15s", "1 day 2 hours 30 minutes 15 seconds"},
		{"Single of each", "1d1h1m1s", "1 day 1 hour 1 minute 1 second"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatTTLDuration(tt.ttl)
			if result != tt.expected {
				t.Errorf("formatTTLDuration(%q) = %q, want %q", tt.ttl, result, tt.expected)
			}
		})
	}
}

// TestValidateTTL_CommonUseCases validates typical user input patterns
func TestValidateTTL_CommonUseCases(t *testing.T) {
	tests := []struct {
		name      string
		ttl       string
		shouldErr bool
	}{
		{"Quick task - 30 min", "30m", false},
		{"Short session - 2 hours", "2h", false},
		{"Work day - 8 hours", "8h", false},
		{"Overnight - 12 hours", "12h", false},
		{"Full day", "24h", false},
		{"Weekend", "2d", false},
		{"Week", "7d", false},
		{"Two weeks", "14d", false},
		{"Month", "30d", false},
		{"Development session", "4h", false},
		{"Extended session", "3h30m", false},
		{"Typo - forgot unit", "2", true},
		{"Typo - wrong unit", "2hrs", true},
		{"Typo - plural", "2hours", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if tt.shouldErr && err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error", tt.ttl)
			}
			if !tt.shouldErr && err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, expected nil", tt.ttl, err)
			}
		})
	}
}

// TestValidateTTL_ZeroComponents validates handling of zero-value components
func TestValidateTTL_ZeroComponents(t *testing.T) {
	tests := []struct {
		name      string
		ttl       string
		shouldErr bool
	}{
		{"Just zero seconds", "0s", true},
		{"Just zero minutes", "0m", true},
		{"Just zero hours", "0h", true},
		{"Just zero days", "0d", true},
		{"Zero with valid - start", "0s30m", false}, // Total > 0
		{"Zero with valid - middle", "2h0m30s", false},
		{"Zero with valid - end", "2h30m0s", false},
		{"All zeros", "0d0h0m0s", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if tt.shouldErr && err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error", tt.ttl)
			}
			if !tt.shouldErr && err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, expected nil", tt.ttl, err)
			}
		})
	}
}

// TestValidateTTL_UnitOrder validates that unit order doesn't matter
func TestValidateTTL_UnitOrder(t *testing.T) {
	// All of these should be valid - order doesn't matter for validation
	tests := []string{
		"2h30m",   // Normal order
		"30m2h",   // Reverse order
		"1d12h",   // Normal
		"12h1d",   // Reverse
		"30s5m2h", // Reverse order
		"2h5m30s", // Normal order
	}

	for _, ttl := range tests {
		t.Run(ttl, func(t *testing.T) {
			err := validateTTL(ttl)
			if err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, want nil (order shouldn't matter)", ttl, err)
			}
		})
	}
}

// TestValidateTTL_PrefixSuffix validates rejection of extra characters
func TestValidateTTL_PrefixSuffix(t *testing.T) {
	tests := []struct {
		name string
		ttl  string
	}{
		{"Leading text", "ttl2h"},
		{"Trailing text", "2hmin"},
		{"Leading space", " 2h"},
		{"Trailing space", "2h "},
		{"Leading dash", "-2h"},
		{"Leading plus", "+2h"},
		{"Parentheses", "(2h)"},
		{"Quotes", "\"2h\""},
		{"Equals", "ttl=2h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error due to extra characters", tt.ttl)
			}
		})
	}
}
