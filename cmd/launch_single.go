package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/compliance"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/dns"
	"github.com/spore-host/spawn/pkg/input"
	"github.com/spore-host/spawn/pkg/locality"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/wizard"
)

func runLaunch(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	waitForSSHSet = cmd.Flags().Changed("wait-for-ssh")
	var kerr error
	keepOnError, kerr = resolveKeepOnError(cmd.Flags().Changed("keep-on-error"), keepOnErrorFlag,
		cmd.Flags().Changed("terminate-on-error"), terminateOnError)
	if kerr != nil {
		return kerr
	}

	// An explicit --os windows is known before anything is resolved, so refuse
	// the flags Windows can't honour here, before any AWS call and ahead of every dispatch below (#1022).
	// The batch-queue and detached-sweep paths never resolve a target OS, so this
	// is the only place they are checked. An auto-detected Windows AMI is caught
	// later, by windowsLifecycleGuard.
	if err := windowsUnsupportedFlags(osFlag, launchFlags); err != nil {
		return err
	}
	// Likewise the flags a parameter sweep can't honour (#1366), before the
	// identity lookup below, so a refused sweep makes no AWS call at all.
	if isSweepLaunch() {
		if err := sweepUnsupportedFlags(launchFlags); err != nil {
			return err
		}
	}

	// And --vpc with neither a subnet nor an --az to pick one in (#1389),
	// which would land in the default VPC.
	if err := validateVPCHasSubnet(batchQueueFile != "" || queueTemplate != ""); err != nil {
		return err
	}

	// Reject an unparseable --ttl/--idle-timeout (etc.) before any AWS call,
	// on every launch path below (#990).
	if err := validateLaunchDurationFlags(); err != nil {
		return err
	}
	// And a spored setting spored would ignore (#1308).
	if _, _, err := sporedSettingsFromFlags(); err != nil {
		return err
	}
	// And an --efs-mount-options option the mount cannot take (#1390): a single
	// launch renders it only after creating its key pair, role and security
	// group.
	if err := validateEFSMountOptionsFlag(); err != nil {
		return err
	}
	// And a malformed or reserved-prefix --tag, before any AWS call and the
	// dispatch below, on every path (#1356).
	if _, err := parseKVTags(launchTags); err != nil {
		return err
	}
	// And a nested-spawn role named by --instance-profile with no --ttl, or by
	// --iam-role at all (#1375), on every path, before any AWS call.
	if err := validateNestedSpawnRoleFlags(launchMode(interactive, instanceType, isTerminal(os.Stdin)) == modeWizard); err != nil {
		return err
	}

	// Get the account ID for audit logging. This is best-effort and resilient:
	// it uses the AWS client's GetAccountID, which falls back to IMDS if STS is
	// unreachable (e.g. an STS VPC endpoint with private DNS this subnet can't
	// route to, #33). A failure here must not block the launch — audit user_id
	// is non-critical metadata.
	identityClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	userID, err := identityClient.GetAccountID(ctx)
	if err != nil {
		userID = "unknown"
		fmt.Fprintf(os.Stderr, "⚠️  Could not determine account ID (STS/IMDS unavailable); continuing with audit user=unknown\n")
	}
	correlationID := uuid.New().String()
	auditLog := newAuditLogger(userID, correlationID)

	// Detect platform
	plat, err := platform.Detect()
	if err != nil {
		return i18n.Te("error.platform_detect_failed", err)
	}

	// Enable colors on Windows
	if plat.OS == "windows" {
		platform.EnableWindowsColors()
	}

	// Check for batch queue mode FIRST
	// Capture positional name argument early (before batch queue / sweep early returns)
	if len(args) > 0 && name == "" {
		name = args[0]
	}

	if err := validateNestedSpawnFlags(); err != nil {
		return err
	}

	if batchQueueFile != "" || queueTemplate != "" {
		// --dry-run/--print-config (#569) is not wired into the batch-queue path
		// yet — refuse rather than silently falling through to a real launch,
		// which would violate the flag's whole safety contract (zero AWS
		// mutation). --estimate-only already works here via launchWithBatchQueue.
		if launchDryRun {
			return fmt.Errorf("--dry-run is not yet supported for --batch-queue/--queue-template launches; use --estimate-only for a cost preview")
		}
		return launchWithBatchQueue(ctx, plat, auditLog)
	}

	// Check for parameter sweep mode (before wizard/config logic)
	if paramFile != "" || params != "" {
		// Same refusal as batch-queue above: --dry-run isn't wired into the sweep
		// path, so it must fail closed rather than silently launch. --estimate-only
		// already covers a sweep cost preview.
		if launchDryRun {
			return fmt.Errorf("--dry-run is not yet supported for parameter-sweep launches (--param-file/--params); use --estimate-only for a cost preview")
		}
		// Build the REAL config and pass it as the sweep's base (spawn#697).
		//
		// This used to construct a two-field struct — Region and InstanceType —
		// and nothing else, so every other CLI flag was dropped on the sweep
		// path. A flag worked only if someone had hand-written a shim for it,
		// which is why the same class was reported and patched six separate
		// times (#525, #539 twice, #549, #667, #673/#674/#675).
		//
		// buildLaunchConfig is exactly what the single-instance path calls, so a
		// sweep now starts from the same config a single launch would get, and
		// each param row overrides it. Its validation applies to sweeps too,
		// which is the point rather than a side effect: --fsx-create without
		// --fsx-s3-bucket was previously accepted-and-ignored on a sweep.
		config, err := sweepBaseConfig(ctx)
		if err != nil {
			return err
		}
		return launchParameterSweep(ctx, config, plat, auditLog)
	}

	// Apply user defaults from ~/.spawn/config.yaml for flags not explicitly set.
	applyLaunchDefaults(cmd)

	// Positional arg takes precedence over --name flag.
	if len(args) > 0 {
		name = args[0]
	}

	var config *aws.LaunchConfig

	// Windows gets a non-burstable default instance type when --os windows is
	// explicit and --instance-type was omitted (#95). Apply it BEFORE launchMode,
	// which would otherwise treat an empty type as "go to the wizard/pipe" and
	// never reach the flags path.
	if instanceType == "" && strings.EqualFold(strings.TrimSpace(osFlag), "windows") {
		instanceType = defaultWindowsInstanceType
		fmt.Fprintf(os.Stderr, "ℹ️  No --instance-type given for Windows; defaulting to %s (non-burstable).\n", defaultWindowsInstanceType)
	}

	// Determine mode: wizard, pipe, or flags. See launchMode for the rules — the
	// key fix (#34): pipe mode requires no --instance-type, so explicit flags
	// never read stdin even when invoked with a piped (non-TTY) stdin, e.g. from
	// a Java/ProcessBuilder subprocess.
	switch launchMode(interactive, instanceType, isTerminal(os.Stdin)) {
	case modeWizard:
		wiz := wizard.NewWizard(plat)
		config, err = wiz.Run(ctx)
		if err != nil {
			return err
		}
	case modePipe:
		// Pipe mode (from truffle): no instance type given and stdin is piped.
		truffleInput, err := input.ParseFromStdin()
		if err != nil {
			return i18n.Te("error.input_parse_failed", err)
		}
		config, err = buildLaunchConfig(truffleInput)
		if err != nil {
			return err
		}
	default: // modeFlags
		// Explicit --instance-type; stdin is ignored (TTY or pipe).
		config, err = buildLaunchConfig(nil)
		if err != nil {
			return err
		}
	}
	// Whatever produced the config (the wizard included), its durations must
	// parse before the security-group, key-pair and FSx work below (#990).
	if err := config.ValidateDurations(); err != nil {
		return err
	}
	if err := requireNestedSpawnTTL(config); err != nil {
		return err
	}

	// Apply custom --tag key=value tags to the instance + its created volumes,
	// so ephemeral spores and their --attach-volume data volumes are attributable
	// in Cost Explorer / cleanup scripts (#161), and --team's spawn:team-id. The
	// sweep and batch-queue paths above apply the same (#1356).
	if err := applyLaunchFlagTags(ctx, config); err != nil {
		return err
	}

	// AWS's 50-tag limit (#1336), before the Strata upload below and everything
	// launchWithProgress creates: the --tag values are in, and preflightLaunchTags
	// stands in for the tags those steps add.
	if err := preflightLaunchTags(config); err != nil {
		return err
	}

	// Strata software environment selection. Skipped under --dry-run/--print-config
	// (#569): resolveStrataEnvironment's UploadLockfile writes to S3 — an actual
	// AWS mutation — so a dry run must not call it. Note the tag as unresolved
	// instead, so the preview doesn't silently omit that a real launch would set
	// strata:lockfile-s3-uri.
	if strataFormation != "" || strataProfile != "" {
		if launchDryRun {
			if config.Tags == nil {
				config.Tags = make(map[string]string)
			}
			config.Tags["strata:lockfile-s3-uri"] = "(unresolved in --dry-run — resolving uploads a lockfile to S3)"
		} else {
			fmt.Fprintf(os.Stderr, "Resolving Strata environment...\n")
			uri, err := resolveStrataEnvironment(ctx, strataFormation, strataProfile, strataRegistry)
			if err != nil {
				return fmt.Errorf("strata: %w", err)
			}
			config.Tags["strata:lockfile-s3-uri"] = uri
			fmt.Fprintf(os.Stderr, "Strata environment resolved: %s\n", uri)
		}
	}

	// Validate
	if config.Name == "" {
		return fmt.Errorf("--name is required: give your spore a name (e.g. --name my-worker)")
	}
	if config.InstanceType == "" {
		// Windows with --os windows is defaulted to m7i.xlarge earlier (before
		// launchMode), so an empty type here is a genuine non-Windows omission.
		return i18n.Te("error.instance_type_required", nil)
	}

	// Resolve the launch region (#732). The precedence lives in one place —
	// resolveLaunchRegion — because this `if region == ""` shape was duplicated
	// at five call sites and fixing one would have left four wrong.
	{
		region, why, rerr := resolveLaunchRegion(ctx, config.Region, config.InstanceType)
		if rerr != nil {
			// Deliberately NOT a fallback to us-east-1. The old default was a
			// guess that could itself fail to offer the instance type, turning
			// "we could not choose" into a confident, wrong choice.
			return fmt.Errorf("could not choose a region for %s: %w\n"+
				"Pass --region, or set AWS_REGION / a default in your spore config",
				config.InstanceType, rerr)
		}
		if config.Region == "" {
			fmt.Fprintf(os.Stderr, "🌍 Region: %s (%s)\n", region, why)
		}
		config.Region = region
	}

	// Initialize AWS client PINNED to the resolved launch region (#276). config.Region
	// is fully resolved above (explicit --region, or auto-detected), so pinning the
	// client here makes every region-sensitive call (caller-identity, pricing, AMI/AZ
	// resolution, RunInstances) use it — not the ambient AWS_REGION/profile default,
	// which previously let a --region launch land in the wrong region.
	awsClient, err := aws.NewClientWithRegion(ctx, config.Region)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Determine compliance mode from flags
	if cmd.Flags().Changed("nist-800-171") {
		complianceMode = "nist-800-171"
	} else if cmd.Flags().Changed("nist-800-53") {
		val, _ := cmd.Flags().GetString("nist-800-53")
		if val == "" {
			val = "low"
		}
		complianceMode = fmt.Sprintf("nist-800-53-%s", val)
	}
	if cmd.Flags().Changed("compliance-strict") {
		complianceStrict, _ = cmd.Flags().GetBool("compliance-strict")
	}

	// Load compliance configuration and validate if enabled
	if complianceMode != "" {
		complianceConfig, err := spawnconfig.LoadComplianceConfig(ctx, complianceMode, complianceStrict)
		if err != nil {
			return fmt.Errorf("failed to load compliance config: %w", err)
		}

		infraConfig, err := spawnconfig.LoadInfrastructureConfig(ctx, "")
		if err != nil {
			return fmt.Errorf("failed to load infrastructure config: %w", err)
		}

		// Apply compliance enforcement to launch config
		validator := compliance.NewValidator(complianceConfig, infraConfig)
		if err := validator.EnforceLaunchConfig(config); err != nil {
			return fmt.Errorf("failed to enforce compliance: %w", err)
		}

		// Mark compliance mode in config for enforcement in aws client
		config.EBSEncrypted = complianceConfig.EnforceEncryptedEBS
		config.IMDSv2Enforced = complianceConfig.EnforceIMDSv2
		config.IMDSv2HopLimit = 1

		// Validate launch configuration
		result, err := validator.ValidateLaunchConfig(ctx, config)
		if err != nil {
			return fmt.Errorf("compliance validation failed: %w", err)
		}

		// Handle validation warnings
		if result.HasWarnings() {
			for _, warning := range result.Warnings {
				fmt.Fprintf(os.Stderr, "⚠️  %s\n", warning)
			}
		}

		// Handle validation violations
		if result.HasViolations() {
			if validator.IsStrictMode() {
				// Strict mode: fail on violations
				fmt.Fprintf(os.Stderr, "\n❌ Compliance validation failed (%d violations):\n", len(result.Violations))
				for _, violation := range result.Violations {
					fmt.Fprintf(os.Stderr, "  [%s] %s: %s\n", violation.ControlID, violation.ControlName, violation.Description)
				}
				return fmt.Errorf("compliance validation failed in strict mode")
			} else {
				// Non-strict mode: show warnings but continue
				fmt.Fprintf(os.Stderr, "\n⚠️  Compliance warnings (%d):\n", len(result.Violations))
				for _, violation := range result.Violations {
					fmt.Fprintf(os.Stderr, "  [%s] %s: %s\n", violation.ControlID, violation.ControlName, violation.Description)
				}
				fmt.Fprintf(os.Stderr, "\nContinuing launch with warnings. Use --compliance-strict to fail on violations.\n\n")
			}
		}

		// Show compliance summary
		if !quiet {
			fmt.Fprintf(os.Stderr, "\n✓ Compliance mode: %s\n", complianceConfig.GetModeDisplayName())
			if config.EBSEncrypted {
				fmt.Fprintf(os.Stderr, "✓ EBS encryption: enforced\n")
			}
			if config.IMDSv2Enforced {
				fmt.Fprintf(os.Stderr, "✓ IMDSv2: enforced\n")
			}
			fmt.Fprintf(os.Stderr, "\n")
		}
	}

	// Zombie-instance guard: default to 1h idle timeout when nothing bounds the
	// instance's lifetime, or require confirmation if --no-timeout was set.
	if err := guardZombieInstance(config); err != nil {
		return err
	}

	// --dry-run / --print-config (#569): resolve the config through every step
	// that's cheap/read-only to preview (AMI auto-detect, OS/Windows guards,
	// MPI/EFA/hibernation pre-flight, MPI placement-group decision, pricing),
	// print it, and return — BEFORE launchWithProgress's first AWS mutation
	// (setupSSHKey's ImportKeyPair). Checked before --estimate-only so a launch
	// invoked with both flags gets the richer preview (--estimate-only alone
	// skips AMI/IAM/SG resolution entirely; this is deliberately fuller).
	// Refuse, before anything is spent, a launch whose storage commitment alone
	// exceeds --cost-limit (#616). Placed above the --dry-run and --estimate-only
	// returns so a preview reports the same refusal a real launch would hit —
	// a preview that says "fine" for a launch that will be rejected is its own bug.
	if err := costLimitPreflight(); err != nil {
		return err
	}

	// Warn before anything is spent when this launch leans on the out-of-band reaper
	// and no reaper covers the account (#624). Placed ABOVE the --dry-run and
	// --estimate-only returns on purpose: those are the "what would this cost me"
	// surfaces, and a reaper that will not reclaim an ephemeral filesystem or a
	// stopped instance is precisely a cost the preview should mention.
	//
	// Not unconditional — see warnIfReaperWontCover. The previous version of
	// this check fired in every account and therefore told the user nothing.
	warnIfReaperWontCover(ctx, awsClient, config.Region, os.Stderr)

	if launchDryRun {
		return runLaunchDryRun(ctx, os.Stdout, awsClient, config)
	}

	// --estimate-only: a true dry-run. Run the same pre-flight instance-type
	// constraint validation a real launch does (#110), BEFORE the cost estimate,
	// so a config that couldn't launch (e.g. --efa on a non-EFA type) surfaces the
	// same actionable error instead of a misleading "estimate complete" (#124).
	if estimateOnly {
		if err := preflightInstanceConstraints(ctx, awsClient, config, mpiEnabled, efaEnabled, config.Hibernate); err != nil {
			return err
		}
		// The same refusal a real launch gives (#1058); an estimate must not report
		// "complete" for a launch that will be rejected.
		if err := aws.CheckCostLimitEnforceable(*config, isWindowsTarget(estimateTargetOS(ctx, awsClient, config))); err != nil {
			return err
		}
		// Priced by truffle, the suite's pricing authority (#533) — never by
		// libs/pricing's static per-family-size guess table, which is wrong by up
		// to 38x for a type or size the table doesn't know (#543). A quote that
		// doesn't say whether it's real or fabricated can't be audited, so a
		// pricing miss is reported plainly rather than silently substituted.
		dp, err := aws.ResolveDisplayPrice(ctx, awsClient.Config(), config.InstanceType, config.Region)
		if err != nil {
			fmt.Fprintf(os.Stderr, "💰 Cost estimate: %v\n", err)
		} else {
			// count, not 1 (#662): --count instances are launched, so quoting one
			// understates the ceiling by exactly that factor on the flag whose
			// whole job is to bound spend.
			renderInstanceCostEstimate(os.Stderr, config.InstanceType, config.Region,
				estimateTargetOS(ctx, awsClient, config), dp.PricePerHour, dp.SourceLabel(), config.TTL, count)
		}
		// --fsx-create provisions the single most expensive thing a launch can
		// create (1200 GiB is the FSx Lustre minimum, ~$174/month) and the estimate
		// used to omit it entirely, so a preview that said "$0.80" was about to
		// create a filesystem two orders of magnitude more expensive (spawn#613).
		// Read the same flag values the real create path reads (fsxStorageCapacity/
		// fsxThroughput), so the preview can't quote a capacity the launch wouldn't use.
		if plan, ok := fsxCreatePlanFromFlags(config.FSxLustreCreate, fsxStorageCapacity, fsxThroughput, config.FSxLifecycle, config.FSxTTL, config.FSxMountPoint); ok {
			renderFSxCostEstimate(os.Stderr, plan)
		}
		// The cap belongs on this surface too (#811): --estimate-only is the other
		// "what would this cost me" answer, and the estimates above are compute and
		// storage quoted separately with no statement of what bounds them.
		if note := costLimitScopeNote(costLimit); note != "" {
			fmt.Fprintf(os.Stderr, "%s\n", note)
		}
		fmt.Fprintf(os.Stderr, "✅ Estimate complete — no instances launched (--estimate-only)\n")
		return nil
	}

	// Show what's about to be launched and its maximum cost, and on an
	// interactive terminal ask; --yes skips the question (#1115).
	if !confirmLaunchCost(ctx, awsClient, config) {
		// Not success: a caller must tell "you said no" from "it launched" (#737).
		return newAborted("launch cancelled")
	}

	// Launch with progress display
	return launchWithProgress(ctx, awsClient, config, plat, auditLog)
}

