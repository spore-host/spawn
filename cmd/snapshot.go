package cmd

import (
	"context"
	"encoding/json"
	"fmt"

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
	snapshotOutput      string
	snapshotTempDir     string
	snapshotTags        []string
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

func init() {
	rootCmd.AddCommand(snapshotCmd)
	snapshotCmd.AddCommand(snapshotCreateCmd)

	snapshotCreateCmd.Flags().StringVar(&snapshotFrom, "from", "", "Source: a directory, a .tar/.tar.gz/.tgz, or a raw disk image — local path or s3://bucket/key (required)")
	snapshotCreateCmd.Flags().Int64Var(&snapshotSizeGiB, "size", 0, "Volume size in GiB the snapshot is built for; the image must fit (required)")
	snapshotCreateCmd.Flags().StringVar(&snapshotName, "name", "", "Name tag for the snapshot (also sets spawn:snapshot-name)")
	snapshotCreateCmd.Flags().StringVar(&snapshotDescription, "description", "", "Snapshot description")
	snapshotCreateCmd.Flags().StringVar(&snapshotRegion, "region", "", "AWS region (default: the configured region)")
	snapshotCreateCmd.Flags().BoolVar(&snapshotEncrypted, "encrypted", false, "Create an encrypted snapshot")
	snapshotCreateCmd.Flags().StringVar(&snapshotKMSKeyARN, "kms-key", "", "Customer-managed KMS key ARN for encryption (implies --encrypted)")
	snapshotCreateCmd.Flags().StringVar(&snapshotTempDir, "temp-dir", "", "Directory for the temporary ext4 image built from a dir/tarball source (default: system temp). Point at a roomy disk for large data.")
	snapshotCreateCmd.Flags().StringArrayVar(&snapshotTags, "tag", nil, "Custom tag key=value to set on the snapshot (repeatable). Merged with the spawn:* baseline; cannot override a spawn: tag.")
	snapshotCreateCmd.Flags().StringVarP(&snapshotOutput, "output", "o", "text", "Output format: text or json")

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

	if snapshotOutput != "json" {
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

	if snapshotOutput == "json" {
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
