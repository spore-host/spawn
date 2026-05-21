package aws

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateScopedS3Policy(t *testing.T) {
	region := "us-east-1"
	accountID := "123456789012"

	policy := GenerateScopedS3Policy(region, accountID)

	// Parse policy JSON
	var policyDoc map[string]interface{}
	if err := json.Unmarshal([]byte(policy), &policyDoc); err != nil {
		t.Fatalf("Failed to parse policy JSON: %v", err)
	}

	// Verify version
	if policyDoc["Version"] != "2012-10-17" {
		t.Errorf("Expected Version 2012-10-17, got %v", policyDoc["Version"])
	}

	// Verify statements exist
	statements, ok := policyDoc["Statement"].([]interface{})
	if !ok || len(statements) == 0 {
		t.Fatal("Policy has no statements")
	}

	// Verify policy contains scoped resources
	policyStr := string(policy)
	if !strings.Contains(policyStr, "spawn-binaries-"+region) {
		t.Error("Policy does not include spawn-binaries bucket for region")
	}
	if !strings.Contains(policyStr, "spawn-results-"+region) {
		t.Error("Policy does not include spawn-results bucket for region")
	}

	// Verify no wildcards in Resource
	if strings.Contains(policyStr, `"Resource": "*"`) {
		t.Error("Policy contains wildcard resources - should be scoped")
	}
}

func TestGenerateScopedDynamoDBPolicy(t *testing.T) {
	region := "us-west-2"
	accountID := "123456789012"

	policy := GenerateScopedDynamoDBPolicy(region, accountID)

	// Parse policy JSON
	var policyDoc map[string]interface{}
	if err := json.Unmarshal([]byte(policy), &policyDoc); err != nil {
		t.Fatalf("Failed to parse policy JSON: %v", err)
	}

	// Verify version
	if policyDoc["Version"] != "2012-10-17" {
		t.Errorf("Expected Version 2012-10-17, got %v", policyDoc["Version"])
	}

	// Verify statements exist
	statements, ok := policyDoc["Statement"].([]interface{})
	if !ok || len(statements) == 0 {
		t.Fatal("Policy has no statements")
	}

	// Verify policy contains spawn tables
	policyStr := string(policy)
	requiredTables := []string{
		"spawn-alerts",
		"spawn-alert-history",
		"spawn-schedules",
		"spawn-queues",
	}

	for _, table := range requiredTables {
		if !strings.Contains(policyStr, table) {
			t.Errorf("Policy does not include required table: %s", table)
		}
	}

	// Verify account ID is in ARNs
	if !strings.Contains(policyStr, accountID) {
		t.Error("Policy does not include account ID in ARNs")
	}

	// Verify region is in ARNs
	if !strings.Contains(policyStr, region) {
		t.Error("Policy does not include region in ARNs")
	}

	// Verify no wildcards in Resource (except for indexes)
	lines := strings.Split(policyStr, "\n")
	for _, line := range lines {
		if strings.Contains(line, `"Resource": "*"`) {
			t.Error("Policy contains wildcard resources - should be scoped to specific tables")
		}
	}
}

func TestGenerateScopedCloudWatchLogsPolicy(t *testing.T) {
	region := "eu-west-1"
	accountID := "123456789012"

	policy := GenerateScopedCloudWatchLogsPolicy(region, accountID)

	// Parse policy JSON
	var policyDoc map[string]interface{}
	if err := json.Unmarshal([]byte(policy), &policyDoc); err != nil {
		t.Fatalf("Failed to parse policy JSON: %v", err)
	}

	// Verify policy contains audit log group
	policyStr := string(policy)
	if !strings.Contains(policyStr, "/aws/spawn/audit") {
		t.Error("Policy does not include audit log group")
	}

	// Verify account ID and region in ARNs
	if !strings.Contains(policyStr, accountID) {
		t.Error("Policy does not include account ID in ARNs")
	}
	if !strings.Contains(policyStr, region) {
		t.Error("Policy does not include region in ARNs")
	}
}

