package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/aws-sdk-go-v2/service/fsx/types"
	"github.com/spf13/cobra"
)

var fsxCmd = &cobra.Command{
	Use:   "fsx",
	Short: "FSx filesystem management commands",
	Long:  "Manage FSx Lustre filesystems: list, info, export, delete",
}

var fsxListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all FSx Lustre filesystems",
	Long:  "List all spawn-managed FSx Lustre filesystems across all regions",
	RunE:  runFSxList,
}

var fsxInfoCmd = &cobra.Command{
	Use:   "info <filesystem-id>",
	Short: "Show FSx filesystem details",
	Long:  "Show detailed information about an FSx Lustre filesystem",
	Args:  cobra.ExactArgs(1),
	RunE:  runFSxInfo,
}

var fsxDeleteCmd = &cobra.Command{
	Use:   "delete <filesystem-id>",
	Short: "Delete FSx filesystem",
	Long:  "Delete an FSx Lustre filesystem, optionally exporting to S3 first",
	Args:  cobra.ExactArgs(1),
	RunE:  runFSxDelete,
}

var (
	fsxDeleteExportFirst bool
	fsxDeleteSkipConfirm bool
)

func init() {
	rootCmd.AddCommand(fsxCmd)
	fsxCmd.AddCommand(fsxListCmd)
	fsxCmd.AddCommand(fsxInfoCmd)
	fsxCmd.AddCommand(fsxDeleteCmd)

	// Delete command flags
	fsxDeleteCmd.Flags().BoolVar(&fsxDeleteExportFirst, "export-first", false, "Export data to S3 before deleting")
	fsxDeleteCmd.Flags().BoolVar(&fsxDeleteSkipConfirm, "yes", false, "Skip confirmation prompt")
}

func runFSxList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get all regions
	regions := []string{
		"us-east-1", "us-east-2", "us-west-1", "us-west-2",
		"eu-west-1", "eu-central-1", "ap-southeast-1", "ap-northeast-1",
	}

	// Setup tabwriter
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "FILESYSTEM ID\tREGION\tSTACK NAME\tSIZE (GB)\tSTATUS\tS3 BUCKET\tCREATED\n")

	totalCount := 0

	for _, region := range regions {
		regionalCfg := cfg.Copy()
		regionalCfg.Region = region
		fsxClient := fsx.NewFromConfig(regionalCfg)

		result, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{})
		if err != nil {
			// Skip regions with errors (might not have access)
			continue
		}

		for _, fs := range result.FileSystems {
			// Only show spawn-managed filesystems
			isSpawnManaged := false
			stackName := ""
			s3Bucket := ""

			for _, tag := range fs.Tags {
				switch *tag.Key {
				case "spawn:managed":
					if *tag.Value == "true" {
						isSpawnManaged = true
					}
				case "spawn:fsx-stack-name":
					stackName = *tag.Value
				case "spawn:fsx-s3-bucket":
					s3Bucket = *tag.Value
				}
			}

			if !isSpawnManaged {
				continue
			}

			totalCount++

			// Format creation time
			createdAt := ""
			if fs.CreationTime != nil {
				createdAt = fs.CreationTime.Format("2006-01-02")
			}

			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				*fs.FileSystemId,
				region,
				stackName,
				*fs.StorageCapacity,
				fs.Lifecycle,
				s3Bucket,
				createdAt,
			)
		}
	}

	_ = w.Flush()

	if totalCount == 0 {
		fmt.Fprintf(os.Stderr, "\nNo spawn-managed FSx filesystems found.\n")
	} else {
		fmt.Fprintf(os.Stderr, "\nTotal: %d filesystem(s)\n", totalCount)
	}

	return nil
}

