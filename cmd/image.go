package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/winiso"
)

// `spawn image import` flags.
var (
	imageImportISO         string
	imageImportBucket      string
	imageImportRegion      string
	imageImportInfraArn    string
	imageImportName        string
	imageImportVersion     string
	imageImportIndex       int64
	imageImportNoSecure    bool
	imageImportExecRole    string
	imageImportS3Key       string
	imageImportInstType    string
	imageImportSubnet      string
	imageImportSGs         []string
	imageImportKeepISO     bool
	imageImportWaitMin     int
	imageImportNoWarm      bool
	imageImportWarmType    string
	imageImportWarmTimeout int
)

var imageCmd = &cobra.Command{
	Use:   "image",
	Short: "Build and manage custom machine images",
	Long: `Create custom AMIs that spawn can launch.

Currently supports importing a Windows 11 ISO into an AMI via AWS EC2 Image
Builder's managed import-disk-image workflow (drivers, EC2Launch, SSM agent and
Defender are pre-staged automatically). See infra/amis/windows/README.md.`,
}

var imageImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import a Windows 11 ISO into an AMI via EC2 Image Builder",
	Long: `Convert a Windows 11 ISO into an AMI using EC2 Image Builder's managed
import-disk-image workflow, then tag it so 'spawn launch --os windows' can use it.

The ISO must be a SUPPORTED, NON-evaluation Windows 11 Enterprise image
(23H2 / 24H2 / 25H2 x64) obtained from the Microsoft 365 admin center. Evaluation,
Media-Creation-Tool, and LTSC ISOs are rejected by the service. Bring your own
Microsoft license (BYOL).

The command self-provisions the IAM roles and Image Builder infrastructure
configuration it needs (idempotent); pass --infra-config-arn only to reuse an
existing/custom one. See infra/amis/windows/README.md.

Examples:
  # Local ISO — staging bucket + infra auto-provisioned, nothing to pre-create:
  spawn image import --iso ./Win11_25H2_Enterprise.iso \
    --name win11-25h2 --image-index 3

  # ISO already in S3 (uppercase .ISO key required by the service):
  spawn image import --iso s3://my-bucket/Win11_25H2_Enterprise.ISO \
    --name win11-25h2`,
	RunE: runImageImport,
}

