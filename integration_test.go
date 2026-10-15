//go:build integration
// +build integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"gopkg.in/yaml.v3"
)

// Integration tests for multi-region sweep features.
// Run with: make build && go test -v -count=1 -tags=integration .
//
// COST: these launch REAL, billable EC2 instances (CLAUDE.md "Cost safety").
// Every sweep they launch is bounded three ways:
//   - TTL: every param file sets `ttl: 30m`, so spored self-terminates;
//   - explicit terminate: launchSweep registers a t.Cleanup that runs
//     `spawn cancel --yes` — registered at launch, so a t.Fatalf part-way
//     through a test still cancels. (--yes is load-bearing: cancel prompts,
//     and with no answer on stdin it aborts without terminating anything.);
//   - leak-check: the same cleanup then asks EC2 directly, in every region the
//     param file names, for instances tagged spawn:sweep-id=<id> that are not
//     terminated, terminates any it finds, and fails the test.
//
// Compile-only in CI and `make check` (scripts/check-build-tags.sh, #913); never
// executed there.

// loadAWSConfig loads AWS config using AWS_PROFILE env var when set (local dev),
// or the default credential chain (CI with configure-aws-credentials action).
func loadAWSConfig(ctx context.Context, defaultProfile string) (aws.Config, error) {
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" {
		profile = defaultProfile
	}
	if profile != "" && os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithSharedConfigProfile(profile))
	}
	return awsconfig.LoadDefaultConfig(ctx)
}

func TestMultiRegionBasicLaunch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create test parameter file
	paramFile := createTempParamFile(t, `
defaults:
  instance_type: t3.micro
  ttl: 30m
  spot: true

params:
  - name: test-east-1
    ami: ami-0e2c8caa4b6378d8c
    region: us-east-1
  - name: test-west-2
    ami: ami-05134c8ef96964280
    region: us-west-2
`)
	defer os.Remove(paramFile)

	// Launch sweep
	t.Log("Launching multi-region sweep...")
	sweepID := launchSweep(t, paramFile, "--max-concurrent", "2", "--detach")
	t.Logf("Sweep ID: %s", sweepID)

	// Wait for instances to launch
	time.Sleep(20 * time.Second)

	// Check status
	status := getSweepStatus(t, sweepID)
	if !status.MultiRegion {
		t.Errorf("Expected multi_region=true, got false")
	}
	if len(status.RegionStatus) != 2 {
		t.Errorf("Expected 2 regions, got %d", len(status.RegionStatus))
	}

	// Verify both regions have work assigned
	for region, rs := range status.RegionStatus {
		t.Logf("Region %s: Launched=%d, Failed=%d, Pending=%d", region, rs.Launched, rs.Failed, len(rs.NextToLaunch))
		if rs.Launched == 0 && rs.Failed == 0 && len(rs.NextToLaunch) == 0 {
			t.Errorf("Region %s has no instances assigned (launched, failed, or pending)", region)
		}
	}

	// Cancel explicitly so the CANCELLED transition can be asserted; the
	// cleanup launchSweep registered still leak-checks afterwards.
	t.Log("Canceling sweep...")
	cancelSweep(t, sweepID)

	// Wait for cancellation
	time.Sleep(5 * time.Second)
	finalStatus := getSweepStatus(t, sweepID)
	if finalStatus.Status != "CANCELLED" {
		t.Logf("Warning: Expected CANCELLED status, got %s", finalStatus.Status)
	}
}

func TestPerRegionConcurrentLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create parameter file with many instances per region
	paramFile := createTempParamFile(t, `
defaults:
  instance_type: t3.micro
  ttl: 30m
  spot: true

params:
  - name: test-east-1
    ami: ami-0e2c8caa4b6378d8c
    region: us-east-1
  - name: test-east-2
    ami: ami-0e2c8caa4b6378d8c
    region: us-east-1
  - name: test-east-3
    ami: ami-0e2c8caa4b6378d8c
    region: us-east-1
  - name: test-west-1
    ami: ami-05134c8ef96964280
    region: us-west-2
  - name: test-west-2
    ami: ami-05134c8ef96964280
    region: us-west-2
  - name: test-west-3
    ami: ami-05134c8ef96964280
    region: us-west-2
`)
	defer os.Remove(paramFile)

	// Launch with per-region limit
	t.Log("Launching sweep with per-region limit of 2...")
	sweepID := launchSweep(t, paramFile, "--max-concurrent", "10", "--max-concurrent-per-region", "2", "--detach")
	t.Logf("Sweep ID: %s", sweepID)

	// Wait for some launches
	time.Sleep(15 * time.Second)

	// Check that no region exceeds limit
	status := getSweepStatus(t, sweepID)
	for region, rs := range status.RegionStatus {
		t.Logf("Region %s: ActiveCount=%d", region, rs.ActiveCount)
		if rs.ActiveCount > 2 {
			t.Errorf("Region %s exceeded per-region limit: %d active instances (max 2)", region, rs.ActiveCount)
		}
	}

}

func TestInstanceTypeFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Use fallback pattern: try expensive GPU first, fallback to cheap
	paramFile := createTempParamFile(t, `
defaults:
  instance_type: p5.48xlarge|g6.xlarge|t3.micro
  ttl: 30m
  spot: true

params:
  - name: test-fallback-1
    ami: ami-0e2c8caa4b6378d8c
    region: us-east-1
`)
	defer os.Remove(paramFile)

	t.Log("Launching sweep with fallback instance types...")
	sweepID := launchSweep(t, paramFile, "--max-concurrent", "1", "--detach")
	t.Logf("Sweep ID: %s", sweepID)

	// Wait for launch attempt
	time.Sleep(20 * time.Second)

	// Check what instance type was actually launched
	status := getSweepStatus(t, sweepID)

	// Find the launched instance
	var foundInstance bool
	for _, inst := range status.Instances {
		if inst.InstanceID != "" {
			foundInstance = true
			t.Logf("Instance launched: RequestedType=%s, ActualType=%s", inst.RequestedType, inst.ActualType)

			if inst.RequestedType == "" {
				t.Error("RequestedType should be populated with pattern")
			}
			if inst.ActualType == "" {
				t.Error("ActualType should be populated with actual instance type")
			}

			// Most likely t3.micro was used (GPU instances usually unavailable as spot)
			if inst.ActualType != "t3.micro" && inst.ActualType != "g6.xlarge" {
				t.Logf("Note: Unexpected instance type %s (expected t3.micro or g6.xlarge)", inst.ActualType)
			}
		}
	}

	if !foundInstance {
		t.Error("No instance was launched")
	}

}

func TestRegionalCostTracking(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	paramFile := createTempParamFile(t, `
defaults:
  instance_type: t3.micro
  ttl: 30m
  spot: true

params:
  - name: test-cost-1
    ami: ami-0e2c8caa4b6378d8c
    region: us-east-1
  - name: test-cost-2
    ami: ami-05134c8ef96964280
    region: us-west-2
`)
	defer os.Remove(paramFile)

	t.Log("Launching sweep for cost tracking test...")
	sweepID := launchSweep(t, paramFile, "--max-concurrent", "2", "--detach")
	t.Logf("Sweep ID: %s", sweepID)

	// Wait for instances to run for a bit
	time.Sleep(30 * time.Second)

	// Check status for cost information
	status := getSweepStatus(t, sweepID)

	foundCosts := false
	for region, rs := range status.RegionStatus {
		t.Logf("Region %s: Cost=$%.2f, InstanceHours=%.1f", region, rs.EstimatedCost, rs.TotalInstanceHours)

		if rs.EstimatedCost > 0 || rs.TotalInstanceHours > 0 {
			foundCosts = true

			// Sanity checks
			if rs.TotalInstanceHours < 0 {
				t.Errorf("Region %s has negative instance hours: %.1f", region, rs.TotalInstanceHours)
			}
			if rs.EstimatedCost < 0 {
				t.Errorf("Region %s has negative cost: $%.2f", region, rs.EstimatedCost)
			}
		}
	}

	if !foundCosts {
		t.Log("Note: No costs tracked yet (instances may not have launched)")
	}

}

