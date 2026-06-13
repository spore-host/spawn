package launcher

import (
	"context"
	"os"
	"testing"
	"time"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// TestProvision_LiveAWS drives the exact path lagotto's --action spawn takes,
// against REAL AWS, to confirm #127 (user-data base64) and #130 (empty KeyName)
// are actually fixed end-to-end — substrate accepted both bugs, so only a real
// RunInstances proves it. Gated by SPAWN_LIVE_PROVISION_TEST=1 so it never runs
// in CI. ALWAYS terminates the instance it launches (deferred), even on failure.
func TestProvision_LiveAWS(t *testing.T) {
	if os.Getenv("SPAWN_LIVE_PROVISION_TEST") != "1" {
		t.Skip("set SPAWN_LIVE_PROVISION_TEST=1 to run the real-AWS provision smoke test")
	}
	region := os.Getenv("SPAWN_LIVE_REGION")
	if region == "" {
		region = "us-east-1"
	}

	ctx := context.Background()
	client, err := spawnaws.NewClient(ctx)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Keyless, SSM-only, short TTL — exactly the headless Lambda case.
	cfg := spawnaws.LaunchConfig{
		InstanceType: "t3.micro", // cheap; we terminate immediately
		Region:       region,
		TTL:          "1h", // backstop in case the test is killed before defer runs
		OnComplete:   "terminate",
		Name:         "provision-livetest",
	}

	result, err := Provision(ctx, client, cfg, Options{}) // no PublicKey => no key pair
	if result != nil && result.InstanceID != "" {
		// Guaranteed cleanup: terminate whatever we launched, regardless of outcome.
		defer func() {
			tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if terr := client.Terminate(tctx, region, result.InstanceID); terr != nil {
				t.Errorf("CLEANUP FAILED — terminate %s manually: %v", result.InstanceID, terr)
			} else {
				t.Logf("terminated %s", result.InstanceID)
			}
		}()
	}
	if err != nil {
		t.Fatalf("Provision (real AWS) failed — a headless-launch blocker remains: %v", err)
	}
	t.Logf("Provision succeeded: instance %s (%s) in %s — #127+#130 confirmed fixed",
		result.InstanceID, result.State, region)
}
