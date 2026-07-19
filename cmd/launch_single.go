package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/libs/pricing"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/compliance"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/input"
	"github.com/spore-host/spawn/pkg/locality"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/userdata"
	"github.com/spore-host/spawn/pkg/wizard"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

func runLaunch(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

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
	auditLog := audit.NewLogger(os.Stderr, userID, correlationID)

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

	if batchQueueFile != "" || queueTemplate != "" {
		return launchWithBatchQueue(ctx, plat, auditLog)
	}

	// Check for parameter sweep mode (before wizard/config logic)
	if paramFile != "" || params != "" {
		// Parameter sweep launch path - config will be built inside launchParameterSweep
		// Create minimal config for sweep orchestration
		config := &aws.LaunchConfig{
			Region:       region,
			InstanceType: instanceType, // May be empty, that's ok for sweeps
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

	// Apply custom --tag key=value tags to the instance + its created volumes,
	// so ephemeral spores and their --attach-volume data volumes are attributable
	// in Cost Explorer / cleanup scripts (#161). Set first so spawn-managed tags
	// (team, strata, the buildTags baseline) take precedence over user tags.
	if len(launchTags) > 0 {
		userTags, err := parseKVTags(launchTags)
		if err != nil {
			return err
		}
		if config.Tags == nil {
			config.Tags = make(map[string]string)
		}
		for k, v := range userTags {
			config.Tags[k] = v
		}
	}

	// Apply team tags if --team specified
	if launchTeamID != "" {
		if config.Tags == nil {
			config.Tags = make(map[string]string)
		}
		config.Tags["spawn:team-id"] = launchTeamID
		// Resolve team name from DynamoDB for the human-readable tag
		if teamName, err := resolveTeamName(ctx, launchTeamID); err == nil && teamName != "" {
			config.Tags["spawn:team-name"] = teamName
		}
	}

	// Strata software environment selection
	if strataFormation != "" || strataProfile != "" {
		fmt.Fprintf(os.Stderr, "Resolving Strata environment...\n")
		uri, err := resolveStrataEnvironment(ctx, strataFormation, strataProfile, strataRegistry)
		if err != nil {
			return fmt.Errorf("strata: %w", err)
		}
		config.Tags["strata:lockfile-s3-uri"] = uri
		fmt.Fprintf(os.Stderr, "Strata environment resolved: %s\n", uri)
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

	// Auto-detect region if not specified
	if config.Region == "" {
		fmt.Fprintf(os.Stderr, "🌍 No region specified, auto-detecting closest region...\n")
		detectedRegion, err := detectBestRegion(ctx, config.InstanceType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not auto-detect region: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Using default: us-east-1\n")
			config.Region = "us-east-1"
		} else {
			fmt.Fprintf(os.Stderr, "✓ Selected region: %s\n", detectedRegion)
			config.Region = detectedRegion
		}
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

	// --estimate-only: a true dry-run. Run the same pre-flight instance-type
	// constraint validation a real launch does (#110), BEFORE the cost estimate,
	// so a config that couldn't launch (e.g. --efa on a non-EFA type) surfaces the
	// same actionable error instead of a misleading "estimate complete" (#124).
	if estimateOnly {
		if err := preflightInstanceConstraints(ctx, awsClient, config, mpiEnabled, efaEnabled, hibernate || config.HibernateOnIdle); err != nil {
			return err
		}
		odPrice := pricing.GetEC2HourlyRate(config.Region, config.InstanceType)
		if odPrice == 0 {
			fmt.Fprintf(os.Stderr, "💰 Cost estimate: pricing data unavailable for %s in %s\n", config.InstanceType, config.Region)
		} else {
			fmt.Fprintf(os.Stderr, "💰 Cost estimate for %s in %s\n", config.InstanceType, config.Region)
			fmt.Fprintf(os.Stderr, "   On-demand:  $%.4f/hr\n", odPrice)
			if config.TTL != "" {
				if d, err := time.ParseDuration(config.TTL); err == nil {
					fmt.Fprintf(os.Stderr, "   TTL cost:   $%.2f (%.0f hr)\n", odPrice*d.Hours(), d.Hours())
				}
			}
		}
		fmt.Fprintf(os.Stderr, "✅ Estimate complete — no instances launched (--estimate-only)\n")
		return nil
	}

	// Launch with progress display
	return launchWithProgress(ctx, awsClient, config, plat, auditLog)
}

func launchWithProgress(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, plat *platform.Platform, auditLog *audit.AuditLogger) error {
	// In JSON mode, suppress the TUI so stdout carries only the JSON object (#21).
	var prog *progress.Progress
	if getOutputFormat() == "json" {
		prog = progress.NewQuietProgress()
	} else {
		prog = progress.NewProgress()
	}

	// Step 1: Detect AMI, resolve OS, and run pre-flight instance-type checks
	// before spending on any AWS resources.
	if err := ensureAMIAndPreflight(ctx, awsClient, config, prog); err != nil {
		return err
	}

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
	var err error

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
		if aws.NeedsAZSubnetResolution(fsxConfig.SubnetID, config.AvailabilityZone) {
			subnetID, serr := awsClient.GetSubnetForAZ(ctx, config.Region, config.AvailabilityZone)
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
	// mounts already live; mounting after it finishes is useless (#166). Skip on
	// Windows: the storage user-data is Linux mount scripting and would corrupt
	// the <powershell> block. EFS/FSx on Windows is out of Phase 1 scope (#55).
	storageScript := ""
	if (efsID != "" || fsxInfo != nil || len(config.AttachVolumes) > 0) && config.TargetOS != "windows" {
		storageConfig := userdata.StorageConfig{}

		// EFS configuration
		if efsID != "" {
			mountOptions, err := getEFSMountOptions()
			if err != nil {
				return fmt.Errorf("failed to get EFS mount options: %w", err)
			}

			storageConfig.EFSEnabled = true
			storageConfig.EFSFilesystemDNS = aws.GetEFSDNSName(efsID, config.Region)
			storageConfig.EFSMountPoint = efsMountPoint
			storageConfig.EFSMountOptions = mountOptions
		}

		// FSx configuration
		if fsxInfo != nil {
			storageConfig.FSxLustreEnabled = true
			storageConfig.FSxFilesystemDNS = fsxInfo.DNSName
			storageConfig.FSxMountName = fsxInfo.MountName
			storageConfig.FSxMountPoint = fsxMountPoint
		}

		// Attached EBS data volumes from snapshots (#144)
		storageConfig.AttachedVolumes = attachedVolumesUserData(config.AttachVolumes)

		storageScript, err = userdata.GenerateStorageUserData(storageConfig)
		if err != nil {
			return fmt.Errorf("failed to generate storage user-data: %w", err)
		}
	}

	// Step 5: Build user data — the storage mount is injected before the user's
	// script (inside the bootstrap), not appended after it (#166).
	userDataScript, err := buildUserData(plat, config, storageScript)
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

	// Validate MPI requirements
	if mpiEnabled {
		if count <= 1 {
			return fmt.Errorf("--mpi requires --count > 1 (need multiple nodes)")
		}
		if jobArrayName == "" {
			return fmt.Errorf("--mpi requires --job-array-name")
		}

		// Decide on a cluster placement group. HPC instance types (hpc6a/hpc7a/
		// hpc7g) don't support cluster placement groups — they get low-latency
		// networking from AWS HPC infrastructure — so --auto-placement-group
		// (on by default) must SKIP them gracefully rather than hard-fail (#104).
		// An explicitly requested --placement-group still errors if unsupported.
		tc := truffleaws.NewClientFromConfig(awsClient.Config())
		clusterPG := func() (bool, error) {
			caps, err := tc.GetCapabilities(ctx, config.InstanceType, config.Region)
			if err != nil {
				return false, err
			}
			return caps.ClusterPlacement, nil
		}
		if mpiPlacementGroup != "" {
			// Explicit request: must be supported (authoritative capability check).
			supported, err := clusterPG()
			if err != nil {
				return fmt.Errorf("placement group validation: %w", err)
			}
			if !supported {
				return fmt.Errorf("instance type %s does not support cluster placement groups (required for --placement-group)", config.InstanceType)
			}
		} else if mpiAutoPlacementGroup {
			supported, err := clusterPG()
			if err != nil {
				return fmt.Errorf("placement group validation: %w", err)
			}
			if supported {
				mpiPlacementGroup = fmt.Sprintf("spawn-mpi-%s", jobArrayName)
				fmt.Fprintf(os.Stderr, "Creating placement group: %s\n", mpiPlacementGroup)
				if err := awsClient.CreatePlacementGroup(ctx, mpiPlacementGroup, config.Region); err != nil {
					return fmt.Errorf("create placement group: %w", err)
				}
			} else {
				fmt.Fprintf(os.Stderr, "ℹ️  %s doesn't support cluster placement groups (HPC instance types use AWS HPC networking); skipping placement group.\n", config.InstanceType)
			}
		}

		// Set placement group in config
		if mpiPlacementGroup != "" {
			config.PlacementGroup = mpiPlacementGroup
		}

		// Validate EFA requirements
		if efaEnabled {
			// Validate in the launch region — some HPC instance types only exist
			// in specific regions and DescribeInstanceTypes returns InvalidInstanceType
			// when queried from a different region (fixes #307).
			if err := awsClient.ValidateInstanceTypeForEFAInRegion(ctx, config.InstanceType, config.Region); err != nil {
				return fmt.Errorf("EFA validation: %w", err)
			}

			// EFA works best with placement groups
			if !mpiAutoPlacementGroup && mpiPlacementGroup == "" {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: EFA works best with placement groups. Consider using --auto-placement-group\n")
			} else {
				fmt.Fprintf(os.Stderr, "✓ EFA enabled with placement group for optimal performance\n")
			}

			// Set EFA in config
			config.EFAEnabled = true
		}

		// Add MPI tags to config
		if config.Tags == nil {
			config.Tags = make(map[string]string)
		}
		config.Tags["spawn:mpi-enabled"] = "true"
		if mpiProcessesPerNode > 0 {
			config.Tags["spawn:mpi-processes-per-node"] = fmt.Sprintf("%d", mpiProcessesPerNode)
		}
	}

	// Check if job array mode (count > 1)
	if count > 1 {
		// Job array launch path
		if jobArrayName == "" {
			return fmt.Errorf("--job-array-name is required when --count > 1")
		}
		if reconcilerMode != "" {
			fmt.Fprintf(os.Stderr, "⚠️  --reconciler is deprecated and ignored; job arrays always use the cohort engine.\n")
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

	// Step 10: Wait for the instance to be usable. For Windows, SSH (22) won't
	// answer until late and the real "usable" signal is the Administrator password
	// becoming available (after EC2Launch runs, post-Sysprep), so wait on that
	// instead of probing port 22 (#95). For Linux, probe SSH as before.
	if config.TargetOS == "windows" {
		prog.Start("Waiting for Windows (password available)")
		if waitForSSH {
			// WaitForPasswordData polls GetPasswordData; it's the earliest reliable
			// "Windows finished first boot" signal. Best-effort: don't fail the
			// launch if it's slow — the instance is up and self-managing via spored.
			if _, err := awsClient.WaitForPasswordData(ctx, config.Region, result.InstanceID, 12*time.Minute); err != nil {
				fmt.Fprintf(os.Stderr, "\n⚠️  Windows is still finishing first boot (Sysprep). The instance is up; `spawn connect %s` will wait for the password.\n", config.Name)
			}
		}
		prog.Complete("Waiting for Windows (password available)")
	} else {
		prog.Start("Waiting for SSH")
		if waitForSSH && result.PublicIP != "" {
			waitForSSHReady(ctx, result.PublicIP, 2*time.Minute)
		}
		prog.Complete("Waiting for SSH")
	}

	// Step 10b: Verify spored actually came up (#50). The bootstrap installs
	// spored asynchronously via cloud-init, so a failed install (bad download,
	// checksum mismatch, arch/network) is invisible to `spawn launch` — leaving a
	// "running" instance with NO lifecycle agent: no TTL enforcement, no idle stop,
	// no completion handling. That's a cost-control hole (a TTL-less zombie). When
	// we waited for readiness (--wait-for-ssh) confirm spored is up over SSM (works
	// keyed or keyless) and fail loudly if not. (An environment where SSM genuinely
	// can't be reached can skip this whole readiness path with --wait-for-ssh=false.)
	if waitForSSH && plat.OS != "windows" {
		prog.Start("Verifying spored agent")
		if err := verifySporedReady(ctx, awsClient, config.Region, result.InstanceID, 5*time.Minute); err != nil {
			prog.Error("Verifying spored agent", err)
			if terminateOnError {
				fmt.Fprintf(os.Stderr, "\n⚠️  spored did not come up; terminating %s (--terminate-on-error)\n", result.InstanceID)
				if terr := awsClient.Terminate(ctx, config.Region, result.InstanceID); terr != nil {
					fmt.Fprintf(os.Stderr, "   terminate failed — DELETE IT MANUALLY (spawn terminate %s): %v\n", result.InstanceID, terr)
				}
				return fmt.Errorf("instance %s launched but spored never came up (terminated): %w", result.InstanceID, err)
			}
			return fmt.Errorf("instance %s launched but spored never came up — it has NO TTL/idle safety net; "+
				"inspect it (spawn connect %s) or terminate it (spawn terminate %s). Re-run with --terminate-on-error to auto-terminate: %w",
				result.InstanceID, result.InstanceID, result.InstanceID, err)
		}
		prog.Complete("Verifying spored agent")
	}

	// Step 11: Register DNS (if requested).
	//
	// DNS registration is performed by SSHing into the instance, so it requires
	// SSH to be confirmed ready. When --wait-for-ssh=false the caller explicitly
	// declined to wait for SSH, so we cannot (and should not) register a record
	// pointing at an instance whose reachability is unconfirmed — skip it rather
	// than block on an SSH that may not yet be up (#56).
	var dnsRecord string
	if dnsName != "" && !waitForSSH {
		fmt.Fprintf(os.Stderr, "ℹ️  Skipping DNS registration (--wait-for-ssh=false); register later with: spawn dns register %s %s\n",
			result.InstanceID, dnsName)
	}
	if dnsName != "" && waitForSSH {
		// Load DNS configuration with precedence
		dnsConfig, err := spawnconfig.LoadDNSConfig(ctx, dnsDomain, dnsAPIEndpoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n⚠️  Failed to load DNS config: %v\n", err)
		} else {
			prog.Start("Registering DNS")
			fqdn, err := registerDNS(plat, result.KeyName, result.InstanceID, result.PublicIP, dnsName, dnsConfig.Domain, dnsConfig.APIEndpoint)
			if err != nil {
				prog.Error("Registering DNS", err)
				// Non-fatal: the instance is fully usable via its public IP /
				// `spawn connect`; DNS is a convenience. Registration is expected to
				// fail in accounts not wired for spore.host DNS.
				fmt.Fprintf(os.Stderr, "\n⚠️  DNS registration failed (non-fatal — use the public IP or `spawn connect %s`): %v\n", config.Name, err)
			} else {
				dnsRecord = fqdn
				prog.Complete("Registering DNS")
			}
		}
	}

	// Output: JSON array or TUI depending on --output flag
	if getOutputFormat() == "json" {
		out := []map[string]interface{}{
			{
				"instance_id":   result.InstanceID,
				"name":          config.Name,
				"instance_type": config.InstanceType,
				"region":        config.Region,
				"public_ip":     result.PublicIP,
				"state":         "running",
				"dns":           dnsRecord,
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
	prog.DisplaySuccess(result.InstanceID, result.PublicIP, connectCmd, config)

	// Show DNS info if registered
	if dnsRecord != "" {
		_, _ = fmt.Fprintf(os.Stdout, "\n🌐 DNS: %s\n", dnsRecord)
		if config.TargetOS == "windows" {
			_, _ = fmt.Fprintf(os.Stdout, "   Connect: spawn connect %s --rdp\n", config.Name)
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "   Connect: ssh %s@%s\n", plat.GetUsername(), dnsRecord)
		}
	}

	return nil
}

// ensureIAMProfile sets config.IamInstanceProfile if unset: either the
// user-specified IAM configuration (--iam-role/--iam-policy/…) or the default
// spored role. Extracted from launchWithProgress (#319); behavior unchanged.
func ensureIAMProfile(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, prog *progress.Progress, auditLog *audit.AuditLogger) error {
	prog.Start("Setting up IAM role")
	if config.IamInstanceProfile == "" {
		// Check if user specified custom IAM configuration
		if iamRole != "" || len(iamPolicy) > 0 || len(iamManagedPolicies) > 0 || iamPolicyFile != "" {
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

			instanceProfile, err := awsClient.CreateOrGetInstanceProfile(ctx, iamConfig)
			if err != nil {
				prog.Error("Setting up IAM role", err)
				auditLog.LogOperation("create_iam_role", iamConfig.RoleName, "failed", err)
				return fmt.Errorf("failed to create IAM instance profile: %w", err)
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
			config.IamInstanceProfile = instanceProfile
			auditLog.LogOperation("create_iam_role", "spored-instance-role", "success", nil)
		}
	}
	prog.Complete("Setting up IAM role")
	return nil
}

// ensureSecurityGroup creates and assigns a managed security group when the
// launch needs one the default SG can't provide: an MPI SG (intra-cluster
// ports) or a Windows SG (RDP 3389 + SSH 22). Otherwise it's a no-op. Extracted
// from launchWithProgress (#319); behavior unchanged.
func ensureSecurityGroup(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, prog *progress.Progress, auditLog *audit.AuditLogger) error {
	if mpiEnabled {
		prog.Start("Creating MPI security group")
		// Get default VPC
		vpcID, err := awsClient.GetDefaultVPC(ctx, config.Region)
		if err != nil {
			prog.Error("Creating MPI security group", err)
			return fmt.Errorf("failed to get default VPC: %w", err)
		}

		// Create or get MPI security group
		sgName := fmt.Sprintf("spawn-mpi-%s", jobArrayName)
		sgID, err := awsClient.CreateOrGetMPISecurityGroup(ctx, config.Region, vpcID, sgName)
		if err != nil {
			prog.Error("Creating MPI security group", err)
			auditLog.LogOperationWithRegion("create_security_group", sgName, config.Region, "failed", err)
			return fmt.Errorf("failed to create MPI security group: %w", err)
		}

		config.SecurityGroupIDs = []string{sgID}
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
		vpcID, err := awsClient.GetDefaultVPC(ctx, config.Region)
		if err != nil {
			prog.Error("Creating Windows security group", err)
			return fmt.Errorf("failed to get default VPC: %w", err)
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
	return nil
}

// ensureAMIAndPreflight detects the AMI (when unset/"auto"), resolves the target
// OS, and runs the pre-flight instance-type guards (Windows lifecycle/burstable,
// nested-virtualization, and MPI/EFA/hibernation feature constraints) before any
// billable resource is created. Extracted from launchWithProgress (#319);
// behavior unchanged.
func ensureAMIAndPreflight(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, prog *progress.Progress) error {
	// Step 1: Detect AMI
	prog.Start("Detecting AMI")
	// "" or "auto" both mean auto-detect the latest AL2023 AMI (#342).
	if config.AMI == "" || strings.EqualFold(config.AMI, "auto") {
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
	// Reject burstable types for Windows (catches auto-detected Windows where the
	// early --os default didn't apply); spend nothing on a launch that'd crawl (#95).
	if err := guardWindowsInstanceType(config.TargetOS, config.InstanceType); err != nil {
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
	if err := preflightInstanceConstraints(ctx, awsClient, config, mpiEnabled, efaEnabled, hibernate || config.HibernateOnIdle); err != nil {
		return err
	}
	return nil
}
