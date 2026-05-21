package orchestrator

import (
	"context"
	"testing"

	"github.com/spore-host/spawn/pkg/testutil"
)

// Note: Substrate's SQS plugin uses the legacy query/form-encoded protocol but
// aws-sdk-go-v2 SQS uses the AWS JSON 1.0 protocol, so CreateQueue / SendMessage
// cannot be exercised via substrate at this time.  These tests focus on the
// constructor and the subset of orchestrator behavior that does not require live
// SQS calls (manual mode, stats, scale decision logic).

func TestNewWithAWSConfig_InitializesCleanly(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	cfg := &Config{
		JobArrayID: "test-array-1",
		QueueURL:   "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue",
		BurstPolicy: BurstPolicy{
			Mode:                "manual",
			MaxCloudInstances:   10,
			QueueDepthThreshold: 100,
		},
	}

	o, err := NewWithAWSConfig(ctx, cfg, env.AWSConfig)
	if err != nil {
		t.Fatalf("NewWithAWSConfig() error = %v", err)
	}
	if o == nil {
		t.Fatal("NewWithAWSConfig() returned nil")
	}
	if o.config.JobArrayID != "test-array-1" {
		t.Errorf("JobArrayID = %q, want %q", o.config.JobArrayID, "test-array-1")
	}
}

func TestNewWithAWSConfig_GetStats_Initial(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	cfg := &Config{
		JobArrayID: "test-array-stats",
		QueueURL:   "https://sqs.us-east-1.amazonaws.com/123456789012/stats-queue",
		BurstPolicy: BurstPolicy{
			Mode: "manual",
		},
	}

	o, err := NewWithAWSConfig(ctx, cfg, env.AWSConfig)
	if err != nil {
		t.Fatalf("NewWithAWSConfig() error = %v", err)
	}

	stats := o.GetStats()
	if stats.ManagedInstances != 0 {
		t.Errorf("ManagedInstances = %d, want 0", stats.ManagedInstances)
	}
	if stats.TotalCostPerHour != 0 {
		t.Errorf("TotalCostPerHour = %f, want 0", stats.TotalCostPerHour)
	}
	if !stats.LastBurstTime.IsZero() {
		t.Errorf("LastBurstTime should be zero, got %v", stats.LastBurstTime)
	}
}

func TestNewWithAWSConfig_Run_ManualMode(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	cfg := &Config{
		JobArrayID: "test-array-manual",
		QueueURL:   "https://sqs.us-east-1.amazonaws.com/123456789012/manual-queue",
		BurstPolicy: BurstPolicy{
			Mode: "manual",
		},
	}

	o, err := NewWithAWSConfig(ctx, cfg, env.AWSConfig)
	if err != nil {
		t.Fatalf("NewWithAWSConfig() error = %v", err)
	}

	// Manual mode returns nil immediately without any AWS calls.
	if err := o.Run(ctx); err != nil {
		t.Errorf("Run() in manual mode = %v, want nil", err)
	}
}

func TestNewWithAWSConfig_ScaleDecisions(t *testing.T) {
	env := testutil.SubstrateServer(t)
	ctx := context.Background()

	cfg := &Config{
		JobArrayID: "test-array-scale",
		QueueURL:   "https://sqs.us-east-1.amazonaws.com/123456789012/scale-queue",
		BurstPolicy: BurstPolicy{
			Mode:                "manual",
			QueueDepthThreshold: 10,
			MaxCloudInstances:   5,
			MinCloudInstances:   0,
			CostBudget:          100.0,
		},
	}

	o, err := NewWithAWSConfig(ctx, cfg, env.AWSConfig)
	if err != nil {
		t.Fatalf("NewWithAWSConfig() error = %v", err)
	}

	// shouldScaleUp: queue depth exceeds threshold and below max.
	if !o.shouldScaleUp(20, 0, 0) {
		t.Error("shouldScaleUp(20, 0, 0) = false, want true")
	}
	// shouldScaleUp: at max cloud instances.
	if o.shouldScaleUp(20, 0, 5) {
		t.Error("shouldScaleUp(20, 0, 5) = true, want false (at max)")
	}
	// shouldScaleDown: queue empty, cloud instances idle.
	if !o.shouldScaleDown(0, 3) {
		t.Error("shouldScaleDown(0, 3) = false, want true")
	}
	// shouldScaleDown: below minimum.
	if o.shouldScaleDown(0, 0) {
		t.Error("shouldScaleDown(0, 0) = true, want false (at min)")
	}
}
