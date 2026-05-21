package cmd

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/registry"
)

var burstCmd = &cobra.Command{
	Use:   "burst",
	Short: "Launch cloud instances to join local job array",
	Long: `Launch EC2 instances that will register with the hybrid registry
and coordinate with local instances to process workloads.

This enables "cloud bursting" where local compute capacity can be
extended with on-demand cloud resources.`,
	Example: `  # Launch 10 instances to help process a job array
  spawn burst --count 10 --job-array-id my-array --instance-type c5.4xlarge

  # Launch with specific AMI
  spawn burst --count 5 --job-array-id genomics --ami ami-abc123

  # Launch Spot instances for cost savings
  spawn burst --count 20 --job-array-id simulation --spot`,
	RunE: runBurst,
}

var (
	burstCount          int
	burstJobArrayID     string
	burstJobArrayName   string
	burstInstanceType   string
	burstAMI            string
	burstSpot           bool
	burstKeyName        string
	burstSubnetID       string
	burstSecurityGroups []string
)

func init() {
	rootCmd.AddCommand(burstCmd)

	burstCmd.Flags().IntVar(&burstCount, "count", 1, "Number of instances to launch")
	burstCmd.Flags().StringVar(&burstJobArrayID, "job-array-id", "", "Job array ID to join (required)")
	burstCmd.Flags().StringVar(&burstJobArrayName, "job-array-name", "", "Job array name (optional)")
	burstCmd.Flags().StringVar(&burstInstanceType, "instance-type", "t3.micro", "EC2 instance type")
	burstCmd.Flags().StringVar(&burstAMI, "ami", "", "AMI ID (auto-detect if not specified)")
	burstCmd.Flags().BoolVar(&burstSpot, "spot", false, "Use Spot instances")
	burstCmd.Flags().StringVar(&burstKeyName, "key-name", "", "SSH key pair name")
	burstCmd.Flags().StringVar(&burstSubnetID, "subnet-id", "", "Subnet ID")
	burstCmd.Flags().StringSliceVar(&burstSecurityGroups, "security-groups", nil, "Security group IDs")

	_ = burstCmd.MarkFlagRequired("job-array-id")
}

func runBurst(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Ensure DynamoDB table exists
	log.Printf("Ensuring DynamoDB registry table exists...")
	if err := registry.EnsureTable(ctx); err != nil {
		return fmt.Errorf("failed to ensure registry table: %w", err)
	}

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	ec2Client := ec2.NewFromConfig(cfg)

	// Auto-detect AMI if not specified
	if burstAMI == "" {
		amiID, err := findLatestSpawnAMI(ctx, ec2Client)
		if err != nil {
			return fmt.Errorf("failed to find AMI: %w", err)
		}
		burstAMI = amiID
		log.Printf("Using AMI: %s", burstAMI)
	}

	// Prepare launch parameters
	launchParams := &LaunchParams{
		Count:          burstCount,
		InstanceType:   burstInstanceType,
		AMI:            burstAMI,
		Spot:           burstSpot,
		KeyName:        burstKeyName,
		SubnetID:       burstSubnetID,
		SecurityGroups: burstSecurityGroups,
		JobArrayID:     burstJobArrayID,
		JobArrayName:   burstJobArrayName,
	}

	// Launch instances
	log.Printf("Launching %d instances to join job array %s...", burstCount, burstJobArrayID)
	instanceIDs, err := launchBurstInstances(ctx, ec2Client, launchParams)
	if err != nil {
		return fmt.Errorf("failed to launch instances: %w", err)
	}

	// Print results
	fmt.Printf("✓ Successfully launched %d instances:\n", len(instanceIDs))
	for i, id := range instanceIDs {
		fmt.Printf("  [%d] %s\n", i, id)
	}
	fmt.Printf("\nInstances will register with job array: %s\n", burstJobArrayID)
	fmt.Printf("Monitor with: spawn status --job-array-id %s\n", burstJobArrayID)

	return nil
}

type LaunchParams struct {
	Count          int
	InstanceType   string
	AMI            string
	Spot           bool
	KeyName        string
	SubnetID       string
	SecurityGroups []string
	JobArrayID     string
	JobArrayName   string
}

func launchBurstInstances(ctx context.Context, client *ec2.Client, params *LaunchParams) ([]string, error) {
	// Build tags
	tags := []types.Tag{
		{Key: aws.String("Name"), Value: aws.String(fmt.Sprintf("spawn-burst-%s", params.JobArrayID))},
		{Key: aws.String("spawn:job-array-id"), Value: aws.String(params.JobArrayID)},
		{Key: aws.String("spawn:burst"), Value: aws.String("true")},
		{Key: aws.String("spawn:on-complete"), Value: aws.String("terminate")},
	}

	if params.JobArrayName != "" {
		tags = append(tags, types.Tag{
			Key:   aws.String("spawn:job-array-name"),
			Value: aws.String(params.JobArrayName),
		})
	}

	// Build launch template
	runInput := &ec2.RunInstancesInput{
		ImageId:      aws.String(params.AMI),
		InstanceType: types.InstanceType(params.InstanceType),
		MinCount:     aws.Int32(int32(params.Count)),
		MaxCount:     aws.Int32(int32(params.Count)),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         tags,
			},
		},
		UserData: aws.String(generateBurstUserData(params)),
	}

	// Add optional parameters
	if params.KeyName != "" {
		runInput.KeyName = aws.String(params.KeyName)
	}

	if params.SubnetID != "" {
		runInput.SubnetId = aws.String(params.SubnetID)
	}

	if len(params.SecurityGroups) > 0 {
		runInput.SecurityGroupIds = params.SecurityGroups
	}

	// Handle Spot instances
	if params.Spot {
		runInput.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType: types.SpotInstanceTypeOneTime,
			},
		}
	}

	// Launch instances
	result, err := client.RunInstances(ctx, runInput)
	if err != nil {
		return nil, fmt.Errorf("failed to run instances: %w", err)
	}

	// Collect instance IDs
	var instanceIDs []string
	for _, instance := range result.Instances {
		instanceIDs = append(instanceIDs, *instance.InstanceId)
	}

	return instanceIDs, nil
}

func generateBurstUserData(params *LaunchParams) string {
	// User data script that registers with DynamoDB on startup
	return `#!/bin/bash
# Spawn burst instance setup

# Wait for spored to be available
while [ ! -f /usr/local/bin/spored ]; do
  sleep 5
done

# Start spored with hybrid registry support
systemctl enable spored
systemctl start spored

# Register with DynamoDB registry happens automatically via spored
`
}

func findLatestSpawnAMI(ctx context.Context, client *ec2.Client) (string, error) {
	// Find the latest spawn AMI
	result, err := client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"self"},
		Filters: []types.Filter{
			{
				Name:   aws.String("name"),
				Values: []string{"spawn-*"},
			},
			{
				Name:   aws.String("state"),
				Values: []string{"available"},
			},
		},
	})

	if err != nil {
		return "", fmt.Errorf("failed to describe images: %w", err)
	}

	if len(result.Images) == 0 {
		return "", fmt.Errorf("no spawn AMIs found (use --ami to specify)")
	}

	// Return the most recent AMI
	latest := result.Images[0]
	for _, img := range result.Images {
		if *img.CreationDate > *latest.CreationDate {
			latest = img
		}
	}

	return *latest.ImageId, nil
}