func launchWithProgress(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, plat *platform.Platform, auditLog *audit.AuditLogger) (err error) {
	// In JSON mode, suppress the TUI so stdout carries only the JSON object (#21).
	var prog *progress.Progress
	if getOutputFormat() == "json" {
		prog = progress.NewQuietProgress()
	} else {
		prog = progress.NewProgress()
	}

	// Once RunInstances has succeeded, any failure terminates the instance
	// (and the ephemeral FSx made for it) unless --keep-on-error (#1298).
	// Deferred here, above the launch, so every return after it is covered.
	var launchedID, launchedFSx string
	defer func() {
		err = finishLaunch(ctx, os.Stderr, awsClient, config.Region, launchedID, launchedFSx, keepOnError, err)
	}()

	// Step 0: pure flag validation, before anything is created (#685).
	//
	// These checks used to run ~286 lines further down, AFTER
	// ensureSecurityGroup. So `spawn launch x --mpi --count 2` with no
	// --job-array-name created a managed security group, then failed validation
	// and left the group behind — named `spawn-mpi-` from the empty suffix, which
	// every such launch would then share. Nothing reaps those (#685's other
	// half), so each attempt added a durable orphan.
	//
	// Validation that needs no AWS call belongs before the first AWS call. The
	// instance-type guards below genuinely need the AMI resolved first, which is
	// why they stay in step 1.
	if err := validateMPIFlags(mpiEnabled, count, jobArrayName); err != nil {
		return err
	}

	// Step 1: Detect AMI, resolve OS, and run pre-flight instance-type checks
	// before spending on any AWS resources.
	if err := ensureAMIAndPreflight(ctx, awsClient, config, prog); err != nil {
		return err
	}

	// Still before any AWS mutation: warn if the box is too small to be trusted
	// to enforce its own TTL (#682). A warning rather than a refusal — see
	// aws.SporedMemoryFloorMiB for why.
	warnIfBelowSporedMemoryFloor(ctx, awsClient, os.Stderr, config.Region, config.InstanceType, config.TTL)

	// Step 2: Setup SSH key
	prog.Start("Setting up SSH key")
	if config.KeyName == "" {
		keyName, err := setupSSHKey(ctx, awsClient, config.Region, config.AMI, plat)
		if err != nil {
			prog.Error("Setting up SSH key", err)
			return err
		}
		config.KeyName = keyName
	}
	prog.Complete("Setting up SSH key")

	// Step 3: Setup IAM instance profile
	if err := ensureIAMProfile(ctx, awsClient, config, prog, auditLog); err != nil {
		return err
	}

	// Step 4: Security group (create for MPI / Windows if needed)
	if err := ensureSecurityGroup(ctx, awsClient, config, prog, auditLog); err != nil {
		return err
	}

	// Step 4.5: Create or get FSx Lustre filesystem
	var fsxInfo *aws.FSxInfo

	// For an EPHEMERAL --fsx-create, the filesystem is created AFTER the launch
	// succeeds (#213) — a capacity-failed launch then never creates an FSx, so
	// there's no orphan and no create/teardown churn on retries (#210). We prepare
	// the resolved config here (validated up front) and carry it to the post-launch
	// step. Durable FSx is still created up front below (a deliberate persisted
	// resource). nil = no ephemeral FSx to create post-launch.
	var pendingFSxConfig *aws.FSxConfig
	var pendingFSxImportPath, pendingFSxExportPath, pendingFSxMountPoint string

	if fsxCreate {
		prog.Start("Creating FSx Lustre filesystem")

		// Generate stack name
		stackName := jobArrayName
		if stackName == "" {
			stackName = name
		}
		if stackName == "" {
			stackName = "fsx"
		}

		// Set import/export paths if not specified
		importPath := fsxImportPath
		if importPath == "" {
			importPath = fmt.Sprintf("s3://%s/", fsxS3Bucket)
		}

		exportPath := fsxExportPath
		if exportPath == "" {
			exportPath = fmt.Sprintf("s3://%s/", fsxS3Bucket)
		}

		throughput := fsxThroughput
		if throughput == 0 {
			throughput = 125 // PERSISTENT_2 minimum; valid values: 125, 250, 500, 1000
		}
		fsxConfig := aws.FSxConfig{
			StackName:                stackName,
			Region:                   config.Region,
			StorageCapacity:          fsxStorageCapacity,
			PerUnitStorageThroughput: throughput,
			S3Bucket:                 fsxS3Bucket,
			ImportPath:               importPath,
			ExportPath:               exportPath,
			AutoCreateBucket:         true,
			SecurityGroupIDs:         config.SecurityGroupIDs,
			Lifecycle:                fsxLifecycle,
		}
		// Co-locate the FSx with the instance's AZ (#194/#208). FSx Lustre is
		// single-AZ and a mounting instance MUST be in the same AZ as the
		// filesystem, so the FSx subnet has to match where the instance lands:
		//   1. An explicitly-pinned subnet wins.
		//   2. Else, if --az pinned the instance's AZ, resolve THAT AZ to a subnet
		//      — otherwise startFSxCreate falls back to subnets[0] of the default
		//      VPC, which silently ignores --az and can put the FSx in a different
		//      AZ than the instance (an unmountable cross-AZ FSx), and on accounts
		//      whose subnets[0] AZ doesn't offer PERSISTENT_2 makes every --az
		//      value fail identically with "not available in this availability
		//      zone" (#208).
		//   3. Else leave it empty: startFSxCreate's default-VPC fallback matches
		//      the instance's own unpinned placement.
		fsxConfig.SubnetID = config.SubnetID
		// So an explicit --vpc picks the FSx subnet from THAT VPC rather than the
		// default one, which would put the filesystem where the instance is not
		// and leave the mount unroutable (#673).
		fsxConfig.VPCID = config.VPCID
		if aws.NeedsAZSubnetResolution(fsxConfig.SubnetID, config.AvailabilityZone) {
			subnetID, serr := awsClient.GetSubnetForAZ(ctx, config.Region, config.AvailabilityZone, config.VPCID)
			if serr != nil {
				prog.Error("Creating FSx Lustre filesystem", serr)
				return fmt.Errorf("FSx create: could not find a subnet in --az %s to co-locate the filesystem with the instance: %w", config.AvailabilityZone, serr)
			}
			fsxConfig.SubnetID = subnetID
		}

		// Ensure Lustre ports on each SG used by both FSx and instances, up front
		// (needed by both the blocking and async paths). Without this the MGS
		// connection on 988 succeeds but follow-on dynamic-port traffic is blocked
		// ("client profile could not be read from MGS", #316).
		for _, sgID := range config.SecurityGroupIDs {
			if err := awsClient.EnsureLustrePorts(ctx, config.Region, sgID); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: could not ensure Lustre ports on %s: %v\n", sgID, err)
			}
		}

		if fsxLifecycle == "ephemeral" && count <= 1 {
			// Single-instance: DEFER creation until after the launch succeeds
			// (#213). Stash the resolved config and create the FSx post-launch, so a
			// capacity-failed launch never creates a filesystem (no orphan, no
			// per-retry churn — #210). The config is fully resolved/validated here;
			// only the CreateFileSystem call moves later. (Job arrays, count>1, keep
			// creating up front below — N instances share the one filesystem and the
			// array dispatch tags each via config.FSxPending, so it must exist before
			// dispatch.)
			cfgCopy := fsxConfig
			pendingFSxConfig = &cfgCopy
			pendingFSxImportPath = importPath
			pendingFSxExportPath = exportPath
			pendingFSxMountPoint = fsxMountPoint
			prog.Skip("Creating FSx Lustre filesystem (deferred until after launch)")
		} else if fsxLifecycle == "ephemeral" {
			// Job array (count>1): the shared ephemeral FSx must exist before the
			// array is dispatched (each instance is tagged spawn:fsx-pending from
			// config). Create it up front. (This path keeps the pre-#213 ordering;
			// the #210 reaper orphan-net is the backstop if the array dispatch fails.)
			prog.Start("Creating FSx Lustre filesystem (async)")
			fsID, cerr := awsClient.CreateFSxLustreFilesystemAsync(ctx, fsxConfig)
			if cerr != nil {
				prog.Error("Creating FSx Lustre filesystem (async)", cerr)
				return fmt.Errorf("failed to start FSx filesystem creation: %w", cerr)
			}
			config.FSxPending = fsID
			launchedFSx = fsID
			config.FSxImportPath = importPath
			config.FSxExportPath = exportPath
			if config.FSxMountPoint == "" {
				config.FSxMountPoint = fsxMountPoint
			}
			prog.Complete("Creating FSx Lustre filesystem (async)")
			fmt.Fprintf(os.Stderr, "   FSx %s is provisioning; array instances will mount it at %s once AVAILABLE (~10 min).\n", fsID, config.FSxMountPoint)
		} else {
			// durable: explicit death clock (#193), reaped on refcount-0 + TTL.
			if fsxLifecycle == "durable" && fsxTTL != "" {
				if d, derr := parseDuration(fsxTTL); derr == nil {
					fsxConfig.TTLDeadline = time.Now().Add(d)
				}
			}
			prog.Start("Creating FSx Lustre filesystem")
			fsxInfo, err = awsClient.CreateFSxLustreFilesystem(ctx, fsxConfig)
			if err != nil {
				prog.Error("Creating FSx Lustre filesystem", err)
				return fmt.Errorf("failed to create FSx filesystem: %w", err)
			}
			prog.Complete("Creating FSx Lustre filesystem")
		}

	} else if fsxID != "" && !fsxSkipValidate {
		prog.Start("Getting FSx filesystem info")

		fsxInfo, err = awsClient.GetFSxFilesystem(ctx, fsxID, config.Region)
		if err != nil {
			prog.Error("Getting FSx filesystem info", err)
			return fmt.Errorf("failed to get FSx info: %w", err)
		}

		prog.Complete("Getting FSx filesystem info")

	} else if fsxRecall != "" {
		prog.Start("Recalling FSx filesystem from S3")

		fsxInfo, err = awsClient.RecallFSxFilesystem(ctx, fsxRecall, config.Region)
		if err != nil {
			prog.Error("Recalling FSx filesystem", err)
			return fmt.Errorf("failed to recall FSx: %w", err)
		}

		prog.Complete("Recalling FSx filesystem from S3")
	} else {
		prog.Skip("FSx Lustre filesystem")
	}

	// Backfill FSx fields into config so buildTags writes spawn:fsx-id,
	// spawn:fsx-mount-name, and spawn:fsx-mount-point on every instance.
	// The --fsx-id path already sets config.FSxLustreID; the --fsx-create
	// and --fsx-recall paths only populated fsxInfo, not config (fixes #314).
	if fsxInfo != nil {
		config.FSxLustreID = fsxInfo.FileSystemID
		config.FSxMountName = fsxInfo.MountName
		if config.FSxMountPoint == "" {
			config.FSxMountPoint = fsxMountPoint
		}
	}

	// Step 4.6: Check data locality (region mismatches)
	if !skipRegionCheck && (efsID != "" || fsxID != "") {
		prog.Start("Checking data locality")

		fsxIDForCheck := ""
		if fsxID != "" {
			fsxIDForCheck = fsxID
		} else if fsxInfo != nil {
			fsxIDForCheck = fsxInfo.FileSystemID
		}

		awsCfg, err := awsClient.GetConfig(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to check data locality: %v\n", err)
			prog.Complete("Checking data locality")
		} else {
			warning, err := locality.CheckDataLocality(ctx, awsCfg, config.Region, efsID, fsxIDForCheck)
			if err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to check data locality: %v\n", err)
				prog.Complete("Checking data locality")
			} else if warning.HasMismatches {
				prog.Complete("Checking data locality")

				// Display warning
				fmt.Fprintf(os.Stderr, "%s", warning.FormatWarning())

				// Prompt for confirmation unless auto-approved
				if !autoYes {
					fmt.Fprintf(os.Stderr, "   Continue with cross-region launch? [y/N]: ")
					var response string
					_, _ = fmt.Scanln(&response)
					response = strings.ToLower(strings.TrimSpace(response))
					if response != "y" && response != "yes" {
						fmt.Fprintf(os.Stderr, "\n❌ Launch cancelled\n")
						return nil
					}
					fmt.Fprintf(os.Stderr, "\n")
				}
			} else {
				prog.Complete("Checking data locality")
			}
		}
	}

	// Build the storage-mount script (EFS / FSx / attached EBS volumes) FIRST, so
	// it can be injected BEFORE the user's script — the workload must see the
	// mounts already live; mounting after it finishes is useless (#166). Skipped
	// on Windows (EFS/FSx on Windows is out of Phase 1 scope, #55). Shared with
	// both sweep paths (#1366).
	storageScript, readyMountPoints, err := launchStorageScript(ctx, awsClient, config, fsxInfo, os.Stderr)
	if err != nil {
		return err
	}

	// Step 5: Build user data — the storage mount is injected before the user's
	// script (inside the bootstrap), not appended after it (#166).
	userDataScript, err := buildUserData(plat, config, storageScript, readyMountPoints)
	if err != nil {
		return fmt.Errorf("failed to build user data: %w", err)
	}

	config.UserData = encodeUserDataForOS(userDataScript, config.TargetOS)

	// Record the instance's primary user (Linux) so spored can run the pre-stop
	// hook as that user instead of root (#63). Windows has a single user and no
	// `su`, so it's left unset there.
	if config.TargetOS != "windows" {
		config.Username = plat.GetUsername()
	}

	// MPI flags are validated at the TOP of this function now (#685), before
	// anything is created. Placement resolution still happens here, after the
	// AMI and instance type are known.
	if mpiEnabled {
		if err := resolveMPIPlacement(ctx, awsClient, config); err != nil {
			return err
		}
	}

	// Check if job array mode (count > 1)
	if count > 1 {
		// Job array launch path
		if err := validateJobArrayFlags(count, jobArrayName); err != nil {
			return err
		}
		// MPI is all-or-nothing (a missing rank makes the cluster useless);
		// a plain array is independent work with a configurable --min-viable.
		if mpiEnabled {
			return launchMPICohort(ctx, awsClient, config, plat, prog, fsxInfo, auditLog)
		}
		return launchPlainArrayCohort(ctx, awsClient, config, plat, prog, fsxInfo, auditLog)
	}

	// Step 6: Launch instance
	prog.Start("Launching instance")
	auditLog.LogOperationWithData("launch_instance", "single", "initiated",
		map[string]interface{}{
			"instance_type": config.InstanceType,
			"region":        config.Region,
		}, nil)
	result, err := awsClient.Launch(ctx, *config)
	if err != nil {
		prog.Error("Launching instance", err)
		auditLog.LogOperationWithRegion("launch_instance", "single", config.Region, "failed", err)
		// No ephemeral-FSx teardown needed here (#213): the ephemeral FSx is now
		// created AFTER this launch succeeds (below), so a failed launch never
		// created one. A capacity failure leaves no billable resource by
		// construction — no orphan, no create/teardown churn on retries (#210).
		return err
	}
	launchedID = result.InstanceID
	auditLog.LogOperationWithData("launch_instance", result.InstanceID, "success",
		map[string]interface{}{
			"instance_type": config.InstanceType,
			"region":        config.Region,
		}, nil)
	prog.Complete("Launching instance")

	// Step 6.5: Now that the instance launched, create the ephemeral FSx and tag
	// the instance spawn:fsx-pending so spored waits → DRA → mounts once AVAILABLE
	// (#194). Created here (post-launch) so capacity failures cost nothing (#213).
	// If the create-or-tag fails, the FSx is torn down rather than leaked (#210
	// backstop); the reaper ephemeral-orphan net is the outer safety net.
	if pendingFSxConfig != nil {
		prog.Start("Creating FSx Lustre filesystem (async)")
		fsID, cerr := awsClient.CreateFSxLustreFilesystemAsync(ctx, *pendingFSxConfig)
		if cerr != nil {
			prog.Error("Creating FSx Lustre filesystem (async)", cerr)
			return fmt.Errorf("instance %s launched, but starting the ephemeral FSx failed: %w", result.InstanceID, cerr)
		}
		mountPoint := pendingFSxMountPoint
		if mountPoint == "" {
			mountPoint = "/fsx"
		}
		tags := map[string]string{
			"spawn:fsx-pending":     fsID,
			"spawn:fsx-mount-point": mountPoint,
		}
		if pendingFSxImportPath != "" {
			tags["spawn:fsx-s3-import-path"] = pendingFSxImportPath
		}
		if pendingFSxExportPath != "" {
			tags["spawn:fsx-s3-export-path"] = pendingFSxExportPath
		}
		if terr := awsClient.UpdateInstanceTags(ctx, config.Region, result.InstanceID, tags); terr != nil {
			// Tagging failed → spored will never see the pending FSx and the reaper
			// refcount will never count it, so delete it rather than leak it.
			if delErr := awsClient.DeleteFSxFilesystem(ctx, fsID, config.Region); delErr != nil {
				prog.Error("Creating FSx Lustre filesystem (async)", terr)
				return fmt.Errorf("instance %s launched but tagging it with pending FSx %s failed (%w) AND deleting the FSx failed — DELETE IT MANUALLY (aws fsx delete-file-system --file-system-id %s): %v", result.InstanceID, fsID, terr, fsID, delErr)
			}
			prog.Error("Creating FSx Lustre filesystem (async)", terr)
			return fmt.Errorf("instance %s launched but tagging it with pending FSx %s failed (deleted the FSx to avoid a leak): %w", result.InstanceID, fsID, terr)
		}
		config.FSxPending = fsID
		config.FSxImportPath = pendingFSxImportPath
		config.FSxExportPath = pendingFSxExportPath
		config.FSxMountPoint = mountPoint
		prog.Complete("Creating FSx Lustre filesystem (async)")
		fmt.Fprintf(os.Stderr, "   FSx %s is provisioning; the instance will mount it at %s once AVAILABLE (~10 min).\n", fsID, mountPoint)
	}

	// Write instance ID to file for workflow integration
	if err := writeOutputID(result.InstanceID, outputIDFile); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write instance ID to file: %v\n", err)
	}

	// Step 7: Wait for the instance to reach running. Use the SDK waiter
	// (returns as soon as it's running) rather than a fixed sleep. Gated by
	// --wait-for-running; the agent/user-data runs asynchronously on the
	// instance and is observed via spored, not by blocking here.
	prog.Start("Waiting for instance")
	if waitForRunning {
		if err := awsClient.WaitForRunning(ctx, config.Region, result.InstanceID, 5*time.Minute); err != nil {
			prog.Error("Waiting for instance", err)
			return err
		}
	}
	prog.Complete("Waiting for instance")

	// Step 9: Get public IP
	prog.Start("Getting public IP")
	publicIP, err := awsClient.GetInstancePublicIP(ctx, config.Region, result.InstanceID)
	if err != nil {
		prog.Error("Getting public IP", err)
		return err
	}
	result.PublicIP = publicIP
	prog.Complete("Getting public IP")

	// Steps 10 and 10b: wait for the instance to be usable, then verify spored
	// (#1296: SSH is waited for only when asked; spored is verified regardless).
	plan := resolvePostLaunchPlan(config.TargetOS, waitForSSHSet, waitForSSH)
	post := runPostLaunchChecks(ctx, os.Stderr, prog, clientPostLaunchSteps(awsClient, config.Region, result.InstanceID),
		plan, config.TargetOS, result.PublicIP, config.Name, result.InstanceID)
	if post.sporedErr != nil {
		return sporedVerificationError(post.sporedErr)
	}
	sporedVerified := post.sporedVerified

	// Step 11: Report DNS (#872).
	//
	// spored registers the record itself, at boot, from the spawn:dns-name tag —
	// signed with the instance role, and renewing the lease that keeps the record
	// alive (#875). The CLI used to register as well, by SSHing in and running an
	// unsigned curl against an API Gateway that no longer exists; it failed on
	// every launch and blamed the user's account for it. spored's attempt was the
	// one that worked all along, so the launch now reports what spored did.
	//
	// The tags are read only once spored has been verified up; before that their
	// absence means nothing, and says so.
	var dnsRecord string
	if config.DNSName != "" {
		var tags map[string]string
		if sporedVerified {
			tags, _ = awsClient.GetInstanceTags(ctx, config.Region, result.InstanceID)
		}
		fqdn := ""
		if acct, aerr := awsClient.GetAccountID(ctx); aerr == nil {
			fqdn = dns.GetFullDNSName(config.DNSName, acct, launchDNSDomain(config))
		}
		var notice string
		dnsRecord, notice = sporedDNSReport(tags, config.DNSName, fqdn, config.Name)
		fmt.Fprint(os.Stderr, notice)
	}

	// Output: JSON array or TUI depending on --output flag
	if getOutputFormat() == "json" {
		out := []map[string]interface{}{
			{
				"instance_id":          result.InstanceID,
				"name":                 config.Name,
				"instance_type":        config.InstanceType,
				"region":               config.Region,
				"public_ip":            result.PublicIP,
				"state":                "running",
				"dns":                  dnsRecord,
				"iam_instance_profile": config.IamInstanceProfile,
				"spored_verified":      sporedVerified,
			},
		}
		jsonBytes, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	// Display success (TUI mode). Prefer `spawn connect <name>`: it resolves the
	// instance's actual launch key (a raw `ssh -i ~/.ssh/id_rsa …` hint fails
	// when the launch used a different key), picks the right user, and falls back
	// to Session Manager. Windows additionally supports --rdp / --ssh.
	var connectCmd string
	if config.TargetOS == "windows" {
		connectCmd = fmt.Sprintf("spawn connect %s --rdp     # graphical; or --ssh, or `spawn connect %s` (PowerShell over SSM)", config.Name, config.Name)
	} else {
		connectCmd = fmt.Sprintf("spawn connect %s", config.Name)
	}
	summary := progress.SuccessSummary{
		InstanceID:     result.InstanceID,
		PublicIP:       result.PublicIP,
		ConnectCommand: connectCmd,
		// The RESOLVED IAM instance profile (#550): previously visible nowhere
		// except ec2:DescribeInstances, so a launch that silently attached the
		// wrong (data-inaccessible) profile was undiagnosable without reading
		// EC2 state out-of-band. `spawn list` and `spawn status` show the same
		// field for an already-launched instance. It sits in the instance
		// details now; printed after the summary it opened a section nothing
		// closed (#1125).
		InstanceProfile: config.IamInstanceProfile,
		DNSRecord:       dnsRecord,
	}
	if dnsRecord != "" {
		switch {
		case config.TargetOS == "windows":
			summary.DNSConnect = fmt.Sprintf("spawn connect %s --rdp", config.Name)
		case post.sshClosed:
			// The groups admit no SSH, so an `ssh` line would only time out (#1296).
		default:
			summary.DNSConnect = fmt.Sprintf("ssh %s@%s", plat.GetUsername(), dnsRecord)
		}
	}
	prog.DisplaySuccess(summary, config)

	return nil
}

