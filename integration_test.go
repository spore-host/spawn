//go:build integration
// +build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Integration tests for multi-region sweep features
// Run with: go test -v -tags=integration ./...

// loadAWSConfig loads AWS config using AWS_PROFILE env var when set (local dev),
// or the default credential chain (CI with configure-aws-credentials action).
func loadAWSConfig(ctx context.Context, defaultProfile string) (awsconfig.Config, error) {
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

	// Cancel sweep to clean up
	t.Log("Canceling sweep for cleanup...")
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

	// Cancel sweep
	t.Log("Canceling sweep...")
	cancelSweep(t, sweepID)
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

	// Cancel sweep
	t.Log("Canceling sweep...")
	cancelSweep(t, sweepID)
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

	// Cancel sweep
	t.Log("Canceling sweep...")
	cancelSweep(t, sweepID)
}

func TestMultiRegionResultCollection(t *testing.T) {
	t.Skip("Skipping result collection test - requires instances to upload results")
	// This test would need real instances to upload result files to S3
	// which takes longer and requires coordinated setup
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

func launchSweep(t *testing.T, paramFile string, args ...string) string {
	t.Helper()

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
				return parts[2]
			}
		}
	}

	t.Fatalf("Could not find Sweep ID in output:\n%s", string(output))
	return ""
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

	cmd := exec.Command("./bin/spawn", "cancel", "--sweep-id", sweepID)
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
	cfg, err := config.LoadDefaultConfig(ctx)
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

// Scheduler Integration Tests

func TestSchedulerOneTimeExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	scheduleID := fmt.Sprintf("test-one-time-%d", time.Now().Unix())

	// Upload parameter file to S3
	paramFile := "../testdata/integration/scheduler/one-time-schedule.yaml"
	paramContent, err := os.ReadFile(paramFile)
	if err != nil {
		t.Fatalf("failed to read parameter file: %v", err)
	}

	s3Key := fmt.Sprintf("test/%s.yaml", scheduleID)
	uploadParamFileToS3(t, ctx, string(paramContent), "spawn-schedules-us-east-1", s3Key)
	t.Cleanup(func() {
		cleanupS3File(t, ctx, "spawn-schedules-us-east-1", s3Key)
	})

	// Create one-time schedule
	executionTime := time.Now().Add(5 * time.Minute)
	createTestSchedule(t, ctx, scheduleID, s3Key, "one-time", executionTime, "", 0)
	t.Cleanup(func() {
		cleanupTestSchedule(t, ctx, scheduleID)
	})

	// Hack: modify execution time to trigger in 30 seconds
	updateScheduleExecutionTime(t, ctx, scheduleID, time.Now().Add(30*time.Second))

	// Wait for execution (max 2 minutes)
	t.Log("Waiting for schedule execution...")
	sweepID := waitForScheduleExecution(t, ctx, scheduleID, 2*time.Minute)
	t.Logf("Schedule executed, sweep ID: %s", sweepID)

	// Verify execution record
	history := getScheduleHistory(t, ctx, scheduleID)
	if len(history) == 0 {
		t.Fatal("No execution history found")
	}

	exec := history[0]
	if exec.Status != "success" {
		t.Errorf("Expected status=success, got %s", exec.Status)
	}
	if exec.SweepID == "" {
		t.Error("Expected sweep_id to be populated")
	}

	// Cleanup: cancel sweep if it's still running
	if sweepID != "" {
		t.Cleanup(func() {
			cancelSweep(t, sweepID)
		})
	}

	t.Log("✓ One-time schedule executed successfully")
}

func TestSchedulerCancelBeforeExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	scheduleID := fmt.Sprintf("test-cancel-%d", time.Now().Unix())

	// Upload parameter file
	paramContent := `regions: [us-east-1]
instance_type: t3.micro
ami: auto
capacity_type: spot
count: 1
tags:
  spawn:test: "true"
`
	s3Key := fmt.Sprintf("test/%s.yaml", scheduleID)
	uploadParamFileToS3(t, ctx, paramContent, "spawn-schedules-us-east-1", s3Key)
	t.Cleanup(func() {
		cleanupS3File(t, ctx, "spawn-schedules-us-east-1", s3Key)
	})

	// Create schedule for future
	executionTime := time.Now().Add(5 * time.Minute)
	createTestSchedule(t, ctx, scheduleID, s3Key, "one-time", executionTime, "", 0)
	t.Cleanup(func() {
		cleanupTestSchedule(t, ctx, scheduleID)
	})

	// Cancel immediately
	updateScheduleStatus(t, ctx, scheduleID, "cancelled")

	// Hack: modify execution time to 10 seconds from now
	updateScheduleExecutionTime(t, ctx, scheduleID, time.Now().Add(10*time.Second))

	// Wait 30 seconds
	time.Sleep(30 * time.Second)

	// Verify no execution occurred
	history := getScheduleHistory(t, ctx, scheduleID)
	if len(history) > 0 {
		t.Errorf("Expected no execution, but found %d execution(s)", len(history))
	}

	// Verify schedule status is cancelled
	record := getScheduleRecord(t, ctx, scheduleID)
	if status := getAttributeString(record, "status"); status != "cancelled" {
		t.Errorf("Expected status=cancelled, got %s", status)
	}

	t.Log("✓ Cancelled schedule did not execute")
}

func TestSchedulerPauseResume(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	scheduleID := fmt.Sprintf("test-pause-%d", time.Now().Unix())

	// Upload parameter file
	paramContent := `regions: [us-east-1]
instance_type: t3.micro
ami: auto
capacity_type: spot
count: 1
tags:
  spawn:test: "true"
`
	s3Key := fmt.Sprintf("test/%s.yaml", scheduleID)
	uploadParamFileToS3(t, ctx, paramContent, "spawn-schedules-us-east-1", s3Key)
	t.Cleanup(func() {
		cleanupS3File(t, ctx, "spawn-schedules-us-east-1", s3Key)
	})

	// Create recurring schedule
	createTestSchedule(t, ctx, scheduleID, s3Key, "recurring", time.Now().Add(1*time.Minute), "*/1 * * * *", 0)
	t.Cleanup(func() {
		cleanupTestSchedule(t, ctx, scheduleID)
	})

	// Pause schedule
	updateScheduleStatus(t, ctx, scheduleID, "paused")

	// Modify next execution to 10 seconds from now
	updateScheduleExecutionTime(t, ctx, scheduleID, time.Now().Add(10*time.Second))

	// Wait 30 seconds
	time.Sleep(30 * time.Second)

	// Verify no execution while paused
	history := getScheduleHistory(t, ctx, scheduleID)
	if len(history) > 0 {
		t.Errorf("Expected no execution while paused, but found %d execution(s)", len(history))
	}

	// Resume schedule
	updateScheduleStatus(t, ctx, scheduleID, "active")

	// Modify next execution to 10 seconds from now
	updateScheduleExecutionTime(t, ctx, scheduleID, time.Now().Add(10*time.Second))

	// Wait for execution
	t.Log("Waiting for schedule execution after resume...")
	sweepID := waitForScheduleExecution(t, ctx, scheduleID, 2*time.Minute)
	t.Logf("Schedule executed after resume, sweep ID: %s", sweepID)

	// Cleanup sweep
	if sweepID != "" {
		t.Cleanup(func() {
			cancelSweep(t, sweepID)
		})
	}

	t.Log("✓ Pause/resume functionality works correctly")
}

func TestSchedulerRecurringExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	scheduleID := fmt.Sprintf("test-recurring-%d", time.Now().Unix())

	// Upload parameter file
	paramContent := `regions: [us-east-1]
instance_type: t3.micro
ami: auto
capacity_type: spot
count: 1
tags:
  spawn:test: "true"
`
	s3Key := fmt.Sprintf("test/%s.yaml", scheduleID)
	uploadParamFileToS3(t, ctx, paramContent, "spawn-schedules-us-east-1", s3Key)
	t.Cleanup(func() {
		cleanupS3File(t, ctx, "spawn-schedules-us-east-1", s3Key)
	})

	// Create recurring schedule with max 3 executions
	createTestSchedule(t, ctx, scheduleID, s3Key, "recurring", time.Now().Add(1*time.Minute), "*/1 * * * *", 3)
	t.Cleanup(func() {
		cleanupTestSchedule(t, ctx, scheduleID)
	})

	// Execute 3 times quickly by hacking execution times
	var sweepIDs []string
	for i := 0; i < 3; i++ {
		t.Logf("Triggering execution %d...", i+1)
		updateScheduleExecutionTime(t, ctx, scheduleID, time.Now().Add(10*time.Second))
		time.Sleep(1 * time.Minute)

		history := getScheduleHistory(t, ctx, scheduleID)
		if len(history) != i+1 {
			t.Errorf("Expected %d executions, got %d", i+1, len(history))
		}
		if len(history) > 0 && history[0].SweepID != "" {
			sweepIDs = append(sweepIDs, history[0].SweepID)
		}
	}

	// Cleanup sweeps
	for _, sweepID := range sweepIDs {
		if sweepID != "" {
			t.Cleanup(func() {
				cancelSweep(t, sweepID)
			})
		}
	}

	// Verify execution count
	record := getScheduleRecord(t, ctx, scheduleID)
	execCount := getAttributeNumber(record, "execution_count")
	if execCount != 3 {
		t.Errorf("Expected execution_count=3, got %d", execCount)
	}

	// Wait 1 more minute and verify no 4th execution
	t.Log("Waiting to verify no 4th execution...")
	time.Sleep(1 * time.Minute)
	history := getScheduleHistory(t, ctx, scheduleID)
	if len(history) > 3 {
		t.Errorf("Expected max 3 executions, got %d", len(history))
	}

	t.Log("✓ Recurring schedule executed correctly with max_executions limit")
}

func TestSchedulerExecutionLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()

	// Test 1: max_executions limit
	t.Run("MaxExecutions", func(t *testing.T) {
		scheduleID := fmt.Sprintf("test-maxexec-%d", time.Now().Unix())

		paramContent := `regions: [us-east-1]
instance_type: t3.micro
ami: auto
capacity_type: spot
count: 1
tags:
  spawn:test: "true"
`
		s3Key := fmt.Sprintf("test/%s.yaml", scheduleID)
		uploadParamFileToS3(t, ctx, paramContent, "spawn-schedules-us-east-1", s3Key)
		t.Cleanup(func() {
			cleanupS3File(t, ctx, "spawn-schedules-us-east-1", s3Key)
		})

		// Create schedule with max 2 executions
		createTestSchedule(t, ctx, scheduleID, s3Key, "recurring", time.Now().Add(1*time.Minute), "*/1 * * * *", 2)
		t.Cleanup(func() {
			cleanupTestSchedule(t, ctx, scheduleID)
		})

		// Trigger 2 executions
		for i := 0; i < 2; i++ {
			updateScheduleExecutionTime(t, ctx, scheduleID, time.Now().Add(10*time.Second))
			time.Sleep(1 * time.Minute)
		}

		// Verify only 2 executions
		history := getScheduleHistory(t, ctx, scheduleID)
		if len(history) != 2 {
			t.Errorf("Expected 2 executions, got %d", len(history))
		}

		t.Log("✓ max_executions limit enforced")
	})

	// Test 2: end_after in past doesn't execute
	t.Run("EndAfterPast", func(t *testing.T) {
		scheduleID := fmt.Sprintf("test-endafter-%d", time.Now().Unix())

		paramContent := `regions: [us-east-1]
instance_type: t3.micro
ami: auto
capacity_type: spot
count: 1
tags:
  spawn:test: "true"
`
		s3Key := fmt.Sprintf("test/%s.yaml", scheduleID)
		uploadParamFileToS3(t, ctx, paramContent, "spawn-schedules-us-east-1", s3Key)
		t.Cleanup(func() {
			cleanupS3File(t, ctx, "spawn-schedules-us-east-1", s3Key)
		})

		// Create schedule with end_after in the past
		createTestScheduleWithEndAfter(t, ctx, scheduleID, s3Key, time.Now().Add(-1*time.Hour))
		t.Cleanup(func() {
			cleanupTestSchedule(t, ctx, scheduleID)
		})

		// Try to execute
		updateScheduleExecutionTime(t, ctx, scheduleID, time.Now().Add(10*time.Second))
		time.Sleep(30 * time.Second)

		// Verify no execution
		history := getScheduleHistory(t, ctx, scheduleID)
		if len(history) > 0 {
			t.Errorf("Expected no execution for expired schedule, got %d", len(history))
		}

		t.Log("✓ end_after limit enforced")
	})
}

