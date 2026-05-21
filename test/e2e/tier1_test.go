//go:build e2e_tier1

package e2e

// Tier 1 — AWS API-only tests. No EC2 instances are launched; cost is ~$0.
//
// Run: go test -v -tags=e2e_tier1 ./test/e2e/ -run TestTier1 -timeout 5m

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// truffleBin returns the path to the truffle binary.
func truffleBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("truffle")
	if err != nil {
		t.Skip("truffle binary not on PATH; run 'cd truffle && go build -o bin/truffle .' and add to PATH")
	}
	return p
}

// TestTier1_TruffleSearch verifies truffle can search instance types via EC2 DescribeInstanceTypes.
func TestTier1_TruffleSearch(t *testing.T) {
	t.Parallel()
	bin := truffleBin(t)
	out, err := exec.Command(bin, "search", "t3.small", "--regions", testRegion, "--output", "json").CombinedOutput() //nolint:gosec // nosemgrep
	if err != nil {
		t.Fatalf("truffle search failed: %v\n%s", err, out)
	}
	if len(strings.TrimSpace(string(out))) == 0 || string(out) == "null\n" || string(out) == "[]\n" {
		t.Skip("truffle search returned empty results (t3.small may not be enabled in this account/region)")
	}
	if !strings.Contains(string(out), "t3.small") {
		t.Fatalf("expected t3.small in results, got:\n%s", out)
	}
	t.Logf("truffle search OK: %d bytes", len(out))
}

// TestTier1_TruffleSpot verifies truffle can fetch Spot pricing via EC2 DescribeSpotPriceHistory.
func TestTier1_TruffleSpot(t *testing.T) {
	t.Parallel()
	bin := truffleBin(t)
	out, err := exec.Command(bin, "spot", "t3.small", "--regions", testRegion, "--sort-by-price").CombinedOutput() //nolint:gosec // nosemgrep
	if err != nil {
		t.Fatalf("truffle spot failed: %v\n%s", err, out)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Skip("truffle spot returned empty results (Spot pricing may not be available for t3.small in this region)")
	}
	t.Logf("truffle spot OK: %d bytes", len(out))
}