// launchDNSDomain is the zone a launched spore registers under: its custom
// target (#891) when it has one, else spore.host — the same choice spored makes
// from the spawn:dns-domain tag.
func launchDNSDomain(config *aws.LaunchConfig) string {
	if config.DNSDomain != "" {
		return config.DNSDomain
	}
	return dns.DefaultDomain
}

// sporedDNSReport turns spored's own DNS tags into what the launch tells the user
// (#551, #872): the record, when spored registered it, and otherwise a notice
// saying what happened or that it has not happened yet. fqdn may be "" when the
// account could not be resolved; the notices then name the short name instead.
func sporedDNSReport(tags map[string]string, dnsName, fqdn, instanceName string) (record, notice string) {
	shown := fqdn
	if shown == "" {
		shown = dnsName
	}
	switch tags["spawn:dns-status"] {
	case "registered":
		if fqdn == "" {
			return "", fmt.Sprintf("ℹ️  spored registered DNS for %q; `spawn status %s` shows the record.\n", dnsName, instanceName)
		}
		return fqdn, ""
	case "failed":
		detail := tags["spawn:dns-error"]
		if detail == "" {
			detail = "no detail reported"
		}
		return "", fmt.Sprintf("\n%s spored could not register DNS for %q (non-fatal — use the public IP or `spawn connect %s`): %s\n",
			i18n.Symbol("warning"), dnsName, instanceName, detail)
	}
	return "", fmt.Sprintf("ℹ️  DNS: spored registers %s once it is running; `spawn status %s` shows the result.\n",
		shown, instanceName)
}

