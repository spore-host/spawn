package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/staging"
)

var (
	stageRegions       string
	stageDestination   string
	stageSweepID       string
	estimateDataSizeGB int
	estimateInstances  int
	estimateRegions    string
	stageDeleteYes     bool
)

// stageCmd represents the stage command
var stageCmd = &cobra.Command{
	Use:   "stage",
	Short: "Manage data staging for multi-region parameter sweeps",
	Long: `Stage data to regional S3 buckets for efficient multi-region parameter sweeps.

Data staging enables cost-optimized data movement by:
- Replicating data once to regional buckets
- Allowing instances to download from local region (free)
- Avoiding repeated cross-region transfers ($0.09/GB)

Cost savings example:
  100GB dataset, 2 regions, 10 instances each:
  - Without staging: $90.00 (cross-region transfers)
  - With staging: $6.60 (one-time replication)
  - Savings: $85.70 (93% reduction)

Commands:
  spawn stage upload <path>     Stage data to regional buckets
  spawn stage list              List staged data
  spawn stage estimate          Estimate staging cost savings
  spawn stage delete <id>       Delete staged data
`,
}

// stageUploadCmd uploads data to regional buckets
var stageUploadCmd = &cobra.Command{
	Use:   "upload <local-path>",
	Short: "Upload data to regional staging buckets",
	Long: `Upload a file or directory to spawn data staging buckets across regions.

The data will be:
1. Uploaded to the primary region
2. Replicated to additional regions
3. Tracked in DynamoDB for lifecycle management
4. Automatically deleted after 7 days

Example:
  spawn stage upload ./reference-genome.fasta \
    --regions us-east-1,us-west-2 \
    --dest /mnt/data/reference.fasta \
    --sweep-id sweep-abc123
`,
	Args: cobra.ExactArgs(1),
	RunE: runStageUpload,
}

// stageListCmd lists staged data
var stageListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all staged data",
	Long:  `List all data currently staged in regional buckets.`,
	RunE:  runStageList,
}

// stageEstimateCmd estimates staging costs
var stageEstimateCmd = &cobra.Command{
	Use:   "estimate",
	Short: "Estimate cost savings from data staging",
	Long: `Estimate the cost difference between:
  A) Single-region storage with cross-region transfers
  B) Regional replication with local transfers

This helps determine if staging is cost-effective for your workload.

Example:
  spawn stage estimate \
    --data-size-gb 100 \
    --instances 10 \
    --regions us-east-1,us-west-2
`,
	RunE: runStageEstimate,
}

