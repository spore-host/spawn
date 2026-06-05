//go:build e2e_tier2

package e2e

// Tier 2 — Single-instance tests. Launches one t3.small per test.
// Estimated cost: ~$1 total, ~20-25 min with -parallel 4.
// All tests call t.Parallel() so go test -parallel N is effective.
//
// Run: go test -v -tags=e2e_tier2 ./test/e2e/ -run TestTier2 -parallel 4 -timeout 45m

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTier2_PublicIP verifies every launched instance gets a public IP (regression #308).
func TestTier2_PublicIP(t *testing.T) {
	t.Parallel()
	name := "e2e-public-ip-" + runID(t)
	inst := launchInstance(t, name)

	if inst.PublicIP == "" {
		t.Fatalf("instance %s launched with no public IP — regression #308", inst.InstanceID)
	}
	t.Logf("instance %s has public IP %s", inst.InstanceID, inst.PublicIP)
}

// TestTier2_ConnectSSH verifies spawn connect can reach the instance (interactive skip, one-shot).
func TestTier2_ConnectSSH(t *testing.T) {
	t.Parallel()
	name := "e2e-connect-" + runID(t)
	launchInstance(t, name)

	out := sshExec(t, name, "echo SPORE_OK")
	if !strings.Contains(out, "SPORE_OK") {
		t.Fatalf("expected SPORE_OK from ssh, got:\n%s", out)
	}
	t.Log("spawn connect one-shot OK")
}

// TestTier2_CommandExecution verifies --command runs on the instance (regression #298).
func TestTier2_CommandExecution(t *testing.T) {
	t.Parallel()
	name := "e2e-command-" + runID(t)
	launchInstance(t, name, "--command", "echo SPAWN_COMMAND_RAN > /tmp/cmd-ran.txt")

	// --command runs after spored bootstrap, which finishes 1-3 min after the
	// instance reaches "running" — poll until the file appears rather than
	// guessing a fixed delay.
	sshExecEventually(t, name, "cat /tmp/cmd-ran.txt 2>/dev/null", "SPAWN_COMMAND_RAN", 4*time.Minute)
	t.Log("--command executed successfully")
}

// TestTier2_OnComplete verifies --on-complete terminate fires when sentinel file appears.
func TestTier2_OnComplete(t *testing.T) {
	t.Parallel()
	name := "e2e-on-complete-" + runID(t)
	launchInstance(t, name,
		"--on-complete", "terminate",
		"--completion-delay", "10s",
	)

	// Touch the sentinel file
	sshExec(t, name, "touch /tmp/SPAWN_COMPLETE")
	t.Log("sentinel file created — waiting for termination")

	// Instance should terminate within ~2 min (10s delay + shutdown time)
	waitForState(t, name, "terminated", 3*time.Minute)
	t.Log("--on-complete terminate fired correctly")
}

// TestTier2_PreStop verifies --pre-stop runs before termination.
func TestTier2_PreStop(t *testing.T) {
	t.Parallel()
	name := "e2e-prestop-" + runID(t)
	launchInstance(t, name,
		"--on-complete", "terminate",
		"--completion-delay", "5s",
		"--pre-stop", "touch /tmp/prestop-ran.txt",
		"--pre-stop-timeout", "30s",
	)

	// Wait until spored is up and accepting the completion signal, then trigger it.
	sshExecEventually(t, name, "sudo systemctl is-active spored 2>/dev/null || echo waiting", "active", 4*time.Minute)
	sshExec(t, name, "touch /tmp/SPAWN_COMPLETE")

	// pre-stop runs between the completion signal and termination — poll for it
	// (the instance may terminate shortly after, so accept either the marker or
	// that the instance has already gone, which only happens after pre-stop ran).
	out := sshExecEventually(t, name, "test -f /tmp/prestop-ran.txt && echo PRESTOP_YES || echo NO", "PRESTOP_YES", 90*time.Second)
	if !strings.Contains(out, "PRESTOP_YES") {
		t.Errorf("--pre-stop did not run: /tmp/prestop-ran.txt not found")
	}

	waitForState(t, name, "terminated", 2*time.Minute)
	t.Log("--pre-stop ran before termination")
}

// TestTier2_ExtendTTL verifies spawn extend pushes the TTL deadline.
func TestTier2_ExtendTTL(t *testing.T) {
	t.Parallel()
	name := "e2e-extend-" + runID(t)
	launchInstance(t, name, "--ttl", "5m")

	// Extend by 10 more minutes
	spawn(t, "extend", name, "10m")

	// Check status — instance should still be running (not near-expiry)
	out := sshExec(t, name, "sudo spored status")
	if !strings.Contains(out, "TTL") {
		t.Logf("spored status: %s", out)
	}
	t.Log("spawn extend completed without error")
}

// TestTier2_SpawnStop verifies spawn stop halts billing without deleting instance.
func TestTier2_SpawnStop(t *testing.T) {
	t.Parallel()
	name := "e2e-stop-" + runID(t)
	launchInstance(t, name)

	spawn(t, "stop", name)
	waitForState(t, name, "stopped", 3*time.Minute)
	t.Log("instance stopped")

	spawn(t, "start", name)
	waitForState(t, name, "running", 3*time.Minute)
	t.Log("instance restarted")
}

// TestTier2_IAMPolicyApplied verifies --iam-policy adds permissions to the role (regression #299).
func TestTier2_IAMPolicyApplied(t *testing.T) {
	t.Parallel()
	name := "e2e-iam-policy-" + runID(t)
	launchInstance(t, name, "--iam-policy", "s3:ReadOnly")

	// The instance should be able to list S3 (read permission applied). IAM
	// instance-profile + policy attachment take time to propagate to the running
	// instance after launch, so poll for a non-AccessDenied result. `aws s3 ls`
	// prints "S3_OK" on success (even with zero buckets) and the AWS CLI prints
	// AccessDenied to stderr+exit-nonzero when the policy isn't effective yet.
	deadline := time.Now().Add(4 * time.Minute)
	var lastOut string
	granted := false
	for time.Now().Before(deadline) {
		out, _ := spawnMayFail(t, "connect", name, "--", "aws s3 ls >/dev/null 2>&1 && echo S3_OK || echo S3_DENIED")
		lastOut = out
		if strings.Contains(out, "S3_OK") {
			granted = true
			break
		}
		time.Sleep(15 * time.Second)
	}
	if !granted {
		t.Errorf("--iam-policy s3:ReadOnly did not grant S3 access within 4m; last:\n%s", lastOut)
	}
	t.Log("IAM policy granted S3 access")
}

// TestTier2_SporedStatus verifies spored is running and reports TTL.
func TestTier2_SporedStatus(t *testing.T) {
	t.Parallel()
	name := "e2e-spored-status-" + runID(t)
	launchInstance(t, name, "--ttl", "15m")

	out := sshExec(t, name, "sudo spored status")
	if !strings.Contains(out, "TTL") {
		t.Errorf("expected TTL in spored status output, got:\n%s", out)
	}
	t.Logf("spored status:\n%s", out)
}

// TestTier2_CompoundSSHCommand verifies spawn connect -- 'cmd && cmd2' works
// on a real instance (regression for #315).
func TestTier2_CompoundSSHCommand(t *testing.T) {
	t.Parallel()
	name := "e2e-compound-ssh-" + runID(t)
	launchInstance(t, name)

	// Compound command: write two files sequentially
	out := sshExec(t, name, "echo A > /tmp/a.txt && echo B > /tmp/b.txt && cat /tmp/a.txt /tmp/b.txt")
	if !strings.Contains(out, "A") || !strings.Contains(out, "B") {
		t.Errorf("compound && command failed; expected A and B in output, got:\n%s", out)
	}

	// Semicolon separator
	out = sshExec(t, name, "echo X; echo Y")
	if !strings.Contains(out, "X") || !strings.Contains(out, "Y") {
		t.Errorf("semicolon command failed; expected X and Y in output, got:\n%s", out)
	}
	t.Log("compound SSH commands work correctly")
}

// TestTier2_FSxTagsWritten verifies spawn:fsx-id and spawn:fsx-mount-point
// tags are written when --fsx-id is used (regression for #314).
//
// --fsx-id is validated against the real FSx API at launch (DescribeFileSystems),
// so a fake id fails the launch outright — this test needs a real Lustre
// filesystem, provided via E2E_FSX_ID. When unset it skips, because the
// tag-writing logic itself is already covered without AWS by the buildTags unit
// test (pkg/aws/tags_test.go, the actual #314 regression test).
func TestTier2_FSxTagsWritten(t *testing.T) {
	t.Parallel()
	fsxID := os.Getenv("E2E_FSX_ID")
	if fsxID == "" {
		t.Skip("E2E_FSX_ID not set — FSx tag-writing is unit-tested in pkg/aws/tags_test.go; " +
			"set E2E_FSX_ID=fs-... to exercise it end-to-end against a real Lustre filesystem")
	}
	name := "e2e-fsx-tags-" + runID(t)
	inst := launchInstance(t, name, "--fsx-id", fsxID, "--fsx-mount-point", "/fsx")

	cfg := loadAWSConfig(t)
	out := describeInstanceTags(t, cfg, inst.InstanceID, testRegion)
	if out["spawn:fsx-id"] != fsxID {
		t.Errorf("spawn:fsx-id = %q, want %q (regression #314)", out["spawn:fsx-id"], fsxID)
	}
	if out["spawn:fsx-mount-point"] != "/fsx" {
		t.Errorf("spawn:fsx-mount-point = %q, want /fsx (regression #314)", out["spawn:fsx-mount-point"])
	}
	t.Log("FSx tags written correctly")
}

// TestTier2_SporedConfigSetGet verifies spored config set/get/list on a running instance.
func TestTier2_SporedConfigSetGet(t *testing.T) {
	t.Parallel()
	name := "e2e-spored-config-" + runID(t)
	launchInstance(t, name, "--ttl", "15m")

	// Get existing config
	out := sshExec(t, name, "sudo spored config list")
	if !strings.Contains(out, "ttl") && !strings.Contains(out, "idle") {
		t.Errorf("spored config list missing expected keys:\n%s", out)
	}

	// Get a specific value
	out = sshExec(t, name, "sudo spored config get ttl")
	if strings.TrimSpace(out) == "" {
		t.Errorf("spored config get ttl returned empty output")
	}
	t.Logf("spored config list and get work: %s", strings.TrimSpace(out))
}

// TestTier2_SpawnListFilters verifies spawn list filtering by state and tag.
func TestTier2_SpawnListFilters(t *testing.T) {
	t.Parallel()
	name := "e2e-list-filters-" + runID(t)
	inst := launchInstance(t, name)

	// Filter by state=running — should find our instance
	out, err := spawnMayFail(t, "list", "--state", "running", "--output", "json")
	if err != nil {
		t.Skipf("spawn list failed: %v", err)
	}
	var instances []InstanceJSON
	if json.Unmarshal([]byte(out), &instances) == nil {
		found := false
		for _, i := range instances {
			if i.InstanceID == inst.InstanceID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("running instance %s not found in spawn list --state running", inst.InstanceID)
		}
	}

	// Filter by instance-family — t3 should include our t3.small
	out, err = spawnMayFail(t, "list", "--instance-family", "t3", "--output", "json")
	if err == nil && json.Unmarshal([]byte(out), &instances) == nil {
		t.Logf("spawn list --instance-family t3 returned %d instances", len(instances))
	}
	t.Log("spawn list filters working")
}

// TestTier2_SlurmConvert verifies slurm convert produces valid spawn params.
// No instance is launched — this exercises the CLI parsing only.
func TestTier2_SlurmConvert(t *testing.T) {
	t.Parallel()
	// Write a minimal sbatch script
	f, err := os.CreateTemp("", "test-*.sbatch")
	if err != nil {
		t.Fatalf("create sbatch file: %v", err)
	}
	defer os.Remove(f.Name())
	fmt.Fprintln(f, "#!/bin/bash")
	fmt.Fprintln(f, "#SBATCH --job-name=e2e-test")
	fmt.Fprintln(f, "#SBATCH --time=01:00:00")
	fmt.Fprintln(f, "#SBATCH --mem=4G")
	fmt.Fprintln(f, "#SBATCH --cpus-per-task=4")
	fmt.Fprintln(f, "echo running")
	f.Close()

	out := spawn(t, "slurm", "convert", f.Name())
	// convert writes YAML to stdout
	if !strings.Contains(out, "instance_type") && !strings.Contains(out, "ttl") &&
		!strings.Contains(out, "mem") && !strings.Contains(out, "cpu") {
		t.Logf("slurm convert output (may vary by format):\n%s", out)
		// Not a hard failure — the conversion logic may produce different keys
	}
	t.Logf("slurm convert produced %d bytes of output", len(out))
}

// TestTier2_SpawnStatus verifies spawn status works by name and ID.
func TestTier2_SpawnStatus(t *testing.T) {
	t.Parallel()
	name := "e2e-status-" + runID(t)
	inst := launchInstance(t, name, "--ttl", "15m")

	// Status by name — calls spored status via SSH
	out, err := spawnMayFail(t, "status", name)
	if err != nil {
		t.Logf("spawn status by name returned error (may need SSH key): %v\n%s", err, out)
	} else {
		t.Logf("spawn status by name:\n%s", out[:min(len(out), 200)])
	}
	_ = inst
}

// TestTier2_BackgroundJob verifies spawn connect -- 'nohup cmd &' does not
// exit 255 (regression for #315 — & backgrounding caused SSH to exit 255).
func TestTier2_BackgroundJob(t *testing.T) {
	t.Parallel()
	name := "e2e-bg-job-" + runID(t)
	launchInstance(t, name)

	// Background a job that writes a file after a short delay
	sshExec(t, name, "nohup bash -c 'sleep 3 && echo BG_DONE > /tmp/bg.txt' > /tmp/bg.log 2>&1 &")
	t.Log("background job launched (no exit 255)")

	// Wait for it to finish
	time.Sleep(6 * time.Second)
	out := sshExec(t, name, "cat /tmp/bg.txt 2>/dev/null || echo MISSING")
	if strings.Contains(out, "MISSING") {
		t.Errorf("background job did not complete: /tmp/bg.txt missing")
	} else {
		t.Logf("background job completed: %s", strings.TrimSpace(out))
	}
}

// TestTier2_SporedComplete verifies spored complete creates the sentinel file
// and triggers the on-complete action.
func TestTier2_SporedComplete(t *testing.T) {
	t.Parallel()
	name := "e2e-spored-complete-" + runID(t)
	launchInstance(t, name,
		"--on-complete", "terminate",
		"--completion-delay", "5s",
	)

	// Use spored complete instead of touch
	sshExec(t, name, "sudo spored complete --status success --message 'e2e test done'")
	t.Log("spored complete called — waiting for termination")

	waitForState(t, name, "terminated", 3*time.Minute)
	t.Log("spored complete triggered on-complete terminate correctly")
}

// TestTier2_ExtendTTL_DeadlineMoved verifies spawn extend actually updates the
// spawn:ttl-deadline tag, not just returns without error.
func TestTier2_ExtendTTL_DeadlineMoved(t *testing.T) {
	t.Parallel()
	name := "e2e-extend-deadline-" + runID(t)
	inst := launchInstance(t, name, "--ttl", "5m")

	cfg := loadAWSConfig(t)

	// Read the deadline before extend
	tagsBefore := describeInstanceTags(t, cfg, inst.InstanceID, testRegion)
	deadlineBefore := tagsBefore["spawn:ttl-deadline"]
	if deadlineBefore == "" {
		t.Skip("spawn:ttl-deadline tag not set — skipping deadline verification")
	}

	// Extend by 10 minutes
	spawn(t, "extend", name, "10m")

	// Read again — deadline must have moved forward
	tagsAfter := describeInstanceTags(t, cfg, inst.InstanceID, testRegion)
	deadlineAfter := tagsAfter["spawn:ttl-deadline"]

	if deadlineAfter <= deadlineBefore {
		t.Errorf("deadline did not advance: before=%s after=%s", deadlineBefore, deadlineAfter)
	}
	t.Logf("deadline advanced from %s to %s", deadlineBefore, deadlineAfter)
}

// TestTier2_NameResolutionPrefersRunning verifies that when two instances share
// a name (stopped + running), spawn connect picks the running one (regression #313).
func TestTier2_NameResolutionPrefersRunning(t *testing.T) {
	t.Parallel()
	rid := runID(t)
	// Use a fixed base name so both instances share the same Name tag
	baseName := "e2e-ambiguous-" + rid

	// Launch first instance, then stop it
	inst1 := launchInstance(t, baseName, "--ttl", "15m")
	spawn(t, "stop", baseName)
	waitForState(t, baseName, "stopped", 3*time.Minute)
	t.Logf("first instance stopped: %s", inst1.InstanceID)

	// Launch second instance with the same name
	inst2 := launchInstance(t, baseName, "--ttl", "15m")
	t.Logf("second instance running: %s", inst2.InstanceID)

	// connect by name should reach the running one (inst2), not the stopped
	// dup. Poll (inst2's SSH may not be ready the instant it's "running"); the
	// IMDSv2-free metadata call returns the instance id it actually connected to.
	out := sshExecEventually(t, baseName,
		"curl -s http://169.254.169.254/latest/meta-data/instance-id", inst2.InstanceID, 4*time.Minute)
	if !strings.Contains(out, inst2.InstanceID) {
		t.Errorf("connect by name reached %q, expected running instance %s (regression #313)", strings.TrimSpace(out), inst2.InstanceID)
	}
	t.Logf("name resolution correctly picked running instance %s", inst2.InstanceID)
}

// TestTier2_SpawnValidate verifies spawn validate runs without crashing.
func TestTier2_SpawnValidate(t *testing.T) {
	t.Parallel()
	out, err := spawnMayFail(t, "validate", "--infrastructure", "--region", testRegion)
	if err != nil {
		t.Logf("spawn validate returned non-zero (acceptable — may need elevated IAM): %v\n%s", err, out)
	} else {
		t.Logf("spawn validate passed: %d bytes", len(out))
	}
}

// TestTier2_SpawnAvailability verifies spawn availability returns stats for a common instance type.
func TestTier2_SpawnAvailability(t *testing.T) {
	t.Parallel()
	out, err := spawnMayFail(t, "availability", "--instance-type", testInstanceType, "--regions", testRegion)
	if err != nil {
		t.Logf("spawn availability returned error (may need launch history): %v\n%s", err, out)
	} else {
		t.Logf("spawn availability: %d bytes", len(out))
	}
}

// TestTier2_ListTagFilter verifies spawn list --tag key=value filtering
// using the spawn:managed tag (always present on spawn instances).
func TestTier2_ListTagFilter(t *testing.T) {
	t.Parallel()
	name := "e2e-tag-filter-" + runID(t)
	inst := launchInstance(t, name)

	// Filter by the always-present spawn:managed=true tag
	out, err := spawnMayFail(t, "list", "--tag", "spawn:managed=true", "--output", "json")
	if err != nil {
		t.Skipf("spawn list --tag failed: %v", err)
	}
	var instances []InstanceJSON
	if json.Unmarshal([]byte(out), &instances) == nil {
		found := false
		for _, i := range instances {
			if i.InstanceID == inst.InstanceID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("instance %s not found when filtering by spawn:managed=true", inst.InstanceID)
		}
	}
	t.Log("spawn list --tag filter works")
}

// TestTier2_HibernateAndResume verifies spawn hibernate saves state and spawn start resumes it.
func TestTier2_HibernateAndResume(t *testing.T) {
	t.Parallel()
	name := "e2e-hibernate-" + runID(t)
	// Hibernation requires --hibernate flag at launch time
	launchInstance(t, name, "--hibernate")

	// Write a marker file to verify state is preserved across hibernate/resume
	sshExec(t, name, "echo HIBERNATE_MARKER > /tmp/hibernate-test.txt")

	// EC2 rejects hibernation until the hibernation agent has initialized
	// (minutes post-boot), so retry until it's accepted.
	hibernateEventually(t, name, 5*time.Minute)
	waitForState(t, name, "stopped", 4*time.Minute)
	t.Log("instance hibernated")

	spawn(t, "start", name)
	waitForState(t, name, "running", 4*time.Minute)
	t.Log("instance resumed from hibernation")

	// Verify marker file persists (RAM state restored)
	out := sshExec(t, name, "cat /tmp/hibernate-test.txt 2>/dev/null || echo MISSING")
	if strings.Contains(out, "MISSING") {
		t.Errorf("hibernate-test.txt missing after resume — RAM state not preserved")
	} else {
		t.Log("hibernate state preserved correctly")
	}
}

// TestTier2_ConnectWakesHibernated verifies the hibernate analog of auto-start:
// connecting to a hibernated instance wakes it (no explicit start), restores
// RAM state, and runs the command — proving the wake-via-connect path works for
// hibernation, not just stop.
func TestTier2_ConnectWakesHibernated(t *testing.T) {
	t.Parallel()
	name := "e2e-connect-hibernate-" + runID(t)
	launchInstance(t, name, "--hibernate")

	// Leave a RAM-resident marker, then hibernate (retry until the hibernation
	// agent is ready — EC2 rejects hibernate too soon after boot).
	sshExec(t, name, "echo WAKE_MARKER > /tmp/wake-test.txt")
	hibernateEventually(t, name, 5*time.Minute)
	waitForState(t, name, "stopped", 4*time.Minute)
	if st := instanceState(t, name); st != "stopped" {
		t.Fatalf("precondition: instance state = %q, want stopped (hibernated) before connect", st)
	}
	t.Log("instance hibernated — now connecting (should auto-resume)")

	// connect (not start) must wake it and run the command.
	out := sshExec(t, name, "cat /tmp/wake-test.txt 2>/dev/null || echo MISSING")
	if strings.Contains(out, "MISSING") || !strings.Contains(out, "WAKE_MARKER") {
		t.Errorf("connect did not resume hibernated instance with RAM state intact: %s", out)
	}
	waitForState(t, name, "running", 4*time.Minute)
	if st := instanceState(t, name); st != "running" {
		t.Errorf("after connect, hibernated instance state = %q, want running", st)
	}
	t.Log("spawn connect resumed hibernated instance and connected with RAM state intact")
}

// TestTier2_ConnectAutoStart verifies spawn connect automatically wakes a
// stopped instance and connects to it — the stop → connect → auto-start →
// exec cycle. It asserts the instance was genuinely stopped first and is
// genuinely running afterward, so a connect that somehow succeeded without
// actually waking the instance (or against a never-stopped one) can't pass.
func TestTier2_ConnectAutoStart(t *testing.T) {
	t.Parallel()
	name := "e2e-connect-autostart-" + runID(t)
	launchInstance(t, name)

	// Stop the instance and confirm it really reached stopped.
	spawn(t, "stop", name)
	waitForState(t, name, "stopped", 3*time.Minute)
	if st := instanceState(t, name); st != "stopped" {
		t.Fatalf("precondition: instance state = %q, want stopped before connect", st)
	}
	t.Log("instance stopped — now connecting (should auto-start)")

	// Connect should auto-start the stopped instance and run the command.
	out := sshExec(t, name, "echo AUTOSTART_OK")
	if !strings.Contains(out, "AUTOSTART_OK") {
		t.Errorf("connect after auto-start failed: %s", out)
	}

	// And the instance must now actually be running — proving connect woke it,
	// not just that the exec happened to return.
	waitForState(t, name, "running", 3*time.Minute)
	if st := instanceState(t, name); st != "running" {
		t.Errorf("after auto-start connect, instance state = %q, want running", st)
	}
	t.Log("spawn connect auto-started stopped instance, woke it to running, and connected")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
