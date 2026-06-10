package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	deleteAMIRegion string
	deleteAMIYes    bool
)

var amiDeleteCmd = &cobra.Command{
	Use:   "delete <ami-id>",
	Short: "Deregister an AMI and delete its backing snapshots",
	Long: `Deregister a spawn-managed AMI and delete its backing EBS snapshots in one
step. If the AMI was produced by EC2 Image Builder (e.g. 'spawn image import'),
the corresponding Image Builder image resource is also deleted so its
name/version is freed.

This is irreversible. Use 'spawn ami list' to find AMIs.

Examples:
  spawn ami delete ami-0123456789abcdef0
  spawn ami delete ami-0123456789abcdef0 --region us-east-1 --yes`,
	Args: cobra.ExactArgs(1),
	RunE: runDeleteAMI,
}

func runDeleteAMI(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	amiID := args[0]

	client, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("init AWS client: %w", err)
	}

	if !deleteAMIYes && getOutputFormat() != "json" {
		fmt.Fprintf(os.Stderr, "Deregister %s and delete its backing snapshots? This cannot be undone. [y/N]: ", amiID)
		var resp string
		_, _ = fmt.Scanln(&resp)
		if resp != "y" && resp != "Y" && resp != "yes" {
			return fmt.Errorf("aborted")
		}
	}

	res, err := client.DeleteAMI(ctx, deleteAMIRegion, amiID)

	// JSON: always emit the full result (it details deleted/retained/errors),
	// then return the error for a non-zero exit if cleanup was incomplete.
	if getOutputFormat() == "json" {
		if res != nil {
			_ = json.NewEncoder(os.Stdout).Encode(res)
		}
		return err
	}

	if res != nil {
		fmt.Printf("Deregistered AMI %s\n", res.AMIID)
		if len(res.DeletedSnapshots) > 0 {
			fmt.Printf("  deleted snapshots:  %s\n", strings.Join(res.DeletedSnapshots, ", "))
		}
		for snap, why := range res.RetainedSnapshots {
			fmt.Printf("  retained snapshot:  %s (%s)\n", snap, why)
		}
		if res.ImageBuilderArn != "" && res.ImageBuilderError == "" {
			fmt.Printf("  image builder resource deleted: %s\n", res.ImageBuilderArn)
		}
		for snap, msg := range res.SnapshotErrors {
			fmt.Fprintf(os.Stderr, "  ⚠️  snapshot %s not deleted: %s\n", snap, msg)
		}
		if res.ImageBuilderError != "" {
			fmt.Fprintf(os.Stderr, "  ⚠️  image builder resource %s not deleted: %s\n", res.ImageBuilderArn, res.ImageBuilderError)
		}
	}
	return err
}

func init() {
	amiGroupCmd.AddCommand(amiDeleteCmd)
	amiDeleteCmd.Flags().StringVar(&deleteAMIRegion, "region", "", "AWS region (default: current region from AWS config)")
	amiDeleteCmd.Flags().BoolVarP(&deleteAMIYes, "yes", "y", false, "Skip the confirmation prompt")
}
