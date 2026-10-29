package cmd

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/regions"
	"github.com/spore-host/spawn/pkg/sweep"
)

func launchParameterSweep(ctx context.Context, baseConfig *aws.LaunchConfig, plat *platform.Platform, auditLog *audit.AuditLogger) error {
	// Validate mutually exclusive flags
	if detach && noDetach {
		return fmt.Errorf("--detach and --no-detach are mutually exclusive")
	}
	if maxConcurrentAuto && maxConcurrent > 0 {
		return fmt.Errorf("--max-concurrent-auto and --max-concurrent are mutually exclusive — pick one")
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

	// Reject TOP-LEVEL keys that look like dangerous spawn settings — ttl:,
	// cost_limit:, sweep_name:, etc. written outside defaults:/grid:/params:,
	// which pkg/params.ParseParamFile would otherwise have silently dropped
	// with no trace at all (#530). Runs before validateSweepParamKeys so a file
	// with both a mistaken top-level key AND a row-level typo gets the
	// top-level error first — that one means a setting the user wrote was never
	// even read, which is the more dangerous failure of the two.
	if err := validateTopLevelParamKeys(paramFile, paramFormat.UnknownTopLevelKeys); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ ERROR: %v\n\n", err)
		return fmt.Errorf("unusable param-file keys")
	}

	// Seed region and instance_type into the file's defaults from the CLI
	// (spawn#697, found by the sweep smoke).
	//
	// The config inversion fixed every path that reads a LaunchConfig, but
	// pkg/sweep/detached.go's cost estimator reads paramSet and
	// paramFormat.Defaults DIRECTLY and never sees a config at all — so
	// `--instance-type t4g.small --param-file rows-without-a-type.json` still
	// failed with "param set 0: no instance_type specified". That is a SEVENTH
	// instance of the class #697 describes, and the first one a unit test could
	// not have found because the estimator only runs on a real launch.
	//
	// Writing the CLI values into Defaults is the semantically correct fix rather
	// than a shim: --instance-type on the command line IS the default instance
	// type for rows that do not set one, which is exactly what Defaults means. A
	// row's own key still wins, and a file-level default the user wrote is never
	// overwritten.
	if baseConfig != nil {
		if paramFormat.Defaults == nil {
			paramFormat.Defaults = map[string]interface{}{}
		}
		seedSweepDefaults(paramFormat.Defaults, *baseConfig)
	}

	// Reject keys that look like spawn settings before anything is launched or
	// priced (#526). This runs before applyCLISpendControlsToSweep so the error
	// names only what the user actually wrote, not the ttl/cost_limit that is
	// about to be injected below.
	if err := validateSweepParamKeys(paramFormat); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ ERROR: %v\n\n", err)
		return fmt.Errorf("unusable param-file keys")
	}

	// The CLI spend-control flags used to be dropped on the floor for a sweep
	// (#525). buildLaunchConfigFromParams "start[s] with an empty config" and the
	// dispatch below copies only Region/InstanceType/Name off baseConfig, so
	// --ttl, --idle-timeout and --cost-limit never reached a single row — while
	// the --no-detach branch below printed "Using safeguards: ttl=..." from the
	// very variables it was about to discard, which is worse than dropping them
	// quietly: it affirmatively tells the operator a safeguard is in place.
	//
	// They are applied here as sweep *defaults*, which is the one place that
	// reaches BOTH orchestration paths: the foreground path merges Defaults into
	// every row (buildLaunchConfigFromParams), and the detached path uploads
	// Defaults to S3 for the Lambda orchestrator, which does the same.
	//
	// This runs before everything that consumes them: the --no-detach safeguard
	// log, the --estimate-only preview (whose per-row duration comes from
	// defaults.ttl, so before this fix `--ttl 4h --estimate-only` on a file with
	// no ttl: quoted a 1h estimate), and applyIdleTimeoutDefault, which now sees
	// an explicit --ttl and stops substituting an unrelated 1h *idle* timeout.
	appliedControls := applyCLISpendControlsToSweep(paramFormat)

	// Every row's ttl/idle_timeout must parse before anything is launched or
	// handed to the orchestrator (#990).
	if err := validateSweepDurations(paramFormat); err != nil {
		return err
	}

	// --volume-size used to be dropped on the floor for a sweep too (#544), the
	// same bug class as #525/#539: it is registered and read on the
	// single-instance launch path (cmd/launch_config.go) but launch_sweep.go
	// never referenced it at all, so every sweep row launched with spawn's
	// hardcoded 20 GiB default (pkg/aws/ebs.go) no matter what --volume-size
	// asked for. Folded into paramFormat.Defaults here, same seam and same
	// precedence rule as the spend controls above: a row's own volume_size: >
	// the CLI flag > the file's defaults:.
	appliedControls = append(appliedControls, applyCLIVolumeSizeToSweep(paramFormat)...)

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

		// If maxConcurrent is 0 (launch all at once), set a reasonable default —
		// unless --max-concurrent-auto will derive a real one below, in which
		// case this arbitrary fallback would just be overwritten (and its
		// stderr message would print out of order relative to the derived value).
		if maxConcurrent == 0 && !maxConcurrentAuto {
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
		// The bound is checked per ROW, against the merged (defaults + row) values
		// the launch will actually use, rather than against the CLI variables
		// alone. Two reasons, both from #525:
		//
		//   * a `ttl:` in the param file is a real bound — it is the one that used
		//     to reach the instances — but this check refused it, so the only way
		//     through was to pass --ttl purely to satisfy the check and then have
		//     its value discarded. One flag to pass the guard, another to take
		//     effect, with nothing saying so.
		//   * a per-row check also catches a file that bounds only SOME of its
		//     rows, which a check on the CLI variables cannot see at all.
		if unbounded := sweepRowsWithoutBound(paramFormat); len(unbounded) > 0 {
			fmt.Fprintf(os.Stderr, "\n❌ ERROR: --no-detach requires a TTL or idle timeout on EVERY row to prevent zombie instances\n")
			fmt.Fprintf(os.Stderr, "   Unbounded: %s\n", strings.Join(unbounded, ", "))
			fmt.Fprintf(os.Stderr, "   Fix with --ttl/--idle-timeout for the whole sweep, or ttl:/idle_timeout: under defaults: or on those rows.\n")
			return fmt.Errorf("--no-detach requires a TTL or idle timeout on every row for safety (%d unbounded)", len(unbounded))
		}
		defTTL, defIdle := sweepRowBound(paramFormat.Defaults, nil)
		fmt.Fprintf(os.Stderr, "   Using safeguards: ttl=%s, idle-timeout=%s\n", defTTL, defIdle)
		fmt.Fprintf(os.Stderr, "   (sweep defaults, CLI flags already folded in; a row's own value wins — #525)\n\n")
	}

	// Generate sweep ID
	name := sweepName
	if name == "" {
		name = "sweep"
	}
	sweepID := generateSweepID(name)

	// Every member's tags must fit in AWS's 50-tag limit (#1336). Checked
	// before anything else, so --estimate-only reports the same refusal and a
	// real sweep fails before applyCLIIAMToSweep creates a role, a key pair is
	// imported or the params are uploaded: RunInstances (or the orchestrator's)
	// would reject the member after all of that.
	if err := preflightSweepTags(*baseConfig, paramFormat, sweepID, name); err != nil {
		return err
	}

	// Write sweep ID to file for workflow integration
	if err := writeOutputID(sweepID, outputIDFile); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write sweep ID to file: %v\n", err)
	}

	// --max-concurrent-auto (#492): resolve region (if not already pinned) and
	// derive a real quota-backed ceiling BEFORE the detached/non-detached branch
	// below, since a sweep auto-enables --detach and that branch needs a
	// concrete maxConcurrent > 0 to take the (cheaper, Lambda-orchestrated) path
	// rather than falling through to the foreground one.
	if maxConcurrentAuto {
		region := baseConfig.Region
		if region == "" {
			resolved, why, derr := resolveLaunchRegion(ctx, "", baseConfig.InstanceType)
			if derr != nil {
				return fmt.Errorf("--max-concurrent-auto: could not choose a region for %s: %w\n"+
					"Pass --region, or set AWS_REGION / a default in your spore config",
					baseConfig.InstanceType, derr)
			}
			region = resolved
			// Pin it on baseConfig too, so the rest of this function (and
			// launchSweepDetached, if we end up there) doesn't re-resolve it.
			baseConfig.Region = region
			fmt.Fprintf(os.Stderr, "🌍 Region: %s (%s)\n", region, why)
		}
		regionClient, derr := aws.NewClientWithRegion(ctx, region)
		if derr != nil {
			return fmt.Errorf("--max-concurrent-auto: create AWS client for %s: %w", region, derr)
		}
		derived, derr := resolveAutoMaxConcurrent(ctx, paramFormat, baseConfig, region, regionClient)
		if derr != nil {
			return fmt.Errorf("--max-concurrent-auto: %w", derr)
		}
		fmt.Fprintf(os.Stderr, "✓ --max-concurrent-auto: derived %d (account quota headroom in %s)\n", derived, region)
		maxConcurrent = derived
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
	if len(appliedControls) > 0 {
		fmt.Fprintf(os.Stderr, "   From the command line: %s (each row's own value wins)\n",
			strings.Join(appliedControls, ", "))
	}
	reportPassthroughParams(paramFormat)
	fmt.Fprintf(os.Stderr, "\n")

	// --estimate-only is handled HERE, above the detached/foreground dispatch
	// below, so "launches nothing" does not depend on which orchestration path
	// this sweep would have taken (#524). It used to be checked only inside
	// launchSweepDetached, which meant any sweep reaching the FOREGROUND path
	// launched every row while the user was asking for a preview — via
	// --no-detach (the documented advice for a heterogeneous sweep, since only
	// the foreground path detects an AMI per config, #372), or via an explicit
	// --detach with no --max-concurrent, which leaves maxConcurrent at 0 and so
	// fails the `detach && maxConcurrent > 0` condition below.
	//
	// Deliberately placed before the AWS client is constructed, so a preview never
	// builds the launch machinery. Pricing each row does now reach AWS — it asks
	// truffle for a live on-demand rate (spore-host/libs#29 removed the static
	// per-family guess that used to answer offline with a fabricated number) — but
	// that is a read-only Price List lookup, memoized per instance shape, and a
	// failure degrades to truffle's static table or marks the row unpriced rather
	// than failing the preview. So --estimate-only still launches nothing and
	// still completes without usable credentials; it just reports what it could
	// not price instead of inventing it.
	if estimateOnly {
		return estimateSweepOnly(ctx, paramFormat)
	}

	// Initialize AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}

	// The CLI IAM flags (--iam-role/--iam-policy/--iam-policy-file/
	// --iam-managed-policies/--iam-role-tags/--iam-allow-full-access) used to be
	// read on the single-instance and batch-queue launch paths but never on this
	// one (#539): every sweep instance silently got the shared
	// spored-instance-role regardless of what was requested, with no warning and
	// exit 0. A wrong/missing IAM role is worse than most dropped launch flags —
	// the workload dies on its first AWS API call with AccessDenied, which reads
	// as a workload bug, not a launch-flag bug, and the whole sweep's spend
	// produces zero result.
	//
	// Resolved here as a sweep default, the one place that reaches BOTH
	// orchestration paths below (same pattern as applyCLISpendControlsToSweep for
	// #525): the foreground path merges Defaults into every row
	// (buildLaunchConfigFromParams's existing iam_role: case), and the detached
	// path uploads Defaults to S3 for the Lambda orchestrator, which reads
	// iam_role from there too (lambda/sweep-orchestrator/main.go). A row's own
	// iam_role: still wins — same precedence already established for spend
	// controls: row's own params: > CLI flag > file's defaults:.
	//
	// A detached sweep's members are launched by the orchestrator, which may
	// pass only the roles spawn creates (#1331): refuse any other profile here,
	// before applyCLIIAMToSweep creates a role for a sweep that cannot use it.
	//
	// A foreground sweep launches with the caller's credentials, so it may use
	// a nested-spawn role, but only on rows with a TTL (#1375). (A detached one
	// refuses the role above, whatever the TTL, #1372.) Same condition as the
	// dispatch below, so each sweep gets the refusal of the path it takes.
	if detach && maxConcurrent > 0 {
		if err := refuseUnpassableDetachedProfiles(paramFormat); err != nil {
			return err
		}
	} else if err := refuseNestedSweepRowsWithoutTTL(paramFormat, instanceProfile); err != nil {
		return err
	}
	iamRecord, err := applyCLIIAMToSweep(ctx, awsClient, paramFormat, auditLog)
	if err != nil {
		return err
	}

	// Check for detached mode (Lambda orchestration)
	if detach && maxConcurrent > 0 {
		return launchSweepDetached(ctx, paramFormat, baseConfig, plat, sweepID, name, maxConcurrent, launchDelay, iamRecord)
	}

	// The config the members are built from, for the checkpoint: a resume
	// rebuilds them from it (#1400). Before anything is created.
	record, err := newSweepMemberRecord(baseConfig, noTimeout, iamRecord)
	if err != nil {
		return err
	}

	// Build launch configs for each parameter set
	launchConfigs, defaultedVerbs, err := foregroundSweepMembers(*baseConfig, paramFormat, sweepID, name)
	if err != nil {
		return err
	}
	hasDefaultApplied := len(defaultedVerbs) > 0

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
		resolved, why, rerr := resolveLaunchRegion(ctx, "", firstConfig.InstanceType)
		if rerr != nil {
			return fmt.Errorf("could not choose a region for %s: %w\n"+
				"Pass --region, or set AWS_REGION / a default in your spore config",
				firstConfig.InstanceType, rerr)
		}
		fmt.Fprintf(os.Stderr, "🌍 Region: %s (%s)\n", resolved, why)
		firstConfig.Region = resolved
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

	// --efa: each member's EFA interface count, as a single launch of its
	// config resolves it (#1407): every card of its own type unless
	// --efa-interfaces says otherwise. Read-only, before anything is created,
	// and so is the Elastic IP check each multi-card member needs.
	if err := resolveSweepEFA(launchConfigs, nil, memoEFACapacity(ctx, awsClient)); err != nil {
		return err
	}
	if err := preflightSweepElasticIPs(ctx, awsClient, launchConfigs, os.Stderr); err != nil {
		return err
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
		decls, err := collectPluginDeclarations()
		if err != nil {
			return err
		}
		if err := windowsPluginGuard(cfg.TargetOS, decls); err != nil {
			return err
		}
	}

	// Look the members' --fsx-id filesystem up before the key pair and IAM role
	// below are set up, so one that is not there (or not in a member's region)
	// fails the sweep before anything is created. Memoized: the user-data step
	// reuses the answers.
	storage := newSweepStorage(awsClient, os.Stderr)
	for i, cfg := range launchConfigs {
		if _, _, err := storage.member(ctx, cfg); err != nil {
			return fmt.Errorf("parameter set %d: %w", i, err)
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

	// Step 3: Setup IAM role. A CLI --iam-role/--iam-policy/--iam-policy-file/
	// --iam-managed-policies was already resolved into a real instance profile and
	// folded into paramFormat.Defaults["iam_role"] by applyCLIIAMToSweep, above
	// the detached/foreground dispatch — so buildLaunchConfigFromParams has
	// already populated every config's IamInstanceProfile from it via the
	// existing iam_role: case, unless a row's own iam_role: overrides it (the
	// established precedence: row's own params: > CLI flag > file's defaults:).
	// This step is therefore unchanged from before #539: it only runs as the
	// fallback for a sweep with no CLI IAM flags and no file-level iam_role:
	// default, ensuring the shared spored-instance-role.
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

	// Zombie-instance guard: the 1h idle-timeout default was applied to every
	// config with no bound (defaultSweepMember, above); warn once for the sweep.
	if hasDefaultApplied {
		fmt.Fprintf(os.Stderr, "\n⚠️  Auto-setting --idle-timeout=1h for all sweep instances\n")
		fmt.Fprintf(os.Stderr, "   Instances will %s after 1 hour of inactivity.\n", strings.Join(defaultedVerbs, " or "))
		fmt.Fprintf(os.Stderr, "   Override with --ttl, --idle-timeout, or --no-timeout\n\n")
	} else if noTimeout {
		fmt.Fprintf(os.Stderr, "\n⚠️  WARNING: --no-timeout specified for sweep\n")
		fmt.Fprintf(os.Stderr, "   Instances will run indefinitely until manually terminated.\n\n")
	}

	// Build user-data for each config, with its storage mounts (#1366).
	if err := renderForegroundSweepUserData(ctx, plat, launchConfigs, storage); err != nil {
		return err
	}

	// Launch, verify and report (#1305): every member's spored is verified, a
	// member whose spored never came up is terminated and counted failed, and a
	// failure that ends the launch terminates every member launched so far.
	guard := newFleetGuard("sweep", os.Stderr, awsClient, launchFleetVerifier(awsClient), keepOnError)
	return runSweepLaunch(ctx, awsClient, guard, sweepRun{
		sweepID: sweepID, name: name, paramFile: paramFile, totalParams: len(paramFormat.Params),
		maxConcurrent: maxConcurrent, launchDelay: launchDelay, configs: launchConfigs,
		record: record,
	})
}

// foregroundSweepMembers builds the config each foreground sweep member is
// launched with: the base (the CLI's config) merged with its row, and the
// per-member defaults. A member neither sweep path can launch as configured is
// refused (sweepMemberGuard, #1366). It returns the idle action each member
// given the 1h idle-timeout default really gets (#1151), once each.
func foregroundSweepMembers(base aws.LaunchConfig, pf *ParamFileFormat, sweepID, name string) ([]*aws.LaunchConfig, []string, error) {
	launchConfigs := make([]*aws.LaunchConfig, 0, len(pf.Params))
	var defaultedVerbs []string
	for i, paramSet := range pf.Params {
		config, defaulted, err := sweepMemberConfig(base, pf.Defaults, paramSet, sweepID, name, i, len(pf.Params), noTimeout)
		if err != nil {
			return nil, nil, err
		}
		if defaulted {
			if v := idleActionVerb(&config); !slices.Contains(defaultedVerbs, v) {
				defaultedVerbs = append(defaultedVerbs, v)
			}
		}
		launchConfigs = append(launchConfigs, &config)
	}
	return launchConfigs, defaultedVerbs, nil
}

// sweepMemberConfig builds foreground parameter set i of total: the base
// merged with defaults and its row, the per-member defaults (the 1h idle
// timeout unless noTimeoutSet), and sweepMemberGuard. The launch and the
// resume both build every member through it, so a resumed member is the one
// the launch would have built (#1400). It reports whether the idle default
// was applied.
func sweepMemberConfig(base aws.LaunchConfig, defaults, row map[string]interface{}, sweepID, name string, i, total int, noTimeoutSet bool) (aws.LaunchConfig, bool, error) {
	config, err := buildLaunchConfigFromParams(base, defaults, row, sweepID, name, i, total)
	if err != nil {
		return config, false, fmt.Errorf("failed to build launch config for parameter set %d: %w", i, err)
	}
	// The per-member defaults, shared with the detached path's tag and user
	// data rendering (#1330), so a member is the same config either way.
	defaulted := defaultSweepMemberWith(&config, base, name, i, noTimeoutSet)
	if err := sweepMemberGuard(config); err != nil {
		return config, false, fmt.Errorf("parameter set %d: %w", i, err)
	}
	return config, defaulted, nil
}

// seedSweepDefaults writes the base's region and instance type into a sweep's
// defaults where the file sets none (#697): what the cost estimator and the
// orchestrator read. Shared by the launch and the resume.
func seedSweepDefaults(defaults map[string]interface{}, base aws.LaunchConfig) {
	if _, set := defaults["instance_type"]; !set && base.InstanceType != "" {
		defaults["instance_type"] = base.InstanceType
	}
	if _, set := defaults["region"]; !set && base.Region != "" {
		defaults["region"] = base.Region
	}
}

// renderForegroundSweepUserData sets each foreground member's user data: its
// spored bootstrap, with the storage-mount script a single launch of the same
// config would run (#1366). Members' target OS and key are already resolved.
func renderForegroundSweepUserData(ctx context.Context, plat *platform.Platform, cfgs []*aws.LaunchConfig, storage *sweepStorage) error {
	for i, cfg := range cfgs {
		storageScript, ready, err := storage.member(ctx, cfg)
		if err != nil {
			return fmt.Errorf("parameter set %d: %w", i, err)
		}
		userDataScript, err := buildUserData(plat, cfg, storageScript, ready)
		if err != nil {
			return fmt.Errorf("failed to build user data: %w", err)
		}
		cfg.UserData = encodeUserDataForOS(userDataScript, cfg.TargetOS)
	}
	return nil
}

// defaultSweepMember fills in what a sweep member's config takes from outside
// its row, on both orchestration paths: the CLI's region and instance type (an
// entry may omit instance_type and fall back to --instance-type, #372), the
// name <sweep>-<index>, and the 1h idle timeout for a member with no ttl and no
// idle_timeout (applyIdleTimeoutDefault). It reports whether that default was
// applied.
func defaultSweepMember(cfg *aws.LaunchConfig, base aws.LaunchConfig, sweepName string, index int) bool {
	return defaultSweepMemberWith(cfg, base, sweepName, index, noTimeout)
}

// defaultSweepMemberWith is defaultSweepMember with --no-timeout passed in: a
// resume takes it from the checkpoint, not the flag.
func defaultSweepMemberWith(cfg *aws.LaunchConfig, base aws.LaunchConfig, sweepName string, index int, noTimeoutSet bool) bool {
	if cfg.Region == "" {
		cfg.Region = base.Region
	}
	if cfg.InstanceType == "" {
		cfg.InstanceType = base.InstanceType
	}
	if cfg.Name == "" {
		cfg.Name = fmt.Sprintf("%s-%d", sweepName, index)
	}
	return applyIdleDefault(cfg, noTimeoutSet)
}

// sweepRun is what runSweepLaunch needs about the sweep.
type sweepRun struct {
	sweepID, name, paramFile string
	totalParams              int
	maxConcurrent            int
	launchDelay              string
	configs                  []*aws.LaunchConfig
	// record is the config the members were built from, recorded in the
	// checkpoint so a resume rebuilds them the same (#1400): with the same
	// IAM profile, which it can re-create (#1385), and the same bounds (#1394).
	record *SweepMemberRecord
}

// runSweepLaunch launches a foreground sweep's parameter sets — through the
// rolling queue, or all at once — verifies each member's spored, and reports.
//
// Parameter sets are independent. A set that failed to launch, or whose spored
// never came up (that member is terminated unless --keep-on-error), is a failed
// set: the sweep exits non-zero and says which, and the verified members are
// kept, since each is enforcing its own TTL. Any other failure after the first
// RunInstances is the launch itself failing, and guard.finish terminates every
// member launched so far (#1305).
func runSweepLaunch(ctx context.Context, l sweepLauncher, guard *fleetGuard, s sweepRun) (err error) {
	defer func() { err = guard.finish(ctx, err) }()
	launchConfigs := s.configs

	var launchedInstances []*aws.LaunchResult
	var failures []string
	var successCount int

	if s.maxConcurrent > 0 && s.maxConcurrent < len(launchConfigs) {
		// Rolling queue mode
		indices := make([]int, len(launchConfigs))
		for i := range indices {
			indices[i] = i
		}
		fresh := &SweepState{
			SweepID:       s.sweepID,
			SweepName:     s.name,
			CreatedAt:     time.Now(),
			ParamFile:     s.paramFile,
			TotalParams:   len(launchConfigs),
			MaxConcurrent: s.maxConcurrent,
			LaunchDelay:   s.launchDelay,
			Member:        s.record,
		}
		launchedInstances, failures, successCount, err = launchWithRollingQueue(ctx, l, guard, fresh, launchConfigs, indices, s.maxConcurrent, s.launchDelay)
		if err != nil {
			return err
		}
	} else {
		// All-at-once mode (maxConcurrent == 0 or >= total params)
		fmt.Fprintf(os.Stderr, "\n🚀 Launching %d instances in parallel...\n\n", len(launchConfigs))
		launchedInstances, failures, successCount = launchAllAtOnce(ctx, l, guard, launchConfigs)
	}
	fmt.Fprintf(os.Stderr, "   %s\n", guard.summary())

	// Handle failures
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Some parameter sets failed (to launch, or spored never came up):\n")
		for _, failure := range failures {
			fmt.Fprintf(os.Stderr, "   • %s\n", failure)
		}
		return partial(fmt.Errorf("%d/%d parameter sets failed; %d launched and kept", len(failures), len(launchConfigs), successCount))
	}

	// Save sweep state
	state := &SweepState{
		SweepID:       s.sweepID,
		SweepName:     s.name,
		CreatedAt:     time.Now(),
		ParamFile:     s.paramFile,
		TotalParams:   s.totalParams,
		MaxConcurrent: s.maxConcurrent,
		LaunchDelay:   s.launchDelay,
		Member:        s.record,
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
				Region:     instance.Region,
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
	fmt.Fprintf(os.Stderr, "Sweep ID:   %s\n", s.sweepID)
	fmt.Fprintf(os.Stderr, "Sweep Name: %s\n", s.name)
	fmt.Fprintf(os.Stderr, "Instances:  %d\n\n", successCount)

	fmt.Fprintf(os.Stderr, "Instances:\n")
	for _, instance := range launchedInstances {
		if instance != nil {
			fmt.Fprintf(os.Stderr, "  • %s (%s) - %s\n", instance.Name, instance.InstanceID, instance.State)
		}
	}

	fmt.Fprintf(os.Stderr, "\nTo view sweep status:\n")
	fmt.Fprintf(os.Stderr, "  spawn list --sweep-id %s\n", s.sweepID)

	return nil
}

// sweepMember is parameter set paramIndex's instance as a fleet member.
func sweepMember(paramIndex int, cfg *aws.LaunchConfig, res *aws.LaunchResult) fleetMember {
	region := res.Region
	if region == "" {
		region = cfg.Region
	}
	return fleetMember{
		label:      fmt.Sprintf("parameter set %d (%s)", paramIndex, res.Name),
		instanceID: res.InstanceID,
		region:     region,
		targetOS:   cfg.TargetOS,
	}
}

// launchAllAtOnce launches all instances in parallel (no rolling queue), then
// verifies each one's spored. A set whose launch or verification failed is
// returned as a failure, its slot in the result nil.
func launchAllAtOnce(ctx context.Context, l sweepLauncher, guard *fleetGuard, launchConfigs []*aws.LaunchConfig) ([]*aws.LaunchResult, []string, int) {
	results := runLaunchBatch(len(launchConfigs), func(idx int) (*aws.LaunchResult, error) {
		res, err := l.Launch(ctx, *launchConfigs[idx])
		if err == nil && res != nil {
			guard.launched(sweepMember(idx, launchConfigs[idx], res))
		}
		return res, err
	})

	// Collect results. Parameter sets are independent, so failures are recorded
	// but successful instances are kept running (no cleanup) — the sweep report
	// surfaces which sets failed.
	launchedInstances := make([]*aws.LaunchResult, len(launchConfigs))
	var failures []string
	var members []fleetMember
	byID := map[string]int{}

	for _, result := range results {
		if result.err != nil {
			failures = append(failures, fmt.Sprintf("Parameter set %d: %v", result.index, result.err))
		} else {
			launchedInstances[result.index] = result.result
			m := sweepMember(result.index, launchConfigs[result.index], result.result)
			members = append(members, m)
			byID[m.instanceID] = result.index
			fmt.Fprintf(os.Stderr, "✓ Launched %s (parameter set %d/%d)\n", result.result.Name, result.index+1, len(launchConfigs))
		}
	}

	// Verify spored on every launched member; one that never came up is a
	// failed set (#1305), terminated unless --keep-on-error.
	verified := map[string]bool{}
	for _, m := range guard.verifyAll(ctx, members) {
		verified[m.instanceID] = true
	}
	successCount := 0
	for _, m := range members {
		idx := byID[m.instanceID]
		if verified[m.instanceID] {
			successCount++
			continue
		}
		failures = append(failures, fmt.Sprintf("Parameter set %d: %v", idx, guard.failure(m.instanceID)))
		launchedInstances[idx] = nil
	}

	return launchedInstances, failures, successCount
}

// sweepLauncher is the slice of *aws.Client a sweep uses; a seam so tests can
// drive it without EC2.
type sweepLauncher interface {
	Launch(ctx context.Context, cfg aws.LaunchConfig) (*aws.LaunchResult, error)
	ListInstances(ctx context.Context, region string, stateFilter string) ([]aws.InstanceInfo, error)
	Terminate(ctx context.Context, region, instanceID string) error
}

// rollingQueuePollInterval is how often the rolling queue checks for finished
// instances. A var so tests need not wait on it.
var rollingQueuePollInterval = 10 * time.Second

// sweepVerifyResult is one member's verification, reported to the queue.
type sweepVerifyResult struct {
	idx int
	err error
}

// launchWithRollingQueue launches instances with rolling queue orchestration.
//
// launchConfigs[i] is parameter set paramIndices[i]: every instance is recorded
// under its parameter index, not its position in launchConfigs. New records
// are appended to state, which is saved as it goes. A fresh launch passes a new
// state and the identity indices; `spawn resume` passes the loaded state and
// only the pending indices, so the records of parameters that already ran, and
// the sweep's TotalParams, survive the resume (#1248).
//
// Each member's spored is verified as it launches, in the background (#1305).
// A member whose spored never came up is terminated (unless --keep-on-error),
// recorded as failed — so `spawn resume` does not count it completed — and its
// slot goes to the next set. The queue returns once every member it launched
// has been verified or has failed.
//
// Completions are polled in every region the active members run in, one
// ListInstances per region per tick (#1251).
func launchWithRollingQueue(ctx context.Context, awsClient sweepLauncher, guard *fleetGuard, state *SweepState, launchConfigs []*aws.LaunchConfig, paramIndices []int, maxConcurrent int, launchDelay string) ([]*aws.LaunchResult, []string, int, error) {
	if len(paramIndices) != len(launchConfigs) {
		return nil, nil, 0, fmt.Errorf("internal error: %d launch configs but %d parameter indices", len(launchConfigs), len(paramIndices))
	}
	sweepID := state.SweepID
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
	var nextToLaunch int

	baseRunning, baseFailed := state.Running, state.Failed
	saveState := func() {
		state.Running = baseRunning + len(activeInstances)
		state.Pending = len(launchConfigs) - nextToLaunch
		state.Failed = baseFailed + len(failures)
		if err := saveSweepState(state); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to save sweep state: %v\n", err)
		}
	}

	// Verification runs beside the queue; results come back on verified.
	verified := make(chan sweepVerifyResult, len(launchConfigs))
	verifying := 0

	// launchOne launches set i and starts verifying it. It reports whether the
	// launch succeeded.
	launchOne := func(i int) bool {
		result, err := awsClient.Launch(ctx, *launchConfigs[i])
		if err != nil {
			failures = append(failures, fmt.Sprintf("Parameter set %d: %v", paramIndices[i], err))
			fmt.Fprintf(os.Stderr, "✗ Failed to launch parameter set %d: %v\n", paramIndices[i]+1, err)
			return false
		}
		launchedInstances[i] = result
		activeInstances[i] = result.InstanceID
		successCount++
		state.Instances = append(state.Instances, InstanceState{
			Index:      paramIndices[i],
			InstanceID: result.InstanceID,
			Region:     result.Region,
			State:      "running",
			LaunchedAt: time.Now(),
		})
		m := sweepMember(paramIndices[i], launchConfigs[i], result)
		guard.launched(m)
		verifying++
		at := time.Now()
		go func() { verified <- sweepVerifyResult{idx: i, err: guard.verifyOne(ctx, m, at)} }()
		return true
	}

	// settle records a finished verification. A failed one is a failed set;
	// it frees its slot (returns true) once the member is gone.
	settle := func(r sweepVerifyResult) bool {
		verifying--
		if r.err == nil {
			return false
		}
		res := launchedInstances[r.idx]
		failures = append(failures, fmt.Sprintf("Parameter set %d: %v", paramIndices[r.idx], r.err))
		successCount--
		for k := range state.Instances {
			if state.Instances[k].InstanceID == res.InstanceID {
				state.Instances[k].State = sweepInstanceFailed
				state.Instances[k].ErrorMessage = r.err.Error()
			}
		}
		launchedInstances[r.idx] = nil
		_, active := activeInstances[r.idx]
		freed := active && !guard.keep
		if freed {
			delete(activeInstances, r.idx)
		}
		saveState()
		return freed
	}

	// fill launches the next pending set for each freed slot.
	fill := func(slots int) {
		for ; slots > 0 && nextToLaunch < len(launchConfigs); slots-- {
			if delay > 0 {
				time.Sleep(delay)
			}
			if launchOne(nextToLaunch) {
				fmt.Fprintf(os.Stderr, "✓ Launched %s (parameter set %d/%d) [%d active, %d pending]\n",
					launchedInstances[nextToLaunch].Name, paramIndices[nextToLaunch]+1, state.TotalParams,
					len(activeInstances), len(launchConfigs)-nextToLaunch-1)
			}
			nextToLaunch++
			saveState()
		}
	}

	// Launch first batch
	initialBatch := maxConcurrent
	if initialBatch > len(launchConfigs) {
		initialBatch = len(launchConfigs)
	}

	fmt.Fprintf(os.Stderr, "Launching initial batch of %d instances...\n", initialBatch)
	for i := 0; i < initialBatch; i++ {
		if launchOne(i) {
			fmt.Fprintf(os.Stderr, "✓ Launched %s (parameter set %d/%d)\n", launchedInstances[i].Name, paramIndices[i]+1, state.TotalParams)
		}

		// Apply launch delay between initial launches
		if i < initialBatch-1 && delay > 0 {
			time.Sleep(delay)
		}
	}
	nextToLaunch = initialBatch

	// Save initial state, on top of whatever the caller's state already holds.
	saveState()

	// interrupted waits out the verifications in flight (each ends promptly on
	// the cancelled context, terminating its member), then reports.
	interrupted := func() ([]*aws.LaunchResult, []string, int, error) {
		for verifying > 0 {
			settle(<-verified)
		}
		fmt.Fprintf(os.Stderr, "\n⚠️  Interrupted. Progress saved to sweep state.\n")
		fmt.Fprintf(os.Stderr, "Resume with: spawn resume --sweep-id %s\n", sweepID)
		return launchedInstances, failures, successCount, partial(ctx.Err())
	}

	// Rolling queue: poll for completions and launch next
	if nextToLaunch < len(launchConfigs) {
		fmt.Fprintf(os.Stderr, "\nMonitoring instances and launching next in queue...\n")
		fmt.Fprintf(os.Stderr, "Press Ctrl-C to stop (sweep can be resumed later)\n\n")

		ticker := time.NewTicker(rollingQueuePollInterval)
		defer ticker.Stop()

		for nextToLaunch < len(launchConfigs) {
			select {
			case <-ctx.Done():
				return interrupted()

			case r := <-verified:
				if settle(r) {
					fill(1)
				}

			case <-ticker.C:
				if len(activeInstances) == 0 {
					// Every slot is free (all launches in it failed): refill.
					fill(maxConcurrent)
					continue
				}
				fill(len(finishedSweepMembers(ctx, awsClient, activeInstances, launchedInstances, launchConfigs)))
			}
		}

		fmt.Fprintf(os.Stderr, "\n✅ All instances launched. Waiting for final batch to complete...\n")
	}

	// Every member launched must be verified before the queue reports.
	for verifying > 0 {
		select {
		case <-ctx.Done():
			return interrupted()
		case r := <-verified:
			settle(r)
		}
	}

	return launchedInstances, failures, successCount, nil
}

// sweepInstanceFailed is the sweep-state record of a member that failed after
// it launched — its spored never came up (#1305). reconcileSweepState counts
// it failed, never completed, whatever EC2 now says.
const sweepInstanceFailed = "failed"

// finishedSweepMembers removes from active, and returns, the members that have
// stopped running. It polls each region the active members run in once
// (#1251): a sweep can span regions, and polling only the first left the
// others' instances looking active for ever. A region whose poll fails decides
// nothing about its members this tick.
func finishedSweepMembers(ctx context.Context, l sweepLauncher, active map[int]string, launched []*aws.LaunchResult, cfgs []*aws.LaunchConfig) []int {
	byRegion := map[string][]int{}
	for idx := range active {
		region := cfgs[idx].Region
		if launched[idx] != nil && launched[idx].Region != "" {
			region = launched[idx].Region
		}
		byRegion[region] = append(byRegion[region], idx)
	}
	regions := make([]string, 0, len(byRegion))
	for r := range byRegion {
		regions = append(regions, r)
	}
	sort.Strings(regions)

	var done []int
	for _, region := range regions {
		instances, err := l.ListInstances(ctx, region, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to query instance states in %s: %v\n", region, err)
			continue
		}
		stateMap := make(map[string]string, len(instances))
		for _, inst := range instances {
			stateMap[inst.InstanceID] = inst.State
		}
		idxs := byRegion[region]
		sort.Ints(idxs)
		for _, idx := range idxs {
			st, exists := stateMap[active[idx]]
			if !exists || st == "terminated" || st == "stopping" || st == "stopped" {
				done = append(done, idx)
			}
		}
	}
	for _, idx := range done {
		delete(active, idx)
	}
	return done
}

// sweepRowBound returns the TTL and idle timeout that will actually apply to one
// row: the row's own value if it has one, otherwise the sweep default. Pass a nil
// row to read the defaults themselves.
//
// A non-string value (`ttl: 3600` rather than `ttl: 1h`) reads as absent here.
// It never reaches a launch: validateSweepDurations and
// buildLaunchConfigFromParams both reject it (#990).
func sweepRowBound(defaults, row map[string]interface{}) (ttl, idle string) {
	pick := func(key string) string {
		for _, m := range []map[string]interface{}{row, defaults} {
			if v, ok := m[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	return pick("ttl"), pick("idle_timeout")
}

// sweepRowsWithoutBound labels every row that would launch with neither a TTL nor
// an idle timeout, for the --no-detach guard's error message. Naming the rows
// matters: on a 30-row file "some row is unbounded" is not actionable.
func sweepRowsWithoutBound(paramFormat *ParamFileFormat) []string {
	var out []string
	for i, row := range paramFormat.Params {
		if ttl, idle := sweepRowBound(paramFormat.Defaults, row); ttl == "" && idle == "" {
			label := fmt.Sprintf("row %d", i)
			if it, ok := row["instance_type"].(string); ok && it != "" {
				label += " (" + it + ")"
			}
			out = append(out, label)
		}
	}
	return out
}

// applyCLISpendControlsToSweep writes the CLI spend-control flags into a sweep's
// `defaults:` so they actually reach each row (#525), and returns a human-readable
// list of what it set for the launch header.
//
// Precedence, most specific wins:
//
//	a row's own params: > the CLI flag > the param file's defaults:
//
// So a per-row `ttl:` (a GPU row on a shorter leash, say) still wins, while a flag
// passed at invocation time beats a value checked into the file — which is the
// same direction cobra/viper use for flags versus config, and the reason
// `--ttl 30m` on a file that says `ttl: 8h` does what it looks like it does.
//
// A non-empty ttl/idleTimeout here can only have come from the command line:
// applyLaunchDefaults (which would otherwise fold in ~/.spawn/config.yaml) runs
// *after* the sweep early-return in launch_single.go, so ~/.spawn defaults never
// silently outrank the param file.
func applyCLISpendControlsToSweep(paramFormat *ParamFileFormat) []string {
	if paramFormat.Defaults == nil {
		paramFormat.Defaults = make(map[string]interface{})
	}
	foldSpendControls(paramFormat.Defaults, ttl, idleTimeout, costLimit)
	var applied []string
	if ttl != "" {
		applied = append(applied, "ttl="+ttl)
	}
	if idleTimeout != "" {
		applied = append(applied, "idle-timeout="+idleTimeout)
	}
	if costLimit > 0 {
		applied = append(applied, fmt.Sprintf("cost-limit=$%.2f", costLimit))
	}
	return applied
}

// applyCLIVolumeSizeToSweep writes --volume-size into a sweep's `defaults:` so
// it actually reaches each row (#544), the same seam and pattern as
// applyCLISpendControlsToSweep above: a row's own volume_size: still wins after
// the merge, a flag passed at invocation time beats a value checked into the
// file's defaults:, and this is the one place that reaches both the foreground
// path (buildLaunchConfigFromParams merges Defaults into every row) and the
// detached path (Defaults are uploaded to S3 for the Lambda orchestrator, which
// reads volume_size from there too).
//
// launchVolumeSize's zero value ("unset") is indistinguishable from an explicit
// --volume-size 0, but RootVolumeSizeGiB already treats 0 as "no override" on
// the single-instance path (cmd/launch_config.go's `if launchVolumeSize > 0`),
// so this mirrors that: there is no way to ask for a 0 GiB root volume, on
// either launch path, and this fix does not add one.
func applyCLIVolumeSizeToSweep(paramFormat *ParamFileFormat) []string {
	if launchVolumeSize <= 0 {
		return nil
	}
	if paramFormat.Defaults == nil {
		paramFormat.Defaults = make(map[string]interface{})
	}
	foldVolumeSize(paramFormat.Defaults, launchVolumeSize)
	return []string{fmt.Sprintf("volume-size=%dGiB", launchVolumeSize)}
}

// foldVolumeSize writes a --volume-size into a sweep's defaults, where it beats
// the file's defaults: and loses to a row's own (#544). Shared by the launch
// and the resume.
func foldVolumeSize(defaults map[string]interface{}, size int32) {
	if size > 0 {
		defaults["volume_size"] = size
	}
}

// cliIAMFlagsRequireCustomProfile reports whether any CLI flag means this launch
// needs its own instance profile rather than the shared spored-instance-role.
//
// Shared by the single-instance path and the sweep path ON PURPOSE. #539 was
// "the sweep path ignores the IAM flags", and its fix listed the flags that
// existed at the time. #614 then added --s3-read/--s3-write to the
// single-instance condition and not to the sweep one, so those two were silently
// dropped on sweeps — the same bug, re-created in the same place, because the
// condition was duplicated instead of shared.
//
// Why it matters that these count as "custom IAM": SetupSporedIAMRole attaches a
// FIXED policy whose S3 grants cover only spawn's own infrastructure buckets. So
// a sweep row reading the caller's own bucket got 403 — which is exactly the
// failure #614 fixed for single launches.
//
// A new IAM-ish flag belongs here, and only here.
func cliIAMFlagsRequireCustomProfile() bool {
	return iamRole != "" ||
		len(iamPolicy) > 0 ||
		len(iamManagedPolicies) > 0 ||
		iamPolicyFile != "" ||
		len(s3ReadBuckets) > 0 ||
		len(s3WriteBuckets) > 0
}

// applyCLIIAMToSweep resolves the CLI IAM flags (--iam-role/--iam-policy/
// --iam-policy-file/--iam-managed-policies/--iam-role-tags/--iam-allow-full-access)
// into a real instance profile ONCE per sweep — the same
// CreateOrGetInstanceProfile call launch_single.go's ensureIAMProfile makes per
// instance — and writes the result into paramFormat.Defaults["iam_role"] so it
// reaches every row via the existing iam_role: case in
// buildLaunchConfigFromParams (#539).
//
// A no-op when none of the flags were passed: the existing SetupSporedIAMRole
// fallback (unchanged, in launchParameterSweep and launchSweepDetached) still
// provides the shared spored-instance-role in that case, exactly as before this
// fix.
//
// --instance-profile (the package-level instanceProfile flag var, #550) is
// checked FIRST and bypasses this entire function's resolution — including the
// --iam-role/--iam-policy/etc. flags — the same precedence
// launch_single.go's ensureIAMProfile gives it on the single-instance path.
//
// It returns the record a resume needs to launch with, and re-create, the same
// profile (#1385), or nil when no flag was passed.
func applyCLIIAMToSweep(ctx context.Context, awsClient *aws.Client, paramFormat *ParamFileFormat, auditLog *audit.AuditLogger) (*SweepIAMRole, error) {
	if instanceProfile != "" {
		auditLog.LogOperation("create_iam_role", instanceProfile, "success", nil)
		if paramFormat.Defaults == nil {
			paramFormat.Defaults = make(map[string]interface{})
		}
		paramFormat.Defaults["iam_role"] = instanceProfile
		fmt.Fprintf(os.Stderr, "\n✓ IAM: using --instance-profile %s verbatim (bypasses --iam-role/--iam-policy resolution; "+
			"each row's own iam_role: still wins)\n", instanceProfile)
		return &SweepIAMRole{Profile: instanceProfile, Verbatim: true, Flags: cliIAMFlagsString()}, nil
	}

	if !cliIAMFlagsRequireCustomProfile() {
		return nil, nil
	}

	// Reject wildcard *:FullAccess templates unless explicitly opted in (2026-06
	// audit, M-sec) — fail before any AWS call, same guard as the single-instance
	// path.
	if err := aws.ValidatePolicyNames(iamPolicy, iamAllowFullAccess); err != nil {
		return nil, err
	}

	iamConfig := aws.IAMRoleConfig{
		RoleName:        iamRole,
		Policies:        iamPolicy,
		ManagedPolicies: iamManagedPolicies,
		PolicyFile:      iamPolicyFile,
		TrustServices:   iamTrustServices,
		Tags:            parseIAMRoleTags(iamRoleTags),
	}

	// --s3-read/--s3-write become a scoped bucket policy built by the SAME
	// function the single-instance and task paths use (#614), so no two paths can
	// disagree about what "read this bucket" grants. Without this, a sweep fell
	// back to spored-instance-role, whose fixed policy covers only spawn's own
	// infrastructure buckets — so every row died on its first `aws s3` call with
	// 403, after paying for the boot.
	if len(s3ReadBuckets) > 0 || len(s3WriteBuckets) > 0 {
		if err := validateS3BucketFlags(s3ReadBuckets, s3WriteBuckets); err != nil {
			return nil, err
		}
		iamConfig.InlinePolicyJSON = taskStagingPolicy(s3ReadBuckets, s3WriteBuckets, "", nil)
	}

	instanceProfile, err := awsClient.CreateOrGetInstanceProfile(ctx, iamConfig)
	if err != nil {
		auditLog.LogOperation("create_iam_role", iamConfig.RoleName, "failed", err)
		return nil, fmt.Errorf("failed to create IAM instance profile: %w", err)
	}
	auditLog.LogOperationWithData("create_iam_role", iamConfig.RoleName, "success",
		map[string]interface{}{
			"instance_profile": instanceProfile,
		}, nil)

	if paramFormat.Defaults == nil {
		paramFormat.Defaults = make(map[string]interface{})
	}
	// Overwrite unconditionally, same as applyCLISpendControlsToSweep: a flag
	// passed at invocation time outranks a value checked into the file's
	// defaults:, and a row's own iam_role: (applied afterwards by
	// buildLaunchConfigFromParams, which merges defaults first) still wins over
	// both.
	paramFormat.Defaults["iam_role"] = instanceProfile
	fmt.Fprintf(os.Stderr, "\n✓ IAM: using instance profile %s from the command line "+
		"(each row's own iam_role: still wins)\n", instanceProfile)
	return newSweepIAMRole(instanceProfile, iamConfig, cliIAMFlagsString())
}

// estimateSweepOnly prints a per-row cost estimate for a sweep and returns
// without launching anything. This is the entirety of --estimate-only's
// behaviour on the sweep path; both orchestration paths reach it from the single
// check in launchParameterSweep (#524).
func estimateSweepOnly(ctx context.Context, paramFormat *ParamFileFormat) error {
	if _, err := estimateAndReportSweep(ctx, paramFormat); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✅ Estimate complete — no instances launched (--estimate-only)\n")
	return nil
}

// reportSweepBudget compares an estimated sweep cost against --budget. It only
// ever prints: --budget is a warning, not a cap, and nothing here blocks a
// launch. Shared by the estimate-only path and the real detached launch so the
// two cannot drift.
//
// partial says the estimate could not price every row, so totalCost is a floor.
// It must change what this prints: "within budget" computed from a total that is
// missing rows is a pass the sweep may not have earned, and silently granting one
// is the same class of error as the fabricated rates that made the total wrong in
// the first place (spore-host/libs#29).
func reportSweepBudget(totalCost float64, partial bool) {
	if budget <= 0 {
		return
	}
	if totalCost > budget {
		fmt.Fprintf(os.Stderr, "⚠️  WARNING: Estimated cost ($%.2f) exceeds budget ($%.2f) by $%.2f\n\n",
			totalCost, budget, totalCost-budget)
		return
	}
	if partial {
		fmt.Fprintf(os.Stderr, "⚠️  Cannot confirm this sweep is within budget: the $%.2f floor fits in $%.2f,\n"+
			"   but rows that could not be priced are missing from it (see above).\n\n",
			totalCost, budget)
		return
	}
	fmt.Fprintf(os.Stderr, "✓ Within budget: $%.2f remaining of $%.2f\n\n",
		budget-totalCost, budget)
}

// launchSweepDetached launches a parameter sweep in detached mode (Lambda orchestration)
func launchSweepDetached(ctx context.Context, paramFormat *ParamFileFormat, baseConfig *aws.LaunchConfig, plat *platform.Platform, sweepID, sweepName string, maxConcurrent int, launchDelay string, iamRecord *SweepIAMRole) error {
	// Determine region (auto-detect if not specified)
	sweepRegion := baseConfig.Region
	if sweepRegion == "" {
		// The old message here printed sweepRegion BEFORE assigning to it, so it
		// always announced the empty string — another line that did not say what
		// it claimed.
		resolved, why, rerr := resolveLaunchRegion(ctx, "", baseConfig.InstanceType)
		if rerr != nil {
			return fmt.Errorf("could not choose a region for %s: %w\n"+
				"Pass --region, or set AWS_REGION / a default in your spore config",
				baseConfig.InstanceType, rerr)
		}
		sweepRegion = resolved
		fmt.Fprintf(os.Stderr, "🌍 Region: %s (%s)\n", sweepRegion, why)
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

	// Seed the launch keys the orchestrator reads but cannot derive (#749).
	//
	// The Lambda never sees a LaunchConfig — it builds RunInstances from the
	// uploaded params — and `ami` was never seeded, so it sent no ImageId and EC2
	// rejected EVERY row. Must run BEFORE the upload below, and before validation,
	// so what gets validated is what gets launched.
	regionClient, rcErr := aws.NewClientWithRegion(ctx, sweepRegion)
	if rcErr != nil {
		return fmt.Errorf("failed to initialize AWS client for %s: %w", sweepRegion, rcErr)
	}
	if err := seedDetachedLaunchKeys(ctx, regionClient, paramFormat, baseConfig, sweepRegion); err != nil {
		return fmt.Errorf("detached sweep: %w", err)
	}
	// Each member's spored bootstrap, built as the foreground path builds it
	// (#1323). After the launch keys, which settle each member's AMI and key.
	if err := seedDetachedBootstrap(paramFormat, *baseConfig, sweepID, sweepName, newDetachedBootstrap(ctx, regionClient, plat)); err != nil {
		return fmt.Errorf("detached sweep: %w", err)
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

	// Estimate cost, print it with its provenance, and check --budget (warning
	// only — see reportSweepBudget). Shared with --estimate-only.
	costEstimate, err := estimateAndReportSweep(ctx, paramFormat)
	if err != nil {
		return err
	}

	// No --estimate-only check here: it is handled once in launchParameterSweep,
	// before the dispatch that chooses this function, so that the guarantee holds
	// on the foreground path too (#524). A second check here would be dead code
	// and would re-create the per-path shape that caused the bug.

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

	// spored's instance profile, for members with none (#1323). After the
	// prompt, so a declined launch creates nothing.
	if err := ensureDetachedInstanceProfile(ctx, regionClient, paramFormat); err != nil {
		return fmt.Errorf("detached sweep: %w", err)
	}

	// The CLI IAM flags' profile, for `spawn sweep resume --detach` to re-create
	// if the idle-role sweep deletes it (#1385). Before the upload, so a sweep
	// the orchestrator can run always has it.
	if iamRecord != nil {
		if err := saveDetachedIAMRecord(sweepID, iamRecord); err != nil {
			return fmt.Errorf("detached sweep: record IAM for resume: %w", err)
		}
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
			Include:    regionsInclude,
			Exclude:    regionsExclude,
			Geographic: regionsGeographic,
		}

		// Validated and applied by pkg/regions, which is where the tests for this
		// logic live (#811). cmd/ had its own byte-identical copy with none.
		rc := regionConstraintFromSweep(constraint)
		if err := regions.ValidateConstraint(rc); err != nil {
			return fmt.Errorf("invalid region constraint: %w", err)
		}

		// Get all regions from parameter file
		allRegions := make([]string, 0, len(regionGroups))
		for region := range regionGroups {
			allRegions = append(allRegions, region)
		}

		// Apply constraints
		filteredRegions, err := regions.ApplyConstraints(allRegions, rc)
		if err != nil {
			return fmt.Errorf("region constraints failed: %w", err)
		}

		// Remove filtered-out regions
		for region := range regionGroups {
			if !regions.Contains(filteredRegions, region) {
				delete(regionGroups, region)
			}
		}

		// Store constraint in record
		record.RegionConstraints = constraint
		record.FilteredRegions = filteredRegions

		fmt.Fprintf(os.Stderr, "🌍 Applied region constraints: %d regions allowed\n", len(filteredRegions))
		fmt.Fprintf(os.Stderr, "   Filtered regions: %v\n", filteredRegions)
		fmt.Fprintf(os.Stderr, "   Constraint: %s\n", rc.String())
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

	// "Queued", not "launched" — and the distinction is load-bearing.
	//
	// The CLI's evidence is that the Lambda was INVOKED. It does not know whether
	// a single row started. For the whole life of this path none ever did: every
	// RunInstances was rejected for want of an ImageId, the Lambda logged "All
	// instances launched and completed", and this message was the only thing the
	// user ever saw (#749). The seeding above fixes the cause, but the CLI still
	// cannot verify the outcome, so it should not keep implying it has.
	fmt.Fprintf(os.Stderr, "✅ Parameter sweep queued successfully!\n\n")
	fmt.Fprintf(os.Stderr, "Sweep ID:          %s\n", sweepID)
	fmt.Fprintf(os.Stderr, "Sweep Name:        %s\n", sweepName)
	fmt.Fprintf(os.Stderr, "Total Parameters:  %d\n", len(paramFormat.Params))
	fmt.Fprintf(os.Stderr, "Max Concurrent:    %d\n", maxConcurrent)
	fmt.Fprintf(os.Stderr, "Region:            %s\n", sweepRegion)
	fmt.Fprintf(os.Stderr, "Orchestration:     Lambda (infra account)\n\n")

	fmt.Fprintf(os.Stderr, "The orchestrator has been invoked and will launch the rows; you can\n")
	fmt.Fprintf(os.Stderr, "disconnect safely. Nothing here confirms a row actually started —\n")
	fmt.Fprintf(os.Stderr, "check that before relying on it:\n")
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