// ensureIAMProfile sets config.IamInstanceProfile if unset: either the
// user-specified IAM configuration (--iam-role/--iam-policy/…) or the default
// spored role. Extracted from launchWithProgress (#319); behavior unchanged.
func ensureIAMProfile(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, prog *progress.Progress, auditLog *audit.AuditLogger) error {
	prog.Start("Setting up IAM role")
	if config.IamInstanceProfile == "" {
		// --instance-profile bypasses ALL resolution below — no create/reuse
		// heuristic, no AWS IAM call at all. It takes precedence even over
		// --iam-role/--iam-policy/etc. so the caller's explicit choice can never
		// be second-guessed by the hash/name matching those flags would otherwise
		// trigger (#550).
		if instanceProfile != "" {
			config.IamInstanceProfile = instanceProfile
			auditLog.LogOperation("create_iam_role", instanceProfile, "success", nil)
			warnUnmanagedProfileDNSGrant(os.Stderr, config)
			prog.Complete("Setting up IAM role")
			return nil
		}
		// Nested spawn (#39): on a nested spore no IAM is written at all; with
		// --nested-spawn the opt-in role is created or reused. Neither touches
		// the default branch below.
		if handled, err := ensureNestedSpawnProfile(ctx, awsClient, config); handled || err != nil {
			if err != nil {
				prog.Error("Setting up IAM role", err)
				return err
			}
			auditLog.LogOperation("create_iam_role", config.IamInstanceProfile, "success", nil)
			prog.Complete("Setting up IAM role")
			return nil
		}
		// Check if user specified custom IAM configuration.
		//
		// --s3-read/--s3-write count as custom IAM config (#614): they must route
		// through CreateOrGetInstanceProfile, because the SetupSporedIAMRole default
		// below attaches a FIXED policy whose S3 grants cover only spawn's own
		// infrastructure buckets — which is exactly why a --command reading the
		// caller's own bucket returned 403.
		// Shared with the sweep path (#539/#614): duplicating this condition is
		// how --s3-read/--s3-write came to be honoured here and silently ignored
		// on sweeps.
		if cliIAMFlagsRequireCustomProfile() {
			// Reject wildcard *:FullAccess templates unless explicitly opted in
			// (2026-06 audit, M-sec). Fail before any AWS call.
			if err := aws.ValidatePolicyNames(iamPolicy, iamAllowFullAccess); err != nil {
				prog.Error("Setting up IAM role", err)
				return err
			}
			// User-specified IAM configuration
			iamConfig := aws.IAMRoleConfig{
				RoleName:        iamRole,
				Policies:        iamPolicy,
				ManagedPolicies: iamManagedPolicies,
				PolicyFile:      iamPolicyFile,
				TrustServices:   iamTrustServices,
				Tags:            parseIAMRoleTags(iamRoleTags),
			}

			// --s3-read/--s3-write become a scoped bucket policy built by the SAME
			// function the task path uses (#614), so launch and task cannot drift in
			// what "read this bucket" means. It lands on the role as its own
			// "spawn-scoped-policy" inline document, so it is additive with
			// --iam-policy / --iam-policy-file rather than replacing them.
			if len(s3ReadBuckets) > 0 || len(s3WriteBuckets) > 0 {
				if err := validateS3BucketFlags(s3ReadBuckets, s3WriteBuckets); err != nil {
					prog.Error("Setting up IAM role", err)
					return err
				}
				iamConfig.InlinePolicyJSON = taskStagingPolicy(s3ReadBuckets, s3WriteBuckets, "", nil)
			}

			instanceProfile, err := awsClient.CreateOrGetInstanceProfile(ctx, iamConfig)
			if err != nil {
				prog.Error("Setting up IAM role", err)
				auditLog.LogOperation("create_iam_role", iamConfig.RoleName, "failed", err)
				return fmt.Errorf("failed to create IAM instance profile: %w", err)
			}
			// CreateOrGetInstanceProfile names the profile after its role.
			if err := ensureDNSInvokeGrant(ctx, awsClient, config, instanceProfile); err != nil {
				prog.Error("Setting up IAM role", err)
				return err
			}
			config.IamInstanceProfile = instanceProfile
			auditLog.LogOperationWithData("create_iam_role", iamConfig.RoleName, "success",
				map[string]interface{}{
					"instance_profile": instanceProfile,
				}, nil)
		} else {
			// Default: use spored IAM role
			instanceProfile, err := awsClient.SetupSporedIAMRole(ctx)
			if err != nil {
				prog.Error("Setting up IAM role", err)
				auditLog.LogOperation("create_iam_role", "spored-instance-role", "failed", err)
				return err
			}
			if err := ensureDNSInvokeGrant(ctx, awsClient, config, aws.SporedRoleName); err != nil {
				prog.Error("Setting up IAM role", err)
				return err
			}
			config.IamInstanceProfile = instanceProfile
			auditLog.LogOperation("create_iam_role", "spored-instance-role", "success", nil)
		}
	} else {
		warnUnmanagedProfileDNSGrant(os.Stderr, config)
	}
	prog.Complete("Setting up IAM role")
	return nil
}