// Helper functions

func createTempParamFile(t *testing.T, content string) string {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "spawn-test-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("Failed to write temp file: %v", err)
	}

	if err := tmpFile.Close(); err != nil {
		t.Fatalf("Failed to close temp file: %v", err)
	}

	return tmpFile.Name()
}

// launchSweep launches a sweep and registers its teardown: cancel (unless the
// test already did) and then an independent leak-check. The regions to check
// are read from the param file BEFORE launching, so a param file naming no
// region fails here instead of launching something nothing would check.
func launchSweep(t *testing.T, paramFile string, args ...string) string {
	t.Helper()

	regions := paramFileRegions(t, paramFile)

	// Build command: spawn launch --param-file <file> <args>
	cmdArgs := []string{"launch", "--param-file", paramFile}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.Command("./bin/spawn", cmdArgs...)
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Launch command failed: %v\nOutput: %s", err, string(output))
	}

	// Parse sweep ID from output
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "Sweep ID:") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				sweepID := parts[2]
				t.Cleanup(func() {
					if !cancelledSweeps[sweepID] {
						cancelSweep(t, sweepID)
					}
					leakCheckSweep(t, sweepID, regions)
				})
				return sweepID
			}
		}
	}

	// Something may have launched even though no ID was printed; the TTL is
	// the only bound left, so say so loudly.
	t.Fatalf("Could not find Sweep ID in output — any instances it launched are bounded only by their TTL:\n%s", string(output))
	return ""
}

// paramFileRegions returns every distinct region a param file names, in its
// defaults or any param. Fatal when there are none: an empty set would make
// the leak-check vacuous.
func paramFileRegions(t *testing.T, paramFile string) []string {
	t.Helper()

	data, err := os.ReadFile(paramFile)
	if err != nil {
		t.Fatalf("read param file: %v", err)
	}
	var pf struct {
		Defaults map[string]interface{}   `yaml:"defaults"`
		Params   []map[string]interface{} `yaml:"params"`
	}
	if err := yaml.Unmarshal(data, &pf); err != nil {
		t.Fatalf("parse param file: %v", err)
	}

	seen := map[string]bool{}
	var regions []string
	for _, m := range append([]map[string]interface{}{pf.Defaults}, pf.Params...) {
		if r, ok := m["region"].(string); ok && r != "" && !seen[r] {
			seen[r] = true
			regions = append(regions, r)
		}
	}
	if len(regions) == 0 {
		t.Fatalf("param file %s names no region; refusing to launch a sweep the leak-check could not cover", paramFile)
	}
	return regions
}

// cancelledSweeps records sweeps a test cancelled itself, so the cleanup
// doesn't cancel twice. Integration tests in this file do not run in parallel.
var cancelledSweeps = map[string]bool{}

