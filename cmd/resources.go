package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	resourcesRegion     string
	resourcesAllRegions bool
	resourcesMine       bool
)

var resourcesCmd = &cobra.Command{
	Use:   "resources",
	Short: "List AWS resources spore.host has created (tagged spawn:managed)",
	Long: `List every AWS resource spore.host created in an account/region, found by
the spawn:managed=true tag via the Resource Groups Tagging API.

By default it lists resources created by you (your IAM principal). Use --all to
include resources created by other principals in the account.`,
	RunE: runResources,
}

func init() {
	rootCmd.AddCommand(resourcesCmd)
	resourcesCmd.Flags().StringVar(&resourcesRegion, "region", "", "AWS region (default: current region from AWS config)")
	resourcesCmd.Flags().BoolVar(&resourcesAllRegions, "all-regions", false, "Search every enabled region")
	resourcesCmd.Flags().BoolVar(&resourcesMine, "all", false, "Include resources created by other principals (default: only yours)")
}

func runResources(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	client, err := aws.NewClient(ctx)
	if err != nil {
		return err
	}

	regions, err := resolveCleanupRegions(ctx, client, resourcesRegion, resourcesAllRegions)
	if err != nil {
		return err
	}

	// --all widens scope to every principal; default is only-mine.
	onlyMine := !resourcesMine

	var all []aws.ManagedResource
	for _, region := range regions {
		found, derr := client.DiscoverManagedResources(ctx, aws.DiscoverOptions{Region: region, OnlyMine: onlyMine})
		if derr != nil {
			fmt.Fprintf(os.Stderr, "⚠️  %s: %v\n", region, derr)
			continue
		}
		all = append(all, found...)
	}

	if len(all) == 0 {
		scope := "you have created"
		if resourcesMine {
			scope = "spore.host has created"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "No spawn-managed resources %s in %s.\n", scope, displayCleanupRegions(regions))
		return nil
	}

	printResourceTable(cmd, all)
	return nil
}

func printResourceTable(cmd *cobra.Command, resources []aws.ManagedResource) {
	tw := newTableWriter(cmd.OutOrStdout())
	fmt.Fprintln(tw, "REGION\tSERVICE\tTYPE\tID\tSTATE\tCREATED")
	for _, r := range resources {
		state := r.State
		if state == "" {
			state = "-"
		}
		created := r.Tags["spawn:created-at"]
		if created == "" {
			created = r.Tags["spawn:launch-time"]
		}
		if created == "" {
			created = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Region, r.Service, r.ResourceType, r.ID, state, created)
	}
	_ = tw.Flush()
	fmt.Fprintf(cmd.OutOrStdout(), "\n%d resource(s).\n", len(resources))
}

// resolveCleanupRegions returns the region list to sweep: every enabled region
// when allRegions is set, else the explicit region, else the client's
// configured region. Shared by resources/cleanup/orphans.
func resolveCleanupRegions(ctx context.Context, client *aws.Client, region string, allRegions bool) ([]string, error) {
	if allRegions {
		regions, err := client.GetEnabledRegions(ctx)
		if err != nil {
			return nil, fmt.Errorf("list enabled regions: %w", err)
		}
		sort.Strings(regions)
		return regions, nil
	}
	if region != "" {
		return []string{region}, nil
	}
	r := client.Config().Region
	if r == "" {
		return nil, fmt.Errorf("no region set: pass --region or configure a default AWS region")
	}
	return []string{r}, nil
}

func displayCleanupRegions(regions []string) string {
	switch len(regions) {
	case 0:
		return "(no regions)"
	case 1:
		return regions[0]
	default:
		return fmt.Sprintf("%d regions", len(regions))
	}
}
