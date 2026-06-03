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

// Tier 0 breadth for control-plane-only commands that need no instance/SSH —
// schedule (list query path) and alerts (full create→list→delete round-trip).
// These were previously deferred as "Tier 2/3" but are backed purely by
// DynamoDB, so Substrate can serve them exactly like team and sweep.

// callerAccount returns the AWS account ID Substrate reports for the test
// creds, used as the user_id on seeded schedule/alert records.
func (e *spawnEnv) callerAccount() string {
	e.t.Helper()
	out, err := sts.NewFromConfig(e.AWSConfig).GetCallerIdentity(
		context.Background(), &sts.GetCallerIdentityInput{})
	if err != nil {
		e.t.Fatalf("GetCallerIdentity: %v", err)
	}
	return aws.ToString(out.Account)
}

// ── schedule ──────────────────────────────────────────────────────────────────

// TestTier0_ScheduleListEmpty verifies `schedule list` on a seeded-but-empty
// table exits 0 with the friendly message (not an error).
func TestTier0_ScheduleListEmpty(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.seedScheduleTable()
	_, stderr, code := env.run("schedule", "list")
	if code != 0 {
		t.Fatalf("schedule list exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(strings.ToLower(stderr), "no schedules") {
		t.Errorf("expected 'no schedules', got:\n%s", stderr)
	}
}

// TestTier0_ScheduleList verifies a seeded schedule owned by the caller is
// listed, and the --status filter narrows results.
func TestTier0_ScheduleList(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.seedScheduleTable()
	acct := env.callerAccount()
	env.seedSchedule(acct, "sched-active-1", "nightly", "active")
	env.seedSchedule(acct, "sched-paused-1", "weekly", "paused")

	// `schedule list` writes the table to stderr.
	_, stderr, code := env.run("schedule", "list")
	if code != 0 {
		t.Fatalf("schedule list exit %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "sched-active-1") || !strings.Contains(stderr, "sched-paused-1") {
		t.Errorf("schedule list missing seeded schedules:\n%s", stderr)
	}

	// --status active should drop the paused one.
	_, stderr, code = env.run("schedule", "list", "--status", "active")
	if code != 0 {
		t.Fatalf("schedule list --status exit %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "sched-active-1") || strings.Contains(stderr, "sched-paused-1") {
		t.Errorf("--status active filter wrong:\n%s", stderr)
	}
}

// seedSchedule inserts one schedule record owned by userID.
func (e *spawnEnv) seedSchedule(userID, id, name, status string) {
	e.t.Helper()
	item := map[string]ddbtypes.AttributeValue{
		"schedule_id":         &ddbtypes.AttributeValueMemberS{Value: id},
		"user_id":             &ddbtypes.AttributeValueMemberS{Value: userID},
		"schedule_name":       &ddbtypes.AttributeValueMemberS{Value: name},
		"status":              &ddbtypes.AttributeValueMemberS{Value: status},
		"schedule_type":       &ddbtypes.AttributeValueMemberS{Value: "recurring"},
		"next_execution_time": &ddbtypes.AttributeValueMemberS{Value: "2026-07-01T02:00:00Z"},
	}
	if _, err := e.DynamoClient().PutItem(context.Background(), &dynamodb.PutItemInput{
		TableName: aws.String("spawn-schedules"),
		Item:      item,
	}); err != nil {
		e.t.Fatalf("seed schedule %s: %v", id, err)
	}
}

// ── alerts ──────────────────────────────────────────────────────────────────

// TestTier0_AlertsRoundTrip exercises the full create → list → delete cycle
// against Substrate (all DynamoDB-backed, no external delivery).
func TestTier0_AlertsRoundTrip(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.seedAlertTables()

	// create
	out := env.runOK("alerts", "create", "sweep-xyz",
		"--on-complete", "--email", "researcher@example.com")
	if !strings.Contains(out, "Alert created:") {
		t.Fatalf("unexpected alerts create output:\n%s", out)
	}
	alertID := extractField(out, "Alert created:")
	if alertID == "" {
		t.Fatalf("no alert ID in create output:\n%s", out)
	}

	// list shows it
	if out := env.runOK("alerts", "list"); !strings.Contains(out, alertID) {
		// list writes to stderr in table mode; fall back to combined check
		_, stderr, _ := env.run("alerts", "list")
		if !strings.Contains(stderr, alertID) && !strings.Contains(out, alertID) {
			t.Errorf("created alert %s not in list:\n%s\n%s", alertID, out, stderr)
		}
	}

	// delete it (now prompts; -y skips — spawn#40)
	env.runOK("alerts", "delete", alertID, "-y")
}

// TestTier0_AlertsListEmpty verifies list on seeded-but-empty tables exits 0.
func TestTier0_AlertsListEmpty(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.seedAlertTables()
	if out := env.runOK("alerts", "list"); !strings.Contains(strings.ToLower(out), "no alerts") {
		// message may be on stderr
		_, stderr, _ := env.run("alerts", "list")
		if !strings.Contains(strings.ToLower(stderr+out), "no alerts") {
			t.Errorf("expected 'no alerts', got stdout:\n%s\nstderr:\n%s", out, stderr)
		}
	}
}

// ── stage ──────────────────────────────────────────────────────────────────

// TestTier0_StageListEmpty verifies `stage list` (a DynamoDB scan) exits 0 with
// the friendly message on a seeded-but-empty table. (upload/delete touch real
// multi-region S3 and stay in Tier 2/3.)
func TestTier0_StageListEmpty(t *testing.T) {
	env := startSpawnSubstrate(t)
	seedDynamoTable(t, env.DynamoClient(), "spawn-staged-data", "staging_id", nil)
	out := env.runOK("stage", "list")
	if !strings.Contains(strings.ToLower(out), "no staged data") {
		_, stderr, _ := env.run("stage", "list")
		if !strings.Contains(strings.ToLower(stderr+out), "no staged data") {
			t.Errorf("expected 'no staged data', got stdout:\n%s\nstderr:\n%s", out, stderr)
		}
	}
}
