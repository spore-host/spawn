package cmd

import (
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestFormatDuration validates duration formatting for various time ranges
func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"Seconds only", 45 * time.Second, "45s"},
		{"Minutes only", 25 * time.Minute, "25m"},
		{"Hours only", 3 * time.Hour, "3h"},
		{"Hours and minutes", 3*time.Hour + 30*time.Minute, "3h30m"},
		{"Days only", 5 * 24 * time.Hour, "5d"},
		{"Days and hours", 2*24*time.Hour + 6*time.Hour, "2d6h"},
		{"Just over 1 minute", 61 * time.Second, "1m"},
		{"Just over 1 hour", 61 * time.Minute, "1h1m"},
		{"Just over 1 day", 25 * time.Hour, "1d1h"},
		{"Exactly 1 hour", 60 * time.Minute, "1h"},
		{"Exactly 1 day", 24 * time.Hour, "1d"},
		{"1 week", 7 * 24 * time.Hour, "7d"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatDuration(tt.duration)
			if result != tt.expected {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.duration, result, tt.expected)
			}
		})
	}
}

// TestFilterInstancesByAZ validates availability zone filtering
func TestFilterInstancesByAZ(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-1", AvailabilityZone: "us-east-1a"},
		{InstanceID: "i-2", AvailabilityZone: "us-east-1b"},
		{InstanceID: "i-3", AvailabilityZone: "us-east-1a"},
	}

	// Set filter
	listAZ = "us-east-1a"
	defer func() { listAZ = "" }()

	filtered := filterInstances(instances)

	if len(filtered) != 2 {
		t.Errorf("Expected 2 instances, got %d", len(filtered))
	}

	for _, inst := range filtered {
		if inst.AvailabilityZone != "us-east-1a" {
			t.Errorf("Instance %s has wrong AZ: %s", inst.InstanceID, inst.AvailabilityZone)
		}
	}
}

// TestFilterInstancesByType validates exact instance type filtering
func TestFilterInstancesByType(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-1", InstanceType: "t3.micro"},
		{InstanceID: "i-2", InstanceType: "m7i.large"},
		{InstanceID: "i-3", InstanceType: "t3.micro"},
	}

	// Set filter
	listInstanceType = "t3.micro"
	defer func() { listInstanceType = "" }()

	filtered := filterInstances(instances)

	if len(filtered) != 2 {
		t.Errorf("Expected 2 instances, got %d", len(filtered))
	}

	for _, inst := range filtered {
		if inst.InstanceType != "t3.micro" {
			t.Errorf("Instance %s has wrong type: %s", inst.InstanceID, inst.InstanceType)
		}
	}
}

// TestFilterInstancesByFamily validates instance family prefix filtering
func TestFilterInstancesByFamily(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-1", InstanceType: "m7i.large"},
		{InstanceID: "i-2", InstanceType: "m7i.xlarge"},
		{InstanceID: "i-3", InstanceType: "t3.micro"},
		{InstanceID: "i-4", InstanceType: "m7i.2xlarge"},
	}

	// Set filter
	listInstanceFamily = "m7i"
	defer func() { listInstanceFamily = "" }()

	filtered := filterInstances(instances)

	if len(filtered) != 3 {
		t.Errorf("Expected 3 instances, got %d", len(filtered))
	}

	for _, inst := range filtered {
		if inst.InstanceType[:3] != "m7i" {
			t.Errorf("Instance %s has wrong family: %s", inst.InstanceID, inst.InstanceType)
		}
	}
}

// TestFilterInstancesByNameTag validates Name tag filtering
func TestFilterInstancesByNameTag(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-1", Name: "test-instance"},
		{InstanceID: "i-2", Name: "prod-instance"},
		{InstanceID: "i-3", Name: "test-instance"},
	}

	// Set filter
	listTag = []string{"Name=test-instance"}
	defer func() { listTag = []string{} }()

	filtered := filterInstances(instances)

	if len(filtered) != 2 {
		t.Errorf("Expected 2 instances, got %d", len(filtered))
	}

	for _, inst := range filtered {
		if inst.Name != "test-instance" {
			t.Errorf("Instance %s has wrong name: %s", inst.InstanceID, inst.Name)
		}
	}
}

// TestFilterInstancesByTTLTag validates TTL tag filtering
func TestFilterInstancesByTTLTag(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-1", TTL: "2h"},
		{InstanceID: "i-2", TTL: "4h"},
		{InstanceID: "i-3", TTL: "2h"},
	}

	// Set filter
	listTag = []string{"spawn:ttl=2h"}
	defer func() { listTag = []string{} }()

	filtered := filterInstances(instances)

	if len(filtered) != 2 {
		t.Errorf("Expected 2 instances, got %d", len(filtered))
	}

	for _, inst := range filtered {
		if inst.TTL != "2h" {
			t.Errorf("Instance %s has wrong TTL: %s", inst.InstanceID, inst.TTL)
		}
	}
}

// TestFilterInstancesByCustomTag validates custom tag filtering
func TestFilterInstancesByCustomTag(t *testing.T) {
	instances := []aws.InstanceInfo{
		{
			InstanceID: "i-1",
			Tags:       map[string]string{"env": "prod"},
		},
		{
			InstanceID: "i-2",
			Tags:       map[string]string{"env": "dev"},
		},
		{
			InstanceID: "i-3",
			Tags:       map[string]string{"env": "prod"},
		},
	}

	// Set filter
	listTag = []string{"env=prod"}
	defer func() { listTag = []string{} }()

	filtered := filterInstances(instances)

	if len(filtered) != 2 {
		t.Errorf("Expected 2 instances, got %d", len(filtered))
	}

	for _, inst := range filtered {
		if inst.Tags["env"] != "prod" {
			t.Errorf("Instance %s has wrong env: %s", inst.InstanceID, inst.Tags["env"])
		}
	}
}

