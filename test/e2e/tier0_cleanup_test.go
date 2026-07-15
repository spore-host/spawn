//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// Tier 0 coverage for the Deep Ephemerality discovery commands (#259), which
// query the Resource Groups Tagging API that Substrate emulates (GetResources
// with TagFilters). These exercise the discovery path end-to-end against the
// emulator: launch a spawn:managed instance, then confirm resources/orphans/
// cleanup see (or correctly classify) it. `cleanup` is run only in its --dry-run
// mode here — destructive --force deletion needs real resources and is
// deferred to a higher tier.

// TestTier0_Resources_ListsLaunchedInstance verifies `spawn resources` finds a
// spawn:managed instance via the tagging API.
func TestTier0_Resources_ListsLaunchedInstance(t *testing.T) {
	env := startSpawnSubstrate(t)

	launched := env.launchOK("rsrc-test", "--instance-type", "t3.small")
	if len(launched) == 0 {
		t.Fatal("launch returned no instances")
	}
	id, _ := launched[0]["instance_id"].(string)
	if id == "" {
		t.Fatalf("launch result missing instance_id: %+v", launched[0])
	}

	// --all so the result isn't filtered by the caller's iam-user (the Substrate
	// caller identity may differ from the launch-time tag).
	out := env.runOK("resources", "--region", "us-east-1", "--all")
	if !strings.Contains(out, id) {
		t.Errorf("`spawn resources` did not list the launched instance %s:\n%s", id, out)
	}
}

// TestTier0_Resources_EmptyAccount reports cleanly when nothing is tagged.
func TestTier0_Resources_EmptyAccount(t *testing.T) {
	env := startSpawnSubstrate(t)
	out := env.runOK("resources", "--region", "us-east-1", "--all")
	if !strings.Contains(strings.ToLower(out), "no spawn-managed resources") {
		t.Errorf("expected an empty-account message, got:\n%s", out)
	}
}

// TestTier0_Cleanup_DryRunPreviewsAndDeletesNothing confirms `cleanup --dry-run`
// previews but removes nothing and exits 0 (execute is the default since #315).
// A running instance must never be removed regardless.
func TestTier0_Cleanup_DryRunPreviewsAndDeletesNothing(t *testing.T) {
	env := startSpawnSubstrate(t)

	launched := env.launchOK("cleanup-test", "--instance-type", "t3.small")
	id, _ := launched[0]["instance_id"].(string)

	// --dry-run: preview only.
	out := env.runOK("cleanup", "--region", "us-east-1", "--all", "--dry-run")
	if !strings.Contains(strings.ToLower(out), "dry run") {
		t.Errorf("cleanup --dry-run should be a dry run, got:\n%s", out)
	}

	// The instance is still running, so it must still exist afterward.
	state, _ := env.describeInstance(id)
	if state == "terminated" || state == "" {
		t.Errorf("dry-run cleanup must not terminate the running instance %s (state=%q)", id, state)
	}
}

// TestTier0_Orphans_RunsClean verifies `spawn orphans` executes and reports
// (a fresh account with only a running instance has no orphans).
func TestTier0_Orphans_RunsClean(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.launchOK("orphan-test", "--instance-type", "t3.small")

	out := env.runOK("orphans", "--region", "us-east-1", "--all")
	// A running instance keeps its shared infra in-use, so nothing should be
	// flagged as an orphan.
	if !strings.Contains(strings.ToLower(out), "no orphaned") {
		t.Logf("orphans output (informational):\n%s", out)
	}
}