// stageDeleteCmd deletes staged data
var stageDeleteCmd = &cobra.Command{
	Use:   "delete <staging-id>",
	Short: "Delete staged data",
	Long:  `Delete staged data from all regions and remove metadata.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runStageDelete,
}

func init() {
	rootCmd.AddCommand(stageCmd)
	stageCmd.AddCommand(stageUploadCmd)
	stageCmd.AddCommand(stageListCmd)
	stageCmd.AddCommand(stageEstimateCmd)
	stageCmd.AddCommand(stageDeleteCmd)
	stageDeleteCmd.Flags().BoolVarP(&stageDeleteYes, "yes", "y", false, "Skip the confirmation prompt")

	// Upload flags
	stageUploadCmd.Flags().StringVar(&stageRegions, "regions", "us-east-1,us-west-2",
		"Comma-separated list of regions to replicate to")
	stageUploadCmd.Flags().StringVar(&stageDestination, "dest", "",
		"Destination path on instances (default: /mnt/data/<filename>)")
	stageUploadCmd.Flags().StringVar(&stageSweepID, "sweep-id", "",
		"Associate with sweep ID for tracking")

	// Estimate flags
	stageEstimateCmd.Flags().IntVar(&estimateDataSizeGB, "data-size-gb", 100,
		"Dataset size in GB")
	stageEstimateCmd.Flags().IntVar(&estimateInstances, "instances", 10,
		"Number of instances per region")
	stageEstimateCmd.Flags().StringVar(&estimateRegions, "regions", "us-east-1,us-west-2",
		"Comma-separated list of regions")
}

func runStageUpload(cmd *cobra.Command, args []string) error {
	localPath := args[0]
	ctx := context.Background()

	// Verify file exists
	if _, err := os.Stat(localPath); err != nil {
		return fmt.Errorf("file not found: %w", err)
	}

	// Parse regions
	regions := parseRegionList(stageRegions)
	if len(regions) == 0 {
		return fmt.Errorf("no regions specified")
	}

	// Generate staging ID
	stagingID := staging.GenerateStagingID()

	// Load AWS config for primary region (use infra account for data staging)
	awsConfig, err := spawnconfig.LoadInfraAWSConfig(ctx, regions[0])
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Get account ID
	accountID, err := getAccountID(ctx)
	if err != nil {
		return fmt.Errorf("get account ID: %w", err)
	}

	// Create staging client
	stagingClient := staging.NewClient(awsConfig, accountID)

	// Upload to primary region
	primaryRegion := regions[0]
	primaryBucket := fmt.Sprintf("spawn-data-%s", primaryRegion)
	fmt.Fprintf(os.Stderr, "Uploading %s to %s...\n", filepath.Base(localPath), primaryRegion)

	s3Key, size, sha256sum, err := stagingClient.UploadToPrimaryRegion(ctx, localPath, stagingID, primaryRegion)
	if err != nil {
		return fmt.Errorf("upload to primary region: %w", err)
	}

	fmt.Fprintf(os.Stderr, "  ✓ Uploaded %d bytes (SHA256: %s...)\n", size, sha256sum[:16])

	// Replicate to other regions
	for _, region := range regions[1:] {
		fmt.Fprintf(os.Stderr, "Replicating to %s...\n", region)
		if err := stagingClient.ReplicateToRegion(ctx, primaryRegion, primaryBucket, s3Key, region); err != nil {
			return fmt.Errorf("replicate to %s: %w", region, err)
		}
		fmt.Fprintf(os.Stderr, "  ✓ Replicated\n")
	}

	// Set default destination if not specified
	destination := stageDestination
	if destination == "" {
		destination = fmt.Sprintf("/mnt/data/%s", filepath.Base(localPath))
	}

	// Record metadata
	metadata := staging.StagingMetadata{
		StagingID:   stagingID,
		LocalPath:   localPath,
		S3Key:       s3Key,
		Regions:     regions,
		SweepID:     stageSweepID,
		CreatedAt:   time.Now(),
		Destination: destination,
		SizeBytes:   size,
		SHA256:      sha256sum,
	}

	if err := stagingClient.RecordMetadata(ctx, metadata); err != nil {
		return fmt.Errorf("record metadata: %w", err)
	}

	// Print summary
	fmt.Fprintf(os.Stderr, "\n✓ Staged to %d regions\n", len(regions))
	fmt.Fprintf(os.Stderr, "Staging ID: %s\n", stagingID)
	fmt.Fprintf(os.Stderr, "Size: %.2f MB\n", float64(size)/(1024*1024))
	fmt.Fprintf(os.Stderr, "Lifetime: 7 days (auto-deleted)\n")
	fmt.Fprintf(os.Stderr, "\nS3 paths:\n")
	for _, region := range regions {
		fmt.Fprintf(os.Stderr, "  - s3://spawn-data-%s/%s\n", region, s3Key)
	}

	// Print usage instructions
	fmt.Fprintf(os.Stderr, "\nTo use in parameter file:\n")
	fmt.Fprintf(os.Stderr, "  data_sources:\n")
	fmt.Fprintf(os.Stderr, "    - s3: %s\n", s3Key)
	fmt.Fprintf(os.Stderr, "      dest: %s\n", destination)
	fmt.Fprintf(os.Stderr, "\nOr reference by staging ID:\n")
	fmt.Fprintf(os.Stderr, "  spawn launch --param-file params.yaml \\\n")
	fmt.Fprintf(os.Stderr, "    --staging-id %s\n", stagingID)

	return nil
}

func runStageList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Load AWS config (infra account for DynamoDB access)
	awsConfig, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	accountID, err := getAccountID(ctx)
	if err != nil {
		return fmt.Errorf("get account ID: %w", err)
	}

	stagingClient := staging.NewClient(awsConfig, accountID)

	// List staged data
	staged, err := stagingClient.ListStagedData(ctx)
	if err != nil {
		return fmt.Errorf("list staged data: %w", err)
	}

	if len(staged) == 0 {
		fmt.Println("No staged data found")
		return nil
	}

	// Print table header
	fmt.Printf("%-25s %-40s %-12s %-8s %-30s\n",
		"STAGING ID", "FILE", "SIZE", "REGIONS", "CREATED")
	fmt.Println(strings.Repeat("-", 120))

	// Print each entry
	for _, s := range staged {
		filename := filepath.Base(s.LocalPath)
		if len(filename) > 40 {
			filename = filename[:37] + "..."
		}

		sizeMB := float64(s.SizeBytes) / (1024 * 1024)
		sizeStr := fmt.Sprintf("%.1f MB", sizeMB)
		if sizeMB > 1024 {
			sizeStr = fmt.Sprintf("%.1f GB", sizeMB/1024)
		}

		regionsStr := fmt.Sprintf("%d", len(s.Regions))

		age := time.Since(s.CreatedAt)
		ageStr := formatDuration(age)

		fmt.Printf("%-25s %-40s %-12s %-8s %-30s\n",
			s.StagingID, filename, sizeStr, regionsStr, ageStr)
	}

	fmt.Printf("\nTotal: %d staged files\n", len(staged))
	return nil
}

func runStageEstimate(cmd *cobra.Command, args []string) error {
	// Parse regions
	regions := parseRegionList(estimateRegions)
	numRegions := len(regions)

	if numRegions < 2 {
		return fmt.Errorf("must specify at least 2 regions for comparison")
	}

	// Calculate estimate
	estimate := staging.EstimateStagingCost(estimateDataSizeGB, numRegions, estimateInstances)

	// Print formatted estimate
	fmt.Println(estimate.FormatCostEstimate())

	// Print break-even analysis
	fmt.Println(staging.BreakEvenAnalysis(estimateDataSizeGB, numRegions))

	// Print recommendations
	fmt.Println("\n📋 Recommendations:")
	if estimate.Savings > 0 {
		fmt.Println("  ✓ Use regional replication for this workload")
		fmt.Printf("  ✓ Run: spawn stage upload <file> --regions %s\n", estimateRegions)
	} else {
		fmt.Println("  • Single-region storage is sufficient for this workload")
		fmt.Println("  • Consider replication if adding more regions or instances")
	}

	return nil
}

func runStageDelete(cmd *cobra.Command, args []string) error {
	stagingID := args[0]
	ctx := context.Background()

	if !confirmYes(stageDeleteYes, fmt.Sprintf("Delete staged data %s?", stagingID)) {
		fmt.Println("Aborted.")
		return nil
	}

	// Load AWS config (infra account for DynamoDB and S3 access)
	awsConfig, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	accountID, err := getAccountID(ctx)
	if err != nil {
		return fmt.Errorf("get account ID: %w", err)
	}

	stagingClient := staging.NewClient(awsConfig, accountID)

	// Get metadata first
	metadata, err := stagingClient.GetMetadata(ctx, stagingID)
	if err != nil {
		return fmt.Errorf("get metadata: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Deleting staged data: %s\n", stagingID)
	fmt.Fprintf(os.Stderr, "  File: %s\n", filepath.Base(metadata.LocalPath))
	fmt.Fprintf(os.Stderr, "  Regions: %d\n", len(metadata.Regions))

	// Delete from S3 in all regions
	for _, region := range metadata.Regions {
		fmt.Fprintf(os.Stderr, "  Deleting from %s...\n", region)

		// Note: Could implement DeleteObject, but S3 lifecycle will clean up in 7 days
		// This avoids needing region-specific configs for each S3 bucket
		fmt.Fprintf(os.Stderr, "    (S3 lifecycle will delete in 7 days)\n")
	}

	// Delete metadata
	if err := stagingClient.DeleteStaging(ctx, stagingID); err != nil {
		return fmt.Errorf("delete metadata: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n✓ Deleted staging metadata\n")
	fmt.Fprintf(os.Stderr, "Note: S3 objects will be deleted by lifecycle policy within 7 days\n")

	return nil
}

// parseRegionList parses a comma-separated list of regions
func parseRegionList(regionsStr string) []string {
	parts := strings.Split(regionsStr, ",")
	regions := make([]string, 0, len(parts))
	for _, part := range parts {
		region := strings.TrimSpace(part)
		if region != "" {
			regions = append(regions, region)
		}
	}
	return regions
}
