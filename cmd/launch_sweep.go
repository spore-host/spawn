package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spore-host/libs/pricing"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/sweep"
)

func launchParameterSweep(ctx context.Context, baseConfig *aws.LaunchConfig, plat *platform.Platform, auditLog *audit.AuditLogger) error {
	// Validate mutually exclusive flags
	if detach && noDetach {
		return fmt.Errorf("--detach and --no-detach are mutually exclusive")
	}

	// Validate workflow integration flags
	if wait && !detach {
		return fmt.Errorf("--wait requires --detach (only works with Lambda orchestration)")
	}

	// Parse parameter file
	var paramFormat *ParamFileFormat
	var err error

	if paramFile != "" {
		paramFormat, err = parseParamFile(paramFile)
		if err != nil {
			return fmt.Errorf("failed to parse parameter file: %w", err)
		}
	} else if params != "" {
		// TODO: Parse inline JSON params
		return fmt.Errorf("inline --params not yet implemented, use --param-file for now")
	} else {
		return fmt.Errorf("either --param-file or --params must be specified for parameter sweep")
	}

	// AUTO-ENABLE DETACHED MODE for parameter sweeps to prevent zombie instances
	// If the CLI disconnects (laptop sleep/shutdown), detached mode ensures:
	// - Sweep state persists in DynamoDB
	// - Lambda continues orchestration
	// - User can resume monitoring with 'spawn sweep status <sweep-id>'
	if !detach && !noDetach {
		detach = true
		fmt.Fprintf(os.Stderr, "\n⚠️  Auto-enabling --detach for parameter sweep\n")
		fmt.Fprintf(os.Stderr, "   This prevents zombie instances if CLI disconnects.\n")
		fmt.Fprintf(os.Stderr, "   Resume monitoring with: spawn sweep status <sweep-id>\n")

		// If maxConcurrent is 0 (launch all at once), set a reasonable default
		if maxConcurrent == 0 {
			// Default to number of params or 10, whichever is less
			defaultConcurrent := len(paramFormat.Params)
			if defaultConcurrent > 10 {
				defaultConcurrent = 10
			}
			maxConcurrent = defaultConcurrent
			fmt.Fprintf(os.Stderr, "   Setting --max-concurrent=%d for controlled launch\n", maxConcurrent)
			fmt.Fprintf(os.Stderr, "   (Override with --max-concurrent=N if needed)\n")
		}
		fmt.Fprintf(os.Stderr, "\n")
	} else if noDetach {
		// User explicitly disabled detached mode - warn about zombie instances
		fmt.Fprintf(os.Stderr, "\n⚠️  WARNING: --no-detach specified\n")
		fmt.Fprintf(os.Stderr, "   If CLI disconnects (laptop sleep/shutdown), instances may become zombies.\n")
		if ttl == "" && idleTimeout == "" {
			fmt.Fprintf(os.Stderr, "\n❌ ERROR: --no-detach requires --ttl or --idle-timeout to prevent zombie instances\n")
			return fmt.Errorf("--no-detach requires --ttl or --idle-timeout for safety")
		}
		fmt.Fprintf(os.Stderr, "   Using safeguards: ttl=%s, idle-timeout=%s\n\n", ttl, idleTimeout)
	}

	// Generate sweep ID
	name := sweepName
	if name == "" {
		name = "sweep"
	}
	sweepID := generateSweepID(name)

	// Write sweep ID to file for workflow integration
	if err := writeOutputID(sweepID, outputIDFile); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write sweep ID to file: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "\n🧪 Parameter Sweep: %s\n", sweepID)
	fmt.Fprintf(os.Stderr, "   Parameters: %d\n", len(paramFormat.Params))
	if maxConcurrent > 0 {
		fmt.Fprintf(os.Stderr, "   Max Concurrent: %d\n", maxConcurrent)
	} else {
		fmt.Fprintf(os.Stderr, "   Mode: All at once\n")
	}
	if detach {
		fmt.Fprintf(os.Stderr, "   Orchestration: Lambda (detached)\n")
	}
	fmt.Fprintf(os.Stderr, "\n")

	// Initialize AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}

	// Check for detached mode (Lambda orchestration)
	if detach && maxConcurrent > 0 {
		return launchSweepDetached(ctx, paramFormat, baseConfig, sweepID, name, maxConcurrent, launchDelay)
	}

	// Build launch configs for each parameter set
	launchConfigs := make([]*aws.LaunchConfig, 0, len(paramFormat.Params))
	for i, paramSet := range paramFormat.Params {
		config, err := buildLaunchConfigFromParams(paramFormat.Defaults, paramSet, sweepID, name, i, len(paramFormat.Params))
		if err != nil {
			return fmt.Errorf("failed to build launch config for parameter set %d: %w", i, err)
		}

		// Copy base config fields that weren't in params. instance_type is a
		// per-entry override (#372): if an entry omits it, fall back to the
		// top-level --instance-type so a mixed file can leave some rows on the
		// CLI default.
		if config.Region == "" {
			config.Region = baseConfig.Region
		}
		if config.InstanceType == "" {
			config.InstanceType = baseConfig.InstanceType
		}
		if config.Name == "" {
			config.Name = fmt.Sprintf("%s-%d", name, i)
		}

		launchConfigs = append(launchConfigs, &config)
	}

	// Setup common resources (AMI, SSH key, IAM role) using first config as template.
	// In JSON mode, suppress the TUI so stdout carries only the JSON array (#21).
	var prog *progress.Progress
	if getOutputFormat() == "json" {
		prog = progress.NewQuietProgress()
	} else {
		prog = progress.NewProgress()
	}

	firstConfig := launchConfigs[0]

	// Auto-detect region if not specified
	if firstConfig.Region == "" {
		fmt.Fprintf(os.Stderr, "🌍 No region specified, auto-detecting closest region...\n")
		detectedRegion, err := detectBestRegion(ctx, firstConfig.InstanceType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not auto-detect region: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Using default: us-east-1\n")
			firstConfig.Region = "us-east-1"
		} else {
			fmt.Fprintf(os.Stderr, "✓ Selected region: %s\n", detectedRegion)
			firstConfig.Region = detectedRegion
		}
	}

	// Apply region to all configs
	for _, cfg := range launchConfigs {
		if cfg.Region == "" {
			cfg.Region = firstConfig.Region
		}
	}

	// Re-pin the AWS client to the resolved sweep region (#276) so AMI/AZ/identity
	// resolution below runs in the target region, not the ambient default. (The
	// client created earlier was only needed for the detached early-return path.)
	awsClient, err = aws.NewClientWithRegion(ctx, firstConfig.Region)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}

	// Step 1: Detect AMI — per config, so a heterogeneous sweep (#372) gets an
	// arch/GPU-appropriate AMI per instance type instead of the first entry's AMI
	// forced onto every entry (which broke arm64/GPU rows). GetRecommendedAMI keys
	// off instance type (arch + GPU), so we memoize by (region, arch, gpu) to avoid
	// redundant SSM lookups when many entries share a family.
	prog.Start("Detecting AMI")
	amiCache := make(map[string]string)
	for _, cfg := range launchConfigs {
		if cfg.AMI != "" {
			continue
		}
		key := fmt.Sprintf("%s|%s|%t", cfg.Region, aws.DetectArchitecture(cfg.InstanceType), aws.DetectGPUInstance(cfg.InstanceType))
		ami, ok := amiCache[key]
		if !ok {
			var err error
			ami, err = awsClient.GetRecommendedAMI(ctx, cfg.Region, cfg.InstanceType)
			if err != nil {
				prog.Error("Detecting AMI", err)
				return fmt.Errorf("detect AMI for %s in %s: %w", cfg.InstanceType, cfg.Region, err)
			}
			amiCache[key] = ami
		}
		cfg.AMI = ami
	}
	prog.Complete("Detecting AMI")

	// Resolve target OS per config (its AMI determines Linux vs Windows) and
	// enforce the Windows lifecycle guard on each before launching any. A sweep
	// may not mix OSes — a single --command / lifecycle model can't span both —
	// so reject a heterogeneous-OS sweep with a clear error.
	osCache := make(map[string]string)
	var sweepOS string
	for i, cfg := range launchConfigs {
		os, ok := osCache[cfg.AMI]
		if !ok {
			os = resolveTargetOS(ctx, awsClient, cfg.Region, cfg.AMI, osFlag)
			osCache[cfg.AMI] = os
		}
		cfg.TargetOS = os
		if i == 0 {
			sweepOS = os
		} else if os != sweepOS {
			return fmt.Errorf("sweep mixes operating systems (%s and %s): a single sweep must be all-Linux or all-Windows", sweepOS, os)
		}
		if err := windowsLifecycleGuard(cfg); err != nil {
			return err
		}
	}

	// Step 2: Setup SSH key
	prog.Start("Setting up SSH key")
	if firstConfig.KeyName == "" {
		keyName, err := setupSSHKey(ctx, awsClient, firstConfig.Region, firstConfig.AMI, plat)
		if err != nil {
			prog.Error("Setting up SSH key", err)
			return err
		}
		// Apply key to all configs
		for _, cfg := range launchConfigs {
			if cfg.KeyName == "" {
				cfg.KeyName = keyName
			}
		}
	}
	prog.Complete("Setting up SSH key")

	// Step 3: Setup IAM role
	prog.Start("Setting up IAM role")
	if firstConfig.IamInstanceProfile == "" {
		instanceProfile, err := awsClient.SetupSporedIAMRole(ctx)
		if err != nil {
			prog.Error("Setting up IAM role", err)
			return err
		}
		// Apply IAM role to all configs
		for _, cfg := range launchConfigs {
			if cfg.IamInstanceProfile == "" {
				cfg.IamInstanceProfile = instanceProfile
			}
		}
	}
	prog.Complete("Setting up IAM role")

	// Zombie-instance guard: apply the 1h idle-timeout default across all sweep
	// configs (shared per-config helper), warning once for the whole sweep.
	hasDefaultApplied := false
	for _, cfg := range launchConfigs {
		if applyIdleTimeoutDefault(cfg) {
			hasDefaultApplied = true
		}
	}
	if hasDefaultApplied {
		fmt.Fprintf(os.Stderr, "\n⚠️  Auto-setting --idle-timeout=1h for all sweep instances\n")
		fmt.Fprintf(os.Stderr, "   Instances will terminate after 1 hour of inactivity.\n")
		fmt.Fprintf(os.Stderr, "   Override with --ttl, --idle-timeout, or --no-timeout\n\n")
	} else if noTimeout {
		fmt.Fprintf(os.Stderr, "\n⚠️  WARNING: --no-timeout specified for sweep\n")
		fmt.Fprintf(os.Stderr, "   Instances will run indefinitely until manually terminated.\n\n")
	}

	// Build user-data for each config. (Storage mounts aren't wired through this
	// sweep path; attach-volume on sweeps would thread a storageScript here.)
	for _, cfg := range launchConfigs {
		userDataScript, err := buildUserData(plat, cfg, "")
		if err != nil {
			return fmt.Errorf("failed to build user data: %w", err)
		}
		cfg.UserData = encodeUserDataForOS(userDataScript, cfg.TargetOS)
	}

	// Launch instances with rolling queue or all at once
	var launchedInstances []*aws.LaunchResult
	var failures []string
	var successCount int

	if maxConcurrent > 0 && maxConcurrent < len(launchConfigs) {
		// Rolling queue mode
		launchedInstances, failures, successCount, err = launchWithRollingQueue(ctx, awsClient, launchConfigs, sweepID, name, maxConcurrent, launchDelay)
		if err != nil {
			return err
		}
	} else {
		// All-at-once mode (maxConcurrent == 0 or >= total params)
		fmt.Fprintf(os.Stderr, "\n🚀 Launching %d instances in parallel...\n\n", len(launchConfigs))
		launchedInstances, failures, successCount = launchAllAtOnce(ctx, awsClient, launchConfigs)
	}

	// Handle failures
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Some instances failed to launch:\n")
		for _, failure := range failures {
			fmt.Fprintf(os.Stderr, "   • %s\n", failure)
		}
		return fmt.Errorf("%d/%d instances failed to launch", len(failures), len(launchConfigs))
	}

	// Save sweep state
	state := &SweepState{
		SweepID:       sweepID,
		SweepName:     name,
		CreatedAt:     time.Now(),
		ParamFile:     paramFile,
		TotalParams:   len(paramFormat.Params),
		MaxConcurrent: maxConcurrent,
		LaunchDelay:   launchDelay,
		Completed:     0,
		Running:       successCount,
		Pending:       0,
		Failed:        0,
		Instances:     make([]InstanceState, 0, successCount),
	}

	for i, instance := range launchedInstances {
		if instance != nil {
			state.Instances = append(state.Instances, InstanceState{
				Index:      i,
				InstanceID: instance.InstanceID,
				State:      "running",
				LaunchedAt: time.Now(),
			})
		}
	}

	if err := saveSweepState(state); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to save sweep state: %v\n", err)
	}

	// Display success
	fmt.Fprintf(os.Stderr, "\n✅ Parameter sweep launched successfully!\n\n")
	fmt.Fprintf(os.Stderr, "Sweep ID:   %s\n", sweepID)
	fmt.Fprintf(os.Stderr, "Sweep Name: %s\n", name)
	fmt.Fprintf(os.Stderr, "Instances:  %d\n\n", successCount)

	fmt.Fprintf(os.Stderr, "Instances:\n")
	for _, instance := range launchedInstances {
		if instance != nil {
			fmt.Fprintf(os.Stderr, "  • %s (%s) - %s\n", instance.Name, instance.InstanceID, instance.State)
		}
	}

	fmt.Fprintf(os.Stderr, "\nTo view sweep status:\n")
	fmt.Fprintf(os.Stderr, "  spawn list --sweep-id %s\n", sweepID)

	return nil
}