var imageVerifyCmd = &cobra.Command{
	Use:   "verify <path-to.iso>",
	Short: "Check whether a Windows ISO is acceptable to EC2 Image Builder",
	Long: `Inspect a local Windows installation ISO and report which editions it
contains and whether 'spawn image import' (EC2 Image Builder import-disk-image)
will accept it — before you spend a real, paid build.

import-disk-image accepts only Windows 11 Enterprise (23H2/24H2/25H2, x64),
non-Evaluation. This reads the ISO's install.wim metadata directly (no mount, no
external tools) and prints each edition with its image index, flags the one to
use, and gives a clear ACCEPTED/REJECTED verdict.

Examples:
  spawn image verify "/Volumes/External HD/Win11_Enterprise_25H2.iso"
  spawn image verify win11.iso -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runImageVerify,
}

var imageStatusCmd = &cobra.Command{
	Use:   "status <image-build-version-arn>",
	Short: "Check the status of an in-progress image import",
	Long: `Report the current state of an EC2 Image Builder image build started by
'spawn image import' (without --wait). Prints PENDING/BUILDING/.../AVAILABLE/FAILED
and, once available, the output AMI id.

Example:
  spawn image status arn:aws:imagebuilder:us-east-1:123456789012:image/win11-25h2/1.0.0/1`,
	Args: cobra.ExactArgs(1),
	RunE: runImageStatus,
}

func runImageStatus(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}
	st, err := awsClient.GetImageStatus(ctx, imageImportRegion, args[0])
	if err != nil {
		return err
	}
	if getOutputFormat() == "json" {
		return json.NewEncoder(os.Stdout).Encode(st)
	}
	fmt.Printf("status: %s\n", st.Status)
	if st.Reason != "" {
		fmt.Printf("reason: %s\n", st.Reason)
	}
	if st.AMI != "" {
		fmt.Printf("ami:    %s\n", st.AMI)
	}
	return nil
}

func runImageVerify(cmd *cobra.Command, args []string) error {
	path := args[0]
	rep, err := winiso.InspectFile(path)
	if err != nil {
		return err
	}

	if getOutputFormat() == "json" {
		return json.NewEncoder(os.Stdout).Encode(rep)
	}

	fmt.Printf("ISO: %s\n\n", path)
	fmt.Printf("  %-3s  %-34s  %-6s  %-7s  %s\n", "IDX", "EDITION", "ARCH", "BUILD", "import-disk-image")
	fmt.Printf("  %-3s  %-34s  %-6s  %-7s  %s\n", "---", "----------------------------------", "------", "-------", "----------------")
	for _, e := range rep.Editions {
		mark := "—"
		if e.Supported {
			mark = "✓ supported"
		} else if e.Eval {
			mark = "✗ Evaluation"
		}
		fmt.Printf("  %-3d  %-34s  %-6s  %-7s  %s\n", e.Index, e.Name, e.Arch, e.Build, mark)
	}

	fmt.Printf("\n%s\n", rep.Summary)
	if rep.Acceptable {
		fmt.Printf("\nNext:\n  spawn image import --iso %q --bucket <s3-bucket> \\\n    --name win11-25h2 --image-index %d\n",
			path, rep.RecommendedIndex)
		return nil
	}
	// Non-zero exit so scripts can gate on it.
	return fmt.Errorf("ISO is not acceptable to import-disk-image")
}

// validateImageImportFlags checks the required/consistent flags before any AWS
// call, so it's unit-testable without credentials. --infra-config-arn is
// intentionally NOT required (it is self-provisioned when omitted).
func validateImageImportFlags(iso, name, bucket string) error {
	if iso == "" {
		return fmt.Errorf("--iso is required (local path or s3://bucket/key.ISO)")
	}
	if name == "" {
		return fmt.Errorf("--name is required (the Image Builder image resource name)")
	}
	if strings.HasPrefix(iso, "s3://") && !strings.HasSuffix(iso, ".ISO") {
		return fmt.Errorf("the S3 object key must end in an uppercase .ISO extension; got %q", iso)
	}
	// A local ISO needs no --bucket: a managed staging bucket is created if one
	// isn't supplied.
	_ = bucket
	return nil
}

func runImageImport(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	if err := validateImageImportFlags(imageImportISO, imageImportName, imageImportBucket); err != nil {
		return err
	}

	jsonOut := getOutputFormat() == "json"
	var prog *progress.Progress
	if jsonOut {
		prog = progress.NewQuietProgress()
	} else {
		prog = progress.NewProgress()
	}

	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	// Resolve the S3 URI: either the ISO is already in S3, or we upload a local
	// file to a staging bucket. import-disk-image requires an uppercase .ISO key.
	// Track what we staged so we can clean it up after the AMI is built.
	var uri, stagedBucket, stagedKey string
	stagedBucketIsManaged := false
	if strings.HasPrefix(imageImportISO, "s3://") {
		uri = imageImportISO
	} else {
		// Resolve the staging bucket. If --bucket was omitted, use (and create)
		// a managed, account+region-scoped bucket so the user doesn't have to
		// name or pre-create one — matching spawn's other managed buckets
		// (e.g. spawn-schedules-<acct>-<region>).
		bucket := imageImportBucket
		if bucket == "" {
			acct, aerr := awsClient.GetAccountID(ctx)
			if aerr != nil {
				return fmt.Errorf("resolve account id for the managed ISO bucket: %w", aerr)
			}
			bucket = fmt.Sprintf("spawn-iso-import-%s-%s", acct, imageImportRegion)
			stagedBucketIsManaged = true
		}
		prog.Start("Preparing the ISO staging bucket")
		if err := awsClient.CreateS3BucketIfNotExists(ctx, bucket, imageImportRegion); err != nil {
			prog.Error("Preparing the ISO staging bucket", err)
			return err
		}
		prog.Complete("Preparing the ISO staging bucket")

		key := imageImportS3Key
		if key == "" {
			base := filepath.Base(imageImportISO)
			// Normalize the extension to uppercase .ISO regardless of input case.
			key = strings.TrimSuffix(base, filepath.Ext(base)) + ".ISO"
		}
		prog.Start("Uploading ISO to S3")
		if err := awsClient.UploadISOToS3(ctx, imageImportRegion, bucket, key, imageImportISO); err != nil {
			prog.Error("Uploading ISO to S3", err)
			return err
		}
		prog.Complete("Uploading ISO to S3")
		uri = fmt.Sprintf("s3://%s/%s", bucket, key)
		stagedBucket, stagedKey = bucket, key
	}

	// Ensure the Image Builder service-linked execution role exists, and capture
	// its full ARN. The SLR lives under aws-service-role/imagebuilder.amazonaws.com/,
	// so the bare name "AWSServiceRoleForImageBuilder" resolves to the wrong ARN
	// and the build fails with "Unable to perform STS Assume role" — use the ARN.
	prog.Start("Ensuring Image Builder service role")
	slrArn, err := awsClient.EnsureImageBuilderSLR(ctx)
	if err != nil {
		prog.Error("Ensuring Image Builder service role", err)
		return err
	}
	prog.Complete("Ensuring Image Builder service role")
	// Use the resolved SLR ARN unless the user supplied a custom execution role.
	execRole := imageImportExecRole
	if !cmd.Flags().Changed("execution-role") {
		execRole = slrArn
	}

	// Resolve the infrastructure configuration: use the provided ARN, or
	// self-provision a default one (IAM role + instance profile + infra config).
	infraArn := imageImportInfraArn
	if infraArn == "" {
		prog.Start("Ensuring import infrastructure")
		infraArn, err = awsClient.EnsureImportInfrastructure(ctx, aws.EnsureImportInfrastructureInput{
			Region:          imageImportRegion,
			InstanceType:    imageImportInstType,
			SubnetID:        imageImportSubnet,
			SecurityGroupID: imageImportSGs,
		})
		if err != nil {
			prog.Error("Ensuring import infrastructure", err)
			return err
		}
		prog.Complete("Ensuring import infrastructure")
	}

	// Kick off the import.
	in := aws.ImportWindowsISOInput{
		Region:                         imageImportRegion,
		Name:                           imageImportName,
		SemanticVersion:                imageImportVersion,
		URI:                            uri,
		ExecutionRole:                  execRole,
		InfrastructureConfigurationArn: infraArn,
	}
	if cmd.Flags().Changed("image-index") {
		idx := imageImportIndex
		in.ImageIndex = &idx
	}
	if imageImportNoSecure {
		secure := false
		in.SecureBoot = &secure
	}

	prog.Start("Starting import-disk-image")
	buildArn, err := awsClient.ImportWindowsISO(ctx, in)
	if err != nil {
		prog.Error("Starting import-disk-image", err)
		return err
	}
	prog.Complete("Starting import-disk-image")

	// The warm-AMI stage (#98) needs the base AMI to exist, so it requires
	// waiting. When warm is enabled (the default) and the user didn't ask to
	// wait, wait anyway with a generous default rather than returning async.
	warm := !imageImportNoWarm
	if warm && imageImportWaitMin <= 0 {
		imageImportWaitMin = 60
	}

	// Async by default (like `spawn create-ami`) when warm is OFF: the build runs
	// 20-40 min in Image Builder. Without --wait, return now with the build ARN
	// and how to check on it; the staged ISO is left in place (cleanup needs the
	// build to finish, which we're not waiting for).
	if imageImportWaitMin <= 0 {
		if jsonOut {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{
				"imageBuildVersionArn": buildArn,
				"uri":                  uri,
				"status":               "building",
			})
		}
		fmt.Printf("\nImport started (building, ~20-40 min). Not waiting (use --wait to block).\n")
		fmt.Printf("  build:  %s\n", buildArn)
		fmt.Printf("  check:  spawn image status %s\n", buildArn)
		if stagedKey != "" {
			fmt.Printf("  note:   staged ISO s3://%s/%s is kept; delete it after the AMI builds (or re-run with --wait to auto-clean).\n", stagedBucket, stagedKey)
		}
		return nil
	}

	// Poll until the AMI is built, up to the requested timeout. This is slow (the
	// service installs Windows from the ISO, stages drivers, snapshots). The wait
	// is interruptible: Ctrl-C is handled like a timeout — the build keeps running
	// in Image Builder, so we detach cleanly (print how to resume) rather than
	// leaving the user wondering, and exit non-zero so it's catchable.
	waitCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	waitLabel := fmt.Sprintf("Building AMI (waiting up to %dm; Ctrl-C to detach)", imageImportWaitMin)
	prog.Start(waitLabel)
	amiID, err := awsClient.WaitForImage(waitCtx, imageImportRegion, buildArn,
		time.Duration(imageImportWaitMin)*time.Minute, func(status string) {
			if !jsonOut {
				fmt.Fprintf(os.Stderr, "  image status: %s\n", status)
			}
		})
	if err != nil {
		prog.Error(waitLabel, err)
		// A timeout or a Ctrl-C/SIGTERM is NOT a build failure — the build keeps
		// running in Image Builder. Surface the async info and exit with a distinct,
		// catchable error so scripts (and humans) can tell "still building / detached"
		// from "failed".
		detached := errors.Is(err, aws.ErrWaitTimeout) || errors.Is(err, context.Canceled) || waitCtx.Err() != nil
		if detached {
			reason := fmt.Sprintf("still building after %dm", imageImportWaitMin)
			if !errors.Is(err, aws.ErrWaitTimeout) {
				reason = "detached (interrupted)"
			}
			if jsonOut {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
					"imageBuildVersionArn": buildArn, "uri": uri, "status": "building",
				})
			} else {
				fmt.Printf("\nImage build %s — not failed, still running in Image Builder.\n", reason)
				fmt.Printf("  check: spawn image status %s\n", buildArn)
				if stagedKey != "" {
					fmt.Printf("  note:  staged ISO s3://%s/%s kept (build unfinished).\n", stagedBucket, stagedKey)
				}
			}
			return fmt.Errorf("image build not finished (%s): %w", reason, err)
		}
		return err
	}
	prog.Complete(waitLabel)

	// Tag it so connect/launch treat it as Windows.
	prog.Start("Tagging AMI spawn:os=windows")
	if err := awsClient.TagAMIWindows(ctx, imageImportRegion, amiID); err != nil {
		// Non-fatal: the AMI registers with Platform=windows anyway.
		prog.Error("Tagging AMI spawn:os=windows", err)
	} else {
		prog.Complete("Tagging AMI spawn:os=windows")
	}

	// Warm-AMI stage (#98): by default, build a "warm" AMI from the imported base
	// so future launches skip the ~30 min Sysprep first boot. Launch a seed off
	// the base, wait until it's fully booted + SSM-registered (so spored + the
	// SSM agent are baked in), image it, then terminate the seed.
	warmAMI := ""
	if warm {
		warmAMI, err = buildWarmAMI(ctx, awsClient, imageImportRegion, imageImportName, imageImportVersion, amiID, prog)
		if err != nil {
			// Non-fatal: the base AMI is built and usable; warn and continue so the
			// user still gets the base AMI rather than losing the whole import.
			fmt.Fprintf(os.Stderr, "\n⚠️  warm-AMI build failed: %v\n   The base AMI %s is still usable (it just boots slowly the first time).\n", err, amiID)
		}
	}

	// Clean up the staged ISO now that the AMI exists — it's a transient artifact
	// (the AMI is self-contained). Only for an ISO we uploaded this run, and only
	// unless --keep-iso. If we created the managed bucket, remove it too when it's
	// left empty.
	if stagedKey != "" && !imageImportKeepISO {
		prog.Start("Cleaning up the staged ISO")
		if err := awsClient.DeleteISOFromS3(ctx, imageImportRegion, stagedBucket, stagedKey, stagedBucketIsManaged); err != nil {
			// Non-fatal: the AMI is built; a leftover ISO is just cost, not breakage.
			prog.Error("Cleaning up the staged ISO", err)
		} else {
			prog.Complete("Cleaning up the staged ISO")
		}
	}

	// The AMI to recommend launching: the warm one if we built it, else the base.
	launchAMI := amiID
	if warmAMI != "" {
		launchAMI = warmAMI
	}

	if jsonOut {
		out := map[string]string{
			"ami":                  launchAMI,
			"baseAmi":              amiID,
			"imageBuildVersionArn": buildArn,
			"uri":                  uri,
		}
		if warmAMI != "" {
			out["warmAmi"] = warmAMI
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("\nImported base AMI: %s\n", amiID)
	if warmAMI != "" {
		fmt.Printf("Warm AMI (fast boot, recommended): %s\n", warmAMI)
	}
	fmt.Printf("Launch it with:\n  spawn launch <name> --ami %s --os windows --ttl 4h\n", launchAMI)
	return nil
}

// buildWarmAMI launches a seed instance from the imported base AMI, waits until
// it has fully finished first boot (SSM Online — proves EC2Launch user-data ran
// and spored + the SSM agent are installed/running), creates a new AMI from it,
// tags it, and terminates the seed. The seed is always terminated, even on
// error. Returns the warm AMI id. (#98)
func buildWarmAMI(ctx context.Context, awsClient *aws.Client, region, name, version, baseAMI string, prog *progress.Progress) (string, error) {
	seedType := imageImportWarmType
	if err := guardWindowsInstanceType("windows", seedType); err != nil {
		return "", err
	}

	plat, err := platform.Detect()
	if err != nil {
		return "", fmt.Errorf("detect platform: %w", err)
	}

	// RSA key (Windows) — reused/imported idempotently.
	keyName, err := setupSSHKey(ctx, awsClient, region, baseAMI, plat)
	if err != nil {
		return "", fmt.Errorf("set up key for warm seed: %w", err)
	}

	// Windows SG (RDP+SSH); reuse the launch helper.
	vpcID, err := awsClient.GetDefaultVPC(ctx, region)
	if err != nil {
		return "", fmt.Errorf("get default VPC: %w", err)
	}
	sgID, err := awsClient.CreateOrGetWindowsSecurityGroup(ctx, region, vpcID, fmt.Sprintf("spawn-windows-%s-warmseed", name), "")
	if err != nil {
		return "", fmt.Errorf("create warm-seed security group: %w", err)
	}

	// Windows user-data (installs spored + ensures SSM agent — baked into warm AMI).
	pubKey, err := spawnPublicKeyForUserData(plat, keyName)
	if err != nil {
		return "", fmt.Errorf("read public key for warm-seed user-data: %w", err)
	}
	userDataScript, err := buildWindowsUserData(string(pubKey))
	if err != nil {
		return "", fmt.Errorf("build warm-seed user-data: %w", err)
	}
	// Launch() passes UserData straight to RunInstances, so encode it ourselves
	// (the cobra launch path does this via encodeUserDataForOS too).
	userData := encodeUserDataForOS(userDataScript, "windows")

	prog.Start("Launching warm-build seed instance")
	res, err := awsClient.Launch(ctx, aws.LaunchConfig{
		InstanceType:     seedType,
		Region:           region,
		AMI:              baseAMI,
		KeyName:          keyName,
		SecurityGroupIDs: []string{sgID},
		TargetOS:         "windows",
		UserData:         userData,
		Name:             name + "-warmseed",
		TTL:              "3h", // safety backstop: reaper kills the seed if we crash
		Tags:             map[string]string{"spawn:purpose": "warm-build-seed"},
	})
	if err != nil {
		prog.Error("Launching warm-build seed instance", err)
		return "", fmt.Errorf("launch warm seed: %w", err)
	}
	seedID := res.InstanceID
	prog.Complete("Launching warm-build seed instance")

	// Always terminate the seed, even on failure below.
	defer func() {
		if terr := awsClient.Terminate(context.Background(), region, seedID); terr != nil {
			fmt.Fprintf(os.Stderr, "⚠️  could not terminate warm-build seed %s: %v (terminate it manually)\n", seedID, terr)
		}
	}()

	// The imported AMI's Sysprep first boot + EC2Launch user-data (which installs
	// spored + ensures the SSM agent, #95) can take ~35-50 min; SSM registration
	// is the long pole. Budget 75 min so we don't clip a still-progressing boot
	// (a live run failed at a too-tight 40 min). Overridable via --warm-timeout.
	waitLabel := "Waiting for seed first boot to finish (SSM Online; can take 35-50 min)"
	prog.Start(waitLabel)
	if err := awsClient.WaitForRunning(ctx, region, seedID, 5*time.Minute); err != nil {
		prog.Error(waitLabel, err)
		return "", fmt.Errorf("warm seed never reached running: %w", err)
	}
	if err := awsClient.WaitForSSMOnline(ctx, region, seedID, time.Duration(imageImportWarmTimeout)*time.Minute); err != nil {
		prog.Error(waitLabel, err)
		return "", fmt.Errorf("warm seed never registered with SSM (first boot incomplete): %w", err)
	}
	prog.Complete(waitLabel)

	prog.Start("Creating warm AMI from seed")
	warmName := fmt.Sprintf("%s-warm-%s", name, version)
	warmID, err := awsClient.CreateAMI(ctx, region, aws.CreateAMIInput{
		InstanceID:  seedID,
		Name:        warmName,
		Description: fmt.Sprintf("spawn warm AMI from %s (fast-boot, first boot already done)", baseAMI),
		NoReboot:    false, // let Windows flush cleanly for a consistent image
	})
	if err != nil {
		prog.Error("Creating warm AMI from seed", err)
		return "", fmt.Errorf("create warm AMI: %w", err)
	}
	if err := awsClient.WaitForAMI(ctx, region, warmID, 30*time.Minute); err != nil {
		prog.Error("Creating warm AMI from seed", err)
		return "", fmt.Errorf("warm AMI never became available: %w", err)
	}
	if err := awsClient.TagAMIWindowsWarm(ctx, region, warmID, baseAMI); err != nil {
		// Non-fatal: the AMI exists; tags are best-effort.
		fmt.Fprintf(os.Stderr, "⚠️  could not tag warm AMI %s: %v\n", warmID, err)
	}
	prog.Complete("Creating warm AMI from seed")
	return warmID, nil
}

func init() {
	rootCmd.AddCommand(imageCmd)
	imageCmd.AddCommand(imageImportCmd)
	imageCmd.AddCommand(imageVerifyCmd)
	imageCmd.AddCommand(imageStatusCmd)
	imageStatusCmd.Flags().StringVar(&imageImportRegion, "region", "us-east-1", "AWS region of the image build")

	f := imageImportCmd.Flags()
	f.StringVar(&imageImportISO, "iso", "", "Windows 11 ISO: local path or s3://bucket/key.ISO (required)")
	f.StringVar(&imageImportBucket, "bucket", "", "S3 bucket to stage a local ISO in (default: managed spawn-iso-import-<account>-<region>, auto-created)")
	f.BoolVar(&imageImportKeepISO, "keep-iso", false, "Keep the staged ISO (and managed bucket) after the AMI is built; by default they are deleted (only applies with --wait)")
	// --wait blocks until the AMI is built (then tags + cleans up). Bare --wait
	// waits up to 60 min; --wait=N waits N minutes. On timeout the build keeps
	// running and the command exits non-zero (distinct from success/failure) so
	// scripts can branch on "still building".
	f.IntVar(&imageImportWaitMin, "wait", 0, "Wait up to N minutes for the AMI (bare --wait = 60); 0 = return immediately (async)")
	imageImportCmd.Flags().Lookup("wait").NoOptDefVal = "60"
	f.StringVar(&imageImportS3Key, "s3-key", "", "S3 object key for the uploaded ISO (default: derived from filename, .ISO)")
	f.StringVar(&imageImportRegion, "region", "us-east-1", "AWS region for the import build")
	f.StringVar(&imageImportInfraArn, "infra-config-arn", "", "Image Builder infrastructure configuration ARN (optional; self-provisioned if omitted)")
	f.StringVar(&imageImportName, "name", "", "Image Builder image resource name (required)")
	f.StringVar(&imageImportVersion, "version", "1.0.0", "Semantic version for the output image (major.minor.patch)")
	f.Int64Var(&imageImportIndex, "image-index", 1, "1-based edition index in a multi-edition ISO")
	f.BoolVar(&imageImportNoSecure, "no-secure-boot", false, "Disable Secure Boot on the output AMI")
	f.StringVar(&imageImportExecRole, "execution-role", "AWSServiceRoleForImageBuilder", "IAM execution role name or ARN")
	// Used only when --infra-config-arn is omitted (self-provisioning the build infra):
	f.StringVar(&imageImportInstType, "instance-type", "m6i.large", "Build instance type (when self-provisioning infra)")
	f.StringVar(&imageImportSubnet, "subnet-id", "", "Subnet for the build instance (when self-provisioning infra)")
	f.StringArrayVar(&imageImportSGs, "security-group-id", nil, "Security group for the build instance, repeatable (when self-provisioning infra)")
	// Warm-AMI stage (#98): on by default — build a fast-boot AMI from the import
	// so future launches skip the ~30 min Sysprep first boot.
	f.BoolVar(&imageImportNoWarm, "no-warm", false, "Skip building the warm (fast-boot) AMI; produce only the raw imported base AMI")
	f.StringVar(&imageImportWarmType, "warm-instance-type", "m7i.xlarge", "Instance type for the warm-build seed (non-burstable; Windows)")
	f.IntVar(&imageImportWarmTimeout, "warm-timeout", 75, "Minutes to wait for the warm seed to finish first boot (SSM Online) before giving up")
}