func TestSchedulerErrorHandling(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	scheduleID := fmt.Sprintf("test-error-%d", time.Now().Unix())

	// Create schedule with invalid S3 key (non-existent file)
	s3Key := fmt.Sprintf("test/nonexistent-%s.yaml", scheduleID)
	createTestSchedule(t, ctx, scheduleID, s3Key, "one-time", time.Now().Add(1*time.Minute), "", 0)
	t.Cleanup(func() {
		cleanupTestSchedule(t, ctx, scheduleID)
	})

	// Trigger execution
	updateScheduleExecutionTime(t, ctx, scheduleID, time.Now().Add(10*time.Second))

	// Wait for execution attempt
	t.Log("Waiting for execution attempt...")
	time.Sleep(1 * time.Minute)

	// Verify execution record with error
	history := getScheduleHistory(t, ctx, scheduleID)
	if len(history) == 0 {
		t.Fatal("Expected execution record, got none")
	}

	exec := history[0]
	if exec.Status == "success" {
		t.Error("Expected status=failed, got success")
	}

	// Note: Error message might not be populated depending on Lambda implementation
	t.Logf("Execution status: %s", exec.Status)

	t.Log("✓ Error handling works correctly")
}

// Queue Integration Tests

func TestQueueSimpleExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Skip("Queue tests require actual EC2 instances - implement after spored agent is ready")

	// TODO: Implement when spored queue runner is ready
	// 1. Read queue config from testdata/integration/queue/simple-queue.json
	// 2. Upload to S3
	// 3. Launch EC2 instance with user-data that runs queue
	// 4. Wait for queue completion (poll S3 for queue-state.json)
	// 5. Verify both jobs completed successfully
	// 6. Terminate instance
}

func TestQueueWithDependencies(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Skip("Queue tests require actual EC2 instances - implement after spored agent is ready")
}

func TestQueueOnFailurePolicies(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Skip("Queue tests require actual EC2 instances - implement after spored agent is ready")
}

func TestQueueRetryLogic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Skip("Queue tests require actual EC2 instances - implement after spored agent is ready")
}

func TestQueueTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Skip("Queue tests require actual EC2 instances - implement after spored agent is ready")
}

func TestQueueStatePersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Skip("Queue tests require actual EC2 instances - implement after spored agent is ready")
}

func TestScheduledBatchQueue(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Skip("Combined workflow test requires both scheduler and queue to be working")
}

// Helper functions for scheduler tests

