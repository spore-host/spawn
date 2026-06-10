package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/winiso"
)

// `spawn image import` flags.
var (
	imageImportISO      string
	imageImportBucket   string
	imageImportRegion   string
	imageImportInfraArn string
	imageImportName     string
	imageImportVersion  string
	imageImportIndex    int64
	imageImportNoSecure bool
	imageImportExecRole string
	imageImportS3Key    string
	imageImportInstType string
	imageImportSubnet   string
	imageImportSGs      []string
	imageImportKeepISO  bool
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

	// Ensure the Image Builder service-linked execution role exists.
	prog.Start("Ensuring Image Builder service role")
	if err := awsClient.EnsureImageBuilderSLR(ctx); err != nil {
		prog.Error("Ensuring Image Builder service role", err)
		return err
	}
	prog.Complete("Ensuring Image Builder service role")

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
		ExecutionRole:                  imageImportExecRole,
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

	// Poll until the AMI is built. This is slow (the service installs Windows
	// from the ISO, stages drivers, snapshots) — minutes to tens of minutes.
	prog.Start("Building AMI (this can take 20-40 min)")
	amiID, err := awsClient.WaitForImage(ctx, imageImportRegion, buildArn, func(status string) {
		if !jsonOut {
			fmt.Fprintf(os.Stderr, "  image status: %s\n", status)
		}
	})
	if err != nil {
		prog.Error("Building AMI (this can take 20-40 min)", err)
		return err
	}
	prog.Complete("Building AMI (this can take 20-40 min)")

	// Tag it so connect/launch treat it as Windows.
	prog.Start("Tagging AMI spawn:os=windows")
	if err := awsClient.TagAMIWindows(ctx, imageImportRegion, amiID); err != nil {
		// Non-fatal: the AMI registers with Platform=windows anyway.
		prog.Error("Tagging AMI spawn:os=windows", err)
	} else {
		prog.Complete("Tagging AMI spawn:os=windows")
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

	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{
			"ami":                  amiID,
			"imageBuildVersionArn": buildArn,
			"uri":                  uri,
		})
	}
	fmt.Printf("\nImported AMI: %s\n", amiID)
	fmt.Printf("Launch it with:\n  spawn launch <name> --ami %s --os windows --ttl 4h\n", amiID)
	return nil
}

func init() {
	rootCmd.AddCommand(imageCmd)
	imageCmd.AddCommand(imageImportCmd)
	imageCmd.AddCommand(imageVerifyCmd)

	f := imageImportCmd.Flags()
	f.StringVar(&imageImportISO, "iso", "", "Windows 11 ISO: local path or s3://bucket/key.ISO (required)")
	f.StringVar(&imageImportBucket, "bucket", "", "S3 bucket to stage a local ISO in (default: managed spawn-iso-import-<account>-<region>, auto-created)")
	f.BoolVar(&imageImportKeepISO, "keep-iso", false, "Keep the staged ISO (and managed bucket) after the AMI is built; by default they are deleted")
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
}
