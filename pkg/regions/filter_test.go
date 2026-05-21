package regions

import (
	"testing"
)

func TestApplyConstraints(t *testing.T) {
	allRegions := []string{
		"us-east-1", "us-west-2", "eu-west-1", "eu-central-1",
		"ap-southeast-1", "ap-northeast-1",
	}

	tests := []struct {
		name        string
		constraint  *RegionConstraint
		expected    []string
		expectError bool
	}{
		{
			name:       "nil constraint",
			constraint: nil,
			expected:   allRegions,
		},
		{
			name:       "empty constraint",
			constraint: &RegionConstraint{},
			expected:   allRegions,
		},
		{
			name: "include us regions",
			constraint: &RegionConstraint{
				Include: []string{"us-*"},
			},
			expected: []string{"us-east-1", "us-west-2"},
		},
		{
			name: "include specific regions",
			constraint: &RegionConstraint{
				Include: []string{"us-east-1", "eu-west-1"},
			},
			expected: []string{"us-east-1", "eu-west-1"},
		},
		{
			name: "exclude eu regions",
			constraint: &RegionConstraint{
				Exclude: []string{"eu-*"},
			},
			expected: []string{"us-east-1", "us-west-2", "ap-southeast-1", "ap-northeast-1"},
		},
		{
			name: "geographic us",
			constraint: &RegionConstraint{
				Geographic: []string{"us"},
			},
			expected: []string{"us-east-1", "us-west-2"},
		},
		{
			name: "geographic eu",
			constraint: &RegionConstraint{
				Geographic: []string{"eu"},
			},
			expected: []string{"eu-west-1", "eu-central-1"},
		},
		{
			name: "geographic ap",
			constraint: &RegionConstraint{
				Geographic: []string{"ap"},
			},
			expected: []string{"ap-southeast-1", "ap-northeast-1"},
		},
		{
			name: "combined: include us, exclude us-east-1",
			constraint: &RegionConstraint{
				Include: []string{"us-*"},
				Exclude: []string{"us-east-1"},
			},
			expected: []string{"us-west-2"},
		},
		{
			name: "combined: geographic eu, exclude eu-central-*",
			constraint: &RegionConstraint{
				Geographic: []string{"eu"},
				Exclude:    []string{"eu-central-*"},
			},
			expected: []string{"eu-west-1"},
		},
		{
			name: "empty result",
			constraint: &RegionConstraint{
				Include:    []string{"us-east-1"},
				Exclude:    []string{"us-east-1"},
				AllowEmpty: false,
			},
			expectError: true,
		},
		{
			name: "empty result allowed",
			constraint: &RegionConstraint{
				Include:    []string{"us-east-1"},
				Exclude:    []string{"us-east-1"},
				AllowEmpty: true,
			},
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ApplyConstraints(allRegions, tt.constraint)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if !equalSlices(result, tt.expected) {
				t.Errorf("got %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestMatchWildcard(t *testing.T) {
	tests := []struct {
		s        string
		pattern  string
		expected bool
	}{
		{"us-east-1", "us-east-1", true},
		{"us-east-1", "us-*", true},
		{"us-east-1", "eu-*", false},
		{"eu-central-1", "eu-*", true},
		{"ap-southeast-1", "ap-*", true},
		{"us-east-1", "*-1", true},
		{"us-east-2", "*-1", false},
		{"us-west-2", "us-*-2", false}, // Only prefix/suffix wildcards supported
	}

	for _, tt := range tests {
		t.Run(tt.s+" matches "+tt.pattern, func(t *testing.T) {
			result := matchWildcard(tt.s, tt.pattern)
			if result != tt.expected {
				t.Errorf("matchWildcard(%q, %q) = %v, want %v",
					tt.s, tt.pattern, result, tt.expected)
			}
		})
	}
}

func TestFilterInclude(t *testing.T) {
	regions := []string{"us-east-1", "us-west-2", "eu-west-1", "ap-southeast-1"}

	tests := []struct {
		name     string
		patterns []string
		expected []string
	}{
		{
			name:     "single exact match",
			patterns: []string{"us-east-1"},
			expected: []string{"us-east-1"},
		},
		{
			name:     "wildcard match",
			patterns: []string{"us-*"},
			expected: []string{"us-east-1", "us-west-2"},
		},
		{
			name:     "multiple patterns",
			patterns: []string{"us-east-1", "eu-*"},
			expected: []string{"us-east-1", "eu-west-1"},
		},
		{
			name:     "no matches",
			patterns: []string{"sa-*"},
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterInclude(regions, tt.patterns)
			if !equalSlices(result, tt.expected) {
				t.Errorf("got %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestFilterExclude(t *testing.T) {
	regions := []string{"us-east-1", "us-west-2", "eu-west-1", "ap-southeast-1"}

	tests := []struct {
		name     string
		patterns []string
		expected []string
	}{
		{
			name:     "single exact match",
			patterns: []string{"us-east-1"},
			expected: []string{"us-west-2", "eu-west-1", "ap-southeast-1"},
		},
		{
			name:     "wildcard match",
			patterns: []string{"us-*"},
			expected: []string{"eu-west-1", "ap-southeast-1"},
		},
		{
			name:     "multiple patterns",
			patterns: []string{"us-*", "eu-*"},
			expected: []string{"ap-southeast-1"},
		},
		{
			name:     "no matches",
			patterns: []string{"sa-*"},
			expected: regions,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterExclude(regions, tt.patterns)
			if !equalSlices(result, tt.expected) {
				t.Errorf("got %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestFilterGeographic(t *testing.T) {
	regions := []string{
		"us-east-1", "us-west-2",
		"eu-west-1", "eu-central-1",
		"ap-southeast-1", "ap-northeast-1",
	}

	tests := []struct {
		name     string
		groups   []string
		expected []string
	}{
		{
			name:     "single group - us",
			groups:   []string{"us"},
			expected: []string{"us-east-1", "us-west-2"},
		},
		{
			name:     "single group - eu",
			groups:   []string{"eu"},
			expected: []string{"eu-west-1", "eu-central-1"},
		},
		{
			name:     "single group - ap",
			groups:   []string{"ap"},
			expected: []string{"ap-southeast-1", "ap-northeast-1"},
		},
		{
			name:     "multiple groups",
			groups:   []string{"us", "eu"},
			expected: []string{"us-east-1", "us-west-2", "eu-west-1", "eu-central-1"},
		},
		{
			name:     "invalid group",
			groups:   []string{"invalid"},
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterGeographic(regions, tt.groups)
			if !equalSlices(result, tt.expected) {
				t.Errorf("got %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestValidateConstraint(t *testing.T) {
	tests := []struct {
		name        string
		constraint  *RegionConstraint
		expectError bool
	}{
		{
			name:        "nil constraint",
			constraint:  nil,
			expectError: false,
		},
		{
			name:        "empty constraint",
			constraint:  &RegionConstraint{},
			expectError: false,
		},
		{
			name: "valid cost tier - low",
			constraint: &RegionConstraint{
				CostTier: "low",
			},
			expectError: false,
		},
		{
			name: "valid cost tier - standard",
			constraint: &RegionConstraint{
				CostTier: "standard",
			},
			expectError: false,
		},
		{
			name: "valid cost tier - premium",
			constraint: &RegionConstraint{
				CostTier: "premium",
			},
			expectError: false,
		},
		{
			name: "invalid cost tier",
			constraint: &RegionConstraint{
				CostTier: "expensive",
			},
			expectError: true,
		},
		{
			name: "valid proximity region",
			constraint: &RegionConstraint{
				ProximityFrom: "us-east-1",
			},
			expectError: false,
		},
		{
			name: "invalid proximity region",
			constraint: &RegionConstraint{
				ProximityFrom: "invalid-region",
			},
			expectError: true,
		},
		{
			name: "valid geographic group",
			constraint: &RegionConstraint{
				Geographic: []string{"us", "eu"},
			},
			expectError: false,
		},
		{
			name: "invalid geographic group",
			constraint: &RegionConstraint{
				Geographic: []string{"invalid"},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateConstraint(tt.constraint)

			if tt.expectError && err == nil {
				t.Errorf("expected error but got none")
			}

			if !tt.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// Helper function to compare slices ignoring order
func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	aMap := make(map[string]bool)
	for _, item := range a {
		aMap[item] = true
	}

	for _, item := range b {
		if !aMap[item] {
			return false
		}
	}

	return true
}