// ensureDNSInvokeGrant gives roleName lambda:InvokeFunctionUrl on a custom DNS
// updater's function (#1158), so spored can register through it. A no-op for
// spore.host's updater, which every spored baseline policy already grants.
func ensureDNSInvokeGrant(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, roleName string) error {
	if config.DNSFunctionARN == "" {
		return nil
	}
	return awsClient.EnsureDNSInvokeGrant(ctx, roleName, config.DNSFunctionARN)
}

// warnUnmanagedProfileDNSGrant: spawn does not edit a role it did not create, so
// with a custom DNS updater the caller's own profile must carry the grant.
func warnUnmanagedProfileDNSGrant(w io.Writer, config *aws.LaunchConfig) {
	if config.DNSEndpoint == "" || isDefaultDNSEndpoint(config.DNSEndpoint) {
		return
	}
	fn := config.DNSFunctionARN
	if fn == "" {
		fn = "the updater's function"
	}
	_, _ = fmt.Fprintf(w, "%s instance profile %s is not managed by spawn: it must grant lambda:InvokeFunctionUrl on %s, or spored cannot register DNS through %s\n",
		i18n.Symbol("warning"), config.IamInstanceProfile, fn, config.DNSEndpoint)
}

// mergeManagedSecurityGroup adds a spawn-managed security group alongside any
// the caller supplied with --security-group-ids, instead of replacing them.
//
// Overwriting was the second half of #667: --security-group-ids was dropped
// before it reached the config, and then dropped AGAIN here whenever spawn
// created a managed MPI or Windows group. An instance can carry up to 5 groups,
// so there is no reason to choose — and no other way to add one to an MPI
// launch, which is exactly the case (EFS/FSx mount targets, a restricted SSH
// CIDR) where an extra group is needed.
func mergeManagedSecurityGroup(existing []string, managed string) []string {
	for _, sg := range existing {
		if sg == managed {
			return existing
		}
	}
	return append(append([]string{}, existing...), managed)
}

