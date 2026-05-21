package sweep

import (
	"testing"
)

func TestGroupParamsByRegion(t *testing.T) {
	tests := []struct {
		name     string
		params   []map[string]interface{}
		defaults map[string]interface{}
		expected map[string][]int
	}{
		{
			name: "All params use default region",
			params: []map[string]interface{}{
				{"name": "job1"},
				{"name": "job2"},
				{"name": "job3"},
			},
			defaults: map[string]interface{}{
				"region": "us-east-1",
			},
			expected: map[string][]int{
				"us-east-1": {0, 1, 2},
			},
		},
		{
			name: "Mixed regions with default",
			params: []map[string]interface{}{
				{"name": "job1", "region": "us-west-2"},
				{"name": "job2"},
				{"name": "job3", "region": "eu-west-1"},
			},
			defaults: map[string]interface{}{
				"region": "us-east-1",
			},
			expected: map[string][]int{
				"us-west-2": {0},
				"us-east-1": {1},
				"eu-west-1": {2},
			},
		},
		{
			name: "No default region (uses fallback)",
			params: []map[string]interface{}{
				{"name": "job1"},
				{"name": "job2"},
			},
			defaults: map[string]interface{}{},
			expected: map[string][]int{
				"us-east-1": {0, 1},
			},
		},
		{
			name: "All params override region",
			params: []map[string]interface{}{
				{"name": "job1", "region": "us-west-2"},
				{"name": "job2", "region": "us-west-2"},
				{"name": "job3", "region": "eu-west-1"},
			},
			defaults: map[string]interface{}{
				"region": "us-east-1",
			},
			expected: map[string][]int{
				"us-west-2": {0, 1},
				"eu-west-1": {2},
			},
		},
		{
			name: "Empty region string in param uses default",
			params: []map[string]interface{}{
				{"name": "job1", "region": ""},
				{"name": "job2"},
			},
			defaults: map[string]interface{}{
				"region": "ap-northeast-1",
			},
			expected: map[string][]int{
				"ap-northeast-1": {0, 1},
			},
		},
		{
			name:   "Empty params",
			params: []map[string]interface{}{},
			defaults: map[string]interface{}{
				"region": "us-east-1",
			},
			expected: map[string][]int{},
		},
		{
			name: "Multiple regions with many params",
			params: []map[string]interface{}{
				{"name": "job1", "region": "us-east-1"},
				{"name": "job2", "region": "us-east-1"},
				{"name": "job3", "region": "us-west-2"},
				{"name": "job4", "region": "us-west-2"},
				{"name": "job5", "region": "eu-west-1"},
				{"name": "job6"},
			},
			defaults: map[string]interface{}{
				"region": "ap-northeast-1",
			},
			expected: map[string][]int{
				"us-east-1":      {0, 1},
				"us-west-2":      {2, 3},
				"eu-west-1":      {4},
				"ap-northeast-1": {5},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GroupParamsByRegion(tt.params, tt.defaults)

			// Check that all expected regions are present
			if len(result) != len(tt.expected) {
				t.Errorf("expected %d regions, got %d", len(tt.expected), len(result))
			}

			// Check each region's indices
			for region, expectedIndices := range tt.expected {
				actualIndices, ok := result[region]
				if !ok {
					t.Errorf("expected region %s not found in result", region)
					continue
				}

				if len(actualIndices) != len(expectedIndices) {
					t.Errorf("region %s: expected %d indices, got %d", region, len(expectedIndices), len(actualIndices))
					continue
				}

				for i, expectedIdx := range expectedIndices {
					if actualIndices[i] != expectedIdx {
						t.Errorf("region %s: expected index %d at position %d, got %d", region, expectedIdx, i, actualIndices[i])
					}
				}
			}
		})
	}
}

func TestGetStringValue(t *testing.T) {
	tests := []struct {
		name         string
		m            map[string]interface{}
		key          string
		defaultValue string
		expected     string
	}{
		{
			name: "Key exists with string value",
			m: map[string]interface{}{
				"region": "us-east-1",
			},
			key:          "region",
			defaultValue: "default",
			expected:     "us-east-1",
		},
		{
			name: "Key does not exist",
			m: map[string]interface{}{
				"region": "us-east-1",
			},
			key:          "instance_type",
			defaultValue: "t3.micro",
			expected:     "t3.micro",
		},
		{
			name: "Key exists with non-string value",
			m: map[string]interface{}{
				"count": 42,
			},
			key:          "count",
			defaultValue: "default",
			expected:     "default",
		},
		{
			name: "Key exists with empty string",
			m: map[string]interface{}{
				"region": "",
			},
			key:          "region",
			defaultValue: "us-east-1",
			expected:     "",
		},
		{
			name:         "Empty map",
			m:            map[string]interface{}{},
			key:          "region",
			defaultValue: "us-east-1",
			expected:     "us-east-1",
		},
		{
			name: "Key exists with nil value",
			m: map[string]interface{}{
				"region": nil,
			},
			key:          "region",
			defaultValue: "us-east-1",
			expected:     "us-east-1",
		},
		{
			name: "Key exists with boolean value",
			m: map[string]interface{}{
				"flag": true,
			},
			key:          "flag",
			defaultValue: "default",
			expected:     "default",
		},
		{
			name: "Key exists with float value",
			m: map[string]interface{}{
				"price": 3.14,
			},
			key:          "price",
			defaultValue: "0.0",
			expected:     "0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getStringValue(tt.m, tt.key, tt.defaultValue)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}