// leakCheckSweep independently verifies, against EC2 rather than the sweep's
// own state, that nothing tagged with this sweep ID is still alive. Anything it
// finds is terminated and the test fails: cancel was supposed to have done that.
//
// It polls for a while rather than checking once, because a detached sweep's
// orchestrator Lambda can launch between our cancel and its next look at the
// cancel flag; an instance that appears in that window is still a leak.
func leakCheckSweep(t *testing.T, sweepID string, regions []string) {
	t.Helper()

	ctx := context.Background()
	const (
		window   = 3 * time.Minute
		interval = 20 * time.Second
	)
	deadline := time.Now().Add(window)
	for {
		for _, region := range regions {
			cfg, err := loadAWSConfig(ctx, "spore-host-dev")
			if err != nil {
				t.Errorf("leak-check %s: load AWS config: %v — CHECK FOR ORPHANED INSTANCES MANUALLY (tag spawn:sweep-id=%s)", region, err, sweepID)
				continue
			}
			cfg.Region = region
			client := ec2.NewFromConfig(cfg)

			var live []string
			p := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{
				Filters: []ec2types.Filter{
					{Name: stringPtr("tag:spawn:sweep-id"), Values: []string{sweepID}},
					{Name: stringPtr("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
				},
			})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					t.Errorf("leak-check %s: describe: %v — CHECK FOR ORPHANED INSTANCES MANUALLY (tag spawn:sweep-id=%s)", region, err, sweepID)
					break
				}
				for _, r := range page.Reservations {
					for _, inst := range r.Instances {
						live = append(live, *inst.InstanceId)
					}
				}
			}
			if len(live) == 0 {
				continue
			}
			t.Errorf("LEAK: sweep %s left %d live instance(s) in %s after cancel: %v — terminating", sweepID, len(live), region, live)
			if _, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: live}); err != nil {
				t.Errorf("leak-check %s: terminate %v: %v — TERMINATE THESE MANUALLY", region, live, err)
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(interval)
	}
}

func getSweepStatus(t *testing.T, sweepID string) *SweepStatus {
	t.Helper()

	cmd := exec.Command("./bin/spawn", "status", "--sweep-id", sweepID, "--json")
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Status command failed: %v\nOutput: %s", err, string(output))
	}

	var status SweepStatus
	if err := json.Unmarshal(output, &status); err != nil {
		t.Fatalf("Failed to parse status JSON: %v\nOutput: %s", err, string(output))
	}

	return &status
}

func cancelSweep(t *testing.T, sweepID string) {
	t.Helper()

	cancelledSweeps[sweepID] = true

	// --yes: cancel asks for confirmation before terminating, and with no
	// answer on stdin it aborts having terminated nothing (#40, #737).
	cmd := exec.Command("./bin/spawn", "cancel", "--sweep-id", sweepID, "--yes")
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("Warning: Cancel command failed: %v\nOutput: %s", err, string(output))
		// Don't fail test on cancel failure - just log it
	} else {
		t.Logf("Sweep canceled: %s", sweepID)
	}
}

// Test data structures (simplified versions)

type SweepStatus struct {
	SweepID      string                     `json:"sweep_id"`
	SweepName    string                     `json:"sweep_name"`
	Status       string                     `json:"status"`
	MultiRegion  bool                       `json:"multi_region"`
	RegionStatus map[string]*RegionProgress `json:"region_status"`
	Instances    []InstanceInfo             `json:"instances"`
	TotalParams  int                        `json:"total_params"`
	Launched     int                        `json:"launched"`
	Failed       int                        `json:"failed"`
}

type RegionProgress struct {
	Launched           int     `json:"launched"`
	Failed             int     `json:"failed"`
	ActiveCount        int     `json:"active_count"`
	NextToLaunch       []int   `json:"next_to_launch"`
	TotalInstanceHours float64 `json:"total_instance_hours"`
	EstimatedCost      float64 `json:"estimated_cost"`
}

type InstanceInfo struct {
	InstanceID    string `json:"instance_id"`
	Region        string `json:"region"`
	RequestedType string `json:"requested_type"`
	ActualType    string `json:"actual_type"`
	State         string `json:"state"`
}

// TestDynamoDBConnection verifies we can connect to DynamoDB
func TestDynamoDBConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	cfg, err := loadAWSConfig(ctx, "spore-host-infra")
	if err != nil {
		t.Fatalf("Failed to load AWS config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	// Try to describe the sweep orchestration table
	_, err = client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: stringPtr("spawn-sweep-orchestration"),
	})

	if err != nil {
		t.Fatalf("Failed to describe DynamoDB table: %v", err)
	}

	t.Log("✓ Successfully connected to DynamoDB")
}

func stringPtr(s string) *string {
	return &s
}
