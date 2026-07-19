package cmd

import (
	"fmt"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

func TestBuildLaunchConfigFromParams_WorkflowStep(t *testing.T) {
	defaults := map[string]interface{}{
		"region": "us-east-1",
		"ttl":    "2h",
		"spot":   true,
	}

	params := map[string]interface{}{
		"step":          "unit-tests",
		"instance_type": "t3.micro",
		"command":       "npm run test:unit",
		"timeout":       "10m",
	}

	config, err := buildLaunchConfigFromParams(defaults, params, "sweep-123", "test-sweep", 0, 5)
	if err != nil {
		t.Fatalf("buildLaunchConfigFromParams failed: %v", err)
	}

	// Check basic fields
	if config.InstanceType != "t3.micro" {
		t.Errorf("Expected instance_type=t3.micro, got %s", config.InstanceType)
	}

	if config.Region != "us-east-1" {
		t.Errorf("Expected region=us-east-1, got %s", config.Region)
	}

	if config.TTL != "2h" {
		t.Errorf("Expected ttl=2h, got %s", config.TTL)
	}

	if !config.Spot {
		t.Errorf("Expected spot=true, got false")
	}

	// Check sweep metadata
	if config.SweepID != "sweep-123" {
		t.Errorf("Expected sweep_id=sweep-123, got %s", config.SweepID)
	}

	if config.SweepName != "test-sweep" {
		t.Errorf("Expected sweep_name=test-sweep, got %s", config.SweepName)
	}

	if config.SweepIndex != 0 {
		t.Errorf("Expected sweep_index=0, got %d", config.SweepIndex)
	}

	if config.SweepSize != 5 {
		t.Errorf("Expected sweep_size=5, got %d", config.SweepSize)
	}

	// Check workflow-specific tags
	if config.Tags == nil {
		t.Fatal("Expected Tags to be non-nil")
	}

	if config.Tags["spawn:step"] != "unit-tests" {
		t.Errorf("Expected spawn:step=unit-tests, got %s", config.Tags["spawn:step"])
	}

	if config.Tags["spawn:command"] != "npm run test:unit" {
		t.Errorf("Expected spawn:command='npm run test:unit', got %s", config.Tags["spawn:command"])
	}

	// Check that custom fields become parameters
	if config.Parameters["timeout"] != "10m" {
		t.Errorf("Expected PARAM_timeout=10m, got %s", config.Parameters["timeout"])
	}
}

func TestBuildLaunchConfigFromParams_NoWorkflowStep(t *testing.T) {
	defaults := map[string]interface{}{
		"region": "us-west-2",
	}

	params := map[string]interface{}{
		"instance_type": "t3.small",
		"alpha":         0.1,
		"beta":          0.5,
	}

	config, err := buildLaunchConfigFromParams(defaults, params, "sweep-456", "param-sweep", 2, 10)
	if err != nil {
		t.Fatalf("buildLaunchConfigFromParams failed: %v", err)
	}

	// Check that this works as a regular parameter sweep (no workflow step)
	if config.InstanceType != "t3.small" {
		t.Errorf("Expected instance_type=t3.small, got %s", config.InstanceType)
	}

	// Tags should be nil since no step/command specified
	if config.Tags != nil {
		if _, hasStep := config.Tags["spawn:step"]; hasStep {
			t.Errorf("Expected no spawn:step tag, but found one")
		}
		if _, hasCommand := config.Tags["spawn:command"]; hasCommand {
			t.Errorf("Expected no spawn:command tag, but found one")
		}
	}

	// Check parameters
	if config.Parameters["alpha"] != "0.1" {
		t.Errorf("Expected PARAM_alpha=0.1, got %s", config.Parameters["alpha"])
	}

	if config.Parameters["beta"] != "0.5" {
		t.Errorf("Expected PARAM_beta=0.5, got %s", config.Parameters["beta"])
	}
}

func TestBuildLaunchConfigFromParams_CommandWithoutStep(t *testing.T) {
	// Test that command can be specified without step (generic command execution)
	params := map[string]interface{}{
		"instance_type": "t3.medium",
		"command":       "python3 train.py",
	}

	config, err := buildLaunchConfigFromParams(map[string]interface{}{}, params, "sweep-789", "cmd-sweep", 0, 1)
	if err != nil {
		t.Fatalf("buildLaunchConfigFromParams failed: %v", err)
	}

	if config.Tags == nil {
		t.Fatal("Expected Tags to be non-nil")
	}

	if config.Tags["spawn:command"] != "python3 train.py" {
		t.Errorf("Expected spawn:command='python3 train.py', got %s", config.Tags["spawn:command"])
	}

	// Step should not be set
	if step, exists := config.Tags["spawn:step"]; exists {
		t.Errorf("Expected no spawn:step tag, but got: %s", step)
	}
}

func TestBuildLaunchConfigFromParams_OverrideDefaults(t *testing.T) {
	// Test that param values override defaults
	defaults := map[string]interface{}{
		"region": "us-east-1",
		"ttl":    "2h",
		"spot":   true,
	}

	params := map[string]interface{}{
		"instance_type": "t3.large",
		"region":        "us-west-2", // Override default region
		"ttl":           "4h",        // Override default TTL
		"spot":          false,       // Override default spot
	}

	config, err := buildLaunchConfigFromParams(defaults, params, "sweep-999", "override-sweep", 0, 1)
	if err != nil {
		t.Fatalf("buildLaunchConfigFromParams failed: %v", err)
	}

	// Check that param values override defaults
	if config.Region != "us-west-2" {
		t.Errorf("Expected region=us-west-2 (overridden), got %s", config.Region)
	}

	if config.TTL != "4h" {
		t.Errorf("Expected ttl=4h (overridden), got %s", config.TTL)
	}

	if config.Spot {
		t.Errorf("Expected spot=false (overridden), got true")
	}
}

// TestBuildLaunchConfigFromParams_HeterogeneousEntry verifies a single sweep entry
// can carry its own instance_type / ami / spot (#372) — the per-entry overrides a
// heterogeneous (price-performance benchmark) sweep needs.
func TestBuildLaunchConfigFromParams_HeterogeneousEntry(t *testing.T) {
	defaults := map[string]interface{}{"ttl": "1h"}
	params := map[string]interface{}{
		"instance_type": "c8g.24xlarge",
		"ami":           "ami-arm64sve",
		"spot":          true,
	}

	config, err := buildLaunchConfigFromParams(defaults, params, "sweep-1", "bench", 2, 6)
	if err != nil {
		t.Fatalf("buildLaunchConfigFromParams failed: %v", err)
	}
	if config.InstanceType != "c8g.24xlarge" {
		t.Errorf("InstanceType = %q, want c8g.24xlarge", config.InstanceType)
	}
	if config.AMI != "ami-arm64sve" {
		t.Errorf("AMI = %q, want ami-arm64sve (per-entry)", config.AMI)
	}
	if !config.Spot {
		t.Error("Spot = false, want true (per-entry)")
	}
}

// TestSweepAMICacheKey_DistinguishesArchAndGPU is the crux of the #372 AMI fix:
// the per-config AMI cache is keyed by (region, arch, GPU), so a heterogeneous
// sweep resolves a distinct AMI per architecture/accelerator instead of forcing
// the first entry's AMI (an x86, non-GPU image) onto arm64/GPU entries. This
// asserts the key derivation the sweep loop uses actually separates the benchmark
// families.
func TestSweepAMICacheKey_DistinguishesArchAndGPU(t *testing.T) {
	key := func(region, it string) string {
		return fmt.Sprintf("%s|%s|%t", region, aws.DetectArchitecture(it), aws.DetectGPUInstance(it))
	}
	region := "us-east-1"
	// A GROMACS price-performance matrix: Intel, AMD, Graviton, and GPU.
	c8i := key(region, "c8i.24xlarge") // x86, no GPU
	c8a := key(region, "c8a.24xlarge") // x86, no GPU
	c8g := key(region, "c8g.24xlarge") // arm64, no GPU
	g6 := key(region, "g6.2xlarge")    // x86, GPU

	// c8i and c8a are both x86/no-GPU → same AMI is correct (one SSM lookup shared).
	if c8i != c8a {
		t.Errorf("expected c8i and c8a to share an AMI key, got %q vs %q", c8i, c8a)
	}
	// arm64 must get a different AMI than x86.
	if c8g == c8i {
		t.Errorf("arm64 (c8g) shares an AMI key with x86 (c8i): %q — arm64 entry would get an x86 AMI", c8g)
	}
	// GPU must get a different AMI than non-GPU x86.
	if g6 == c8i {
		t.Errorf("GPU (g6) shares an AMI key with non-GPU (c8i): %q — GPU entry would get a non-GPU AMI", g6)
	}
	// Region is part of the key.
	if key("us-west-2", "c8i.24xlarge") == c8i {
		t.Error("AMI key ignores region")
	}
}
