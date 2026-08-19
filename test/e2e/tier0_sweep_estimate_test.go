//go:build e2e_tier0

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// Tier 0 regression coverage for #524: --estimate-only must launch nothing on a
// parameter sweep, whichever orchestration path the sweep would have taken.
//
// The check used to live only inside launchSweepDetached, so a sweep that
// reached the FOREGROUND path — via --no-detach, or via an explicit --detach
// with no --max-concurrent — ran the full launch while the user was asking for a
// preview. The three subtests below are the three paths; (b) and (c) are the
// ones that failed before the fix.
//
// Why the assertion is "zero instances in EC2" and not "the estimateOnly flag
// was honoured": the bug WAS a correct flag check, sitting behind a dispatch. A
// test that trusts an internal flag would have passed for the entire time the
// bug was live. Only the observable distinguishes the two.

// writeSweepParamFile writes a minimal two-row param file and returns its path.
// Two rows rather than one so a partial launch is still caught, and c5.large
// because Substrate models that family.
func writeSweepParamFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sweep.yaml")
	body := `defaults:
  on_complete: terminate
params:
  - instance_type: c5.large
  - instance_type: c5.xlarge
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write param file: %v", err)
	}
	return path
}

// everyInstanceID returns every instance Substrate knows about, in any state,
// read straight from EC2.
//
// It deliberately does NOT go through `spawn list --state all`. That flag passes
// "all" through as a literal instance-state-name filter, which matches nothing,
// so it returns [] unconditionally (#527) — using it here would make this whole
// test unfailable, which is the same defect class the test exists to catch.
func (e *spawnEnv) everyInstanceID() []string {
	e.t.Helper()
	out, err := e.EC2Client().DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{})
	if err != nil {
		e.t.Fatalf("DescribeInstances: %v", err)
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			ids = append(ids, aws.ToString(inst.InstanceId))
		}
	}
	return ids
}

// requireNothingLaunched asserts the emulator holds no instances at all, and
// that spawn's own inventory agrees. Both halves matter: EC2 is ground truth,
// and `spawn list` is what a user would check.
func (e *spawnEnv) requireNothingLaunched(what string) {
	e.t.Helper()
	if ids := e.everyInstanceID(); len(ids) != 0 {
		e.t.Errorf("%s: expected ZERO instances, EC2 has %d: %v", what, len(ids), ids)
	}
	if listed := mustJSONArray(e.t, e.runOK("list", "-o", "json")); len(listed) != 0 {
		e.t.Errorf("%s: spawn list reports %d instances, want 0: %v", what, len(listed), listed)
	}
}

// estimateOnlySweep runs a sweep with --estimate-only plus the caller's path
// flags, requires exit 0, and returns stderr (where the estimate is printed).
func (e *spawnEnv) estimateOnlySweep(paramFile string, pathFlags ...string) string {
	e.t.Helper()
	args := append([]string{
		"launch", "est-check",
		"--param-file", paramFile,
		"--region", "us-east-1",
		"--estimate-only",
		"--wait-for-running=false",
		"--wait-for-ssh=false",
		"-y",
	}, pathFlags...)
	stdout, stderr, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("spawn %v: expected exit 0, got %d\nstdout:\n%s\nstderr:\n%s",
			args, code, stdout, stderr)
	}
	return stderr
}

// TestTier0_SweepEstimateOnly_DetachedLaunchesNothing is the path that was
// already correct. It is here so the fix cannot regress into "only the path we
// just fixed is safe".
func TestTier0_SweepEstimateOnly_DetachedLaunchesNothing(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.estimateOnlySweep(writeSweepParamFile(t))
	env.requireNothingLaunched("--estimate-only (detached, default)")
}

// TestTier0_SweepEstimateOnly_NoDetachLaunchesNothing is the #524 case.
//
// --no-detach is the documented advice for a heterogeneous sweep, because the
// foreground path is the only one that detects an AMI per config (#372). Before
// the fix, taking that advice silently disabled --estimate-only and launched
// every row. --ttl is required by the --no-detach guard.
func TestTier0_SweepEstimateOnly_NoDetachLaunchesNothing(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.estimateOnlySweep(writeSweepParamFile(t), "--no-detach", "--ttl", "1h")
	env.requireNothingLaunched("--estimate-only --no-detach")
}

// TestTier0_SweepEstimateOnly_ExplicitDetachLaunchesNothing is the second way
// into the foreground path, and the more surprising one: an explicit --detach
// with no --max-concurrent leaves maxConcurrent at 0, because the min(len,10)
// default is applied only inside the AUTO-enable branch. The dispatch condition
// `detach && maxConcurrent > 0` is then false, so the user who asked for
// detached orchestration got the foreground path — with no estimate and, before
// the fix, a live launch.
func TestTier0_SweepEstimateOnly_ExplicitDetachLaunchesNothing(t *testing.T) {
	env := startSpawnSubstrate(t)
	env.estimateOnlySweep(writeSweepParamFile(t), "--detach")
	env.requireNothingLaunched("--estimate-only --detach (maxConcurrent=0)")
}