func createTestSchedule(t *testing.T, ctx context.Context, scheduleID, paramFileKey, scheduleType string, executionTime time.Time, cronExpr string, maxExecutions int) {
	t.Helper()

	cfg, err := loadAWSConfig(ctx, "spore-host-dev")
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	item := map[string]types.AttributeValue{
		"schedule_id":        &types.AttributeValueMemberS{Value: scheduleID},
		"parameter_file_key": &types.AttributeValueMemberS{Value: paramFileKey},
		"schedule_type":      &types.AttributeValueMemberS{Value: scheduleType},
		"status":             &types.AttributeValueMemberS{Value: "active"},
		"execution_count":    &types.AttributeValueMemberN{Value: "0"},
		"created_at":         &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
	}

	if scheduleType == "one-time" {
		item["execution_time"] = &types.AttributeValueMemberS{Value: executionTime.Format(time.RFC3339)}
	} else if scheduleType == "recurring" {
		item["cron_expression"] = &types.AttributeValueMemberS{Value: cronExpr}
		item["next_execution_time"] = &types.AttributeValueMemberS{Value: executionTime.Format(time.RFC3339)}
		if maxExecutions > 0 {
			item["max_executions"] = &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", maxExecutions)}
		}
	}

	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: stringPtr("spawn-schedules"),
		Item:      item,
	})
	if err != nil {
		t.Fatalf("failed to create test schedule: %v", err)
	}
}

func uploadParamFileToS3(t *testing.T, ctx context.Context, content, bucket, key string) {
	t.Helper()

	cfg, err := loadAWSConfig(ctx, "spore-host-infra")
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(cfg)

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   strings.NewReader(content),
	})
	if err != nil {
		t.Fatalf("failed to upload parameter file: %v", err)
	}
}

func cleanupS3File(t *testing.T, ctx context.Context, bucket, key string) {
	t.Helper()

	cfg, err := loadAWSConfig(ctx, "spore-host-infra")
	if err != nil {
		t.Logf("Warning: failed to load AWS config: %v", err)
		return
	}

	client := s3.NewFromConfig(cfg)

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		t.Logf("Warning: failed to delete S3 file: %v", err)
	}
}

func updateScheduleExecutionTime(t *testing.T, ctx context.Context, scheduleID string, nextTime time.Time) {
	t.Helper()

	cfg, err := loadAWSConfig(ctx, "spore-host-dev")
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	// Determine which field to update based on schedule type
	record := getScheduleRecord(t, ctx, scheduleID)
	scheduleType := getAttributeString(record, "schedule_type")

	var updateExpr string
	if scheduleType == "one-time" {
		updateExpr = "SET execution_time = :time"
	} else {
		updateExpr = "SET next_execution_time = :time"
	}

	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: stringPtr("spawn-schedules"),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
		UpdateExpression: &updateExpr,
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":time": &types.AttributeValueMemberS{Value: nextTime.Format(time.RFC3339)},
		},
	})
	if err != nil {
		t.Fatalf("failed to update schedule execution time: %v", err)
	}
}

func updateScheduleStatus(t *testing.T, ctx context.Context, scheduleID, status string) {
	t.Helper()

	cfg, err := loadAWSConfig(ctx, "spore-host-dev")
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	updateExpr := "SET #status = :status"
	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: stringPtr("spawn-schedules"),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
		UpdateExpression: &updateExpr,
		ExpressionAttributeNames: map[string]string{
			"#status": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":status": &types.AttributeValueMemberS{Value: status},
		},
	})
	if err != nil {
		t.Fatalf("failed to update schedule status: %v", err)
	}
}

func waitForScheduleExecution(t *testing.T, ctx context.Context, scheduleID string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		history := getScheduleHistory(t, ctx, scheduleID)
		if len(history) > 0 && history[0].SweepID != "" {
			return history[0].SweepID
		}

		if time.Now().After(deadline) {
			t.Fatalf("schedule did not execute within timeout")
		}

		select {
		case <-ticker.C:
			// Continue polling
		}
	}
}

