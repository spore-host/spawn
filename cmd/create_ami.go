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
	createAMIForce       bool
)

// createAMIForceHelp is shared by `spawn create-ami` and `spawn ami create`.
const createAMIForceHelp = "Create the image even if spawn could not remove its --command run-state files (completion file, exit code, the embedded command) from the instance first; they will be baked into the AMI"

var createAMICmd = &cobra.Command{
	Use:   "create-ami <instance-id-or-name>",
	Short: "Create an AMI from a running instance",
	Long: `Create an AMI from a running instance with automatic tagging.

The AMI will be tagged with spawn metadata for easy discovery and management.
It also records where it came from: spawn:image-spawn-version (this spawn's
version) and, when the source instance has them, its spawn:version,
spawn:spored-version, spawn:mpi-enabled and spawn:job-array-index as
spawn:source-version, spawn:source-spored-version, spawn:source-mpi-enabled
and spawn:source-job-array-index. --tag cannot set these. 'spawn ami list'
shows the source version.

Before imaging, spawn removes its --command run state from the instance over
SSM: the completion file, /tmp/SPAWN_* (the exit code), the rendered and
embedded command, and the sweep environment. Otherwise an instance launched
from the image starts with a stale SPAWN_COMPLETE and --on-complete fires at
boot. If that cannot be done (no SSM, the instance is stopped, the command is
still running), the image is not created; --force creates it anyway.

Root's authorized_keys on the source is left as it is. An image of an MPI
cohort member (--mpi) carries the cohort's cluster key there; an instance
launched from the image removes every line ending in spawn-mpi-cohort at its
first boot and keeps every other line. A non-zero rank imaged with spawn before
v0.147.0 also carries rank 0's key without that tag, which cannot be told from
your own keys, so an image like that should be rebuilt: launch from it, delete
the root@ip-... line in /root/.ssh/authorized_keys that is not yours, and image
that instance.

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
	createAMICmd.Flags().BoolVar(&createAMIForce, "force", false, createAMIForceHelp)

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

	tags, err := parseCreateAMITags(createAMITags)
	if err != nil {
		return err
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

	amiID, err := newAMIImager(client).createFromInstance(ctx, instance, aws.CreateAMIInput{
		InstanceID:  instance.InstanceID,
		Name:        createAMIName,
		Description: description,
		Tags:        tags,
		SourceTags:  instance.Tags,
		NoReboot:    !createAMIReboot,
	}, createAMIForce, os.Stderr)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(os.Stdout, "\n✅ AMI creation initiated!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   AMI ID:    %s\n", amiID)
	_, _ = fmt.Fprintf(os.Stdout, "   Name:      %s\n", createAMIName)
	_, _ = fmt.Fprintf(os.Stdout, "   Region:    %s\n", instance.Region)
	_, _ = fmt.Fprintf(os.Stdout, "   Instance:  %s\n", instance.InstanceID)

	// Display tags: what CreateAMI wrote, provenance included.
	tags = aws.ImageCreateTags(tags, instance.Tags)
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

// parseCreateAMITags parses --tag key=value values. A key spawn writes as the
// image's provenance (#1364) is refused: it records where the image came
// from, and a value the user picked would mislead every check keyed on it.
func parseCreateAMITags(in []string) (map[string]string, error) {
	tags := make(map[string]string, len(in))
	for _, tagStr := range in {
		parts := strings.SplitN(tagStr, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid tag format: %s (expected key=value)", tagStr)
		}
		if aws.IsImageProvenanceTagKey(parts[0]) {
			return nil, fmt.Errorf("--tag %s: spawn writes this tag itself, from the source instance and this spawn's version; it cannot be set", parts[0])
		}
		tags[parts[0]] = parts[1]
	}
	return tags, nil
}
