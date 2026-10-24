package tasklaunch

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/storage"
	"github.com/spore-host/spawn/pkg/taskproto"
	"github.com/spore-host/spawn/pkg/userdata"
)

// CompletionFile is the on-instance file the generated wrapper writes and
// spored watches to fire on_complete. It matches the wrapper's SPAWN_COMPLETE
// path and provider/ec2.go's default, but we tag it EXPLICITLY at launch (below)
// so on-complete teardown never depends on provider-side defaulting that is
// gated on the spawn:on-complete tag being visible when spored first boots
// (EC2 tag eventual-consistency); see spawn#406.
const CompletionFile = "/tmp/SPAWN_COMPLETE"

// buildLaunchConfig maps a sized TaskSpec to the minimal aws.LaunchConfig for one
// task instance. AMI, UserData, and KeyName are deliberately left empty for
// launcher.Provision to fill (auto AMI, keyless/SSM, wrapper-as-user-data); the
// scoped instance profile is set so Provision skips its default-role step.
func taskLaunchConfig(spec *taskproto.TaskSpec, sized *taskproto.SizeResult, region, iamProfile, wrapper string) aws.LaunchConfig {
	cfg := aws.LaunchConfig{
		InstanceType:    sized.InstanceType,
		Region:          region,
		JobArrayCommand: wrapper, // Provision embeds this via /etc/spawn/command
		Spot:            spec.Resources.Purchase == taskproto.PurchaseSpot,
		TTL:             spec.Lifecycle.TTL,
		OnComplete:      spec.EffectiveOnComplete(),
		// Tag the completion file + a short grace explicitly so spored watches the
		// wrapper's SPAWN_COMPLETE regardless of tag-visibility timing (spawn#406).
		CompletionFile:     CompletionFile,
		CompletionDelay:    "10s",
		Name:               spec.TaskID,
		IamInstanceProfile: iamProfile,
		Tags:               map[string]string{"spawn:task-id": spec.TaskID},
	}
	// Optional launch-time placement (nf-spawn's ext.* analogs): a specific AMI/AZ
	// and attached EBS reference-data volumes. FSx/EFS mounting is threaded via the
	// launcher's StorageScript (built in Launch), not here.
	cfg.AMI = spec.Placement.AMI
	cfg.AvailabilityZone = spec.Placement.AvailabilityZone
	// Empty launches into the region's default VPC; Plan refuses that up front
	// when the region has none (spawn#1267).
	cfg.SubnetID = spec.Placement.SubnetID
	for _, v := range spec.Placement.Volumes {
		cfg.AttachVolumes = append(cfg.AttachVolumes, aws.AttachVolumeSpec{
			SnapshotID: v.Snapshot,
			MountPoint: v.MountPath,
			ReadOnly:   v.ReadOnly,
		})
	}
	// resources.disk_gib / lifecycle.cost_limit are the TaskSpec equivalents of
	// --volume-size / --cost-limit on the regular launch path (#556): both flow
	// into the SAME aws.LaunchConfig fields the CLI flags populate
	// (cmd/launch_config.go), so a task and a plain launch converge on identical
	// downstream behavior. Left unset (zero) when omitted from the spec — same
	// "0 = use AMI default" / "0 = disabled" convention --volume-size and
	// --cost-limit already use, so an unset TaskSpec field never changes
	// existing behavior.
	if spec.Resources.DiskGiB > 0 {
		cfg.RootVolumeSizeGiB = spec.Resources.DiskGiB
	}
	if spec.Lifecycle.CostLimit > 0 {
		cfg.CostLimit = spec.Lifecycle.CostLimit
	}
	return cfg
}

// taskStorageScript builds the boot-time mount script for a task's placement
// storage: attached EBS volumes (from snapshots), an existing EFS filesystem, and
// an existing FSx for Lustre filesystem. Returns "" when the placement requests
// none. The script is threaded into the headless launcher via
// launcher.Options.StorageScript and runs before the workload command (#166).
func taskStorageScript(ctx context.Context, client fsxLookup, spec *taskproto.TaskSpec, region string) (string, error) {
	return taskStorageScriptWith(spec, region, func(id string) (string, string, error) {
		info, err := client.GetFSxFilesystem(ctx, id, region)
		if err != nil {
			return "", "", err
		}
		return info.DNSName, info.MountName, nil
	})
}

