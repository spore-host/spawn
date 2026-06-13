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
	Long: `Populate a new EBS snapshot directly from a raw disk/filesystem image
using the EBS direct APIs — no EC2 instance and no attached volume. The result
is a snapshot you can attach with 'spawn launch --attach-volume snap-xxx:/mount'.

IMPORTANT: --from must be a RAW block image (a filesystem image whose bytes are
the block device verbatim), NOT an archive. A .tar.gz of files will be written
to the snapshot byte-for-byte and will not mount. To turn a directory or tarball
into a filesystem image you currently build the image yourself (e.g. on Linux:
'truncate -s 20G img.raw && mkfs.ext4 -d <dir> img.raw'), then pass img.raw here.
Building the filesystem for you is tracked as #147 Part B.

Examples:
  # From a local raw filesystem image:
  spawn snapshot create --from ./kraken2.raw --size 20 --name kraken2-k2pluspf

  # From a raw image already in S3:
  spawn snapshot create --from s3://my-bucket/kraken2.raw --size 20 \
    --name kraken2-k2pluspf --region us-east-1`,
	RunE: runSnapshotCreate,
}

func init() {
	rootCmd.AddCommand(snapshotCmd)
	snapshotCmd.AddCommand(snapshotCreateCmd)

	snapshotCreateCmd.Flags().StringVar(&snapshotFrom, "from", "", "Raw disk image source: a local path or s3://bucket/key (required)")
	snapshotCreateCmd.Flags().Int64Var(&snapshotSizeGiB, "size", 0, "Volume size in GiB the snapshot is built for; the image must fit (required)")
	snapshotCreateCmd.Flags().StringVar(&snapshotName, "name", "", "Name tag for the snapshot (also sets spawn:snapshot-name)")
	snapshotCreateCmd.Flags().StringVar(&snapshotDescription, "description", "", "Snapshot description")
	snapshotCreateCmd.Flags().StringVar(&snapshotRegion, "region", "", "AWS region (default: the configured region)")
	snapshotCreateCmd.Flags().BoolVar(&snapshotEncrypted, "encrypted", false, "Create an encrypted snapshot")
	snapshotCreateCmd.Flags().StringVar(&snapshotKMSKeyARN, "kms-key", "", "Customer-managed KMS key ARN for encryption (implies --encrypted)")
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

	tags := map[string]string{
		"spawn:managed": "true",
		"spawn:source":  "ebs-direct",
	}
	if snapshotName != "" {
		tags["Name"] = snapshotName
		tags["spawn:snapshot-name"] = snapshotName
	}

	src, err := awsClient.OpenImageSource(ctx, snapshotFrom, region)
	if err != nil {
		return err
	}
	defer src.Close()

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
	}, src)
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
