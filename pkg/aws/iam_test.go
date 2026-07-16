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

// TestSporedTagPermissionsScoped is the #174 regression guard: spored's
// ec2:CreateTags / ec2:DeleteTags must be conditioned on
// ec2:ResourceTag/spawn:managed=true (so a compromised spore can't tag an
// arbitrary instance into scope and then terminate it). Previously CreateTags
// was granted on "*" with no condition.
func TestSporedTagPermissionsScoped(t *testing.T) {
	c := &Client{}
	policy := c.buildInlinePolicy(nil) // base spored policy, no extra templates

	stmts, ok := policy["Statement"].([]interface{})
	if !ok {
		t.Fatalf("policy has no Statement array: %T", policy["Statement"])
	}

	foundTagStmt := false
	for _, s := range stmts {
		stmt, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		actions := actionSet(stmt["Action"])
		hasCreate := actions["ec2:CreateTags"]
		if !hasCreate {
			continue
		}
		foundTagStmt = true

		// The statement granting CreateTags MUST carry the managed-tag condition.
		cond, _ := stmt["Condition"].(map[string]interface{})
		se, _ := cond["StringEquals"].(map[string]interface{})
		if se["ec2:ResourceTag/spawn:managed"] != "true" {
			t.Errorf("ec2:CreateTags is granted WITHOUT the spawn:managed=true condition (#174): %+v", stmt)
		}
	}
	if !foundTagStmt {
		t.Error("no statement grants ec2:CreateTags — spored needs it for its lifecycle tags")
	}
}

// TestSporedDNSInvokeGrant asserts the spored base policy grants
// lambda:InvokeFunctionUrl on the DNS-updater function and nothing broader (#173).
// This is the caller half of the IAM-auth cutover — the role authorizes itself to
// call the Function URL; the Lambda authorizes the verified caller account.
func TestSporedDNSInvokeGrant(t *testing.T) {
	c := &Client{}
	policy := c.buildInlinePolicy(nil)

	stmts, _ := policy["Statement"].([]interface{})
	found := false
	for _, s := range stmts {
		stmt, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		if !actionSet(stmt["Action"])["lambda:InvokeFunctionUrl"] {
			continue
		}
		found = true
		res, _ := stmt["Resource"].(string)
		if !strings.Contains(res, "function:spawn-dns-updater") {
			t.Errorf("InvokeFunctionUrl not scoped to the dns-updater function: %q", res)
		}
		if res == "*" {
			t.Error("InvokeFunctionUrl granted on '*' — must be scoped to the dns-updater ARN")
		}
	}
	if !found {
		t.Error("no statement grants lambda:InvokeFunctionUrl — spored can't call the DNS Function URL under AWS_IAM (#173)")
	}
}

// TestSporedFSxMountGrant asserts the spored base policy grants the FSx APIs the
// async-mount path needs (#194/#221) — without these the mount silently failed
// with AccessDenied (the spored role previously had no fsx:* perms).
func TestSporedFSxMountGrant(t *testing.T) {
	c := &Client{}
	policy := c.buildInlinePolicy(nil)
	stmts, _ := policy["Statement"].([]interface{})

	need := map[string]bool{
		"fsx:DescribeFileSystems":             false,
		"fsx:CreateDataRepositoryAssociation": false,
	}
	for _, s := range stmts {
		stmt, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		for a := range actionSet(stmt["Action"]) {
			if _, ok := need[a]; ok {
				need[a] = true
			}
		}
	}
	for a, found := range need {
		if !found {
			t.Errorf("spored policy missing %s — async FSx mount (#221) would AccessDenied", a)
		}
	}
}

// actionSet normalizes a statement's Action (string or []interface{}) to a set.
func actionSet(a interface{}) map[string]bool {
	out := map[string]bool{}
	switch v := a.(type) {
	case string:
		out[v] = true
	case []interface{}:
		for _, x := range v {
			if s, ok := x.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func TestValidatePolicyNames(t *testing.T) {
	cases := []struct {
		name      string
		policies  []string
		allowFull bool
		wantErr   bool
		errSubstr string
	}{
		{name: "scoped ok", policies: []string{"s3:ReadOnly", "dynamodb:WriteOnly"}, wantErr: false},
		{name: "empty ok", policies: nil, wantErr: false},
		{name: "fullaccess blocked by default", policies: []string{"s3:FullAccess"}, wantErr: true, errSubstr: "wildcard access"},
		{name: "fullaccess allowed with opt-in", policies: []string{"s3:FullAccess"}, allowFull: true, wantErr: false},
		{name: "dynamodb full blocked", policies: []string{"dynamodb:FullAccess"}, wantErr: true, errSubstr: "wildcard access"},
		{name: "sqs full blocked", policies: []string{"sqs:FullAccess"}, wantErr: true, errSubstr: "wildcard access"},
		{name: "unknown template", policies: []string{"s3:Bogus"}, wantErr: true, errSubstr: "unknown IAM policy template"},
		{name: "mix scoped + full blocks", policies: []string{"s3:ReadOnly", "sqs:FullAccess"}, wantErr: true, errSubstr: "wildcard access"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePolicyNames(tc.policies, tc.allowFull)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantErr && tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("error %q missing %q", err.Error(), tc.errSubstr)
			}
		})
	}
}