// taskStorageScriptWith is taskStorageScript with the one AWS lookup it needs,
// the FSx filesystem's DNS and mount names, supplied by the caller, so the user
// data preflight can build the script without calling AWS.
func taskStorageScriptWith(spec *taskproto.TaskSpec, region string, lookupFSx func(id string) (dnsName, mountName string, err error)) (string, error) {
	p := spec.Placement
	if len(p.Volumes) == 0 && p.EFSID == "" && p.FSxLustreID == "" {
		return "", nil
	}
	sc := userdata.StorageConfig{}

	// Reuse the CLI's device-name assignment so the mount script's DeviceName
	// matches the block-device mapping cfg.AttachVolumes produces.
	var volSpecs []aws.AttachVolumeSpec
	for _, v := range p.Volumes {
		volSpecs = append(volSpecs, aws.AttachVolumeSpec{
			SnapshotID: v.Snapshot,
			MountPoint: v.MountPath,
			ReadOnly:   v.ReadOnly,
		})
	}
	sc.AttachedVolumes = userdata.AttachedVolumesFrom(volSpecs)
	if p.EFSID != "" {
		opts, err := efsMountOptions()
		if err != nil {
			return "", fmt.Errorf("EFS mount options: %w", err)
		}
		sc.EFSEnabled = true
		sc.EFSFilesystemDNS = aws.GetEFSDNSName(p.EFSID, region)
		// EffectiveEFSMountPoint (spawn#570 sub-issue 3): honors
		// Placement.EFSMountPoint when the spec overrides it, defaulting to
		// "/efs" otherwise — the SAME default+override resolution
		// wrapper.go's containerMountDirs uses, so a containerized task's
		// `docker run -v` always bind-mounts wherever this script actually
		// mounted the filesystem, not a second, independently hardcoded path.
		sc.EFSMountPoint = p.EffectiveEFSMountPoint()
		sc.EFSMountOptions = opts
	}
	if p.FSxLustreID != "" {
		dnsName, mountName, err := lookupFSx(p.FSxLustreID)
		if err != nil {
			return "", fmt.Errorf("look up FSx %s: %w", p.FSxLustreID, err)
		}
		sc.FSxLustreEnabled = true
		sc.FSxFilesystemDNS = dnsName
		sc.FSxMountName = mountName
		// EffectiveFSxMountPoint: see the EFS comment above — same
		// override-or-default resolution shared with wrapper.go.
		sc.FSxMountPoint = p.EffectiveFSxMountPoint()
	}
	return userdata.GenerateStorageUserData(sc)
}

// fsxLookup is the AWS call the storage script needs for an FSx for Lustre
// placement.
type fsxLookup interface {
	GetFSxFilesystem(ctx context.Context, filesystemID, region string) (*aws.FSxInfo, error)
}

// efsMountOptions is the mount option string for a task's EFS placement:
// spawn's "general" EFS profile. It is what `spawn task run` has always used,
// since the --efs-profile/--efs-mount-options flags belong to `spawn launch`
// and their defaults are the general profile.
func efsMountOptions() (string, error) {
	opts, err := storage.GetEFSProfile(storage.EFSProfileGeneral)
	if err != nil {
		return "", fmt.Errorf("failed to get EFS profile: %w", err)
	}
	return opts.ToMountString(), nil
}

// provisionTask launches cfg with opts through provision and, when a Spot launch fails
// for capacity and the spec asks for an on-demand fallback, retries once
// on-demand (a minimal single-region echo of lagotto's capacity fallthrough;
// AZ-spread is out of scope). cfg.Spot is cleared on fallback so the caller
// reports the purchase option that actually launched.
//
// Both attempts receive the same opts, so the fallback instance gets the
// same storage mounts and terminal-flush hook as the Spot attempt would have:
// before #1144 the retry passed an empty Options{} and launched without its
// EFS/FSx/volume mounts and without the hook that writes a completion record on
// a TTL kill.
func provisionTask(out io.Writer, provision func(aws.LaunchConfig, launcher.Options) (*aws.LaunchResult, error), cfg *aws.LaunchConfig, opts launcher.Options, fallback string) (*aws.LaunchResult, error) {
	result, err := provision(*cfg, opts)
	if err != nil && cfg.Spot && fallback == taskproto.PurchaseOnDemand && isCapacityErr(err) {
		fmt.Fprintf(out, "⚠ spot capacity unavailable (%v); retrying on-demand per fallback...\n", classifyLaunchErr(err))
		cfg.Spot = false
		result, err = provision(*cfg, opts)
	}
	return result, err
}

// isCapacityErr reports whether a launch error is a capacity class (worth a
// fallback retry), keyed on the AWS error code carried by aws.LaunchError.
func isCapacityErr(err error) bool {
	return classifyLaunchErr(err) == taskproto.RetryCapacity
}

// classifyLaunchErr extracts the AWS error code from a launch error and maps it
// to a taskproto RetryClass.
func classifyLaunchErr(err error) taskproto.RetryClass {
	var le *aws.LaunchError
	if errors.As(err, &le) {
		return taskproto.ClassifyLaunchError(le.Code)
	}
	return taskproto.RetryNone
}