// TestTier1_TruffleQuotas verifies truffle can query service quotas.
func TestTier1_TruffleQuotas(t *testing.T) {
	t.Parallel()
	bin, err := exec.LookPath("truffle")
	if err != nil {
		t.Skip("truffle binary not on PATH")
	}
	out, err := exec.Command(bin, "quotas", "--regions", testRegion).CombinedOutput() //nolint:gosec // nosemgrep
	if err != nil {
		t.Fatalf("truffle quotas: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Standard") && !strings.Contains(string(out), "vCPU") {
		t.Fatalf("expected quota output with vCPU info, got:\n%s", out)
	}
	t.Logf("quotas output: %d bytes", len(out))
}

// TestTier1_TruffleFind verifies truffle natural-language find works.
func TestTier1_TruffleFind(t *testing.T) {
	t.Parallel()
	bin, err := exec.LookPath("truffle")
	if err != nil {
		t.Skip("truffle binary not on PATH")
	}

	out, err := exec.Command(bin, "find", "graviton", "--regions", testRegion).CombinedOutput() //nolint:gosec // nosemgrep
	if err != nil {
		t.Fatalf("truffle find: %v\n%s", err, out)
	}
	if len(out) == 0 {
		t.Fatal("expected output from truffle find graviton")
	}
	t.Logf("truffle find graviton: %d bytes", len(out))
}

// TestTier1_EstimateOnly verifies --estimate-only exits without launching (regression #305).
func TestTier1_EstimateOnly(t *testing.T) {
	t.Parallel()
	name := "e2e-estimate-only-" + runID(t)

	out, err := spawnMayFail(t,
		"launch", name,
		"--instance-type", testInstanceType,
		"--region", testRegion,
		"--ttl", "1h",
		"--estimate-only",
	)
	if err != nil {
		t.Fatalf("--estimate-only returned error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Estimate complete") && !strings.Contains(out, "estimate-only") && !strings.Contains(out, "/hr") {
		t.Fatalf("expected estimate output, got:\n%s", out)
	}

	// Verify no instance was launched
	listOut, _ := spawnMayFail(t, "list", "--output", "json")
	if strings.Contains(listOut, name) {
		t.Errorf("--estimate-only launched an instance! Found %q in list output", name)
		// Clean up the accidentally launched instance
		t.Cleanup(func() { spawnMayFail(t, "stop", name) })
	}
	t.Log("--estimate-only correctly printed estimate without launching")
}

// TestTier1_EFAValidationRegion verifies EFA validation uses the launch region (regression #307).
func TestTier1_EFAValidationRegion(t *testing.T) {
	t.Parallel()
	// hpc6a.48xlarge only exists in us-east-2. With the old code (using default
	// region us-east-1), this would return InvalidInstanceType. With the fix it
	// should return EfaSupported=true — but since we're not actually launching,
	// we only need --estimate-only to not error out on the EFA validation step.
	name := "e2e-efa-region-" + runID(t)
	out, err := spawnMayFail(t,
		"launch", name,
		"--instance-type", "hpc6a.48xlarge",
		"--region", "us-east-2",
		"--efa",
		"--estimate-only",
	)
	// We accept either success (estimate printed) OR an EFA-validation-specific error.
	// What we must NOT see is "InvalidInstanceType" — that was the regression.
	if strings.Contains(out, "InvalidInstanceType") {
		t.Errorf("EFA validation queried wrong region: got InvalidInstanceType for hpc6a.48xlarge\n%s", out)
	}
	// If err == nil, the estimate ran cleanly — best outcome.
	if err == nil {
		t.Log("EFA validation passed in correct region (us-east-2)")
	} else {
		// EFA may fail for other reasons (quota, not enabled) — that's fine.
		t.Logf("EFA estimate returned non-zero (acceptable): %v\n%s", err, out)
	}
}

// TestTier1_PlacementGroupRegion verifies --mpi uses the correct region for
// placement group creation (regression for #317 — group was created in the
// client's default region, not the launch region, so RunInstances in the
// target region returned InvalidPlacementGroup.Unknown).
// Uses --estimate-only so no instances are launched.
func TestTier1_PlacementGroupRegion(t *testing.T) {
	t.Parallel()
	name := "e2e-pg-region-" + runID(t)
	// Launch in us-east-2 with MPI. The old code would create the placement group
	// in us-east-1 (client default). With the fix it uses --region us-east-2.
	// --estimate-only aborts before RunInstances so this is free.
	out, err := spawnMayFail(t,
		"launch", name,
		"--instance-type", "c5n.18xlarge",
		"--count", "2",
		"--job-array-name", name,
		"--region", "us-east-2",
		"--mpi",
		"--estimate-only",
	)
	// With the fix the estimate should succeed (placement group creation happens
	// after the estimate bail-out). We just verify no region-mismatch error.
	if strings.Contains(out, "InvalidPlacementGroup") {
		t.Errorf("placement group created in wrong region: %s", out)
	}
	if err == nil {
		t.Log("MPI + us-east-2 estimate OK")
	} else {
		t.Logf("estimate returned non-zero (acceptable if quota/capacity issue): %v\n%s", err, out)
	}
}

// TestTier1_LagottoWatchLifecycle creates, extends, and cancels a lagotto watch.
func TestTier1_LagottoWatchLifecycle(t *testing.T) {
	lagottoBin, err := exec.LookPath("lagotto")
	if err != nil {
		t.Skip("lagotto binary not on PATH")
	}

	ctx := context.Background()
	_ = ctx

	// Create watch
	out, err := exec.Command(lagottoBin, "watch", "t3.small", "--ttl", "1h", "--action", "hold").CombinedOutput() //nolint:gosec // nosemgrep
	if err != nil {
		outStr := string(out)
		if strings.Contains(outStr, "AccessDenied") || strings.Contains(outStr, "not authorized") {
			t.Skipf("lagotto DynamoDB not permitted in this account/role — needs dynamodb:PutItem on lagotto-watches: %s", outStr)
		}
		t.Fatalf("lagotto watch: %v\n%s", err, out)
	}
	// Parse watch ID from output
	var watchID string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Created watch ") {
			watchID = strings.TrimPrefix(line, "Created watch ")
			watchID = strings.TrimSpace(watchID)
		}
	}
	if watchID == "" {
		t.Fatalf("could not parse watch ID from output:\n%s", out)
	}
	t.Logf("created watch: %s", watchID)
	t.Cleanup(func() {
		exec.Command(lagottoBin, "cancel", watchID).Run() //nolint:gosec // nosemgrep
	})

	// Extend
	out, err = exec.Command(lagottoBin, "extend", watchID, "--ttl", "2h").CombinedOutput() //nolint:gosec // nosemgrep
	if err != nil {
		t.Fatalf("lagotto extend: %v\n%s", err, out)
	}

	// Status
	out, err = exec.Command(lagottoBin, "status", watchID).CombinedOutput() //nolint:gosec // nosemgrep
	if err != nil {
		t.Fatalf("lagotto status: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), watchID) {
		t.Errorf("expected watch ID in status output, got:\n%s", out)
	}

	// Cancel
	out, err = exec.Command(lagottoBin, "cancel", watchID).CombinedOutput() //nolint:gosec // nosemgrep
	if err != nil {
		t.Fatalf("lagotto cancel: %v\n%s", err, out)
	}
	t.Logf("watch lifecycle complete: %s", watchID)
}

// TestTier1_SpawnDefaults verifies spawn defaults set/get/unset round-trip.
func TestTier1_SpawnDefaults(t *testing.T) {
	// Set
	spawn(t, "defaults", "set", "idle-timeout", "45m")

	// Get
	out := spawn(t, "defaults", "list")
	if !strings.Contains(out, "idle-timeout") {
		t.Errorf("expected idle-timeout in defaults list, got:\n%s", out)
	}

	// Unset
	spawn(t, "defaults", "unset", "idle-timeout")

	// Verify removed
	out = spawn(t, "defaults", "list")
	if strings.Contains(out, "idle-timeout: 45m") {
		t.Errorf("idle-timeout still present after unset:\n%s", out)
	}
	t.Log("defaults set/unset round-trip OK")
}
