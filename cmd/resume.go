package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/sweep"
)

var (
	resumeSweepID           string
	resumeMaxConcurrent     int
	resumeMaxConcurrentAuto bool
	resumeDetach            bool
	resumeKeepOnError       bool
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
	registerResumeFlags(resumeCmd)

	rootCmd.AddCommand(resumeCmd)
}

// registerResumeFlags declares the flags runResume reads, on any command that
// delegates to it. Both the deprecated `spawn resume` and its replacement
// `spawn sweep resume` call this, so a flag added here reaches both spellings
// and a user who follows the deprecation notice keeps every option (#935).
// --sweep-id is not included: `spawn sweep resume` takes it positionally.
func registerResumeFlags(c *cobra.Command) {
	c.Flags().IntVar(&resumeMaxConcurrent, "max-concurrent", 0, "Override max concurrent instances (0 = use original)")
	c.Flags().BoolVar(&resumeMaxConcurrentAuto, "max-concurrent-auto", false, "Re-derive max concurrent instances from the account's real AWS quota headroom for the pending parameter sets' instance type(s)/region, instead of the original or a user-supplied number (spawn#494). Not yet supported with --detach.")
	c.Flags().BoolVar(&resumeDetach, "detach", false, "Run sweep orchestration in Lambda")
	c.Flags().BoolVar(&resumeKeepOnError, "keep-on-error", false, "Keep a relaunched instance whose spored never came up instead of terminating it. It then has no TTL/idle/cost-limit enforcement until spored is confirmed running")
}

