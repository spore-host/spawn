//go:build e2e_tier0

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Tier 0 coverage for parameter-sweep query commands. A full multi-instance
// sweep launch is Tier 3 territory; here we exercise the read/query surface
// (list-sweeps) against seeded orchestration state, asserting JSON validity,
// user-scoped filtering, and empty-state behavior.

const sweepTable = "spawn-sweep-orchestration"

// callerARN returns the ARN Substrate reports for the harness's test creds, so
// seeded records can carry the same user_id spawn filters on.
func (e *spawnEnv) callerARN() string {
	e.t.Helper()
	out, err := sts.NewFromConfig(e.AWSConfig).GetCallerIdentity(
		context.Background(), &sts.GetCallerIdentityInput{})
	if err != nil {
		e.t.Fatalf("GetCallerIdentity: %v", err)
	}
	return aws.ToString(out.Arn)
}

// seedSweep inserts one sweep-orchestration record owned by the given user.
func (e *spawnEnv) seedSweep(userID, sweepID, name, status, region string) {
	e.t.Helper()
	item := map[string]ddbtypes.AttributeValue{
		"sweep_id":     &ddbtypes.AttributeValueMemberS{Value: sweepID},
		"sweep_name":   &ddbtypes.AttributeValueMemberS{Value: name},
		"user_id":      &ddbtypes.AttributeValueMemberS{Value: userID},
		"status":       &ddbtypes.AttributeValueMemberS{Value: status},
		"total_params": &ddbtypes.AttributeValueMemberN{Value: "4"},
		"launched":     &ddbtypes.AttributeValueMemberN{Value: "4"},
		"failed":       &ddbtypes.AttributeValueMemberN{Value: "0"},
		"region":       &ddbtypes.AttributeValueMemberS{Value: region},
		"created_at":   &ddbtypes.AttributeValueMemberS{Value: "2026-01-01T00:00:00Z"},
	}
	if _, err := e.DynamoClient().PutItem(context.Background(), &dynamodb.PutItemInput{
		TableName: aws.String(sweepTable),
		Item:      item,
	}); err != nil {
		e.t.Fatalf("seed sweep %s: %v", sweepID, err)
	}
}

// TestTier0_ListSweepsEmpty verifies list-sweeps on a seeded-but-empty table
// returns a valid (empty) JSON array and exits 0.
func TestTier0_ListSweepsEmpty(t *testing.T) {
	env := startSpawnSubstrate(t)
	seedDynamoTable(t, env.DynamoClient(), sweepTable, "sweep_id", nil)

	arr := mustJSONArray(t, env.runOK("list-sweeps", "--json"))
	if len(arr) != 0 {
		t.Errorf("expected no sweeps, got %d: %v", len(arr), arr)
	}
}

// TestTier0_ListSweeps verifies a seeded sweep owned by the caller is returned
// with the expected fields, and that user-scoped filtering excludes sweeps
// owned by someone else.
func TestTier0_ListSweeps(t *testing.T) {
	env := startSpawnSubstrate(t)
	seedDynamoTable(t, env.DynamoClient(), sweepTable, "sweep_id", nil)

	mine := env.callerARN()
	env.seedSweep(mine, "swp-001", "grid-search", "COMPLETED", "us-east-1")
	env.seedSweep("arn:aws:iam::999999999999:root", "swp-other", "not-mine", "RUNNING", "us-east-1")

	arr := mustJSONArray(t, env.runOK("list-sweeps", "--json"))
	if len(arr) != 1 {
		t.Fatalf("expected exactly 1 (caller-owned) sweep, got %d: %v", len(arr), arr)
	}
	requireKeys(t, arr[0], "sweep_id", "sweep_name", "status")
	if arr[0]["sweep_id"] != "swp-001" {
		t.Errorf("wrong sweep returned: %v", arr[0])
	}
	if arr[0]["sweep_name"] != "grid-search" {
		t.Errorf("sweep_name mismatch: %v", arr[0]["sweep_name"])
	}
}

// TestTier0_ListSweepsStatusFilter verifies --status narrows results.
func TestTier0_ListSweepsStatusFilter(t *testing.T) {
	env := startSpawnSubstrate(t)
	seedDynamoTable(t, env.DynamoClient(), sweepTable, "sweep_id", nil)

	mine := env.callerARN()
	env.seedSweep(mine, "swp-done", "done", "COMPLETED", "us-east-1")
	env.seedSweep(mine, "swp-run", "running", "RUNNING", "us-east-1")

	arr := mustJSONArray(t, env.runOK("list-sweeps", "--json", "--status", "RUNNING"))
	if len(arr) != 1 || arr[0]["sweep_id"] != "swp-run" {
		t.Errorf("--status RUNNING should return only swp-run, got: %v", arr)
	}
}

// TestTier0_ListSweepsInvalidSince is the negative case: a bad --since date
// must fail with a helpful message, not panic. Note spawn validates --since
// lazily inside the per-sweep loop, so a caller-owned sweep must exist for the
// validation to be reached.
func TestTier0_ListSweepsInvalidSince(t *testing.T) {
	env := startSpawnSubstrate(t)
	seedDynamoTable(t, env.DynamoClient(), sweepTable, "sweep_id", nil)
	env.seedSweep(env.callerARN(), "swp-x", "x", "COMPLETED", "us-east-1")

	_, stderr, code := env.run("list-sweeps", "--json", "--since", "not-a-date")
	if code == 0 {
		t.Fatalf("invalid --since should fail, got exit 0")
	}
	if !strings.Contains(strings.ToLower(stderr), "since") {
		t.Errorf("expected an error mentioning --since, got:\n%s", stderr)
	}
}
