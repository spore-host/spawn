package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	createAMIName        string
	createAMIDescription string
	createAMITags        []string
	createAMIReboot      bool
	createAMIWait        bool
)

var createAMICmd = &cobra.Command{
	Use:   "create-ami <instance-id-or-name>",
	Short: "Create an AMI from a running instance",
	Long: `Create an AMI from a running instance with automatic tagging.

The AMI will be tagged with spawn metadata for easy discovery and management.

Examples:
  # Create AMI from instance
  spawn create-ami my-instance --name pytorch-2.2-cuda12

  # With custom tags
  spawn create-ami i-abc123 \
    --name my-stack-v1.0 \
    --description "My custom software stack" \
    --tag stack=myapp \
    --tag version=1.0 \
    --tag gpu=true

  # Wait for AMI to be available
  spawn create-ami my-instance --name my-ami --wait

  # Allow reboot (default is no-reboot)
  spawn create-ami my-instance --name my-ami --reboot`,
	Args: cobra.ExactArgs(1),
	RunE: runCreateAMI,
}

func init() {
	rootCmd.AddCommand(createAMICmd)

	createAMICmd.Flags().StringVar(&createAMIName, "name", "", "Name for the AMI (required)")
	createAMICmd.Flags().StringVar(&createAMIDescription, "description", "", "Description for the AMI")
	createAMICmd.Flags().StringArrayVar(&createAMITags, "tag", []string{}, "Tags in key=value format (can be specified multiple times)")
	createAMICmd.Flags().BoolVar(&createAMIReboot, "reboot", false, "Reboot instance before creating AMI (default: no-reboot)")
	createAMICmd.Flags().BoolVar(&createAMIWait, "wait", false, "Wait for AMI to become available")

	_ = createAMICmd.MarkFlagRequired("name")
}

func runCreateAMI(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	identifier := args[0]

	// Create AWS client
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Resolve instance
	fmt.Fprintf(os.Stderr, "Resolving instance %s...\n", identifier)
	instance, err := resolveInstance(ctx, client, identifier)
	if err != nil {
		return err
	}

	// Check instance state
	if instance.State != "running" && instance.State != "stopped" {
		return fmt.Errorf("instance %s is in invalid state for AMI creation: %s", instance.InstanceID, instance.State)
	}

	fmt.Fprintf(os.Stderr, "Found instance %s in %s (state: %s)\n", instance.InstanceID, instance.Region, instance.State)

	// Parse tags
	tags := make(map[string]string)
	for _, tagStr := range createAMITags {
		parts := strings.SplitN(tagStr, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid tag format: %s (expected key=value)", tagStr)
		}
		tags[parts[0]] = parts[1]
	}

	// Add automatic spawn tags
	tags["spawn:managed"] = "true"
	tags["spawn:created"] = time.Now().UTC().Format(time.RFC3339)
	tags["spawn:created-from"] = instance.InstanceID
	tags["spawn:source-region"] = instance.Region

	// Detect architecture and GPU from instance type
	arch := aws.DetectArchitecture(instance.InstanceType)
	gpu := aws.DetectGPUInstance(instance.InstanceType)

	// Add arch and gpu tags if not already specified
	if _, exists := tags["spawn:arch"]; !exists {
		tags["spawn:arch"] = arch
	}
	if _, exists := tags["spawn:gpu"]; !exists {
		if gpu {
			tags["spawn:gpu"] = "true"
		} else {
			tags["spawn:gpu"] = "false"
		}
	}

	// Get base AMI ID from instance (for tracking base AMI age)
	baseAMI, err := client.GetInstanceAMI(ctx, instance.Region, instance.InstanceID)
	if err == nil && baseAMI != "" {
		tags["spawn:base-ami"] = baseAMI
		fmt.Fprintf(os.Stderr, "Tracking base AMI: %s\n", baseAMI)
	}

	// Default description
	description := createAMIDescription
	if description == "" {
		description = fmt.Sprintf("Created from %s by spawn", instance.InstanceID)
	}

	// Create AMI
	fmt.Fprintf(os.Stderr, "\nCreating AMI '%s'...\n", createAMIName)
	if !createAMIReboot {
		fmt.Fprintf(os.Stderr, "(no-reboot mode - instance will not be rebooted)\n")
	}

	amiID, err := client.CreateAMI(ctx, instance.Region, aws.CreateAMIInput{
		InstanceID:  instance.InstanceID,
		Name:        createAMIName,
		Description: description,
		Tags:        tags,
		NoReboot:    !createAMIReboot,
	})
	if err != nil {
		return fmt.Errorf("failed to create AMI: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "\n✅ AMI creation initiated!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   AMI ID:    %s\n", amiID)
	_, _ = fmt.Fprintf(os.Stdout, "   Name:      %s\n", createAMIName)
	_, _ = fmt.Fprintf(os.Stdout, "   Region:    %s\n", instance.Region)
	_, _ = fmt.Fprintf(os.Stdout, "   Instance:  %s\n", instance.InstanceID)

	// Display tags
	if len(tags) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "\n   Tags:\n")
		for key, value := range tags {
			if strings.HasPrefix(key, "spawn:") {
				_, _ = fmt.Fprintf(os.Stdout, "     %s: %s\n", key, value)
			}
		}
	}

	// Wait for AMI if requested
	if createAMIWait {
		fmt.Fprintf(os.Stderr, "\nWaiting for AMI to become available (this may take several minutes)...\n")

		err = client.WaitForAMI(ctx, instance.Region, amiID, 15*time.Minute)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n⚠️  Warning: Failed to wait for AMI: %v\n", err)
			fmt.Fprintf(os.Stderr, "   AMI creation is still in progress.\n")
			fmt.Fprintf(os.Stderr, "   Check status: aws ec2 describe-images --image-ids %s\n", amiID)
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "\n✅ AMI is now available!\n")
		}
	} else {
		fmt.Fprintf(os.Stderr, "\nAMI creation is in progress. This may take several minutes.\n")
		fmt.Fprintf(os.Stderr, "Check status: spawn list-amis\n")
		fmt.Fprintf(os.Stderr, "Or: aws ec2 describe-images --image-ids %s\n", amiID)
	}

	_, _ = fmt.Fprintf(os.Stdout, "\nUse AMI:\n")
	_, _ = fmt.Fprintf(os.Stdout, "  spawn launch --instance-type %s --ami %s\n", instance.InstanceType, amiID)

	return nil
}
