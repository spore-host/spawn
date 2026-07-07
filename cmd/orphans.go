package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	orphansRegion     string
	orphansAllRegions bool
	orphansAll        bool
)

var orphansCmd = &cobra.Command{
	Use:   "orphans",
	Short: "Report spawn-managed resources that look abandoned",
	Long: `Report spawn-managed resources that appear orphaned — present but with no
running instance using them:

  - EBS volumes in the 'available' state
  - security groups not attached to any instance
  - the shared infrastructure (key pair, IAM role) when no instances remain
  - Elastic IPs that are unassociated, or attached to a stopped instance
    (an EIP keeps billing even while the instance is stopped)

This is a read-only report. Use 'spawn cleanup' to remove anything — except
Elastic IPs: spawn never allocates them, so it never releases them. Any EIP
listed is yours to release with 'aws ec2 release-address'.`,
	RunE: runOrphans,
}

func init() {
	rootCmd.AddCommand(orphansCmd)
	orphansCmd.Flags().StringVar(&orphansRegion, "region", "", "AWS region (default: current region from AWS config)")
	orphansCmd.Flags().BoolVar(&orphansAllRegions, "all-regions", false, "Search every enabled region")
	orphansCmd.Flags().BoolVar(&orphansAll, "all", false, "Include resources created by other principals (default: only yours)")
}

func runOrphans(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	client, err := aws.NewClient(ctx)
	if err != nil {
		return err
	}

	regions, err := resolveCleanupRegions(ctx, client, orphansRegion, orphansAllRegions)
	if err != nil {
		return err
	}
	onlyMine := !orphansAll

	var orphans []aws.ManagedResource
	for _, region := range regions {
		rs, derr := client.DiscoverManagedResources(ctx, aws.DiscoverOptions{Region: region, OnlyMine: onlyMine})
		if derr != nil {
			fmt.Fprintf(os.Stderr, "⚠️  %s: %v\n", region, derr)
			continue
		}

		// Any running/pending instance in the region means the shared infra
		// (SG, key pair, IAM) is in use — only flag clearly-detached resources.
		hasRunning := false
		for _, r := range rs {
			if r.IsRunningInstance() {
				hasRunning = true
				break
			}
		}

		for _, r := range rs {
			if aws.IsLikelyOrphan(r, hasRunning) {
				orphans = append(orphans, r)
			}
		}
	}

	out := cmd.OutOrStdout()
	if len(orphans) == 0 {
		fmt.Fprintf(out, "No orphaned spawn-managed resources in %s.\n", displayCleanupRegions(regions))
		return nil
	}

	printResourceTable(cmd, orphans)
	fmt.Fprintln(out, "\nRun 'spawn cleanup' to remove these (running instances are never removed).")
	return nil
}