// TestFilterInstancesMultipleTags validates multiple tag filters (AND logic)
func TestFilterInstancesMultipleTags(t *testing.T) {
	instances := []aws.InstanceInfo{
		{
			InstanceID: "i-1",
			Name:       "test",
			Tags:       map[string]string{"env": "prod"},
		},
		{
			InstanceID: "i-2",
			Name:       "test",
			Tags:       map[string]string{"env": "dev"},
		},
		{
			InstanceID: "i-3",
			Name:       "other",
			Tags:       map[string]string{"env": "prod"},
		},
	}

	// Set multiple filters (must match ALL)
	listTag = []string{"Name=test", "env=prod"}
	defer func() { listTag = []string{} }()

	filtered := filterInstances(instances)

	if len(filtered) != 1 {
		t.Errorf("Expected 1 instance, got %d", len(filtered))
	}

	if len(filtered) > 0 {
		if filtered[0].InstanceID != "i-1" {
			t.Errorf("Wrong instance filtered: %s", filtered[0].InstanceID)
		}
	}
}

// TestFilterInstancesCombined validates multiple different filters
func TestFilterInstancesCombined(t *testing.T) {
	instances := []aws.InstanceInfo{
		{
			InstanceID:       "i-1",
			InstanceType:     "m7i.large",
			AvailabilityZone: "us-east-1a",
			Name:             "test",
		},
		{
			InstanceID:       "i-2",
			InstanceType:     "m7i.xlarge",
			AvailabilityZone: "us-east-1a",
			Name:             "test",
		},
		{
			InstanceID:       "i-3",
			InstanceType:     "m7i.large",
			AvailabilityZone: "us-east-1b",
			Name:             "test",
		},
	}

	// Set multiple filter types
	listInstanceFamily = "m7i"
	listAZ = "us-east-1a"
	listTag = []string{"Name=test"}
	defer func() {
		listInstanceFamily = ""
		listAZ = ""
		listTag = []string{}
	}()

	filtered := filterInstances(instances)

	// Only i-1 and i-2 should match (m7i family, us-east-1a, Name=test)
	if len(filtered) != 2 {
		t.Errorf("Expected 2 instances, got %d", len(filtered))
	}
}

// TestFilterInstancesNoMatches validates empty result handling
func TestFilterInstancesNoMatches(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-1", InstanceType: "t3.micro"},
		{InstanceID: "i-2", InstanceType: "t3.small"},
	}

	// Set filter that matches nothing
	listInstanceType = "m7i.large"
	defer func() { listInstanceType = "" }()

	filtered := filterInstances(instances)

	if len(filtered) != 0 {
		t.Errorf("Expected 0 instances, got %d", len(filtered))
	}
}

// TestFilterInstancesInvalidTagFormat validates handling of malformed tag filters
func TestFilterInstancesInvalidTagFormat(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-1", Name: "test"},
	}

	// Set invalid tag filter (no equals sign)
	listTag = []string{"InvalidTagNoEquals"}
	defer func() { listTag = []string{} }()

	// Should not panic, should just ignore invalid filter
	filtered := filterInstances(instances)

	// All instances should pass (invalid filter is ignored)
	if len(filtered) != 1 {
		t.Errorf("Expected 1 instance (invalid filter ignored), got %d", len(filtered))
	}
}

// TestFilterInstancesEmptyFilter validates no filtering when no filters set
func TestFilterInstancesEmptyFilter(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-1"},
		{InstanceID: "i-2"},
		{InstanceID: "i-3"},
	}

	// Ensure all filters are empty
	listAZ = ""
	listInstanceType = ""
	listInstanceFamily = ""
	listTag = []string{}

	filtered := filterInstances(instances)

	if len(filtered) != 3 {
		t.Errorf("Expected all 3 instances, got %d", len(filtered))
	}
}

// TestInstanceFamilyExtraction validates family extraction from instance type
func TestInstanceFamilyExtraction(t *testing.T) {
	tests := []struct {
		instanceType string
		family       string
		shouldMatch  bool
	}{
		{"m7i.large", "m7i", true},
		{"m7i.xlarge", "m7i", true},
		{"m7i.2xlarge", "m7i", true},
		{"t3.micro", "m7i", false},
		{"t3.micro", "t3", true},
		{"c6a.large", "c6a", true},
		{"g5.xlarge", "g5", true},
	}

	for _, tt := range tests {
		t.Run(tt.instanceType, func(t *testing.T) {
			instances := []aws.InstanceInfo{
				{InstanceID: "i-test", InstanceType: tt.instanceType},
			}

			listInstanceFamily = tt.family
			defer func() { listInstanceFamily = "" }()

			filtered := filterInstances(instances)

			matchFound := len(filtered) > 0
			if matchFound != tt.shouldMatch {
				t.Errorf("Instance type %s with family filter %s: expected match=%v, got match=%v",
					tt.instanceType, tt.family, tt.shouldMatch, matchFound)
			}
		})
	}
}