// ensureSecurityGroup creates and assigns a managed security group when the
// launch needs one the default SG can't provide: an MPI SG (intra-cluster
// ports) or a Windows SG (RDP 3389 + SSH 22), plus the --efa-group SG when one
// is named. Otherwise it's a no-op. Extracted
// from launchWithProgress (#319); behavior unchanged.
func ensureSecurityGroup(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, prog *progress.Progress, auditLog *audit.AuditLogger) error {
	if mpiEnabled {
		prog.Start("Creating MPI security group")
		// Get default VPC
		// Honour --vpc (#673); falls back to the default VPC when unset.
		vpcID, err := awsClient.ResolveVPC(ctx, config.Region, config.VPCID)
		if err != nil {
			prog.Error("Creating MPI security group", err)
			return err
		}

		// Create or get MPI security group
		sgName := fmt.Sprintf("spawn-mpi-%s", jobArrayName)
		sgID, err := awsClient.CreateOrGetMPISecurityGroup(ctx, config.Region, vpcID, sgName)
		if err != nil {
			prog.Error("Creating MPI security group", err)
			auditLog.LogOperationWithRegion("create_security_group", sgName, config.Region, "failed", err)
			return fmt.Errorf("failed to create MPI security group: %w", err)
		}

		config.SecurityGroupIDs = mergeManagedSecurityGroup(config.SecurityGroupIDs, sgID)
		auditLog.LogOperationWithData("create_security_group", sgName, "success",
			map[string]interface{}{
				"security_group_id": sgID,
				"region":            config.Region,
			}, nil)
		prog.Complete("Creating MPI security group")
	} else if config.TargetOS == "windows" && len(config.SecurityGroupIDs) == 0 {
		// Windows needs RDP (3389) + SSH (22); the default SG won't open 3389, so
		// RDP would be impossible (#95). Create a managed Windows SG.
		prog.Start("Creating Windows security group")
		// Honour --vpc (#673); falls back to the default VPC when unset.
		vpcID, err := awsClient.ResolveVPC(ctx, config.Region, config.VPCID)
		if err != nil {
			prog.Error("Creating Windows security group", err)
			return err
		}
		if allowCIDR == "" || allowCIDR == "0.0.0.0/0" {
			fmt.Fprintf(os.Stderr, "⚠️  Opening RDP (3389) + SSH (22) to 0.0.0.0/0; restrict with --allow-cidr <your-ip>/32.\n")
		}
		sgName := fmt.Sprintf("spawn-windows-%s", config.Name)
		sgID, err := awsClient.CreateOrGetWindowsSecurityGroup(ctx, config.Region, vpcID, sgName, allowCIDR)
		if err != nil {
			prog.Error("Creating Windows security group", err)
			auditLog.LogOperationWithRegion("create_security_group", sgName, config.Region, "failed", err)
			return fmt.Errorf("failed to create Windows security group: %w", err)
		}
		config.SecurityGroupIDs = []string{sgID}
		auditLog.LogOperationWithData("create_security_group", sgName, "success",
			map[string]interface{}{"security_group_id": sgID, "region": config.Region}, nil)
		prog.Complete("Creating Windows security group")
	} else {
		prog.Skip("Creating security group")
	}
	// --efa-group (#1168): after the branches above, so it is added to whatever
	// group they chose rather than standing in for it.
	return ensureEFAGroup(ctx, awsClient, config, efaGroup, prog, auditLog)
}

