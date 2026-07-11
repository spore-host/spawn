package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	snapshotRegion      string
	snapshotFrom        string
	snapshotName        string
	snapshotSizeGiB     int64
	snapshotDescription string
	snapshotEncrypted   bool
	snapshotKMSKeyARN   string
	snapshotTempDir     string
	snapshotTags        []string
	snapshotMountRW     bool
)

var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Build and manage EBS data snapshots for --attach-volume",
	Long: `Create EBS snapshots from raw disk images without launching an EC2
instance, so large reference data (a Kraken2 DB, BLAST index, ML weights) can be
attached to spores via 'spawn launch --attach-volume' instead of being baked
into a custom AMI.`,
}

var snapshotCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Build an EBS snapshot from a raw disk image (no instance)",
	Long: `Populate a new EBS snapshot directly using the EBS direct APIs — no EC2
instance and no attached volume. The result is a snapshot you can attach with
'spawn launch --attach-volume snap-xxx:/mount'.

--from accepts any of:
  - a directory      → its contents are packed into an ext4 filesystem image
  - a .tar/.tar.gz/.tgz archive → unpacked into an ext4 filesystem image
  - a raw disk image → streamed verbatim (its bytes ARE the block device)

Directories and tarballs are converted to ext4 in-process (pure Go — no mkfs and
no builder instance), so this works the same from macOS, Linux, or Windows. The
ext4 filesystem is sized to the data and capped at --size.

For a directory or tarball source, the ext4 image is built in a local temp file
first (~the uncompressed data size — e.g. ~16 GB for a 16 GB DB) and removed when
done; ensure that much free space, or use --temp-dir to point at a roomier disk.
A raw image needs no scratch (it streams source → snapshot directly). Memory stays
low regardless of size (the upload streams blocks concurrently, never buffering
the whole image).

The upload sends the uncompressed image to AWS over your connection; for a large
DB over a slow uplink, run this from AWS CloudShell or a small in-region EC2
instance so the upload is AWS-internal.

Examples:
  # From a directory:
  spawn snapshot create --from ./kraken2-db/ --size 20 --name kraken2-k2pluspf

  # From a tarball (local or in S3):
  spawn snapshot create --from ./k2_pluspf.tar.gz --size 20 --name kraken2
  spawn snapshot create --from s3://genome-idx/k2_pluspf.tar.gz --size 20 \
    --name kraken2 --region us-east-1

  # From a raw filesystem image you already built:
  spawn snapshot create --from ./kraken2.raw --size 20 --name kraken2`,
	RunE: runSnapshotCreate,
}

var snapshotMountCmd = &cobra.Command{
	Use:   "mount <snapshot-id> <mount-point>",
	Short: "Create a volume from a snapshot and mount it on THIS EC2 instance",
	Long: `Convenience for the head-node side of the reference-data-volume recipe:
create an EBS volume from a snapshot, attach it to the instance this command runs
on, and mount it (read-only by default) at <mount-point>.

This only works when run ON an EC2 instance (it identifies itself via IMDS). It's
the one-command equivalent of: aws ec2 create-volume --snapshot-id … &&
aws ec2 attach-volume … && sudo mount -o ro …. Use it on a spawn head node (or
any EC2 box running 'nextflow run') so an nf-core pipeline's head-side db_path
validation finds the DB. Tasks don't need this — 'spawn launch --attach-volume'
mounts the volume on each task automatically.

Example:
  sudo spawn snapshot mount snap-0abc123 /opt/databases/kraken2`,
	Args: cobra.ExactArgs(2),
	RunE: runSnapshotMount,
}

func init() {
	rootCmd.AddCommand(snapshotCmd)
	snapshotCmd.AddCommand(snapshotCreateCmd)
	snapshotCmd.AddCommand(snapshotMountCmd)
	snapshotMountCmd.Flags().BoolVar(&snapshotMountRW, "rw", false, "Mount read-write (default: read-only — the right choice for shared reference data)")

	snapshotCreateCmd.Flags().StringVar(&snapshotFrom, "from", "", "Source: a directory, a .tar/.tar.gz/.tgz, or a raw disk image — local path or s3://bucket/key (required)")
	snapshotCreateCmd.Flags().Int64Var(&snapshotSizeGiB, "size", 0, "Volume size in GiB the snapshot is built for; the image must fit (required)")
	snapshotCreateCmd.Flags().StringVar(&snapshotName, "name", "", "Name tag for the snapshot (also sets spawn:snapshot-name)")
	snapshotCreateCmd.Flags().StringVar(&snapshotDescription, "description", "", "Snapshot description")
	snapshotCreateCmd.Flags().StringVar(&snapshotRegion, "region", "", "AWS region (default: the configured region)")
	snapshotCreateCmd.Flags().BoolVar(&snapshotEncrypted, "encrypted", false, "Create an encrypted snapshot")
	snapshotCreateCmd.Flags().StringVar(&snapshotKMSKeyARN, "kms-key", "", "Customer-managed KMS key ARN for encryption (implies --encrypted)")
	snapshotCreateCmd.Flags().StringVar(&snapshotTempDir, "temp-dir", "", "Directory for the temporary ext4 image built from a dir/tarball source (default: system temp). Point at a roomy disk for large data.")
	snapshotCreateCmd.Flags().StringArrayVar(&snapshotTags, "tag", nil, "Custom tag key=value to set on the snapshot (repeatable). Merged with the spawn:* baseline; cannot override a spawn: tag.")

	_ = snapshotCreateCmd.MarkFlagRequired("from")
	_ = snapshotCreateCmd.MarkFlagRequired("size")
}

