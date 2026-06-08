package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/sweep"
)

var (
	resumeSweepID       string
	resumeMaxConcurrent int
	resumeDetach        bool
)

var resumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume an interrupted parameter sweep",
	Long: `Resume an interrupted parameter sweep from checkpoint.

Reads the sweep state from ~/.spawn/sweeps/<sweep-id>.json,
queries EC2 for current instance states, and continues launching
pending parameter sets with rolling queue orchestration.

Examples:
  # Resume sweep with original settings
  spawn resume --sweep-id hyperparam-20260115-abc123

  # Resume with different max-concurrent
  spawn resume --sweep-id <id> --max-concurrent 5

  # Resume in detached mode (Lambda)
  spawn resume --sweep-id <id> --detach
`,
	RunE: runResume,
}

func init() {
	resumeCmd.Flags().StringVar(&resumeSweepID, "sweep-id", "", "Sweep ID to resume (required)")
	_ = resumeCmd.MarkFlagRequired("sweep-id")
	resumeCmd.Flags().IntVar(&resumeMaxConcurrent, "max-concurrent", 0, "Override max concurrent instances (0 = use original)")
	resumeCmd.Flags().BoolVar(&resumeDetach, "detach", false, "Run sweep orchestration in Lambda")

	rootCmd.AddCommand(resumeCmd)
}

