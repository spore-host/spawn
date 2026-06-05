// Package e2e contains end-to-end integration tests for spawn, truffle, and lagotto.
//
// Tests are split into three independently runnable tiers:
//
//	Tier 1 — API-only (no EC2 instances, ~free):
//	  go test -v -tags=e2e_tier1 ./test/e2e/ -run TestTier1
//
//	Tier 2 — Single instance (~$0.50, 15–20 min):
//	  go test -v -tags=e2e_tier2 ./test/e2e/ -run TestTier2 -timeout 30m
//
//	Tier 3 — Multi-instance ($2–$5, 20–35 min):
//	  go test -v -tags=e2e_tier3 ./test/e2e/ -run TestTier3 -timeout 60m
//
// All tiers require AWS credentials via AWS_PROFILE=spore-host-dev or
// the standard credential chain. A compiled spawn binary must be on PATH
// or at ./bin/spawn relative to the spawn/ module root.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ── Configuration ─────────────────────────────────────────────────────────────

const (
	testRegion       = "us-east-1"
	testInstanceType = "t3.small" // cheap, available everywhere
	testTagKey       = "spawn:e2e-test-run"
	defaultTTL       = "15m"
)

// spawnBin returns the path to the spawn binary.
// Looks for ./bin/spawn first, then falls back to PATH.
func spawnBin(t *testing.T) string {
	t.Helper()
	// Walk up to find the spawn module root (contains go.mod with module spawn)
	_, file, _, _ := runtime.Caller(0)
	// file is .../spawn/test/e2e/helpers.go
	spawnRoot := filepath.Join(filepath.Dir(file), "..", "..")
	candidate := filepath.Join(spawnRoot, "bin", "spawn")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	// Fall back to PATH
	p, err := exec.LookPath("spawn")
	if err != nil {
		t.Fatalf("spawn binary not found at %s or in PATH; run 'make build' first", candidate)
	}
	return p
}

// runID returns a per-test unique run ID for tagging resources.
// Uses test name + timestamp to remain unique across parallel runs.
func runID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
}

// ── AWS helpers ───────────────────────────────────────────────────────────────

func loadAWSConfig(t *testing.T) aws.Config {
	t.Helper()
	ctx := context.Background()
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" && os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		profile = "spore-host-dev"
	}
	var opts []func(*awsconfig.LoadOptions) error
	opts = append(opts, awsconfig.WithRegion(testRegion))
	if profile != "" && os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	return cfg
}

// ── spawn CLI wrapper ─────────────────────────────────────────────────────────