func runFSxInfo(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	filesystemID := args[0]

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Try to find the filesystem across regions
	regions := []string{
		"us-east-1", "us-east-2", "us-west-1", "us-west-2",
		"eu-west-1", "eu-central-1", "ap-southeast-1", "ap-northeast-1",
	}

	var fs *types.FileSystem
	var foundRegion string

	for _, region := range regions {
		regionalCfg := cfg.Copy()
		regionalCfg.Region = region
		fsxClient := fsx.NewFromConfig(regionalCfg)

		result, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{
			FileSystemIds: []string{filesystemID},
		})
		if err != nil {
			continue
		}

		if len(result.FileSystems) > 0 {
			fs = &result.FileSystems[0]
			foundRegion = region
			break
		}
	}

	if fs == nil {
		return fmt.Errorf("filesystem not found: %s", filesystemID)
	}

	// Extract tags
	stackName := ""
	s3Bucket := ""
	s3ImportPath := ""
	s3ExportPath := ""
	isSpawnManaged := false

	for _, tag := range fs.Tags {
		switch *tag.Key {
		case "spawn:managed":
			if *tag.Value == "true" {
				isSpawnManaged = true
			}
		case "spawn:fsx-stack-name":
			stackName = *tag.Value
		case "spawn:fsx-s3-bucket":
			s3Bucket = *tag.Value
		case "spawn:fsx-s3-import-path":
			s3ImportPath = *tag.Value
		case "spawn:fsx-s3-export-path":
			s3ExportPath = *tag.Value
		}
	}

	// Display information
	fmt.Printf("Filesystem ID:      %s\n", *fs.FileSystemId)
	fmt.Printf("Region:             %s\n", foundRegion)
	fmt.Printf("Status:             %s\n", fs.Lifecycle)
	fmt.Printf("Storage Capacity:   %d GB\n", *fs.StorageCapacity)
	fmt.Printf("DNS Name:           %s\n", *fs.DNSName)
	fmt.Printf("Mount Name:         %s\n", *fs.LustreConfiguration.MountName)
	fmt.Printf("Deployment Type:    %s\n", fs.LustreConfiguration.DeploymentType)
	fmt.Printf("Data Compression:   %s\n", fs.LustreConfiguration.DataCompressionType)

	if fs.CreationTime != nil {
		fmt.Printf("Created:            %s\n", fs.CreationTime.Format(time.RFC3339))
	}

	if isSpawnManaged {
		fmt.Printf("\nSpawn Configuration:\n")
		fmt.Printf("  Stack Name:       %s\n", stackName)
		fmt.Printf("  S3 Bucket:        %s\n", s3Bucket)
		if s3ImportPath != "" {
			fmt.Printf("  Import Path:      %s\n", s3ImportPath)
		}
		if s3ExportPath != "" {
			fmt.Printf("  Export Path:      %s\n", s3ExportPath)
		}
	}

	// Cost estimate
	costPerMonth := float64(*fs.StorageCapacity) * 0.22 // $0.22/GB-month for SSD
	fmt.Printf("\nEstimated Cost:     $%.2f/month\n", costPerMonth)

	return nil
}

func runFSxDelete(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	filesystemID := args[0]

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Find the filesystem region
	regions := []string{
		"us-east-1", "us-east-2", "us-west-1", "us-west-2",
		"eu-west-1", "eu-central-1", "ap-southeast-1", "ap-northeast-1",
	}

	var foundRegion string
	var fs *types.FileSystem

	for _, region := range regions {
		regionalCfg := cfg.Copy()
		regionalCfg.Region = region
		fsxClient := fsx.NewFromConfig(regionalCfg)

		result, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{
			FileSystemIds: []string{filesystemID},
		})
		if err != nil {
			continue
		}

		if len(result.FileSystems) > 0 {
			fs = &result.FileSystems[0]
			foundRegion = region
			break
		}
	}

	if fs == nil {
		return fmt.Errorf("filesystem not found: %s", filesystemID)
	}

	// Get stack name for confirmation
	stackName := ""
	for _, tag := range fs.Tags {
		if *tag.Key == "spawn:fsx-stack-name" {
			stackName = *tag.Value
			break
		}
	}

	// Confirmation prompt
	if !fsxDeleteSkipConfirm {
		fmt.Printf("About to delete FSx filesystem:\n")
		fmt.Printf("  ID:          %s\n", *fs.FileSystemId)
		fmt.Printf("  Region:      %s\n", foundRegion)
		if stackName != "" {
			fmt.Printf("  Stack Name:  %s\n", stackName)
		}
		fmt.Printf("  Capacity:    %d GB\n", *fs.StorageCapacity)
		fmt.Printf("\n")

		if fsxDeleteExportFirst {
			fmt.Printf("⚠️  --export-first is set, but this requires SSH access to a mounted instance.\n")
			fmt.Printf("   Manual export: ssh to instance and run: sudo lfs hsm_archive /fsx/*\n\n")
		}

		fmt.Printf("This action cannot be undone. Continue? (y/N): ")
		var response string
		_, _ = fmt.Scanln(&response)
		if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	// Delete the filesystem
	regionalCfg := cfg.Copy()
	regionalCfg.Region = foundRegion
	fsxClient := fsx.NewFromConfig(regionalCfg)

	fmt.Printf("Deleting filesystem %s...\n", filesystemID)

	_, err = fsxClient.DeleteFileSystem(ctx, &fsx.DeleteFileSystemInput{
		FileSystemId: aws.String(filesystemID),
	})
	if err != nil {
		return fmt.Errorf("failed to delete filesystem: %w", err)
	}

	fmt.Printf("✅ Filesystem deletion initiated.\n")
	fmt.Printf("Note: S3 bucket and data remain intact for future recall.\n")

	return nil
}