func runSnapshotCreate(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if snapshotSizeGiB <= 0 {
		return fmt.Errorf("--size must be > 0 GiB")
	}
	if snapshotKMSKeyARN != "" {
		snapshotEncrypted = true
	}

	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	region := snapshotRegion
	if region == "" {
		region = awsClient.Config().Region
	}

	// Custom provenance tags (#161), merged UNDER the spawn:* baseline so a user
	// tag can't clobber spawn:managed / spawn:source etc.
	tags, err := parseKVTags(snapshotTags)
	if err != nil {
		return err
	}
	tags["spawn:managed"] = "true"
	tags["spawn:source"] = "ebs-direct"
	if snapshotName != "" {
		tags["Name"] = snapshotName
		tags["spawn:snapshot-name"] = snapshotName
	}

	// Resolve --from to a raw ext4-image reader: a raw image streams verbatim;
	// a directory or tar/tar.gz is converted to an ext4 image in-process (no
	// instance, no mkfs — pure Go) (#147). Cap the filesystem at the volume size.
	maxBytes := snapshotSizeGiB * 1024 * 1024 * 1024
	prepared, err := awsClient.PrepareSnapshotImage(ctx, snapshotFrom, region, maxBytes, snapshotTempDir)
	if err != nil {
		return err
	}
	defer prepared.Cleanup()

	if getOutputFormat() != "json" {
		fmt.Fprintf(cmd.OutOrStdout(), "Building EBS snapshot from %s (%d GiB volume) in %s...\n", snapshotFrom, snapshotSizeGiB, region)
	}

	res, err := awsClient.BuildSnapshotFromReader(ctx, aws.BuildSnapshotInput{
		Region:      region,
		Description: snapshotDescription,
		VolumeSize:  snapshotSizeGiB,
		Tags:        tags,
		Encrypted:   snapshotEncrypted,
		KMSKeyARN:   snapshotKMSKeyARN,
	}, prepared.Reader)
	if err != nil {
		return err
	}

	if getOutputFormat() == "json" {
		b, _ := json.MarshalIndent(map[string]any{
			"snapshotId": res.SnapshotID,
			"region":     region,
			"blocksPut":  res.BlocksPut,
			"bytesRead":  res.BytesRead,
			"blockSize":  res.BlockSize,
		}, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Created %s in %s (%d blocks, %d bytes)\n", res.SnapshotID, region, res.BlocksPut, res.BytesRead)
	fmt.Fprintf(cmd.OutOrStdout(), "  Attach it:  spawn launch <name> --attach-volume %s:/mount/point:ro\n", res.SnapshotID)
	return nil
}

func runSnapshotMount(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	snapshotID := args[0]
	mountPoint := args[1]

	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Creating a volume from %s and attaching it to this instance...\n", snapshotID)
	res, err := awsClient.AttachSnapshotToSelf(ctx, snapshotID)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "  volume %s attached as %s on %s\n", res.VolumeID, res.DeviceName, res.InstanceID)

	// Resolve the live device: on Nitro the requested /dev/sdf is remapped to an
	// NVMe device. AL2023's ec2-utils udev rules create a /dev/sdf symlink; prefer
	// it, falling back to the bare name. (Mirrors the launch-time mount logic.)
	dev := res.DeviceName
	if _, statErr := os.Stat(dev); statErr != nil {
		// Symlink not present (older udev / different naming) — let mount try the
		// requested name anyway; surface a clear hint if it fails.
		fmt.Fprintf(cmd.OutOrStderr(), "  note: %s not present yet; the kernel may expose it as an NVMe device.\n", dev)
	}

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("create mount point %s: %w", mountPoint, err)
	}

	// Snapshot-backed volume already has a filesystem — never reformat.
	mountArgs := []string{"-o", "noatime"}
	if !snapshotMountRW {
		mountArgs = []string{"-o", "ro,noatime"}
	}
	mountArgs = append(mountArgs, dev, mountPoint)
	mountCmd := exec.CommandContext(ctx, "mount", mountArgs...) // #nosec G204 -- dev/mountPoint are this command's own resolved values
	if out, mErr := mountCmd.CombinedOutput(); mErr != nil {
		return fmt.Errorf("mount %s at %s failed: %w\n%s\n(if %s isn't the live device, the Nitro NVMe remap may have moved it — check `lsblk`)", dev, mountPoint, mErr, string(out), dev)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Mounted %s at %s (%s)\n", res.VolumeID, mountPoint, map[bool]string{true: "read-write", false: "read-only"}[snapshotMountRW])
	return nil
}