// launchAllAtOnce launches all instances in parallel (no rolling queue)
func launchAllAtOnce(ctx context.Context, awsClient *aws.Client, launchConfigs []*aws.LaunchConfig) ([]*aws.LaunchResult, []string, int) {
	results := runLaunchBatch(len(launchConfigs), func(idx int) (*aws.LaunchResult, error) {
		return awsClient.Launch(ctx, *launchConfigs[idx])
	})

	// Collect results. Parameter sets are independent, so failures are recorded
	// but successful instances are kept running (no cleanup) — the sweep report
	// surfaces which sets failed.
	launchedInstances := make([]*aws.LaunchResult, len(launchConfigs))
	var failures []string
	successCount := 0

	for _, result := range results {
		if result.err != nil {
			failures = append(failures, fmt.Sprintf("Parameter set %d: %v", result.index, result.err))
		} else {
			launchedInstances[result.index] = result.result
			successCount++
			fmt.Fprintf(os.Stderr, "✓ Launched %s (parameter set %d/%d)\n", result.result.Name, result.index+1, len(launchConfigs))
		}
	}

	return launchedInstances, failures, successCount
}

// launchWithRollingQueue launches instances with rolling queue orchestration
func launchWithRollingQueue(ctx context.Context, awsClient *aws.Client, launchConfigs []*aws.LaunchConfig, sweepID, sweepName string, maxConcurrent int, launchDelay string) ([]*aws.LaunchResult, []string, int, error) {
	fmt.Fprintf(os.Stderr, "\n🚀 Launching parameter sweep with rolling queue...\n")
	fmt.Fprintf(os.Stderr, "   Max concurrent: %d\n", maxConcurrent)
	fmt.Fprintf(os.Stderr, "   Launch delay: %s\n\n", launchDelay)

	// Parse launch delay
	delay, err := time.ParseDuration(launchDelay)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("invalid launch delay %q: %w", launchDelay, err)
	}

	// Initialize tracking
	launchedInstances := make([]*aws.LaunchResult, len(launchConfigs))
	var failures []string
	successCount := 0

	// Track active instances (index -> instance ID)
	activeInstances := make(map[int]string)
	nextToLaunch := 0

	// Launch first batch
	initialBatch := maxConcurrent
	if initialBatch > len(launchConfigs) {
		initialBatch = len(launchConfigs)
	}

	fmt.Fprintf(os.Stderr, "Launching initial batch of %d instances...\n", initialBatch)
	for i := 0; i < initialBatch; i++ {
		result, err := awsClient.Launch(ctx, *launchConfigs[i])
		if err != nil {
			failures = append(failures, fmt.Sprintf("Parameter set %d: %v", i, err))
		} else {
			launchedInstances[i] = result
			activeInstances[i] = result.InstanceID
			successCount++
			fmt.Fprintf(os.Stderr, "✓ Launched %s (parameter set %d/%d)\n", result.Name, i+1, len(launchConfigs))
		}

		// Apply launch delay between initial launches
		if i < initialBatch-1 && delay > 0 {
			time.Sleep(delay)
		}
	}
	nextToLaunch = initialBatch

	// Save initial state
	state := &SweepState{
		SweepID:       sweepID,
		SweepName:     sweepName,
		CreatedAt:     time.Now(),
		ParamFile:     paramFile,
		TotalParams:   len(launchConfigs),
		MaxConcurrent: maxConcurrent,
		LaunchDelay:   launchDelay,
		Completed:     0,
		Running:       len(activeInstances),
		Pending:       len(launchConfigs) - nextToLaunch,
		Failed:        len(failures),
		Instances:     make([]InstanceState, 0, len(launchConfigs)),
	}

	for i, instance := range launchedInstances {
		if instance != nil {
			state.Instances = append(state.Instances, InstanceState{
				Index:      i,
				InstanceID: instance.InstanceID,
				State:      "running",
				LaunchedAt: time.Now(),
			})
		}
	}

	if err := saveSweepState(state); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to save sweep state: %v\n", err)
	}

	// Rolling queue: poll for completions and launch next
	if nextToLaunch < len(launchConfigs) {
		fmt.Fprintf(os.Stderr, "\nMonitoring instances and launching next in queue...\n")
		fmt.Fprintf(os.Stderr, "Press Ctrl-C to stop (sweep can be resumed later)\n\n")

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for nextToLaunch < len(launchConfigs) {
			select {
			case <-ctx.Done():
				fmt.Fprintf(os.Stderr, "\n⚠️  Interrupted. Progress saved to sweep state.\n")
				fmt.Fprintf(os.Stderr, "Resume with: spawn resume --sweep-id %s\n", sweepID)
				return launchedInstances, failures, successCount, ctx.Err()

			case <-ticker.C:
				// Query instance states
				instanceIDs := make([]string, 0, len(activeInstances))
				for _, id := range activeInstances {
					instanceIDs = append(instanceIDs, id)
				}

				if len(instanceIDs) == 0 {
					continue
				}

				// Get instance states
				instances, err := awsClient.ListInstances(ctx, launchConfigs[0].Region, "")
				if err != nil {
					fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to query instance states: %v\n", err)
					continue
				}

				// Build state map
				stateMap := make(map[string]string)
				for _, inst := range instances {
					stateMap[inst.InstanceID] = inst.State
				}

				// Check for terminated instances
				var toRemove []int
				for idx, instID := range activeInstances {
					state, exists := stateMap[instID]
					if !exists || state == "terminated" || state == "stopping" || state == "stopped" {
						toRemove = append(toRemove, idx)
					}
				}

				// Remove terminated instances and launch next
				for _, idx := range toRemove {
					delete(activeInstances, idx)

					// Wait launch delay if specified
					if delay > 0 {
						time.Sleep(delay)
					}

					// Launch next pending instance
					if nextToLaunch < len(launchConfigs) {
						result, err := awsClient.Launch(ctx, *launchConfigs[nextToLaunch])
						if err != nil {
							failures = append(failures, fmt.Sprintf("Parameter set %d: %v", nextToLaunch, err))
							fmt.Fprintf(os.Stderr, "✗ Failed to launch parameter set %d: %v\n", nextToLaunch+1, err)
						} else {
							launchedInstances[nextToLaunch] = result
							activeInstances[nextToLaunch] = result.InstanceID
							successCount++
							fmt.Fprintf(os.Stderr, "✓ Launched %s (parameter set %d/%d) [%d active, %d pending]\n",
								result.Name, nextToLaunch+1, len(launchConfigs),
								len(activeInstances), len(launchConfigs)-nextToLaunch-1)

							// Update state file
							state.Running = len(activeInstances)
							state.Pending = len(launchConfigs) - nextToLaunch - 1
							state.Failed = len(failures)
							state.Instances = append(state.Instances, InstanceState{
								Index:      nextToLaunch,
								InstanceID: result.InstanceID,
								State:      "running",
								LaunchedAt: time.Now(),
							})

							if err := saveSweepState(state); err != nil {
								fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to save sweep state: %v\n", err)
							}
						}
						nextToLaunch++
					}
				}
			}
		}

		fmt.Fprintf(os.Stderr, "\n✅ All instances launched. Waiting for final batch to complete...\n")
	}

	return launchedInstances, failures, successCount, nil
}

