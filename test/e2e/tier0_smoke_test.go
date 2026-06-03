//go:build e2e_tier0

package e2e

import (
	"strings"
	"testing"
)

// TestTier0_Smoke_LaunchListTerminate is the proof-of-approach: the real spawn
// binary, against Substrate, launches an instance, lists it, and terminates it
// — asserting the JSON contract and exit codes at each step. It validates that
// the CLI-against-substrate harness is sound; the broader Tier 0 suite builds on
// it.
func TestTier0_Smoke_LaunchListTerminate(t *testing.T) {
	env := startSpawnSubstrate(t)

	// version: pure-local, no AWS — quickest sanity that the binary runs.
	if out := env.runOK("version"); strings.TrimSpace(out) == "" {
		t.Fatal("spawn version produced no output")
	}

	// launch -o json → array of launched instances with instance_id.
	arr := env.launchOK("tier0-smoke", "--instance-type", "t3.small")
	if len(arr) != 1 {
		t.Fatalf("expected 1 launched instance, got %d", len(arr))
	}
	requireKeys(t, arr[0], "instance_id", "name", "instance_type", "region")
	id, _ := arr[0]["instance_id"].(string)
	if !strings.HasPrefix(id, "i-") {
		t.Fatalf("expected instance_id starting with i-, got %q", id)
	}
	if name, _ := arr[0]["name"].(string); name != "tier0-smoke" {
		t.Errorf("name = %q, want tier0-smoke", name)
	}

	// list -o json → the instance is present.
	found := false
	for _, inst := range mustJSONArray(t, env.runOK("list", "-o", "json")) {
		if inst["instance_id"] == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("launched instance %s not found in spawn list output", id)
	}

	// terminate -y → exits 0.
	env.runOK("terminate", id, "-y")
}
