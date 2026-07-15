package cmd

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/sweep"
)

var (
	cancelSweepID string
	cancelYes     bool
)

var cancelCmd = &cobra.Command{
	Use:   "cancel",
	Short: "Cancel a running parameter sweep",
	Long: `Cancel a running parameter sweep and terminate all instances.

Queries DynamoDB for the sweep state, terminates all running/pending
instances via cross-account access, and updates the sweep status to CANCELLED.

Examples:
  # Cancel a running sweep
  spawn cancel --sweep-id sweep-20260116-abc123
`,
	RunE: runCancel,
}

func init() {
	cancelCmd.Flags().StringVar(&cancelSweepID, "sweep-id", "", "Sweep ID to cancel (required)")
	_ = cancelCmd.MarkFlagRequired("sweep-id")
	cancelCmd.Flags().BoolVarP(&cancelYes, "yes", "y", false, "Skip the confirmation prompt")

	rootCmd.AddCommand(cancelCmd)
}

func runCancel(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	fmt.Fprintf(os.Stderr, "\n🛑 Cancelling Parameter Sweep\n")
	fmt.Fprintf(os.Stderr, "   Sweep ID: %s\n\n", cancelSweepID)

	// Load AWS config for spore-host-infra (where DynamoDB lives)
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get user identity for audit logging
	userID, err := aws.NewClientFromConfig(cfg).GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	correlationID := uuid.New().String()

	// Initialize audit logger
	auditLog := audit.NewLogger(os.Stderr, userID, correlationID)
	auditLog.LogOperation("cancel_sweep", cancelSweepID, "initiated", nil)

	// Query sweep state
	fmt.Fprintf(os.Stderr, "📊 Querying sweep state...\n")
	state, err := sweep.QuerySweepStatus(ctx, cfg, cancelSweepID)
	if err != nil {
		return fmt.Errorf("failed to query sweep state: %w", err)
	}

	// Display current status
	fmt.Fprintf(os.Stderr, "\n   Sweep Name: %s\n", state.SweepName)
	fmt.Fprintf(os.Stderr, "   Status: %s\n", state.Status)
	if state.MultiRegion && len(state.RegionStatus) > 0 {
		regions := make([]string, 0, len(state.RegionStatus))
		for region := range state.RegionStatus {
			regions = append(regions, region)
		}
		fmt.Fprintf(os.Stderr, "   Type: Multi-Region\n")
		fmt.Fprintf(os.Stderr, "   Regions: %v\n", regions)
	} else {
		fmt.Fprintf(os.Stderr, "   Region: %s\n", state.Region)
	}
	fmt.Fprintf(os.Stderr, "   Progress: %d/%d launched\n", state.Launched, state.TotalParams)

	// Check if already cancelled or completed
	if state.Status == "CANCELLED" {
		fmt.Fprintf(os.Stderr, "\n⚠️  Sweep is already cancelled\n")
		return nil
	}
	if state.Status == "COMPLETED" {
		fmt.Fprintf(os.Stderr, "\n⚠️  Sweep is already completed\n")
		return nil
	}

	// Group instances by region for multi-region sweeps
	instancesByRegion := make(map[string][]string)
	for _, inst := range state.Instances {
		if inst.InstanceID != "" && (inst.State == "pending" || inst.State == "running") {
			region := inst.Region
			if region == "" {
				// Fall back to sweep region for legacy instances
				region = state.Region
			}
			instancesByRegion[region] = append(instancesByRegion[region], inst.InstanceID)
		}
	}

	totalToTerminate := 0
	for _, instances := range instancesByRegion {
		totalToTerminate += len(instances)
	}

	fmt.Fprintf(os.Stderr, "\n🔍 Found %d instances to terminate", totalToTerminate)
	if state.MultiRegion && len(instancesByRegion) > 1 {
		fmt.Fprintf(os.Stderr, " across %d regions", len(instancesByRegion))
	}
	fmt.Fprintf(os.Stderr, "\n\n")

	// Confirm before terminating — cancel destroys live compute, so it must
	// not proceed unattended without --yes (spawn#40).
	if totalToTerminate > 0 {
		prompt := fmt.Sprintf("Cancel sweep %s and terminate %d instance(s)? This cannot be undone.",
			cancelSweepID, totalToTerminate)
		if !confirmYes(cancelYes, prompt) {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return nil
		}
	}

	// Terminate instances if any
	if totalToTerminate > 0 {
		fmt.Fprintf(os.Stderr, "⚡ Terminating instances...\n")
		auditLog.LogOperationWithData("terminate_instances", cancelSweepID, "initiated",
			map[string]interface{}{
				"instance_count": totalToTerminate,
				"region_count":   len(instancesByRegion),
			}, nil)

		if state.MultiRegion && len(instancesByRegion) > 1 {
			// Multi-region: terminate concurrently per region
			if err := terminateMultiRegion(ctx, instancesByRegion); err != nil {
				auditLog.LogOperation("terminate_instances", cancelSweepID, "failed", err)
				return fmt.Errorf("failed to terminate instances: %w", err)
			}
		} else {
			// Single region: use existing logic
			region := state.Region
			if len(instancesByRegion) == 1 {
				for r := range instancesByRegion {
					region = r
				}
			}

			devCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
			if err != nil {
				return fmt.Errorf("failed to load dev account config: %w", err)
			}

			instances := []string{}
			for _, instList := range instancesByRegion {
				instances = append(instances, instList...)
			}

			if err := sweep.TerminateSweepInstancesDirect(ctx, devCfg, instances); err != nil {
				auditLog.LogOperationWithRegion("terminate_instances", cancelSweepID, region, "failed", err)
				return fmt.Errorf("failed to terminate instances: %w", err)
			}
			fmt.Fprintf(os.Stderr, "   Terminated %d instances in %s\n", len(instances), region)
		}

		auditLog.LogOperationWithData("terminate_instances", cancelSweepID, "success",
			map[string]interface{}{
				"instance_count": totalToTerminate,
			}, nil)
	}

	// Update sweep status to CANCELLED and set cancel flag
	fmt.Fprintf(os.Stderr, "\n📝 Updating sweep status to CANCELLED...\n")
	state.CancelRequested = true
	state.Status = "CANCELLED"
	state.CompletedAt = time.Now().Format(time.RFC3339)
	if err := sweep.SaveSweepState(ctx, cfg, state); err != nil {
		auditLog.LogOperation("update_sweep_status", cancelSweepID, "failed", err)
		return fmt.Errorf("failed to update sweep status: %w", err)
	}

	auditLog.LogOperation("cancel_sweep", cancelSweepID, "success", nil)

	fmt.Fprintf(os.Stderr, "\n✅ Sweep cancelled successfully!\n")
	fmt.Fprintf(os.Stderr, "   Sweep ID: %s\n", cancelSweepID)
	if totalToTerminate > 0 {
		fmt.Fprintf(os.Stderr, "   Terminated: %d instances\n", totalToTerminate)
	}
	fmt.Fprintf(os.Stderr, "\n")

	return nil
}

// terminateMultiRegion terminates instances across multiple regions concurrently
func terminateMultiRegion(ctx context.Context, instancesByRegion map[string][]string) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for region, instances := range instancesByRegion {
		if len(instances) == 0 {
			continue
		}

		wg.Add(1)
		go func(r string, instList []string) {
			defer wg.Done()

			fmt.Fprintf(os.Stderr, "   Terminating %d instances in %s...\n", len(instList), r)

			// Load regional config
			regionalCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(r))
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("failed to load config for %s: %w", r, err)
				}
				mu.Unlock()
				return
			}

			// Terminate instances in this region
			if err := sweep.TerminateSweepInstancesDirect(ctx, regionalCfg, instList); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("failed to terminate in %s: %w", r, err)
				}
				mu.Unlock()
				return
			}

			fmt.Fprintf(os.Stderr, "   ✓ Terminated %d instances in %s\n", len(instList), r)
		}(region, instances)
	}

	wg.Wait()
	return firstErr
}
