package cmd

import (
	"testing"
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Test completion.go utility functions

func TestStringPtr(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Non-empty string",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "Empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "String with spaces",
			input:    "hello world",
			expected: "hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stringPtr(tt.input)
			if result == nil {
				t.Fatal("expected non-nil pointer")
			}
			if *result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, *result)
			}
		})
	}
}

func TestStringValue(t *testing.T) {
	tests := []struct {
		name     string
		input    *string
		expected string
	}{
		{
			name:     "Non-nil pointer",
			input:    stringPtr("hello"),
			expected: "hello",
		},
		{
			name:     "Nil pointer",
			input:    nil,
			expected: "",
		},
		{
			name:     "Empty string pointer",
			input:    stringPtr(""),
			expected: "",
		},
		{
			name:     "String with special characters",
			input:    stringPtr("hello-world_123"),
			expected: "hello-world_123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stringValue(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestGetInstanceName(t *testing.T) {
	tests := []struct {
		name     string
		tags     []ec2types.Tag
		expected string
	}{
		{
			name: "Has Name tag",
			tags: []ec2types.Tag{
				{Key: stringPtr("Name"), Value: stringPtr("my-instance")},
				{Key: stringPtr("Environment"), Value: stringPtr("prod")},
			},
			expected: "my-instance",
		},
		{
			name: "No Name tag",
			tags: []ec2types.Tag{
				{Key: stringPtr("Environment"), Value: stringPtr("prod")},
				{Key: stringPtr("Owner"), Value: stringPtr("alice")},
			},
			expected: "unnamed",
		},
		{
			name:     "Empty tags list",
			tags:     []ec2types.Tag{},
			expected: "unnamed",
		},
		{
			name:     "Nil tags list",
			tags:     nil,
			expected: "unnamed",
		},
		{
			name: "Name tag with empty value",
			tags: []ec2types.Tag{
				{Key: stringPtr("Name"), Value: stringPtr("")},
			},
			expected: "",
		},
		{
			name: "Multiple tags, Name is last",
			tags: []ec2types.Tag{
				{Key: stringPtr("Environment"), Value: stringPtr("dev")},
				{Key: stringPtr("Owner"), Value: stringPtr("bob")},
				{Key: stringPtr("Name"), Value: stringPtr("test-instance")},
			},
			expected: "test-instance",
		},
		{
			name: "Name tag with nil value",
			tags: []ec2types.Tag{
				{Key: stringPtr("Name"), Value: nil},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getInstanceName(tt.tags)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestGetTagValue(t *testing.T) {
	tests := []struct {
		name     string
		tags     []ec2types.Tag
		key      string
		expected string
	}{
		{
			name: "Tag exists",
			tags: []ec2types.Tag{
				{Key: stringPtr("Environment"), Value: stringPtr("production")},
				{Key: stringPtr("Owner"), Value: stringPtr("alice")},
			},
			key:      "Environment",
			expected: "production",
		},
		{
			name: "Tag does not exist",
			tags: []ec2types.Tag{
				{Key: stringPtr("Environment"), Value: stringPtr("production")},
			},
			key:      "Owner",
			expected: "",
		},
		{
			name:     "Empty tags list",
			tags:     []ec2types.Tag{},
			key:      "Environment",
			expected: "",
		},
		{
			name: "Tag with empty value",
			tags: []ec2types.Tag{
				{Key: stringPtr("Environment"), Value: stringPtr("")},
			},
			key:      "Environment",
			expected: "",
		},
		{
			name: "Multiple tags with same key (returns first)",
			tags: []ec2types.Tag{
				{Key: stringPtr("Owner"), Value: stringPtr("alice")},
				{Key: stringPtr("Owner"), Value: stringPtr("bob")},
			},
			key:      "Owner",
			expected: "alice",
		},
		{
			name: "Case-sensitive key matching",
			tags: []ec2types.Tag{
				{Key: stringPtr("environment"), Value: stringPtr("dev")},
			},
			key:      "Environment",
			expected: "",
		},
		{
			name: "Tag with nil value",
			tags: []ec2types.Tag{
				{Key: stringPtr("Owner"), Value: nil},
			},
			key:      "Owner",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getTagValue(tt.tags, tt.key)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// Test dns.go utility functions

func TestIsValidDNSName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{
			name:     "Valid lowercase alphanumeric",
			input:    "myinstance",
			expected: true,
		},
		{
			name:     "Valid with hyphens",
			input:    "my-instance-01",
			expected: true,
		},
		{
			name:     "Valid with numbers",
			input:    "instance123",
			expected: true,
		},
		{
			name:     "Invalid with uppercase",
			input:    "MyInstance",
			expected: false,
		},
		{
			name:     "Invalid with underscore",
			input:    "my_instance",
			expected: false,
		},
		{
			name:     "Invalid with dot",
			input:    "my.instance",
			expected: false,
		},
		{
			name:     "Invalid with space",
			input:    "my instance",
			expected: false,
		},
		{
			name:     "Empty string",
			input:    "",
			expected: false,
		},
		{
			name:     "Single character valid",
			input:    "a",
			expected: true,
		},
		{
			name:     "Single hyphen",
			input:    "-",
			expected: true,
		},
		{
			name:     "All hyphens",
			input:    "---",
			expected: true,
		},
		{
			name:     "Numbers only",
			input:    "12345",
			expected: true,
		},
		{
			name:     "Invalid special character",
			input:    "instance@123",
			expected: false,
		},
		{
			name:     "Valid long name",
			input:    "my-very-long-instance-name-with-many-hyphens-123",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isValidDNSName(tt.input)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestValueOrDash(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Non-empty string",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "Empty string",
			input:    "",
			expected: "-",
		},
		{
			name:     "String with spaces",
			input:    "hello world",
			expected: "hello world",
		},
		{
			name:     "Single space",
			input:    " ",
			expected: " ",
		},
		{
			name:     "Dash character",
			input:    "-",
			expected: "-",
		},
		{
			name:     "Multiple dashes",
			input:    "---",
			expected: "---",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := valueOrDash(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// Test collect.go utility functions

func TestToFloat64(t *testing.T) {
	tests := []struct {
		name        string
		input       interface{}
		expectedVal float64
		expectedOk  bool
	}{
		{
			name:        "float64 value",
			input:       float64(3.14),
			expectedVal: 3.14,
			expectedOk:  true,
		},
		{
			name:        "float32 value",
			input:       float32(2.5),
			expectedVal: 2.5,
			expectedOk:  true,
		},
		{
			name:        "int value",
			input:       int(42),
			expectedVal: 42.0,
			expectedOk:  true,
		},
		{
			name:        "int64 value",
			input:       int64(100),
			expectedVal: 100.0,
			expectedOk:  true,
		},
		{
			name:        "int32 value",
			input:       int32(50),
			expectedVal: 50.0,
			expectedOk:  true,
		},
		{
			name:        "string value (not convertible)",
			input:       "hello",
			expectedVal: 0,
			expectedOk:  false,
		},
		{
			name:        "bool value (not convertible)",
			input:       true,
			expectedVal: 0,
			expectedOk:  false,
		},
		{
			name:        "nil value",
			input:       nil,
			expectedVal: 0,
			expectedOk:  false,
		},
		{
			name:        "Zero float64",
			input:       float64(0),
			expectedVal: 0,
			expectedOk:  true,
		},
		{
			name:        "Negative float64",
			input:       float64(-3.14),
			expectedVal: -3.14,
			expectedOk:  true,
		},
		{
			name:        "Negative int",
			input:       int(-42),
			expectedVal: -42.0,
			expectedOk:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, ok := toFloat64(tt.input)
			if ok != tt.expectedOk {
				t.Errorf("expected ok=%v, got ok=%v", tt.expectedOk, ok)
			}
			if val != tt.expectedVal {
				t.Errorf("expected val=%v, got val=%v", tt.expectedVal, val)
			}
		})
	}
}

func TestSortResultsByMetric(t *testing.T) {
	tests := []struct {
		name     string
		results  []SweepResult
		metric   string
		expected []string // Expected order by SweepID
	}{
		{
			name: "Sort by float64 metric (descending)",
			results: []SweepResult{
				{SweepID: "job1", Metrics: map[string]interface{}{"score": float64(5.5)}},
				{SweepID: "job2", Metrics: map[string]interface{}{"score": float64(10.2)}},
				{SweepID: "job3", Metrics: map[string]interface{}{"score": float64(3.1)}},
			},
			metric:   "score",
			expected: []string{"job2", "job1", "job3"},
		},
		{
			name: "Sort by int metric (descending)",
			results: []SweepResult{
				{SweepID: "job1", Metrics: map[string]interface{}{"count": int(50)}},
				{SweepID: "job2", Metrics: map[string]interface{}{"count": int(100)}},
				{SweepID: "job3", Metrics: map[string]interface{}{"count": int(25)}},
			},
			metric:   "count",
			expected: []string{"job2", "job1", "job3"},
		},
		{
			name: "Sort by mixed numeric types",
			results: []SweepResult{
				{SweepID: "job1", Metrics: map[string]interface{}{"value": float64(5.5)}},
				{SweepID: "job2", Metrics: map[string]interface{}{"value": int(10)}},
				{SweepID: "job3", Metrics: map[string]interface{}{"value": int64(3)}},
			},
			metric:   "value",
			expected: []string{"job2", "job1", "job3"},
		},
		{
			name: "Sort by string metric (fallback to string comparison)",
			results: []SweepResult{
				{SweepID: "job1", Metrics: map[string]interface{}{"name": "alpha"}},
				{SweepID: "job2", Metrics: map[string]interface{}{"name": "charlie"}},
				{SweepID: "job3", Metrics: map[string]interface{}{"name": "bravo"}},
			},
			metric:   "name",
			expected: []string{"job2", "job3", "job1"}, // "charlie" > "bravo" > "alpha" (descending)
		},
		{
			name:     "Empty results",
			results:  []SweepResult{},
			metric:   "score",
			expected: []string{},
		},
		{
			name: "Single result",
			results: []SweepResult{
				{SweepID: "job1", Metrics: map[string]interface{}{"score": float64(5.5)}},
			},
			metric:   "score",
			expected: []string{"job1"},
		},
		{
			name: "Equal values maintain stable order",
			results: []SweepResult{
				{SweepID: "job1", Metrics: map[string]interface{}{"score": float64(5.5)}},
				{SweepID: "job2", Metrics: map[string]interface{}{"score": float64(5.5)}},
				{SweepID: "job3", Metrics: map[string]interface{}{"score": float64(5.5)}},
			},
			metric:   "score",
			expected: []string{"job1", "job2", "job3"}, // Stable sort
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Make a copy to avoid modifying test data
			results := make([]SweepResult, len(tt.results))
			copy(results, tt.results)

			sortResultsByMetric(results, tt.metric)

			if len(results) != len(tt.expected) {
				t.Fatalf("expected %d results, got %d", len(tt.expected), len(results))
			}

			for i, expectedID := range tt.expected {
				if results[i].SweepID != expectedID {
					t.Errorf("position %d: expected %q, got %q", i, expectedID, results[i].SweepID)
				}
			}
		})
	}
}

func TestSortResultsByMetric_MissingMetric(t *testing.T) {
	results := []SweepResult{
		{
			SweepID:      "job1",
			SweepIndex:   1,
			InstanceID:   "i-123",
			Parameters:   map[string]interface{}{"param1": "value1"},
			Metrics:      map[string]interface{}{"score": float64(5.5)},
			DownloadedAt: time.Now(),
		},
		{
			SweepID:      "job2",
			SweepIndex:   2,
			InstanceID:   "i-456",
			Parameters:   map[string]interface{}{"param1": "value2"},
			Metrics:      map[string]interface{}{"score": float64(10.2)},
			DownloadedAt: time.Now(),
		},
	}

	// Should not panic when metric exists
	sortResultsByMetric(results, "score")

	// Should not panic when metric doesn't exist
	sortResultsByMetric(results, "nonexistent")
}
