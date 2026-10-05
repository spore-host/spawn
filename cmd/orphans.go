package cmd

import (
	"context"
	"fmt"
	"io"
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
  - cluster placement groups with no instances left in them
  - the shared infrastructure (key pair, IAM role) when no instances remain
  - Elastic IPs that are unassociated, or attached to a stopped instance
    (an EIP keeps billing even while the instance is stopped)

This is a read-only report. 'spawn cleanup' removes orphaned EBS volumes,
security groups, placement groups, key pairs, and IAM roles. Elastic IPs are reported but
never released by spawn — spawn never allocates them. Before releasing an
EIP with 'aws ec2 release-address', verify ownership using the tags column
in the report (or 'aws ec2 describe-addresses'), as that operation is
irreversible.`,
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
	scopeHidden := 0
	for _, region := range regions {
		rs, hidden, derr := client.DiscoverManagedResources(ctx, aws.DiscoverOptions{Region: region, OnlyMine: onlyMine})
		if derr != nil {
			fmt.Fprintf(os.Stderr, "⚠️  %s: %v\n", region, derr)
			continue
		}
		scopeHidden += hidden

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
		// Never let "nothing here" stand alone when the scope hid something
		// (#708). This command reporting zero in an account holding 21 orphans is
		// what made the old --mine behaviour dangerous rather than merely wrong.
		reportScopeHidden(out, scopeHidden)
		return nil
	}

	printResourceTable(cmd, orphans)
	hasNonAddress := false
	for _, r := range orphans {
		if r.ResourceType != "address" {
			hasNonAddress = true
			break
		}
	}
	if hasNonAddress {
		fmt.Fprintln(out, "\nRun 'spawn cleanup' to remove these (running instances are never removed).")
	}
	reportScopeHidden(out, scopeHidden)
	return nil
}

// reportScopeHidden names resources the --mine scope excluded, so a zero result
// is never mistaken for a clean account (spawn#708).
//
// Before #708, --mine filtered on spawn:iam-user — a tag spawn writes only to
// instances and volumes — so every security group, IAM role and key pair was
// silently dropped. `spawn orphans` printed "No orphaned spawn-managed
// resources" in an account holding 21 of them, the oldest three months old.
// Untagged resources are now in scope, so what remains here is genuinely
// another principal's; saying so is still better than implying there is nothing.
func reportScopeHidden(w io.Writer, hidden int) {
	if hidden <= 0 {
		return
	}
	fmt.Fprintf(w, "\n(%d resource(s) tagged for another principal were excluded; "+
		"use --all to include them.)\n", hidden)
}
