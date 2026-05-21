package availability

import (
	"testing"
	"time"
)

func TestMakeStatID(t *testing.T) {
	tests := []struct {
		name         string
		region       string
		instanceType string
		expected     string
	}{
		{
			name:         "Basic stat ID",
			region:       "us-east-1",
			instanceType: "c5.xlarge",
			expected:     "us-east-1#c5.xlarge",
		},
		{
			name:         "Different region",
			region:       "eu-west-1",
			instanceType: "m5.large",
			expected:     "eu-west-1#m5.large",
		},
		{
			name:         "Large instance type",
			region:       "ap-northeast-1",
			instanceType: "p4d.24xlarge",
			expected:     "ap-northeast-1#p4d.24xlarge",
		},
		{
			name:         "Metal instance",
			region:       "us-west-2",
			instanceType: "c5n.metal",
			expected:     "us-west-2#c5n.metal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := makeStatID(tt.region, tt.instanceType)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestCalculateBackoff(t *testing.T) {
	tests := []struct {
		name                string
		consecutiveFailures int
		expected            time.Duration
	}{
		{
			name:                "Zero failures",
			consecutiveFailures: 0,
			expected:            0,
		},
		{
			name:                "First failure",
			consecutiveFailures: 1,
			expected:            5 * time.Minute, // backoffInitial
		},
		{
			name:                "Second failure",
			consecutiveFailures: 2,
			expected:            10 * time.Minute, // 5 * 2^1
		},
		{
			name:                "Third failure",
			consecutiveFailures: 3,
			expected:            20 * time.Minute, // 5 * 2^2
		},
		{
			name:                "Fourth failure",
			consecutiveFailures: 4,
			expected:            40 * time.Minute, // 5 * 2^3
		},
		{
			name:                "Fifth failure (capped at max)",
			consecutiveFailures: 5,
			expected:            60 * time.Minute, // 5 * 2^4 = 80, but capped at 60
		},
		{
			name:                "Many failures (capped at max)",
			consecutiveFailures: 10,
			expected:            60 * time.Minute, // Capped at backoffMax
		},
		{
			name:                "Negative failures (edge case)",
			consecutiveFailures: -1,
			expected:            0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateBackoff(tt.consecutiveFailures)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestCalculateScore(t *testing.T) {
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour).Format(time.RFC3339)
	oneDayAgo := now.Add(-24 * time.Hour).Format(time.RFC3339)
	oneWeekAgo := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	tests := []struct {
		name     string
		stats    *AvailabilityStats
		expected float64
		epsilon  float64 // Allow small floating point differences
	}{
		{
			name: "No data (neutral score)",
			stats: &AvailabilityStats{
				SuccessCount: 0,
				FailureCount: 0,
			},
			expected: 0.5,
			epsilon:  0.0,
		},
		{
			name: "Perfect success rate (100%)",
			stats: &AvailabilityStats{
				SuccessCount: 100,
				FailureCount: 0,
				LastSuccess:  now.Format(time.RFC3339),
			},
			expected: 1.0,
			epsilon:  0.01,
		},
		{
			name: "Perfect failure rate (0%)",
			stats: &AvailabilityStats{
				SuccessCount: 0,
				FailureCount: 100,
			},
			expected: 0.0,
			epsilon:  0.01,
		},
		{
			name: "50% success rate",
			stats: &AvailabilityStats{
				SuccessCount: 50,
				FailureCount: 50,
				LastSuccess:  now.Format(time.RFC3339),
			},
			expected: 0.5,
			epsilon:  0.01,
		},
		{
			name: "High success rate with recent success",
			stats: &AvailabilityStats{
				SuccessCount: 90,
				FailureCount: 10,
				LastSuccess:  oneHourAgo,
			},
			expected: 0.88, // ~90% * (0.8 + 0.2 * high_recency)
			epsilon:  0.05,
		},
		{
			name: "High success rate with old success",
			stats: &AvailabilityStats{
				SuccessCount: 90,
				FailureCount: 10,
				LastSuccess:  oneWeekAgo,
			},
			expected: 0.72, // ~90% * (0.8 + 0.2 * low_recency)
			epsilon:  0.05,
		},
		{
			name: "Success rate with no last_success timestamp",
			stats: &AvailabilityStats{
				SuccessCount: 80,
				FailureCount: 20,
				LastSuccess:  "",
			},
			expected: 0.80, // 80% * (0.8 + 0.2 * 1.0) - recency defaults to 1.0
			epsilon:  0.01,
		},
		{
			name: "Invalid last_success timestamp (treated as no timestamp)",
			stats: &AvailabilityStats{
				SuccessCount: 80,
				FailureCount: 20,
				LastSuccess:  "invalid",
			},
			expected: 0.80, // 80% * (0.8 + 0.2 * 1.0) - recency defaults to 1.0
			epsilon:  0.01,
		},
		{
			name: "Low success rate with recent success",
			stats: &AvailabilityStats{
				SuccessCount: 20,
				FailureCount: 80,
				LastSuccess:  now.Format(time.RFC3339),
			},
			expected: 0.2, // ~20% * (0.8 + 0.2 * 1.0)
			epsilon:  0.01,
		},
		{
			name: "One success, many failures",
			stats: &AvailabilityStats{
				SuccessCount: 1,
				FailureCount: 99,
				LastSuccess:  oneDayAgo,
			},
			expected: 0.01, // ~1% with minimal recency bonus
			epsilon:  0.02,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculateScore(tt.stats)
			diff := result - tt.expected
			if diff < 0 {
				diff = -diff
			}
			if diff > tt.epsilon {
				t.Errorf("expected %v (±%v), got %v (diff: %v)", tt.expected, tt.epsilon, result, diff)
			}
		})
	}
}

func TestCalculateScore_Boundaries(t *testing.T) {
	tests := []struct {
		name        string
		stats       *AvailabilityStats
		minExpected float64
		maxExpected float64
	}{
		{
			name: "Score should be between 0 and 1",
			stats: &AvailabilityStats{
				SuccessCount: 100,
				FailureCount: 0,
				LastSuccess:  time.Now().Format(time.RFC3339),
			},
			minExpected: 0.0,
			maxExpected: 1.0,
		},
		{
			name: "Score with very old success should not go negative",
			stats: &AvailabilityStats{
				SuccessCount: 50,
				FailureCount: 50,
				LastSuccess:  time.Now().Add(-365 * 24 * time.Hour).Format(time.RFC3339),
			},
			minExpected: 0.0,
			maxExpected: 1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculateScore(tt.stats)
			if result < tt.minExpected || result > tt.maxExpected {
				t.Errorf("score %v is outside expected range [%v, %v]", result, tt.minExpected, tt.maxExpected)
			}
		})
	}
}