// ensureAMIAndPreflight detects the AMI (when unset/"auto"), resolves the target
// OS, and runs the pre-flight instance-type guards (Windows lifecycle/burstable,
// nested-virtualization, and MPI/EFA/hibernation feature constraints) before any
// billable resource is created. Extracted from launchWithProgress (#319);
// behavior unchanged.
func ensureAMIAndPreflight(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, prog *progress.Progress) error {
	// Step 0a: --vpc --az with no subnet launches into a subnet of that VPC
	// and AZ, chosen and printed here (#1389), before anything is created.
	if err := resolveVPCSubnet(ctx, awsClient, config, os.Stderr); err != nil {
		return err
	}
	// Step 0: --vpc and --subnet-id must agree (#673). Passing both is the
	// natural way to use --vpc, and EC2's own error for a mismatch arrives only
	// at RunInstances and names neither flag. Checked before anything is created.
	if err := awsClient.ValidateSubnetInVPC(ctx, config.Region, config.SubnetID, config.VPCID); err != nil {
		return err
	}

	// Step 1: Detect AMI
	prog.Start("Detecting AMI")
	// "" or "auto" both mean auto-detect the latest AL2023 AMI (#342).
	if isAutoAMI(config.AMI) {
		// Auto-detect only ever finds a LINUX image. With --os windows it used to
		// proceed anyway: the instance was labelled Windows everywhere — preview,
		// spored check, Windows guards — and booted Amazon Linux (#884).
		if err := guardWindowsNeedsAMI(osFlag); err != nil {
			prog.Error("Detecting AMI", err)
			return err
		}
		ami, err := awsClient.GetRecommendedAMI(ctx, config.Region, config.InstanceType)
		if err != nil {
			prog.Error("Detecting AMI", err)
			return err
		}
		config.AMI = ami
	}
	prog.Complete("Detecting AMI")

	// Step 1b: Resolve target OS (--os override, else auto-detect from the AMI)
	// and enforce the Windows lifecycle guard before we spend on a launch.
	config.TargetOS = resolveTargetOS(ctx, awsClient, config.Region, config.AMI, osFlag)
	if err := windowsLifecycleGuard(config); err != nil {
		return err
	}
	decls, err := collectPluginDeclarations()
	if err != nil {
		return err
	}
	if err := windowsPluginGuard(config.TargetOS, decls); err != nil {
		return err
	}
	// Reject burstable types for Windows (catches auto-detected Windows where the
	// early --os default didn't apply); spend nothing on a launch that'd crawl (#95).
	if err := guardWindowsInstanceType(config.TargetOS, config.InstanceType); err != nil {
		return err
	}
	// The custom DNS target spored registers with (#891), validated before any
	// resource is created.
	if err := resolveLaunchDNSTarget(ctx, awsClient, config); err != nil {
		return err
	}

	// A --cost-limit spored would enforce at the Linux rate (#1058). Launch refuses
	// it too, but only after IAM, security group, key and any FSx are created.
	if err := aws.CheckCostLimitEnforceable(*config, isWindowsTarget(config.TargetOS)); err != nil {
		return err
	}

	// Reject --nested-virtualization on an instance type that can't do it, before
	// spending on a launch RunInstances would reject cryptically (#91).
	if config.NestedVirtualization {
		if err := awsClient.ValidateInstanceTypeForNestedVirtualization(ctx, config.InstanceType, config.Region); err != nil {
			return err
		}
	}

	// Pre-flight: validate instance-type feature constraints (MPI placement group,
	// EFA, hibernation) BEFORE creating any AWS resources (IAM role, security
	// group), so an unsupported combination fails fast with an actionable message
	// instead of cryptically after several API calls (#110).
	if err := preflightInstanceConstraints(ctx, awsClient, config, mpiEnabled, efaEnabled, config.Hibernate); err != nil {
		return err
	}
	// --efa: how many EFA interfaces (#661). Before any resource is created, so
	// --efa-interfaces above what the type supports fails here.
	if _, err := resolveEFAInterfaces(ctx, awsClient, config); err != nil {
		return err
	}
	// More than one of them needs one subnet (#1410): picked in --az, or the
	// launch refused here, rather than at RunInstances after the key pair and
	// the spored role exist.
	if err := resolveEFASubnet(ctx, awsClient, config, cohortHasAZChain(config, count), os.Stderr); err != nil {
		return err
	}
	// More than one EFA interface means an Elastic IP per instance (#661):
	// check the region has that many left before anything is created.
	if err := preflightElasticIPQuota(ctx, awsClient, config, count, os.Stderr); err != nil {
		return err
	}
	return nil
}