// buildTaskLaunch assembles the launch config and launcher options for one task
// attempt: the wrapper (the command, staged I/O and completion record), the
// terminal-flush hook, and the storage script. Pure; the preflight and the real
// launch both call it, so they measure the same user data.
func buildTaskLaunch(spec *taskproto.TaskSpec, sized *taskproto.SizeResult, region, profile, resultsPrefix, runID, storageScript string) (aws.LaunchConfig, launcher.Options, error) {
	// GPU-capable if the spec asked for GPUs or the sized instance is a GPU family
	// (e.g. sized from families:["g5"] without an explicit gpus count) — same
	// classifier that selects the GPU DLAMI. Drives `--gpus all` + NVIDIA Container
	// Toolkit setup in the wrapper (spawn#601/#606).
	gpu := spec.Resources.GPUs > 0 || aws.DetectGPUInstance(sized.InstanceType)

	// One options value for both generators, so the wrapper and the flush hook
	// cannot disagree about the prefix or the run id they write (spawn#764).
	wrapperOpts := taskproto.WrapperOptions{
		ResultsPrefix: resultsPrefix,
		Region:        region,
		RunID:         runID,
		GPU:           gpu,
	}

	wrapper, err := taskproto.GenerateWrapper(spec, wrapperOpts)
	if err != nil {
		return aws.LaunchConfig{}, launcher.Options{}, fmt.Errorf("build task wrapper: %w", err)
	}

	// The terminal-flush hook (spawn#632). The wrapper's log upload and
	// completion-record write both sit after the user command, so a task killed by
	// its own TTL/cost limit mid-command left nothing at all to re-size from. This
	// script is installed root-owned and run by spored before it stops or
	// terminates — i.e. while the network is still up — and no-ops when the wrapper
	// already wrote the real record.
	flushHook, err := taskproto.GenerateFlushScript(spec, wrapperOpts)
	if err != nil {
		return aws.LaunchConfig{}, launcher.Options{}, fmt.Errorf("build terminal-flush hook: %w", err)
	}

	cfg := taskLaunchConfig(spec, sized, region, profile, wrapper)
	opts := launcher.Options{
		StorageScript:   storageScript,
		TaskFlushScript: flushHook,
	}
	return cfg, opts, nil
}

// taskUserDataSize builds the user data launcher.Provision will send for cfg
// and opts, the exact gzip+base64 bytes, and returns its decoded size (what
// EC2's 16 KiB limit counts). Over a limit it returns a TaskSpec-specific error.
func taskUserDataSize(cfg aws.LaunchConfig, opts launcher.Options) (int, error) {
	encoded, err := launcher.BuildUserData(cfg, opts)
	if err != nil {
		return 0, err
	}
	if err := aws.CheckUserDataSize(encoded); err != nil {
		return 0, taskUserDataTooLarge(err)
	}
	decoded, _ := aws.UserDataSize(encoded)
	return decoded, nil
}

// taskUserDataTooLarge explains a user-data size failure in TaskSpec terms.
func taskUserDataTooLarge(err error) error {
	var tooLarge *aws.UserDataTooLargeError
	if !errors.As(err, &tooLarge) {
		return err
	}
	return fmt.Errorf("this task cannot launch: the generated wrapper and bootstrap are %d bytes after gzip, and EC2 allows %d bytes of user data (spawn#1063).\n"+
		"The command, and every inputs/outputs entry, ride in the user data. Move long inline scripts out of `command` into a staged file under `inputs:`, "+
		"and stage many per-file inputs as one directory prefix", tooLarge.DecodedBytes, aws.MaxUserDataBytes)
}

// Placeholders for the values the preflight cannot know without an AWS call.
// Each has the same length as the real value, so the user data measures the
// same size to within gzip's handling of a few different characters; the real
// launch re-checks the exact bytes before RunInstances.
const (
	preflightAccountID = "000000000000"                         // 12 digits, like every AWS account ID
	preflightRunID     = "00000000-0000-0000-0000-000000000000" // uuid.NewString() length
	preflightProfile   = "spawn-task-preflight"                 // not in the user data
)

// taskUserDataPreflight measures the task's user data before any AWS call:
// the same assembly Launch performs (buildTaskLaunch, staging when stage is
// set, then launcher.BuildUserData), with placeholders for the account ID, run
// ID and any FSx names. It returns the decoded size and the size of the staged
// object (0 when nothing is staged), or an error when the spec cannot launch
// because the user data is over EC2's limit.
func taskUserDataPreflight(spec *taskproto.TaskSpec, sized *taskproto.SizeResult, region string, stage bool) (userData, staged int, err error) {
	resultsPrefix := taskproto.EffectiveResultsPrefix(spec, preflightAccountID, region)
	storageScript, err := taskStorageScriptWith(spec, region, func(id string) (string, string, error) {
		// fs-<17 hex>.fsx.<region>.amazonaws.com and an 8-character mount name,
		// the shapes FSx for Lustre returns.
		return id + ".fsx." + region + ".amazonaws.com", "abcdefgh", nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("build task storage: %w", err)
	}
	cfg, opts, err := buildTaskLaunch(spec, sized, region, preflightProfile, resultsPrefix, preflightRunID, storageScript)
	if err != nil {
		return 0, 0, err
	}
	if stage {
		staged = len(stageTaskFiles(cfg.JobArrayCommand, &opts, region, taskproto.StagingBucket(preflightAccountID, region), spec.TaskID).Body)
	}
	userData, err = taskUserDataSize(cfg, opts)
	return userData, staged, err
}
