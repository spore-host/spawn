//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// Tier 0 lifecycle coverage: launch variants and instance state transitions
// against Substrate. Each test starts a fresh emulator (startSpawnSubstrate) so
// per-account control-plane state (SSH key, IAM role) never bleeds across tests.

// TestTier0_Launch_JobArray verifies --count N launches N instances tagged as a
// job array, all discoverable via list.
func TestTier0_Launch_JobArray(t *testing.T) {
	env := startSpawnSubstrate(t)
	arr := env.launchOK("arr", "--instance-type", "t3.small", "--count", "3", "--job-array-name", "batch")
	if len(arr) != 3 {
		t.Fatalf("--count 3 launched %d instances, want 3", len(arr))
	}
	ids := map[string]bool{}
	for _, inst := range arr {
		id, _ := inst["instance_id"].(string)
		if !strings.HasPrefix(id, "i-") {
			t.Errorf("bad instance_id %q", id)
		}
		ids[id] = true
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 distinct instance IDs, got %d", len(ids))
	}
	// All three should appear in list.
	listed := 0
	for _, inst := range mustJSONArray(t, env.runOK("list", "-o", "json")) {
		if id, _ := inst["instance_id"].(string); ids[id] {
			listed++
		}
	}
	if listed != 3 {
		t.Errorf("list returned %d of the 3 job-array instances", listed)
	}
}

// TestTier0_Launch_Spot verifies a Spot launch succeeds and is reported.
func TestTier0_Launch_Spot(t *testing.T) {
	env := startSpawnSubstrate(t)
	arr := env.launchOK("spotty", "--instance-type", "t3.small", "--spot")
	if len(arr) != 1 {
		t.Fatalf("spot launch returned %d instances", len(arr))
	}
	requireKeys(t, arr[0], "instance_id", "instance_type")
}

// TestTier0_Launch_VolumeSize verifies --volume-size is accepted and the launch
// succeeds (regression for #11).
func TestTier0_Launch_VolumeSize(t *testing.T) {
	env := startSpawnSubstrate(t)
	arr := env.launchOK("bigdisk", "--instance-type", "t3.small", "--volume-size", "100")
	if len(arr) != 1 || !strings.HasPrefix(arr[0]["instance_id"].(string), "i-") {
		t.Fatalf("volume-size launch failed: %+v", arr)
	}
}

// TestTier0_Launch_AMIAuto verifies --ami auto is treated as auto-detect (not
// passed literally to EC2 — regression for #15). Auto-detect resolves via the
// managed-AMI SSM parameter Substrate now serves.
func TestTier0_Launch_AMIAuto(t *testing.T) {
	env := startSpawnSubstrate(t)
	arr := env.launchOK("autoami", "--instance-type", "t3.small", "--ami", "auto")
	if len(arr) != 1 || !strings.HasPrefix(arr[0]["instance_id"].(string), "i-") {
		t.Fatalf("--ami auto launch failed: %+v", arr)
	}
}

// TestTier0_Launch_EstimateOnly verifies --estimate-only launches nothing
// (regression for #305): exit 0, and list shows no instance.
func TestTier0_Launch_EstimateOnly(t *testing.T) {
	env := startSpawnSubstrate(t)
	_, _, code := env.run("launch", "est", "--instance-type", "t3.small",
		"--region", "us-east-1", "--estimate-only", "-y", "-o", "json")
	if code != 0 {
		t.Fatalf("--estimate-only exit = %d, want 0", code)
	}
	if n := len(mustJSONArray(t, env.runOK("list", "-o", "json"))); n != 0 {
		t.Errorf("--estimate-only should launch nothing, but list shows %d instances", n)
	}
}

// TestTier0_StateTransitions exercises stop → start → terminate on a single
// instance: each command must resolve the instance and exit 0. Note: stop and
// hibernate take no -y; only terminate prompts. We assert the commands succeed
// (spawn's behavior) rather than substrate's exact post-transition state, which
// is emulator-specific.
func TestTier0_StateTransitions(t *testing.T) {
	env := startSpawnSubstrate(t)
	arr := env.launchOK("life", "--instance-type", "t3.small")
	id := arr[0]["instance_id"].(string)

	env.runOK("stop", id)
	env.runOK("start", id)
	env.runOK("terminate", id, "-y")
}

// TestTier0_TerminateRemovesFromList verifies that terminating a freshly
// launched (running) instance removes it from the running set. This exercises
// spawn's multi-filter DescribeInstances call (tag:spawn:managed AND
// instance-state-name=running) — substrate#305, fixed in substrate v0.68.0,
// previously applied only the first filter and leaked terminated instances.
func TestTier0_TerminateRemovesFromList(t *testing.T) {
	env := startSpawnSubstrate(t)
	arr := env.launchOK("term", "--instance-type", "t3.small")
	id := arr[0]["instance_id"].(string)

	env.runOK("terminate", id, "-y")

	for _, inst := range mustJSONArray(t, env.runOK("list", "--state", "running", "-o", "json")) {
		if inst["instance_id"] == id {
			t.Errorf("terminated instance %s still listed as running", id)
		}
	}
}

// TestTier0_Hibernate verifies hibernate is accepted on a launched instance.
func TestTier0_Hibernate(t *testing.T) {
	env := startSpawnSubstrate(t)
	arr := env.launchOK("hib", "--instance-type", "t3.small")
	id := arr[0]["instance_id"].(string)
	env.runOK("hibernate", id)
}