func runResume(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	// Check if detached mode requested - handle early since detached sweeps have no local state
	if resumeDetach {
		return resumeSweepDetached(ctx, resumeSweepID)
	}

	// Load sweep state (for locally-orchestrated sweeps)
	state, err := loadSweepState(resumeSweepID)
	if err != nil {
		return fmt.Errorf("failed to load sweep state: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n📋 Resuming Parameter Sweep\n")
	fmt.Fprintf(os.Stderr, "   Sweep ID: %s\n", state.SweepID)
	fmt.Fprintf(os.Stderr, "   Sweep Name: %s\n", state.SweepName)
	fmt.Fprintf(os.Stderr, "   Created: %s\n", state.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "   Total Params: %d\n", state.TotalParams)
	fmt.Fprintf(os.Stderr, "   Completed: %d\n", state.Completed)
	fmt.Fprintf(os.Stderr, "   Running: %d\n", state.Running)
	fmt.Fprintf(os.Stderr, "   Pending: %d\n", state.Pending)
	fmt.Fprintf(os.Stderr, "   Failed: %d\n\n", state.Failed)

	// Load original parameter file
	if state.ParamFile == "" {
		return fmt.Errorf("parameter file path not found in sweep state")
	}

	paramFormat, err := parseParamFile(state.ParamFile)
	if err != nil {
		return fmt.Errorf("failed to reload parameter file %q: %w", state.ParamFile, err)
	}

	// Validate parameter count matches
	if len(paramFormat.Params) != state.TotalParams {
		return fmt.Errorf("parameter count mismatch: state file has %d params but parameter file has %d",
			state.TotalParams, len(paramFormat.Params))
	}

	// Initialize AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}

	// Query EC2 to reconcile state
	fmt.Fprintf(os.Stderr, "🔍 Querying EC2 to reconcile instance states...\n")

	// Determine region from existing instances
	region := ""
	for _, inst := range state.Instances {
		if inst.InstanceID != "" {
			// Get instance region (we'll query all instances in the same region)
			region = paramFormat.Defaults["region"].(string)
			break
		}
	}

	if region == "" {
		region = "us-east-1" // Default
	}

	// Query all instances in sweep
	allInstances, err := awsClient.ListInstances(ctx, region, "")
	if err != nil {
		return fmt.Errorf("failed to query instances: %w", err)
	}

	// Build map of instance ID -> current state
	instanceStates := make(map[string]string)
	for _, inst := range allInstances {
		if inst.InstanceID != "" {
			instanceStates[inst.InstanceID] = inst.State
		}
	}

	// Update state based on EC2 reality
	var stillRunning []int
	var completed []int
	var failed []int
	var pending []int

	for i := 0; i < state.TotalParams; i++ {
		// Find this parameter index in state
		found := false
		for _, inst := range state.Instances {
			if inst.Index == i {
				found = true
				// Check if instance still running
				if inst.InstanceID != "" {
					ec2State, exists := instanceStates[inst.InstanceID]
					if exists && (ec2State == "running" || ec2State == "pending") {
						stillRunning = append(stillRunning, i)
					} else if exists && ec2State == "terminated" {
						completed = append(completed, i)
					} else if exists {
						// stopping, stopped, etc.
						failed = append(failed, i)
					} else {
						// Instance not found in EC2 - assume terminated
						completed = append(completed, i)
					}
				} else {
					// Never launched successfully
					failed = append(failed, i)
				}
				break
			}
		}
		if !found {
			// Parameter index not in state -> pending
			pending = append(pending, i)
		}
	}

	fmt.Fprintf(os.Stderr, "✅ Reconciled state:\n")
	fmt.Fprintf(os.Stderr, "   Still running: %d\n", len(stillRunning))
	fmt.Fprintf(os.Stderr, "   Completed: %d\n", len(completed))
	fmt.Fprintf(os.Stderr, "   Failed: %d\n", len(failed))
	fmt.Fprintf(os.Stderr, "   Pending: %d\n\n", len(pending))

	if len(pending) == 0 {
		fmt.Fprintf(os.Stderr, "✅ No pending parameters. Sweep is complete!\n")
		return nil
	}

	// Override max-concurrent if specified
	maxConcurrent := state.MaxConcurrent
	if resumeMaxConcurrent > 0 {
		maxConcurrent = resumeMaxConcurrent
		fmt.Fprintf(os.Stderr, "🔧 Overriding max-concurrent: %d -> %d\n\n", state.MaxConcurrent, maxConcurrent)
	}

	// Build launch configs for pending parameters
	plat, err := platform.Detect()
	if err != nil {
		return fmt.Errorf("failed to detect platform: %w", err)
	}
	launchConfigs := make([]*aws.LaunchConfig, 0, len(pending))

	for _, idx := range pending {
		paramSet := paramFormat.Params[idx]
		config, err := buildLaunchConfigFromParams(paramFormat.Defaults, paramSet, state.SweepID, state.SweepName, idx, state.TotalParams)
		if err != nil {
			return fmt.Errorf("failed to build launch config for parameter set %d: %w", idx, err)
		}

		// Set region
		if config.Region == "" {
			config.Region = region
		}

		// Set name
		if config.Name == "" {
			config.Name = fmt.Sprintf("%s-%d", state.SweepName, idx)
		}

		launchConfigs = append(launchConfigs, &config)
	}

	// We need to setup shared resources (AMI, SSH key, IAM role) for the pending configs
	// Use the first pending config as template
	if len(launchConfigs) == 0 {
		fmt.Fprintf(os.Stderr, "✅ No pending parameters to launch.\n")
		return nil
	}

	firstConfig := launchConfigs[0]

	// Detect AMI
	if firstConfig.AMI == "" {
		ami, err := awsClient.GetRecommendedAMI(ctx, firstConfig.Region, firstConfig.InstanceType)
		if err != nil {
			return fmt.Errorf("failed to detect AMI: %w", err)
		}
		for _, cfg := range launchConfigs {
			if cfg.AMI == "" {
				cfg.AMI = ami
			}
		}
	}

	// Setup SSH key
	if firstConfig.KeyName == "" {
		keyName, err := setupSSHKey(ctx, awsClient, firstConfig.Region, firstConfig.AMI, plat)
		if err != nil {
			return fmt.Errorf("failed to setup SSH key: %w", err)
		}
		for _, cfg := range launchConfigs {
			if cfg.KeyName == "" {
				cfg.KeyName = keyName
			}
		}
	}

	// Setup IAM role
	if firstConfig.IamInstanceProfile == "" {
		instanceProfile, err := awsClient.SetupSporedIAMRole(ctx)
		if err != nil {
			return fmt.Errorf("failed to setup IAM role: %w", err)
		}
		for _, cfg := range launchConfigs {
			if cfg.IamInstanceProfile == "" {
				cfg.IamInstanceProfile = instanceProfile
			}
		}
	}

	// Build user-data for each config
	for _, cfg := range launchConfigs {
		userDataScript, err := buildUserData(plat, cfg)
		if err != nil {
			return fmt.Errorf("failed to build user data: %w", err)
		}
		cfg.UserData = base64.StdEncoding.EncodeToString([]byte(userDataScript))
	}

	// Continue rolling queue from checkpoint
	fmt.Fprintf(os.Stderr, "🚀 Continuing rolling queue...\n\n")

	// Calculate how many slots are available
	activeSlots := len(stillRunning)
	availableSlots := maxConcurrent - activeSlots

	if availableSlots <= 0 {
		fmt.Fprintf(os.Stderr, "⏳ Max concurrent limit reached (%d active). Waiting for instances to terminate...\n", activeSlots)
	} else {
		fmt.Fprintf(os.Stderr, "📊 %d slots available (max: %d, active: %d)\n\n", availableSlots, maxConcurrent, activeSlots)
	}

	// Use the existing rolling queue implementation
	fmt.Fprintf(os.Stderr, "🚀 Launching %d pending parameters with rolling queue...\n\n", len(pending))

	// Call the rolling queue launcher
	resumedInstances, failures, successCount, err := launchWithRollingQueue(
		ctx,
		awsClient,
		launchConfigs,
		state.SweepID,
		state.SweepName,
		maxConcurrent,
		state.LaunchDelay,
	)

	// Report results
	fmt.Fprintf(os.Stderr, "\n✅ Resume complete!\n")
	fmt.Fprintf(os.Stderr, "   Successfully launched: %d\n", successCount)
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "   Failed: %d\n", len(failures))
		for _, msg := range failures {
			fmt.Fprintf(os.Stderr, "     • %s\n", msg)
		}
	}

	// Display all resumed instances
	if len(resumedInstances) > 0 {
		fmt.Fprintf(os.Stderr, "\nResumed instances:\n")
		for _, inst := range resumedInstances {
			fmt.Fprintf(os.Stderr, "  • %s (%s) - %s\n", inst.Name, inst.InstanceID, inst.State)
		}
	}

	if err != nil {
		return fmt.Errorf("sweep resumed with errors: %w", err)
	}

	return nil
}