// elasticIPQuotaFor reads the region's Elastic IP usage and quota (read-only).
// A package-level seam so tests need no AWS.
var elasticIPQuotaFor = func(ctx context.Context, awsClient *aws.Client, region string) (aws.ElasticIPQuota, error) {
	return awsClient.ElasticIPQuotaInRegion(ctx, region)
}

// preflightElasticIPQuota refuses a multi-interface EFA launch that would run
// out of Elastic IPs, saying how many are in use of how many and that
// --efa-interfaces 1 needs none. Each instance of the launch (`--count`) needs
// one. When the quota cannot be read (no servicequotas:GetServiceQuota, say) it
// warns and lets the launch go on: Launch still terminates an instance whose
// AllocateAddress fails, so the check is an early answer, not the only guard.
func preflightElasticIPQuota(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, instances int, warn io.Writer) error {
	if !config.EFAEnabled || config.EFAInterfaces <= 1 {
		return nil
	}
	need := instances
	if need < 1 {
		need = 1
	}
	q, err := elasticIPQuotaFor(ctx, awsClient, config.Region)
	if err != nil {
		fmt.Fprintf(warn, "⚠️  could not check the Elastic IP quota in %s (%v); a launch past it fails at AllocateAddress and terminates the instance\n", config.Region, err)
		return nil
	}
	return aws.CheckElasticIPHeadroom(q, config.Region, need)
}

// efaCapacityFor reads an instance type's EFA capacity (DescribeInstanceTypes,
// read-only). A package-level seam so tests need no AWS.
var efaCapacityFor = func(ctx context.Context, awsClient *aws.Client, instanceType, region string) (aws.EFACapacity, error) {
	return awsClient.EFACapacityInRegion(ctx, instanceType, region)
}

// resolveEFAInterfaces settles config.EFAInterfaces for an --efa launch (#661):
// --efa-interfaces 0 (the default) becomes the type's MaximumEfaInterfaces, one
// per network card; a count above it is an error. It runs on every --efa launch,
// --mpi or not — the non-MPI path used to reach RunInstances having checked EFA
// only through truffle's capability flag, which carries no count.
func resolveEFAInterfaces(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig) (aws.EFACapacity, error) {
	return resolveEFAInterfacesWith(config, func(instanceType, region string) (aws.EFACapacity, error) {
		return efaCapacityFor(ctx, awsClient, instanceType, region)
	})
}

// efaCapacityLookup reads an instance type's EFA capacity in a region.
type efaCapacityLookup func(instanceType, region string) (aws.EFACapacity, error)

// resolveEFAInterfacesWith is resolveEFAInterfaces with the capacity read
// through lookup: a sweep resolves every member through one memoized lookup
// (#1407).
func resolveEFAInterfacesWith(config *aws.LaunchConfig, lookup efaCapacityLookup) (aws.EFACapacity, error) {
	if !config.EFAEnabled {
		return aws.EFACapacity{}, nil
	}
	capacity, err := lookup(config.InstanceType, config.Region)
	if err != nil {
		return aws.EFACapacity{}, fmt.Errorf("EFA validation: %w", err)
	}
	n, err := capacity.ResolveInterfaces(config.EFAInterfaces)
	if err != nil {
		return aws.EFACapacity{}, err
	}
	config.EFAInterfaces = n
	// --efa-secondary-type efa on a type with one EFA card: nothing to apply
	// it to (#1404).
	if err := config.ValidateEFASecondaryType(); err != nil {
		return aws.EFACapacity{}, err
	}
	return capacity, nil
}

// validateEFASecondaryTypeFlag is the check --efa-secondary-type needs no AWS
// call for (#1404): a known value, and for efa, --efa and an
// --efa-interfaces other than 1. --efa alone (--efa-interfaces 0, every card)
// counts as multi-card here; resolveEFAInterfaces refuses it if the type turns
// out to have one EFA card.
func validateEFASecondaryTypeFlag(value string, efa bool, interfaces int) error {
	cfg := aws.LaunchConfig{EFASecondaryType: value, EFAEnabled: efa, EFAInterfaces: interfaces}
	if interfaces == 0 {
		cfg.EFAInterfaces = 2 // "every card": settled by resolveEFAInterfaces
	}
	return cfg.ValidateEFASecondaryType()
}

// sweepBaseConfig is the config every member of a parameter sweep starts from,
// foreground or detached: the one a single launch would build, with --tag and
// --team merged in (#1356). Each param row overrides it.
func sweepBaseConfig(ctx context.Context) (*aws.LaunchConfig, error) {
	config, err := buildLaunchConfig(nil)
	if err != nil {
		return nil, err
	}
	// Keep the two fields the old dispatch set explicitly. Region may have
	// been resolved from config/defaults rather than the flag, and
	// InstanceType is legitimately empty on a sweep whose rows set it.
	if config.Region == "" {
		config.Region = region
	}
	if config.InstanceType == "" {
		config.InstanceType = instanceType
	}
	if err := applyLaunchFlagTags(ctx, config); err != nil {
		return nil, err
	}
	return config, nil
}