func runResume(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if resumeMaxConcurrentAuto && resumeMaxConcurrent > 0 {
		return fmt.Errorf("--max-concurrent-auto and --max-concurrent are mutually exclusive — pick one")
	}

	// Check if detached mode requested - handle early since detached sweeps have no local state
	if resumeDetach {
		// --max-concurrent-auto needs the pending parameter sets' instance
		// types, which for a detached sweep live in S3 (SweepRecord only
		// stores an S3ParamsKey, not the params themselves) — no download
		// helper exists yet (only the upload side, UploadParamsToS3), and
		// there's currently no override path from resume into the stored
		// SweepRecord.MaxConcurrent the Lambda orchestrator reads. spawn#494
		// tracks this as a separate follow-up; reject explicitly for now
		// rather than silently ignoring the flag.
		if resumeMaxConcurrentAuto {
			return fmt.Errorf("--max-concurrent-auto is not yet supported with --detach (spawn#494)")
		}
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

	// Same top-level-key check launchParameterSweep runs (#530). The original
	// launch would already have refused a file with a dangerous top-level key,
	// but the file on disk can change between launch and resume, and resume
	// reloads it fresh rather than trusting the saved state.
	if err := validateTopLevelParamKeys(state.ParamFile, paramFormat.UnknownTopLevelKeys); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ ERROR: %v\n\n", err)
		return fmt.Errorf("unusable param-file keys")
	}
	// The profile the original CLI IAM flags resolved to: the param file never
	// held it, so without this a resumed row fell back to
	// spored-instance-profile (#1385). Before the nested-role check, which then
	// sees the same profile the original launch did.
	applyRecordedIAM(state.IAMRole, paramFormat)
	// Resume rebuilds each row from the file and the checkpoint (no --ttl), so
	// a row naming a nested-spawn role needs a ttl: in the file (#1375).
	if err := refuseNestedSweepRowsWithoutTTL(paramFormat, ""); err != nil {
		return err
	}

	// Validate parameter count matches
	if len(paramFormat.Params) != state.TotalParams {
		return fmt.Errorf("parameter count mismatch: state file has %d params but parameter file has %d",
			state.TotalParams, len(paramFormat.Params))
	}

	// Resolve the region of every launched instance and the fallback for
	// pending ones (#495). This used to be an unchecked
	// Defaults["region"].(string) — a panic for a param file with no region
	// default — followed by a silent us-east-1 fallback, which reconciled
	// against, and launched into, a region the sweep may never have used.
	regions, err := resolveResumeRegions(state, paramFormat, spawnRegion, spawnconfig.SharedRegion())
	if err != nil {
		return err
	}
	// Resolved before any AWS client exists: a sweep with no knowable
	// region is refused without touching the network.

	// Initialize AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}

	// Query EC2 to reconcile state
	fmt.Fprintf(os.Stderr, "🔍 Querying EC2 to reconcile instance states...\n")

	// Query every region the sweep's instances live in.
	var allInstances []aws.InstanceInfo
	for _, r := range regions.distinct() {
		found, err := awsClient.ListInstances(ctx, r, "")
		if err != nil {
			return fmt.Errorf("failed to query instances in %s: %w", r, err)
		}
		allInstances = append(allInstances, found...)
	}

	// Build map of instance ID -> current state
	instanceStates := make(map[string]string)
	profileByID := make(map[string]string)
	for _, inst := range allInstances {
		if inst.InstanceID != "" {
			instanceStates[inst.InstanceID] = inst.State
			profileByID[inst.InstanceID] = inst.IAMRole
		}
	}
	// The profile each of the sweep's own instances still visible in EC2 was
	// launched with: the only evidence of the original IAM flags a checkpoint
	// written before #1385 has.
	origProfiles := make(map[int]string)
	for _, inst := range state.Instances {
		if p := profileByID[inst.InstanceID]; p != "" {
			origProfiles[inst.Index] = p
		}
	}
	if err := refuseOldCheckpointRoleDrift(state, paramFormat, origProfiles); err != nil {
		return err
	}

	// Update state based on EC2 reality
	stillRunning, completed, failed, pending := reconcileSweepState(state, instanceStates)

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
		config, err := buildLaunchConfigFromParams(aws.LaunchConfig{}, paramFormat.Defaults, paramSet, state.SweepID, state.SweepName, idx, state.TotalParams)
		if err != nil {
			return fmt.Errorf("failed to build launch config for parameter set %d: %w", idx, err)
		}

		// Set region: the parameter file's own (per-set or default) region
		// wins, then the resolved fallback. With neither there is no region
		// to launch into, and guessing one is how a sweep lands in the wrong
		// account region (#495).
		if config.Region == "" {
			config.Region = regions.fallback
		}
		if config.Region == "" {
			return fmt.Errorf("cannot determine a region for pending parameter set %d: "+
				"the parameter file sets no region and the sweep state records none — pass --region", idx)
		}

		// Set name
		if config.Name == "" {
			config.Name = fmt.Sprintf("%s-%d", state.SweepName, idx)
		}

		launchConfigs = append(launchConfigs, &config)
	}

	// --max-concurrent-auto (#494): now that the pending launch configs are
	// built (each with a resolved InstanceType/Region/Spot), derive a real
	// quota-backed ceiling instead of the original or a user-typed number.
	if resumeMaxConcurrentAuto && len(launchConfigs) > 0 {
		region := launchConfigs[0].Region
		regionClient, rerr := aws.NewClientWithRegion(ctx, region)
		if rerr != nil {
			return fmt.Errorf("--max-concurrent-auto: create AWS client for %s: %w", region, rerr)
		}
		derived, rerr := resolveAutoMaxConcurrentFromConfigs(ctx, launchConfigs, region, regionClient)
		if rerr != nil {
			return fmt.Errorf("--max-concurrent-auto: %w", rerr)
		}
		fmt.Fprintf(os.Stderr, "✓ --max-concurrent-auto: derived %d (account quota headroom in %s)\n", derived, region)
		fmt.Fprintf(os.Stderr, "🔧 Overriding max-concurrent: %d -> %d\n\n", state.MaxConcurrent, derived)
		maxConcurrent = derived
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

	// Setup IAM role: spored's shared profile for every member without one.
	// (This checked only the first config, so a later member without an
	// iam_role: launched with no profile at all when the first had one.)
	if slices.ContainsFunc(launchConfigs, func(c *aws.LaunchConfig) bool { return c.IamInstanceProfile == "" }) {
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
		userData, err := resumeUserData(plat, cfg)
		if err != nil {
			return err
		}
		cfg.UserData = userData
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
	state.Completed, state.Running, state.Failed = len(completed), len(stillRunning), len(failed)
	// Each relaunched member's spored is verified, as on a fresh sweep (#1305).
	guard := newFleetGuard("sweep resume", os.Stderr, awsClient, clientFleetVerifier(awsClient), resumeKeepOnError)
	resumedInstances, failures, successCount, err := relaunchPending(ctx, awsClient, state, launchConfigs, pending, maxConcurrent, guard, awsClient)
	if pe := (*resumeProfileError)(nil); errors.As(err, &pe) {
		return err // refused before the first launch: nothing to report or clean up
	}

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

	fmt.Fprintf(os.Stderr, "   %s\n", guard.summary())

	if err != nil {
		return guard.finish(ctx, fmt.Errorf("sweep resumed with errors: %w", err))
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

	// The CLI IAM flags' profile may have been deleted as idle since the sweep
	// last ran (#1385): re-create it before the orchestrator launches with it.
	if err := ensureDetachedResumeProfile(ctx, sweepID, func(ctx context.Context) (resumeProfileEnsurer, error) {
		return aws.NewClient(ctx)
	}); err != nil {
		return err
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

// resumeRegionSet is where a resumed sweep's instances live: the region of
// each launched instance, and the region pending parameter sets that name none
// of their own are launched into.
type resumeRegionSet struct {
	byInstance map[string]string
	fallback   string
}

// distinct returns the regions to reconcile, sorted.
func (r resumeRegionSet) distinct() []string {
	seen := map[string]bool{}
	var out []string
	for _, reg := range r.byInstance {
		if !seen[reg] {
			seen[reg] = true
			out = append(out, reg)
		}
	}
	sort.Strings(out)
	return out
}

// paramSetRegion is the region parameter set idx asks for: its own "region",
// else the file's default. Checked assertions — a missing or non-string value
// is "no region", never a panic (#495).
func paramSetRegion(pf *ParamFileFormat, idx int) string {
	if pf != nil && idx >= 0 && idx < len(pf.Params) {
		if r, ok := pf.Params[idx]["region"].(string); ok && r != "" {
			return r
		}
	}
	if pf != nil {
		if r, ok := pf.Defaults["region"].(string); ok && r != "" {
			return r
		}
	}
	return ""
}

// resolveResumeRegions works out where a sweep being resumed lives (#495).
//
// flagRegion is --region as typed; configRegion is the resolved shared config
// region (--region > SPORE_REGION/AWS_REGION > spore config), the same source
// the original launch read (resolveLaunchRegion).
//
// A launched instance's region is, in order: the region recorded for it in the
// sweep state (from its LaunchResult), the region its parameter set asks for,
// or configRegion. If none of those exists the instance cannot be reconciled
// and this is an error, not a guess.
//
// The fallback for pending sets that name no region is --region, else the
// sweep's one recorded region if every launched instance agrees on it, else
// configRegion. It may be empty; the caller then refuses to launch a set with
// no region. There is no hard-coded default: us-east-1 was one (#495).
func resolveResumeRegions(state *SweepState, pf *ParamFileFormat, flagRegion, configRegion string) (resumeRegionSet, error) {
	set := resumeRegionSet{byInstance: map[string]string{}}
	recorded := map[string]bool{}
	for _, inst := range state.Instances {
		if inst.InstanceID == "" {
			continue
		}
		reg := inst.Region
		if reg != "" {
			recorded[reg] = true
		}
		if reg == "" {
			reg = paramSetRegion(pf, inst.Index)
		}
		if reg == "" {
			reg = configRegion
		}
		if reg == "" {
			return set, fmt.Errorf("cannot determine the region of sweep instance %s (parameter set %d): "+
				"the sweep state does not record it and the parameter file sets no region — pass --region",
				inst.InstanceID, inst.Index)
		}
		set.byInstance[inst.InstanceID] = reg
	}
	set.fallback = flagRegion
	if set.fallback == "" && len(recorded) == 1 {
		for reg := range recorded {
			set.fallback = reg
		}
	}
	if set.fallback == "" {
		set.fallback = configRegion
	}
	return set, nil
}

// relaunchPending launches the pending parameter sets of a loaded sweep.
// launchConfigs[i] is parameter set pending[i]. The new instances are merged
// into state — the loaded one, never a fresh one — so the records of the
// parameters that already ran, the sweep's TotalParams and its metadata all
// survive; a fresh state here made a second resume relaunch completed work
// (#1248).
//
// guard verifies each relaunched member's spored and terminates one that never
// comes up, as on a fresh sweep (#1305); the caller defers guard.finish.
//
// Before the first launch, every spawn-created instance profile the members
// name is made to exist again through iam (ensureResumeProfiles): the idle-role
// sweep may have deleted one since the sweep last ran (#1385).
func relaunchPending(ctx context.Context, l sweepLauncher, state *SweepState, launchConfigs []*aws.LaunchConfig, pending []int, maxConcurrent int, guard *fleetGuard, iam resumeProfileEnsurer) ([]*aws.LaunchResult, []string, int, error) {
	if err := ensureResumeProfiles(ctx, iam, state.IAMRole, state.ParamFile, launchConfigs); err != nil {
		return nil, nil, 0, &resumeProfileError{err}
	}
	return launchWithRollingQueue(ctx, l, guard, state, launchConfigs, pending, maxConcurrent, state.LaunchDelay)
}

// reconcileSweepState classifies every parameter index of the sweep against
// the EC2 states of its recorded instances (instance ID -> state). An index
// with no record is pending: it is what `spawn resume` launches. That is only
// right if every launch, including a resume's, records its instance under the
// parameter index it ran (#1248).
func reconcileSweepState(state *SweepState, instanceStates map[string]string) (stillRunning, completed, failed, pending []int) {
	for i := 0; i < state.TotalParams; i++ {
		// Find this parameter index in state
		found := false
		for _, inst := range state.Instances {
			if inst.Index == i {
				found = true
				// Check if instance still running
				if inst.InstanceID != "" && inst.State == sweepInstanceFailed {
					// Launched, but spored never came up and it was terminated
					// (#1305): a failure, not a completion.
					failed = append(failed, i)
				} else if inst.InstanceID != "" {
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

	return stillRunning, completed, failed, pending
}

// resumeUserData builds a resumed sweep member's user data, encoded the way
// every other launch path encodes it. It used to be base64 of the plain script,
// with no gzip, and the inline bootstrap alone is about 25 KB, so every resumed
// Linux launch was over EC2's limits and RunInstances refused it (#1274).
func resumeUserData(plat *platform.Platform, cfg *aws.LaunchConfig) (string, error) {
	script, err := buildUserData(plat, cfg, "", nil)
	if err != nil {
		return "", fmt.Errorf("failed to build user data: %w", err)
	}
	return encodeUserDataForOS(script, cfg.TargetOS), nil
}