func TestBuildTrustPolicyWithAccount(t *testing.T) {
	client := &Client{}
	services := []string{"ec2", "lambda"}
	accountID := "123456789012"

	policy := client.buildTrustPolicyWithAccount(services, accountID)

	// Marshal to JSON for inspection
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("Failed to marshal policy: %v", err)
	}

	policyStr := string(policyJSON)

	// Verify account condition exists
	if !strings.Contains(policyStr, "aws:SourceAccount") {
		t.Error("Trust policy does not include aws:SourceAccount condition")
	}

	// Verify account ID is in condition
	if !strings.Contains(policyStr, accountID) {
		t.Error("Trust policy does not include account ID in condition")
	}

	// Verify principals
	if !strings.Contains(policyStr, "ec2.amazonaws.com") {
		t.Error("Trust policy does not include ec2 principal")
	}
	if !strings.Contains(policyStr, "lambda.amazonaws.com") {
		t.Error("Trust policy does not include lambda principal")
	}

	// Verify AssumeRole action
	if !strings.Contains(policyStr, "sts:AssumeRole") {
		t.Error("Trust policy does not include sts:AssumeRole action")
	}
}

func TestBuildTrustPolicyLegacy(t *testing.T) {
	client := &Client{}
	services := []string{"ec2"}

	policy := client.buildTrustPolicy(services)

	// Marshal to JSON for inspection
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("Failed to marshal policy: %v", err)
	}

	policyStr := string(policyJSON)

	// Verify NO account condition (legacy behavior)
	if strings.Contains(policyStr, "aws:SourceAccount") {
		t.Error("Legacy trust policy should not include aws:SourceAccount condition")
	}

	// Verify principal exists
	if !strings.Contains(policyStr, "ec2.amazonaws.com") {
		t.Error("Trust policy does not include ec2 principal")
	}
}

// TestPolicyScoping verifies that scoped policies don't use wildcards
func TestPolicyScoping(t *testing.T) {
	tests := []struct {
		name     string
		policy   string
		wantWild bool
	}{
		{
			name:     "Scoped S3 policy",
			policy:   GenerateScopedS3Policy("us-east-1", "123456789012"),
			wantWild: false,
		},
		{
			name:     "Scoped DynamoDB policy",
			policy:   GenerateScopedDynamoDBPolicy("us-east-1", "123456789012"),
			wantWild: false,
		},
		{
			name:     "Scoped CloudWatch Logs policy",
			policy:   GenerateScopedCloudWatchLogsPolicy("us-east-1", "123456789012"),
			wantWild: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasWildcard := strings.Contains(tt.policy, `"Resource": "*"`)
			if hasWildcard != tt.wantWild {
				if hasWildcard {
					t.Error("Policy contains wildcard resource when it should be scoped")
				} else {
					t.Error("Policy is scoped when wildcard was expected")
				}
			}
		})
	}
}

// TestPolicyJSONValidity ensures all generated policies are valid JSON
func TestPolicyJSONValidity(t *testing.T) {
	policies := []struct {
		name   string
		policy string
	}{
		{"S3", GenerateScopedS3Policy("us-east-1", "123456789012")},
		{"DynamoDB", GenerateScopedDynamoDBPolicy("us-east-1", "123456789012")},
		{"CloudWatch", GenerateScopedCloudWatchLogsPolicy("us-east-1", "123456789012")},
	}

	for _, p := range policies {
		t.Run(p.name, func(t *testing.T) {
			var policyDoc map[string]interface{}
			if err := json.Unmarshal([]byte(p.policy), &policyDoc); err != nil {
				t.Errorf("Generated policy is not valid JSON: %v", err)
			}

			// Verify required fields
			if _, ok := policyDoc["Version"]; !ok {
				t.Error("Policy missing Version field")
			}
			if _, ok := policyDoc["Statement"]; !ok {
				t.Error("Policy missing Statement field")
			}
		})
	}
}