// spawn runs the spawn CLI and returns combined output.
func spawn(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(spawnBin(t), args...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("spawn %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	// Log output so we can debug silent failures (exit 0 but no instance created)
	if len(out) > 0 {
		t.Logf("spawn %s:\n%s", args[0], strings.TrimSpace(string(out)))
	}
	return string(out)
}

// spawnMayFail runs spawn and returns output + error without failing the test.
func spawnMayFail(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(spawnBin(t), args...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// spawnStdout runs spawn and returns ONLY stdout (stderr discarded). Use this
// for `-o json` calls: spawn writes progress/log noise (e.g. "Searching region
// ...") to stderr, so CombinedOutput would prepend non-JSON text and break
// json.Unmarshal — which silently stalled every waitForRunning/State poll (#56).
func spawnStdout(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(spawnBin(t), args...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd.Env = os.Environ()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run() // stderr goes to the child's /dev/null (not captured)
	return stdout.String(), err
}

// ── Instance lifecycle helpers ────────────────────────────────────────────────

// InstanceJSON is the minimal shape returned by spawn list --output json.
type InstanceJSON struct {
	InstanceID   string            `json:"instance_id"`
	Name         string            `json:"name"`
	InstanceType string            `json:"instance_type"`
	State        string            `json:"state"`
	Region       string            `json:"region"`
	PublicIP     string            `json:"public_ip"`
	Tags         map[string]string `json:"tags"`
}

// launchSem bounds how many `spawn launch` invocations run concurrently. With
// ~22 t.Parallel() tests each launching, an unbounded burst overwhelms the
// launch path and AWS API (a contributing factor in #56). Capping only the
// launch burst — not go test -parallel — keeps the running/assertion phases
// fully parallel while smoothing the launch storm. Cost control is unaffected
// (TTL, reaper, t.Cleanup are untouched).
var launchSem = make(chan struct{}, 6)

// launchInstance launches a single t3.small test instance and registers cleanup.
// Returns the launched InstanceJSON once the instance is running.
func launchInstance(t *testing.T, name string, extraArgs ...string) InstanceJSON {
	t.Helper()

	args := []string{
		"launch", name,
		"--instance-type", testInstanceType,
		"--region", testRegion,
		"--ttl", defaultTTL, // hard safety ceiling
		"--wait-for-running=false", // we poll ourselves via waitForRunning
		"--wait-for-ssh=false",     // we SSH via sshExec after waitForRunning
	}
	args = append(args, extraArgs...)

	// Throttle the launch burst, then release before the (long) running phase.
	launchSem <- struct{}{}
	spawn(t, args...)
	<-launchSem

	// Register cleanup — terminate by name regardless of test outcome.
	t.Cleanup(func() { terminateByName(t, name) })

	return waitForRunning(t, name, 10*time.Minute)
}

// terminateByName terminates (not stops) all non-terminated instances with the
// given name, across whatever region each is in. Terminating — never stopping —
// is the only correct cleanup for an ephemeral test instance (#47).
func terminateByName(t *testing.T, name string) {
	t.Helper()
	out, err := spawnStdout(t, "list", "--output", "json")
	if err != nil {
		return
	}
	var instances []InstanceJSON
	if json.Unmarshal([]byte(out), &instances) != nil {
		return
	}
	for _, inst := range instances {
		if inst.Name == name && inst.State != "terminated" {
			cfg := loadAWSConfig(t)
			ctx := context.Background()
			regionalCfg := cfg.Copy()
			regionalCfg.Region = inst.Region
			if inst.Region == "" {
				regionalCfg.Region = testRegion
			}
			ec2Client := ec2.NewFromConfig(regionalCfg)
			ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{ //nolint:errcheck
				InstanceIds: []string{inst.InstanceID},
			})
			t.Logf("cleanup: terminated %s (%s)", inst.Name, inst.InstanceID)
		}
	}
}

// waitForRunning polls until an instance named `name` is running or the timeout fires.
// Filters to testRegion to avoid scanning all regions on every poll.
func waitForRunning(t *testing.T, name string, timeout time.Duration) InstanceJSON {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastState string
	for time.Now().Before(deadline) {
		out, err := spawnStdout(t, "list", "--region", testRegion, "--output", "json")
		if err == nil {
			var instances []InstanceJSON
			if json.Unmarshal([]byte(out), &instances) == nil {
				for _, inst := range instances {
					if inst.Name == name {
						if inst.State != lastState {
							t.Logf("  %s → %s (%.0fs elapsed)", name, inst.State, time.Since(deadline.Add(-timeout)).Seconds())
							lastState = inst.State
						}
						if inst.State == "running" {
							return inst
						}
					}
				}
				if lastState == "" {
					t.Logf("  waiting for %s to appear in spawn list... (%.0fs elapsed)", name, time.Since(deadline.Add(-timeout)).Seconds())
					lastState = "not-yet-visible"
				}
			}
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("instance %q did not reach running state within %v (last state: %s)", name, timeout, lastState)
	return InstanceJSON{}
}

// waitForState polls until the named instance reaches the target state.
// Filters to testRegion to avoid scanning all regions on every poll.
func waitForState(t *testing.T, name, state string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := spawnStdout(t, "list", "--region", testRegion, "--output", "json")
		if err == nil {
			var instances []InstanceJSON
			if json.Unmarshal([]byte(out), &instances) == nil {
				for _, inst := range instances {
					if inst.Name == name && inst.State == state {
						return
					}
				}
				// Instance not in list at all → terminated
				if state == "terminated" {
					return
				}
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("instance %q did not reach state %q within %v", name, state, timeout)
}

// instanceState returns the current state of the named instance via spawn list,
// or "" if it isn't found. Used for point-in-time state assertions (e.g. that
// connect actually woke a stopped instance to running).
func instanceState(t *testing.T, name string) string {
	t.Helper()
	out, err := spawnStdout(t, "list", "--region", testRegion, "--output", "json")
	if err != nil {
		return ""
	}
	var instances []InstanceJSON
	if json.Unmarshal([]byte(out), &instances) != nil {
		return ""
	}
	for _, inst := range instances {
		if inst.Name == name {
			return inst.State
		}
	}
	return ""
}

// sshExec runs a command on the instance via spawn connect one-shot mode.
func sshExec(t *testing.T, name, cmd string) string {
	t.Helper()
	out := spawn(t, "connect", name, "--", cmd)
	return out
}

// terminateByTag force-terminates all EC2 instances tagged with key=value in all regions.
// Used as cleanup; errors are logged but do not fail the test.
func terminateByTag(t *testing.T, key, value string) {
	t.Helper()
	cfg := loadAWSConfig(t)
	ctx := context.Background()
	ec2Client := ec2.NewFromConfig(cfg)

	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: stringPtr("tag:" + key), Values: []string{value}},
			{Name: stringPtr("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	})
	if err != nil {
		t.Logf("cleanup: describe instances: %v", err)
		return
	}
	var ids []string
	for _, r := range result.Reservations {
		for _, inst := range r.Instances {
			ids = append(ids, *inst.InstanceId)
		}
	}
	if len(ids) == 0 {
		return
	}
	if _, err := ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: ids,
	}); err != nil {
		t.Logf("cleanup: terminate instances %v: %v", ids, err)
	} else {
		t.Logf("cleanup: terminated %v", ids)
	}
}

func stringPtr(s string) *string { return &s }

// describeInstanceTags returns all EC2 tags for an instance as a map.
func describeInstanceTags(t *testing.T, cfg aws.Config, instanceID, region string) map[string]string {
	t.Helper()
	ctx := context.Background()
	regionalCfg := cfg.Copy()
	regionalCfg.Region = region
	ec2Client := ec2.NewFromConfig(regionalCfg)

	out, err := ec2Client.DescribeTags(ctx, &ec2.DescribeTagsInput{
		Filters: []ec2types.Filter{
			{Name: stringPtr("resource-id"), Values: []string{instanceID}},
		},
	})
	if err != nil {
		t.Logf("DescribeTags(%s): %v", instanceID, err)
		return nil
	}
	tags := make(map[string]string, len(out.Tags))
	for _, tag := range out.Tags {
		if tag.Key != nil && tag.Value != nil {
			tags[*tag.Key] = *tag.Value
		}
	}
	return tags
}