// launchSweepDetached launches a parameter sweep in detached mode (Lambda orchestration)
func launchSweepDetached(ctx context.Context, paramFormat *ParamFileFormat, baseConfig *aws.LaunchConfig, sweepID, sweepName string, maxConcurrent int, launchDelay string) error {
	// Determine region (auto-detect if not specified)
	sweepRegion := baseConfig.Region
	if sweepRegion == "" {
		fmt.Fprintf(os.Stderr, "🌍 No region specified, auto-detecting closest region...\n")
		detectedRegion, err := detectBestRegion(ctx, baseConfig.InstanceType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not auto-detect region: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Using default: us-east-1\n")
			sweepRegion = "us-east-1"
		} else {
			fmt.Fprintf(os.Stderr, "✓ Selected region: %s\n", sweepRegion)
			sweepRegion = detectedRegion
		}
	}

	// Load dev account config to get account ID
	// IMPORTANT: Always use spore-host-dev profile for target account ID
	// regardless of what AWS_PROFILE is set in environment
	devCfg, err := spawnconfig.LoadComputeAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get AWS account ID from dev account
	accountID, err := aws.NewClientFromConfig(devCfg).GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get AWS account ID: %w", err)
	}

	// Use spore-host-infra config for Lambda/S3/DynamoDB operations
	infraCfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load infra AWS config: %w", err)
	}

	// Convert ParamFileFormat to sweep.ParamFileFormat
	sweepParamFormat := &sweep.ParamFileFormat{
		Defaults: paramFormat.Defaults,
		Params:   paramFormat.Params,
	}

	// Validate parameter sets before launching (best-effort, warn on failure)
	fmt.Fprintf(os.Stderr, "🔍 Validating parameter sets...\n")
	if err := sweep.ValidateParameterSets(ctx, infraCfg, sweepParamFormat, accountID); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Parameter validation skipped (requires cross-account access)\n")
		fmt.Fprintf(os.Stderr, "   Parameters will be validated by Lambda orchestrator\n\n")
	} else {
		fmt.Fprintf(os.Stderr, "✓ All parameter sets validated\n\n")
	}

	// Estimate cost
	fmt.Fprintf(os.Stderr, "💰 Estimating cost...\n")
	costEstimate, err := pricing.EstimateSweepCost(&pricing.ParamFileFormat{
		Defaults: paramFormat.Defaults,
		Params:   paramFormat.Params,
	})
	if err != nil {
		return fmt.Errorf("failed to estimate cost: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n%s\n\n", costEstimate.Display())

	// Check budget
	if budget > 0 {
		if costEstimate.TotalCost > budget {
			fmt.Fprintf(os.Stderr, "⚠️  WARNING: Estimated cost ($%.2f) exceeds budget ($%.2f) by $%.2f\n\n",
				costEstimate.TotalCost, budget, costEstimate.TotalCost-budget)
		} else {
			fmt.Fprintf(os.Stderr, "✓ Within budget: $%.2f remaining of $%.2f\n\n",
				budget-costEstimate.TotalCost, budget)
		}
	}

	// If estimate-only, exit here
	if estimateOnly {
		fmt.Fprintf(os.Stderr, "✅ Cost estimate complete (--estimate-only specified)\n")
		return nil
	}

	// If not auto-approved, prompt for confirmation
	if !autoYes {
		fmt.Fprintf(os.Stderr, "Launch sweep? [Y/n]: ")
		var response string
		_, _ = fmt.Scanln(&response)
		response = strings.ToLower(strings.TrimSpace(response))
		if response != "" && response != "y" && response != "yes" {
			fmt.Fprintf(os.Stderr, "\n❌ Launch cancelled by user\n")
			return nil
		}
		fmt.Fprintf(os.Stderr, "\n")
	}

	// Upload parameters to S3
	fmt.Fprintf(os.Stderr, "📤 Uploading parameters to S3...\n")
	s3Key, err := sweep.UploadParamsToS3(ctx, infraCfg, sweepParamFormat, sweepID, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to upload params to S3: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Uploaded: %s\n\n", s3Key)

	// Create DynamoDB record
	fmt.Fprintf(os.Stderr, "💾 Creating sweep orchestration record...\n")
	record := &sweep.SweepRecord{
		SweepID:                sweepID,
		SweepName:              sweepName,
		S3ParamsKey:            s3Key,
		MaxConcurrent:          maxConcurrent,
		MaxConcurrentPerRegion: maxConcurrentPerRegion,
		LaunchDelay:            launchDelay,
		TotalParams:            len(paramFormat.Params),
		Region:                 sweepRegion,
		AWSAccountID:           accountID,
		EstimatedCost:          costEstimate.TotalCost,
		Budget:                 budget,
	}

	// Check if multi-region sweep
	regionGroups := sweep.GroupParamsByRegion(sweepParamFormat.Params, sweepParamFormat.Defaults)

	// Apply region constraints if specified
	if shouldApplyRegionConstraints() {
		constraint := &sweep.RegionConstraint{
			Include:       regionsInclude,
			Exclude:       regionsExclude,
			Geographic:    regionsGeographic,
			ProximityFrom: proximityFrom,
			CostTier:      costTier,
		}

		// Validate constraint
		if err := validateRegionConstraint(constraint); err != nil {
			return fmt.Errorf("invalid region constraint: %w", err)
		}

		// Get all regions from parameter file
		allRegions := make([]string, 0, len(regionGroups))
		for region := range regionGroups {
			allRegions = append(allRegions, region)
		}

		// Apply constraints
		filteredRegions, err := applyRegionConstraints(allRegions, constraint)
		if err != nil {
			return fmt.Errorf("region constraints failed: %w", err)
		}

		// Remove filtered-out regions
		for region := range regionGroups {
			if !containsString(filteredRegions, region) {
				delete(regionGroups, region)
			}
		}

		// Store constraint in record
		record.RegionConstraints = constraint
		record.FilteredRegions = filteredRegions

		fmt.Fprintf(os.Stderr, "🌍 Applied region constraints: %d regions allowed\n", len(filteredRegions))
		fmt.Fprintf(os.Stderr, "   Filtered regions: %v\n", filteredRegions)
		fmt.Fprintf(os.Stderr, "   Constraint: %s\n", formatConstraint(constraint))
	}

	if len(regionGroups) == 1 {
		// Single region - use that as the sweep region
		for region := range regionGroups {
			record.Region = region
			break
		}
	} else if len(regionGroups) > 1 {
		// Multi-region sweep
		record.MultiRegion = true
		record.RegionStatus = make(map[string]*sweep.RegionProgress)

		regions := make([]string, 0, len(regionGroups))
		for region, indices := range regionGroups {
			regions = append(regions, region)
			record.RegionStatus[region] = &sweep.RegionProgress{
				NextToLaunch: indices,
				Launched:     0,
				Failed:       0,
				ActiveCount:  0,
			}
		}

		fmt.Fprintf(os.Stderr, "🌍 Multi-region sweep detected: %v\n", regions)
	}

	// Set distribution mode (only applies to multi-region sweeps)
	if record.MultiRegion {
		record.DistributionMode = distributionMode
		if distributionMode == "opportunistic" {
			fmt.Fprintf(os.Stderr, "📊 Distribution mode: opportunistic (prioritize available regions)\n")
		}
	}

	err = sweep.CreateSweepRecord(ctx, infraCfg, record)
	if err != nil {
		return fmt.Errorf("failed to create sweep record: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Record created\n\n")

	// Invoke Lambda orchestrator
	fmt.Fprintf(os.Stderr, "🚀 Invoking Lambda orchestrator...\n")
	err = sweep.InvokeSweepOrchestrator(ctx, infraCfg, sweepID)
	if err != nil {
		return fmt.Errorf("failed to invoke Lambda: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Lambda invoked\n\n")

	// Display success
	fmt.Fprintf(os.Stderr, "✅ Parameter sweep queued successfully!\n\n")
	fmt.Fprintf(os.Stderr, "Sweep ID:          %s\n", sweepID)
	fmt.Fprintf(os.Stderr, "Sweep Name:        %s\n", sweepName)
	fmt.Fprintf(os.Stderr, "Total Parameters:  %d\n", len(paramFormat.Params))
	fmt.Fprintf(os.Stderr, "Max Concurrent:    %d\n", maxConcurrent)
	fmt.Fprintf(os.Stderr, "Region:            %s\n", sweepRegion)
	fmt.Fprintf(os.Stderr, "Orchestration:     Lambda (infra account)\n\n")

	fmt.Fprintf(os.Stderr, "The sweep is now running in Lambda. You can disconnect safely.\n\n")
	fmt.Fprintf(os.Stderr, "To check status:\n")
	fmt.Fprintf(os.Stderr, "  spawn status --sweep-id %s\n\n", sweepID)
	fmt.Fprintf(os.Stderr, "To resume if needed:\n")
	fmt.Fprintf(os.Stderr, "  spawn resume --sweep-id %s --detach\n", sweepID)

	// Wait for completion if requested
	if wait {
		timeout, _ := time.ParseDuration(waitTimeout)
		if err := waitForSweepCompletion(ctx, sweepID, timeout); err != nil {
			return fmt.Errorf("wait failed: %w", err)
		}
	}

	return nil
}

// waitForSweepCompletion polls sweep status until completion or timeout
func waitForSweepCompletion(ctx context.Context, sweepID string, timeout time.Duration) error {
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	startTime := time.Now()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	fmt.Fprintf(os.Stderr, "\n⏳ Waiting for sweep to complete...\n")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			status, err := sweep.QuerySweepStatus(ctx, cfg, sweepID)
			if err != nil {
				return fmt.Errorf("failed to query status: %w", err)
			}

			fmt.Fprintf(os.Stderr, "   Progress: %d/%d launched, Status: %s\n",
				status.Launched, status.TotalParams, status.Status)

			switch status.Status {
			case "COMPLETED":
				fmt.Fprintf(os.Stderr, "✅ Sweep completed successfully\n")
				return nil
			case "FAILED":
				return fmt.Errorf("sweep failed: %s", status.ErrorMessage)
			case "CANCELLED":
				return fmt.Errorf("sweep was cancelled")
			}

			// Check timeout
			if timeout > 0 && time.Since(startTime) > timeout {
				return fmt.Errorf("timeout waiting for completion")
			}
		}
	}
}
