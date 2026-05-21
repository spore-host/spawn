package cmd

import (
	"testing"
)

// Test launch.go utility functions

func TestFormatInstanceName(t *testing.T) {
	tests := []struct {
		name         string
		template     string
		jobArrayName string
		index        int
		expected     string
	}{
		{
			name:         "Default template",
			template:     "",
			jobArrayName: "compute",
			index:        0,
			expected:     "compute-0",
		},
		{
			name:         "Default template with different name",
			template:     "",
			jobArrayName: "my-job",
			index:        5,
			expected:     "my-job-5",
		},
		{
			name:         "Custom template with index only",
			template:     "worker-{index}",
			jobArrayName: "compute",
			index:        3,
			expected:     "worker-3",
		},
		{
			name:         "Custom template with job-array-name only",
			template:     "instance-{job-array-name}",
			jobArrayName: "batch-job",
			index:        0,
			expected:     "instance-batch-job",
		},
		{
			name:         "Custom template with both variables",
			template:     "{job-array-name}-node-{index}",
			jobArrayName: "mpi-cluster",
			index:        7,
			expected:     "mpi-cluster-node-7",
		},
		{
			name:         "Template with multiple variable occurrences",
			template:     "{index}-{job-array-name}-{index}",
			jobArrayName: "test",
			index:        2,
			expected:     "2-test-2",
		},
		{
			name:         "Template with no variables",
			template:     "static-name",
			jobArrayName: "compute",
			index:        0,
			expected:     "static-name",
		},
		{
			name:         "Template with extra text",
			template:     "prod-{job-array-name}-instance-{index}-ec2",
			jobArrayName: "api",
			index:        12,
			expected:     "prod-api-instance-12-ec2",
		},
		{
			name:         "Large index number",
			template:     "{job-array-name}-{index}",
			jobArrayName: "big-job",
			index:        999,
			expected:     "big-job-999",
		},
		{
			name:         "Zero index",
			template:     "{job-array-name}-{index}",
			jobArrayName: "job",
			index:        0,
			expected:     "job-0",
		},
		{
			name:         "Job array name with hyphens",
			template:     "",
			jobArrayName: "my-complex-job-name",
			index:        1,
			expected:     "my-complex-job-name-1",
		},
		{
			name:         "Template with literal braces",
			template:     "worker-{index}-{{debug}}",
			jobArrayName: "test",
			index:        5,
			expected:     "worker-5-{{debug}}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatInstanceName(tt.template, tt.jobArrayName, tt.index)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestParseIAMRoleTags(t *testing.T) {
	tests := []struct {
		name     string
		tags     []string
		expected map[string]string
	}{
		{
			name: "Single tag",
			tags: []string{"Environment=production"},
			expected: map[string]string{
				"Environment": "production",
			},
		},
		{
			name: "Multiple tags",
			tags: []string{
				"Environment=production",
				"Team=backend",
				"Owner=alice",
			},
			expected: map[string]string{
				"Environment": "production",
				"Team":        "backend",
				"Owner":       "alice",
			},
		},
		{
			name:     "Empty tags list",
			tags:     []string{},
			expected: map[string]string{},
		},
		{
			name:     "Nil tags list",
			tags:     nil,
			expected: map[string]string{},
		},
		{
			name: "Tag with equals in value",
			tags: []string{"Formula=E=mc^2"},
			expected: map[string]string{
				"Formula": "E=mc^2",
			},
		},
		{
			name: "Tag with empty value",
			tags: []string{"EmptyTag="},
			expected: map[string]string{
				"EmptyTag": "",
			},
		},
		{
			name: "Invalid tag (no equals sign)",
			tags: []string{
				"ValidTag=value",
				"InvalidTag",
				"AnotherValid=test",
			},
			expected: map[string]string{
				"ValidTag":     "value",
				"AnotherValid": "test",
				// InvalidTag is skipped
			},
		},
		{
			name: "Tag with spaces in value",
			tags: []string{"Description=This is a description"},
			expected: map[string]string{
				"Description": "This is a description",
			},
		},
		{
			name: "Tag with special characters",
			tags: []string{
				"URL=https://example.com",
				"Email=user@example.com",
				"Path=/var/log/app.log",
			},
			expected: map[string]string{
				"URL":   "https://example.com",
				"Email": "user@example.com",
				"Path":  "/var/log/app.log",
			},
		},
		{
			name: "Duplicate keys (last one wins)",
			tags: []string{
				"Environment=dev",
				"Environment=prod",
			},
			expected: map[string]string{
				"Environment": "prod",
			},
		},
		{
			name: "Tag with only equals sign",
			tags: []string{"="},
			expected: map[string]string{
				"": "",
			},
		},
		{
			name: "Tag with key but no value after equals",
			tags: []string{"Key="},
			expected: map[string]string{
				"Key": "",
			},
		},
		{
			name: "Multiple equals signs",
			tags: []string{"Math=1+1=2"},
			expected: map[string]string{
				"Math": "1+1=2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseIAMRoleTags(tt.tags)

			if len(result) != len(tt.expected) {
				t.Errorf("expected %d tags, got %d", len(tt.expected), len(result))
			}

			for key, expectedValue := range tt.expected {
				actualValue, exists := result[key]
				if !exists {
					t.Errorf("expected key %q not found in result", key)
					continue
				}
				if actualValue != expectedValue {
					t.Errorf("for key %q: expected %q, got %q", key, expectedValue, actualValue)
				}
			}

			// Check for unexpected keys
			for key := range result {
				if _, expected := tt.expected[key]; !expected {
					t.Errorf("unexpected key %q in result with value %q", key, result[key])
				}
			}
		})
	}
}

func TestMatchesContinent(t *testing.T) {
	tests := []struct {
		name          string
		region        string
		continentCode string
		expected      bool
	}{
		// North America
		{
			name:          "US East matches North America",
			region:        "us-east-1",
			continentCode: "NA",
			expected:      true,
		},
		{
			name:          "US West matches North America",
			region:        "us-west-2",
			continentCode: "NA",
			expected:      true,
		},
		{
			name:          "Canada matches North America",
			region:        "ca-central-1",
			continentCode: "NA",
			expected:      true,
		},
		{
			name:          "US region does not match Europe",
			region:        "us-east-1",
			continentCode: "EU",
			expected:      false,
		},
		// Europe
		{
			name:          "EU West matches Europe",
			region:        "eu-west-1",
			continentCode: "EU",
			expected:      true,
		},
		{
			name:          "EU Central matches Europe",
			region:        "eu-central-1",
			continentCode: "EU",
			expected:      true,
		},
		{
			name:          "EU region does not match Asia",
			region:        "eu-west-1",
			continentCode: "AS",
			expected:      false,
		},
		// Asia
		{
			name:          "AP Southeast matches Asia",
			region:        "ap-southeast-1",
			continentCode: "AS",
			expected:      true,
		},
		{
			name:          "AP Northeast matches Asia",
			region:        "ap-northeast-1",
			continentCode: "AS",
			expected:      true,
		},
		{
			name:          "AP South (India) matches Asia",
			region:        "ap-south-1",
			continentCode: "AS",
			expected:      true,
		},
		{
			name:          "Middle East matches Asia",
			region:        "me-south-1",
			continentCode: "AS",
			expected:      true,
		},
		{
			name:          "Israel matches Asia",
			region:        "il-central-1",
			continentCode: "AS",
			expected:      true,
		},
		// South America
		{
			name:          "SA East matches South America",
			region:        "sa-east-1",
			continentCode: "SA",
			expected:      true,
		},
		{
			name:          "SA region does not match North America",
			region:        "sa-east-1",
			continentCode: "NA",
			expected:      false,
		},
		// Africa
		{
			name:          "AF South matches Africa",
			region:        "af-south-1",
			continentCode: "AF",
			expected:      true,
		},
		// Empty/Unknown
		{
			name:          "Empty continent code returns false",
			region:        "us-east-1",
			continentCode: "",
			expected:      false,
		},
		{
			name:          "Unknown region returns false",
			region:        "unknown-region-1",
			continentCode: "NA",
			expected:      false,
		},
		{
			name:          "Empty region returns false",
			region:        "",
			continentCode: "NA",
			expected:      false,
		},
		// Edge cases
		{
			name:          "Region with prefix match (ap-south)",
			region:        "ap-south-1",
			continentCode: "AS",
			expected:      true,
		},
		{
			name:          "Partial prefix does not match",
			region:        "uswest-1", // Missing hyphen
			continentCode: "NA",
			expected:      false,
		},
		{
			name:          "Case sensitive matching",
			region:        "US-EAST-1", // Different case
			continentCode: "NA",
			expected:      false, // Should be false since prefix check is case-sensitive
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := matchesContinent(tt.region, tt.continentCode)
			if result != tt.expected {
				t.Errorf("matchesContinent(%q, %q) = %v, expected %v",
					tt.region, tt.continentCode, result, tt.expected)
			}
		})
	}
}

func TestMatchesContinent_AllContinents(t *testing.T) {
	// Test that each continent code works with at least one region
	continentTests := map[string][]string{
		"NA": {"us-east-1", "us-west-2", "ca-central-1"},
		"EU": {"eu-west-1", "eu-central-1", "eu-north-1"},
		"AS": {"ap-southeast-1", "ap-northeast-1", "me-south-1", "il-central-1"},
		"SA": {"sa-east-1"},
		"AF": {"af-south-1"},
	}

	for continent, regions := range continentTests {
		for _, region := range regions {
			if !matchesContinent(region, continent) {
				t.Errorf("Expected %s to match continent %s", region, continent)
			}
		}
	}
}
