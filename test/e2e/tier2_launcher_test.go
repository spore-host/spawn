//go:build e2e_tier2

package e2e

// Tier 2 — library path. Unlike the other Tier 2 tests (which drive the spawn
// CLI binary), this exercises the exported pkg/launcher.Provision directly —
// the path SDK consumers like lagotto's capacity-poller take. Substrate (Tier 0)
// accepts malformed user-data and an empty KeyName, so it green-lit both #127
// (user-data not base64) and #130 (empty KeyName) — bugs that broke every
// headless launch. Only a real RunInstances catches that class, hence Tier 2.

import (
	"context"
	"testing"
	"time"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
)

// TestTier2_LauncherProvision launches a keyless, SSM-only instance via
// launcher.Provision (no SSH key — exactly the headless Lambda case) and
// confirms RunInstances accepts it (regressions #127, #130). Cleanup is
// registered by name, same as launchInstance, so the reaper safety net + TTL
// apply.
func TestTier2_LauncherProvision(t *testing.T) {
	t.Parallel()
	name := "e2e-provision-" + runID(t)

	client := spawnaws.NewClientFromConfig(loadAWSConfig(t))
	t.Cleanup(func() { terminateByName(t, name) })

	ctx := context.Background()
	result, err := launcher.Provision(ctx, client, spawnaws.LaunchConfig{
		InstanceType: testInstanceType,
		Region:       testRegion,
		TTL:          defaultTTL, // hard safety ceiling, same as launchInstance
		OnComplete:   "terminate",
		Name:         name,
	}, launcher.Options{}) // no PublicKey => no key pair (SSM-only)
	if err != nil {
		t.Fatalf("launcher.Provision failed on real AWS — a headless-launch blocker remains: %v", err)
	}
	if result.InstanceID == "" {
		t.Fatal("Provision returned an empty instance ID")
	}
	t.Logf("Provision launched %s (%s) — headless user-data + keyless launch accepted (#127, #130)",
		result.InstanceID, result.State)

	// Confirm it actually reaches running (not just that RunInstances returned).
	inst := waitForRunning(t, name, 10*time.Minute)
	if inst.InstanceID != result.InstanceID {
		t.Errorf("waitForRunning found %s, Provision returned %s", inst.InstanceID, result.InstanceID)
	}
}
