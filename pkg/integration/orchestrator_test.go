//go:build integration
// +build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spore-host/spawn/pkg/orchestrator"
	"github.com/spore-host/spawn/pkg/registry"
)

// TestOrchestratorLoadConfig tests loading orchestrator config from file
func TestOrchestratorLoadConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "orchestrator.yaml")

	configYAML := `
job_array_id: test-pipeline
queue_url: https://sqs.us-east-1.amazonaws.com/123456789012/test-queue
region: us-east-1

burst_policy:
  mode: auto
  queue_depth_threshold: 50
  local_capacity: 2
  max_cloud_instances: 10
  min_cloud_instances: 0
  cost_budget: 5.0
  scale_down_delay: 5m
  check_interval: 1m
  instance_type: t3.micro
  ami: ami-12345
  spot: true
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := orchestrator.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	// Verify config loaded correctly
	if cfg.JobArrayID != "test-pipeline" {
		t.Errorf("JobArrayID = %v, want test-pipeline", cfg.JobArrayID)
	}
	if cfg.BurstPolicy.Mode != "auto" {
		t.Errorf("Mode = %v, want auto", cfg.BurstPolicy.Mode)
	}
	if cfg.BurstPolicy.QueueDepthThreshold != 50 {
		t.Errorf("QueueDepthThreshold = %v, want 50", cfg.BurstPolicy.QueueDepthThreshold)
	}
	if cfg.BurstPolicy.MaxCloudInstances != 10 {
		t.Errorf("MaxCloudInstances = %v, want 10", cfg.BurstPolicy.MaxCloudInstances)
	}
	if cfg.BurstPolicy.CostBudget != 5.0 {
		t.Errorf("CostBudget = %v, want 5.0", cfg.BurstPolicy.CostBudget)
	}
	if cfg.BurstPolicy.InstanceType != "t3.micro" {
		t.Errorf("InstanceType = %v, want t3.micro", cfg.BurstPolicy.InstanceType)
	}
	if !cfg.BurstPolicy.Spot {
		t.Errorf("Spot = %v, want true", cfg.BurstPolicy.Spot)
	}

	// Verify durations parsed correctly
	checkInterval := cfg.BurstPolicy.GetCheckInterval()
	if checkInterval.Seconds() != 60 {
		t.Errorf("CheckInterval = %v seconds, want 60", checkInterval.Seconds())
	}

	scaleDownDelay := cfg.BurstPolicy.GetScaleDownDelay()
	if scaleDownDelay.Seconds() != 300 {
		t.Errorf("ScaleDownDelay = %v seconds, want 300", scaleDownDelay.Seconds())
	}

	t.Logf("Orchestrator config loaded successfully")
}

// TestOrchestratorManualMode tests that orchestrator in manual mode does not auto-burst
func TestOrchestratorManualMode(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "orchestrator.yaml")

	configYAML := `
job_array_id: manual-test
queue_url: https://sqs.us-east-1.amazonaws.com/123456789012/test-queue
region: us-east-1

burst_policy:
  mode: manual
  queue_depth_threshold: 100
  max_cloud_instances: 10
  instance_type: t3.micro
  ami: ami-12345
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := orchestrator.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	orch, err := orchestrator.New(ctx, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// In manual mode, Run should return immediately
	// We can't easily test this without actually running it,
	// but we can verify the config is correct
	if cfg.BurstPolicy.Mode != "manual" {
		t.Errorf("Expected manual mode, got %v", cfg.BurstPolicy.Mode)
	}

	stats := orch.GetStats()
	if stats.ManagedInstances != 0 {
		t.Errorf("Expected 0 managed instances initially, got %d", stats.ManagedInstances)
	}

	t.Logf("Manual mode orchestrator created successfully")
}

// TestRegistryTableCreation tests that EnsureTable works
func TestRegistryTableCreation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()

	// This should create the table if it doesn't exist, or succeed if it does
	err := registry.EnsureTable(ctx)
	if err != nil {
		t.Fatalf("EnsureTable() error = %v", err)
	}

	// Call again - should be idempotent
	err = registry.EnsureTable(ctx)
	if err != nil {
		t.Fatalf("EnsureTable() second call error = %v", err)
	}

	t.Logf("Registry table ensured successfully")
}

// TestOrchestratorScalingDecisions tests the orchestrator's scaling logic
func TestOrchestratorScalingDecisions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "orchestrator.yaml")

	configYAML := `
job_array_id: scaling-test
queue_url: https://sqs.us-east-1.amazonaws.com/123456789012/test-queue
region: us-east-1

burst_policy:
  mode: auto
  queue_depth_threshold: 50
  max_cloud_instances: 10
  cost_budget: 5.0
  instance_type: t3.micro
  ami: ami-12345
  spot: true
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg, err := orchestrator.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	orch, err := orchestrator.New(ctx, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Note: We can't easily test actual scaling without:
	// 1. A real SQS queue with messages
	// 2. Permission to launch EC2 instances
	// 3. A real job array ID with registered instances
	//
	// This test verifies the orchestrator can be created successfully
	// Real scaling tests would require a test AWS environment

	stats := orch.GetStats()
	if stats.ManagedInstances != 0 {
		t.Errorf("Expected 0 managed instances initially, got %d", stats.ManagedInstances)
	}
	if stats.TotalCostPerHour != 0 {
		t.Errorf("Expected 0 cost initially, got $%.2f", stats.TotalCostPerHour)
	}

	t.Logf("Orchestrator scaling logic initialized successfully")
	t.Logf("Note: Full scaling tests require live AWS resources")
}
