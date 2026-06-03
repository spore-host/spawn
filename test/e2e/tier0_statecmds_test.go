//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// Tier 0 coverage for the stateful, non-instance commands: those backed by a
// local config file (defaults) or by DynamoDB / Route53 control-plane state
// (team, schedule, alerts, dns). Each asserts spawn's behavior given the AWS
// responses Substrate returns — round-trip persistence and exit codes — not
// emulator internals.

// TestTier0_Defaults verifies set → list → unset round-trips through the
// local ~/.spawn/config.yaml (isolated per-test via the harness HOME). Keys
// are the idle-management settings spawn recognizes (see KnownDefaultKeys).
func TestTier0_Defaults(t *testing.T) {
	env := startSpawnSubstrate(t)

	env.runOK("defaults", "set", "idle-timeout", "30m")
	env.runOK("defaults", "set", "hibernate-on-idle", "true")

	out := env.runOK("defaults", "list")
	if !strings.Contains(out, "30m") || !strings.Contains(out, "true") {
		t.Errorf("defaults list missing set values:\n%s", out)
	}

	env.runOK("defaults", "unset", "idle-timeout")
	out = env.runOK("defaults", "list")
	if strings.Contains(out, "30m") {
		t.Errorf("idle-timeout still present after unset:\n%s", out)
	}
	if !strings.Contains(out, "true") {
		t.Errorf("unrelated default (hibernate-on-idle) lost after unset:\n%s", out)
	}
}

// TestTier0_DefaultsPersistAcrossInvocations confirms a value set by one
// process is visible to a separate later invocation (the config file is the
// only shared state — each run() is a fresh process).
func TestTier0_DefaultsPersistAcrossInvocations(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.runOK("defaults", "set", "idle-timeout", "8h")
	// A completely separate process must observe it.
	if out := env.runOK("defaults", "list"); !strings.Contains(out, "8h") {
		t.Errorf("default did not persist across invocations:\n%s", out)
	}
}

// TestTier0_DefaultsRejectsUnknownKey is the negative case: an unknown key
// must exit nonzero with a helpful message, never silently succeed.
func TestTier0_DefaultsRejectsUnknownKey(t *testing.T) {
	env := startSpawnSubstrate(t)
	_, stderr, code := env.run("defaults", "set", "instance-type", "t3.large")
	if code == 0 {
		t.Fatalf("setting an unknown default key should fail, got exit 0")
	}
	if !strings.Contains(stderr, "unknown default key") {
		t.Errorf("expected 'unknown default key' message, got:\n%s", stderr)
	}
}

// TestTier0_Team exercises create → list → show → delete against the seeded
// DynamoDB tables. Team commands resolve the caller ARN via STS, which
// Substrate serves.
func TestTier0_Team(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.seedTeamTables()

	out := env.runOK("team", "create", "--name", "alpha", "--description", "the alpha team")
	if !strings.Contains(out, "Created team") || !strings.Contains(out, "alpha") {
		t.Fatalf("unexpected team create output:\n%s", out)
	}
	// Extract the generated team ID from the "ID:    <uuid>" line.
	teamID := extractField(out, "ID:")
	if teamID == "" {
		t.Fatalf("no team ID in create output:\n%s", out)
	}

	if out := env.runOK("team", "list"); !strings.Contains(out, "alpha") {
		t.Errorf("created team not in list:\n%s", out)
	}
	if out := env.runOK("team", "show", teamID); !strings.Contains(out, "alpha") {
		t.Errorf("team show missing name:\n%s", out)
	}

	// team delete takes the ID only (no confirmation flag).
	env.runOK("team", "delete", teamID)
	// After deletion the team is gone from the list.
	if out := env.runOK("team", "list"); strings.Contains(out, teamID) {
		t.Errorf("deleted team %s still listed:\n%s", teamID, out)
	}
}

// TestTier0_TeamListEmpty verifies list on seeded-but-empty tables exits 0 with
// a friendly message rather than erroring.
func TestTier0_TeamListEmpty(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.seedTeamTables()
	if out := env.runOK("team", "list"); !strings.Contains(strings.ToLower(out), "no teams") {
		t.Errorf("expected 'no teams' message, got:\n%s", out)
	}
}

// extractField returns the whitespace-trimmed remainder of the first line
// containing label, after that label. Returns "" if not found.
func extractField(out, label string) string {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, label); i >= 0 {
			return strings.TrimSpace(line[i+len(label):])
		}
	}
	return ""
}