func resumeSweepDetached(ctx context.Context, sweepID string) error {
	fmt.Fprintf(os.Stderr, "\n🔄 Resuming sweep in detached mode...\n")
	fmt.Fprintf(os.Stderr, "   Sweep ID: %s\n\n", sweepID)

	// Load spore-host-infra config for DynamoDB and Lambda
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Query current sweep state from DynamoDB
	fmt.Fprintf(os.Stderr, "📊 Querying current sweep state...\n")
	state, err := sweep.QuerySweepStatus(ctx, cfg, sweepID)
	if err != nil {
		return fmt.Errorf("failed to query sweep state: %w", err)
	}

	// Display current status
	fmt.Fprintf(os.Stderr, "\n   Sweep Name: %s\n", state.SweepName)
	fmt.Fprintf(os.Stderr, "   Status: %s\n", state.Status)
	fmt.Fprintf(os.Stderr, "   Progress: %d/%d launched\n", state.Launched, state.TotalParams)
	fmt.Fprintf(os.Stderr, "   Failed: %d\n", state.Failed)
	fmt.Fprintf(os.Stderr, "   Next to launch: %d\n\n", state.NextToLaunch)

	// Validate resumable
	if state.Status == "COMPLETED" {
		return fmt.Errorf("sweep already completed (status: COMPLETED)")
	}

	if state.NextToLaunch >= state.TotalParams {
		// Check if there are still active instances
		activeCount := 0
		for _, inst := range state.Instances {
			if inst.State == "pending" || inst.State == "running" {
				activeCount++
			}
		}
		if activeCount == 0 {
			return fmt.Errorf("all parameters already launched (%d/%d)", state.NextToLaunch, state.TotalParams)
		}
		fmt.Fprintf(os.Stderr, "ℹ️  All parameters launched, but %d instances still running\n", activeCount)
		fmt.Fprintf(os.Stderr, "   Re-invoking Lambda to monitor completion...\n\n")
	}

	// Re-invoke Lambda orchestrator
	fmt.Fprintf(os.Stderr, "🚀 Re-invoking Lambda orchestrator...\n")
	if err := sweep.InvokeSweepOrchestrator(ctx, cfg, sweepID); err != nil {
		return fmt.Errorf("failed to invoke Lambda: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n✅ Sweep resumed in detached mode!\n")
	fmt.Fprintf(os.Stderr, "   Lambda will continue orchestration from checkpoint\n")
	fmt.Fprintf(os.Stderr, "   Check status: spawn status --sweep-id %s\n\n", sweepID)

	return nil
}