func getScheduleHistory(t *testing.T, ctx context.Context, scheduleID string) []ScheduleExecution {
	t.Helper()

	cfg, err := loadAWSConfig(ctx, "spore-host-dev")
	if err != nil {
		t.Logf("Warning: failed to load AWS config: %v", err)
		return nil
	}

	client := dynamodb.NewFromConfig(cfg)

	result, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              stringPtr("spawn-schedule-history"),
		KeyConditionExpression: stringPtr("schedule_id = :sid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sid": &types.AttributeValueMemberS{Value: scheduleID},
		},
		ScanIndexForward: boolPtr(false), // Most recent first
	})
	if err != nil {
		t.Logf("Warning: failed to query schedule history: %v", err)
		return nil
	}

	var history []ScheduleExecution
	for _, item := range result.Items {
		exec := ScheduleExecution{
			ScheduleID: scheduleID,
			Status:     getAttributeString(item, "status"),
			SweepID:    getAttributeString(item, "sweep_id"),
		}
		if execTime := getAttributeString(item, "execution_time"); execTime != "" {
			exec.ExecutionTime, _ = time.Parse(time.RFC3339, execTime)
		}
		history = append(history, exec)
	}

	return history
}

func getScheduleRecord(t *testing.T, ctx context.Context, scheduleID string) map[string]types.AttributeValue {
	t.Helper()

	cfg, err := loadAWSConfig(ctx, "spore-host-dev")
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: stringPtr("spawn-schedules"),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
	})
	if err != nil {
		t.Fatalf("failed to get schedule record: %v", err)
	}

	return result.Item
}

func cleanupTestSchedule(t *testing.T, ctx context.Context, scheduleID string) {
	t.Helper()

	cfg, err := loadAWSConfig(ctx, "spore-host-dev")
	if err != nil {
		t.Logf("Warning: failed to load AWS config: %v", err)
		return
	}

	client := dynamodb.NewFromConfig(cfg)

	_, err = client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: stringPtr("spawn-schedules"),
		Key: map[string]types.AttributeValue{
			"schedule_id": &types.AttributeValueMemberS{Value: scheduleID},
		},
	})
	if err != nil {
		t.Logf("Warning: failed to delete schedule: %v", err)
	}
}

func getAttributeString(item map[string]types.AttributeValue, key string) string {
	if v, ok := item[key].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func getAttributeNumber(item map[string]types.AttributeValue, key string) int {
	if v, ok := item[key].(*types.AttributeValueMemberN); ok {
		var n int
		fmt.Sscanf(v.Value, "%d", &n)
		return n
	}
	return 0
}

func createTestScheduleWithEndAfter(t *testing.T, ctx context.Context, scheduleID, paramFileKey string, endAfter time.Time) {
	t.Helper()

	cfg, err := loadAWSConfig(ctx, "spore-host-dev")
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	client := dynamodb.NewFromConfig(cfg)

	item := map[string]types.AttributeValue{
		"schedule_id":        &types.AttributeValueMemberS{Value: scheduleID},
		"parameter_file_key": &types.AttributeValueMemberS{Value: paramFileKey},
		"schedule_type":      &types.AttributeValueMemberS{Value: "one-time"},
		"status":             &types.AttributeValueMemberS{Value: "active"},
		"execution_count":    &types.AttributeValueMemberN{Value: "0"},
		"created_at":         &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		"execution_time":     &types.AttributeValueMemberS{Value: time.Now().Add(1 * time.Minute).Format(time.RFC3339)},
		"end_after":          &types.AttributeValueMemberS{Value: endAfter.Format(time.RFC3339)},
	}

	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: stringPtr("spawn-schedules"),
		Item:      item,
	})
	if err != nil {
		t.Fatalf("failed to create test schedule: %v", err)
	}
}

func boolPtr(b bool) *bool {
	return &b
}

type ScheduleExecution struct {
	ScheduleID    string
	ExecutionTime time.Time
	SweepID       string
	Status        string
}
