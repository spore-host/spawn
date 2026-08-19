package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	cleanupRegion     string
	cleanupAllRegions bool
	cleanupForce      bool
	cleanupDryRun     bool
	cleanupYes        bool
	cleanupAll        bool
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove spawn-managed AWS infrastructure (security groups, key pairs, IAM, …)",
	Long: `Remove the shared AWS resources spore.host created (tagged spawn:managed),
in dependency order. Running instances are NEVER removed — stop or terminate
them first.

Preview what would be removed with --dry-run; otherwise cleanup prompts for
confirmation (skip with --yes) and then deletes. By default it acts only on
resources you created; --all widens to every principal in the account.

A log of everything removed is written to ~/.spawn/cleanup-<timestamp>.log.`,
	RunE: runCleanup,
}

func init() {
	rootCmd.AddCommand(cleanupCmd)
	cleanupCmd.Flags().StringVar(&cleanupRegion, "region", "", "AWS region (default: current region from AWS config)")
	cleanupCmd.Flags().BoolVar(&cleanupAllRegions, "all-regions", false, "Clean up every enabled region")
	cleanupCmd.Flags().BoolVar(&cleanupDryRun, "dry-run", false, "Preview what would be removed without deleting anything")
	// --force was the old opt-in-to-delete flag; execute is now the default
	// (prompt-gated), so --force is a no-op kept only for back-compat (#315).
	cleanupCmd.Flags().BoolVar(&cleanupForce, "force", false, "Deprecated: execute is now the default; use --dry-run to preview")
	_ = cleanupCmd.Flags().MarkDeprecated("force", "cleanup now executes by default; use --dry-run to preview")
	cleanupCmd.Flags().BoolVarP(&cleanupYes, "yes", "y", false, "Skip the confirmation prompt")
	cleanupCmd.Flags().BoolVar(&cleanupAll, "all", false, "Include resources created by other principals (default: only yours)")
}

func runCleanup(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	client, err := aws.NewClient(ctx)
	if err != nil {
		return err
	}

	regions, err := resolveCleanupRegions(ctx, client, cleanupRegion, cleanupAllRegions)
	if err != nil {
		return err
	}
	onlyMine := !cleanupAll

	var found []aws.ManagedResource
	for _, region := range regions {
		rs, derr := client.DiscoverManagedResources(ctx, aws.DiscoverOptions{Region: region, OnlyMine: onlyMine})
		if derr != nil {
			fmt.Fprintf(os.Stderr, "⚠️  %s: %v\n", region, derr)
			continue
		}
		found = append(found, rs...)
	}

	out := cmd.OutOrStdout()
	if len(found) == 0 {
		fmt.Fprintf(out, "Nothing to clean up in %s.\n", displayCleanupRegions(regions))
		return nil
	}

	running, addresses, alreadyGone, removable := splitCleanupResources(found)

	printResourceTable(cmd, found)

	if len(addresses) > 0 {
		fmt.Fprintf(os.Stderr, "\nℹ️  %d Elastic IP(s) shown are user-owned — spawn never releases an EIP. If unneeded, release each yourself:\n", len(addresses))
		for _, a := range addresses {
			fmt.Fprintf(os.Stderr, "    aws ec2 release-address --allocation-id %s   # %s (%s)\n", a.ID, a.PublicIP, a.Region)
		}
	}

	if len(running) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  %d running/pending instance(s) will NOT be removed (stop or terminate them first):\n", len(running))
		for _, r := range running {
			fmt.Fprintf(os.Stderr, "    %s (%s)\n", r.ID, r.Region)
		}
		if cleanupDryRun {
			// Preview mode: just report and stop.
		} else {
			fmt.Fprintln(os.Stderr, "\nRefusing to clean up shared infrastructure while instances are still running.")
			return fmt.Errorf("running instances present; stop/terminate them or wait for TTL, then re-run")
		}
	}

	if len(alreadyGone) > 0 {
		fmt.Fprintf(os.Stderr, "\nℹ️  %d resource(s) shown are tag-mapping residue for things that no longer exist (the Resource Groups Tagging API's index outlives the resource) — nothing to remove for these:\n", len(alreadyGone))
		for _, r := range alreadyGone {
			fmt.Fprintf(os.Stderr, "    %s %s (%s)\n", r.ResourceType, r.ID, r.Region)
		}
	}

	if cleanupDryRun {
		if len(removable) == 0 {
			fmt.Fprintf(out, "\nDry run: 0 resource(s) would be removed")
			if len(alreadyGone) > 0 {
				fmt.Fprintf(out, " (%d tag mapping(s) are residue for resources that no longer exist)", len(alreadyGone))
			}
			fmt.Fprintln(out, ".")
			return nil
		}
		fmt.Fprintf(out, "\nDry run: %d resource(s) would be removed. Re-run without --dry-run to delete.\n", len(removable))
		return nil
	}

	if len(removable) == 0 {
		if len(alreadyGone) > 0 {
			fmt.Fprintln(out, "\nNo removable resources (only already-gone tag residue and/or running instances present).")
		} else {
			fmt.Fprintln(out, "\nNo removable resources (only running instances present).")
		}
		return nil
	}

	if !confirmYes(cleanupYes, fmt.Sprintf("Permanently remove %d spawn-managed resource(s)?", len(removable))) {
		fmt.Fprintln(out, "Aborted.")
		return nil
	}

	logPath, logFile := openCleanupLog()
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}

	removed, failed := 0, 0
	for _, r := range deletionOrderCmd(removable) {
		if rerr := client.RemoveResource(ctx, r); rerr != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  ✗ %s %s (%s): %v\n", r.Service, r.ID, r.Region, rerr)
			writeCleanupLog(logFile, fmt.Sprintf("FAILED %s\t%s\t%s\t%v", r.ARN, r.Region, r.ResourceType, rerr))
			continue
		}
		removed++
		fmt.Fprintf(out, "  ✓ removed %s %s (%s)\n", r.ResourceType, r.ID, r.Region)
		writeCleanupLog(logFile, fmt.Sprintf("REMOVED %s\t%s\t%s", r.ARN, r.Region, r.ResourceType))
	}

	fmt.Fprintf(out, "\nRemoved %d resource(s)", removed)
	if failed > 0 {
		fmt.Fprintf(out, ", %d failed", failed)
	}
	if logPath != "" {
		fmt.Fprintf(out, " — log: %s", logPath)
	}
	fmt.Fprintln(out, ".")
	if failed > 0 {
		return fmt.Errorf("%d resource(s) could not be removed", failed)
	}
	return nil
}

// splitCleanupResources partitions a discovery sweep into the four buckets
// cleanup's output depends on:
//   - running: running/pending instances — never removed, gate cleanup unless --dry-run
//   - addresses: Elastic IPs — spawn never allocates or releases them (#262)
//   - alreadyGone: resources whose State is already "deleted" — Resource Groups
//     Tagging API tag mappings outlive the resources they describe, so a
//     discovery sweep routinely returns ids for things no longer there.
//     Counting these as "removable" overstates both the dry-run preview and
//     the real confirmation prompt (spawn#516) — they must be split out, not
//     folded into removable just because they aren't running or an address.
//   - removable: everything else, the actual candidates for RemoveResource.
func splitCleanupResources(found []aws.ManagedResource) (running, addresses, alreadyGone, removable []aws.ManagedResource) {
	for _, r := range found {
		switch {
		case r.IsRunningInstance():
			running = append(running, r)
		case r.ResourceType == "address":
			addresses = append(addresses, r)
		case r.State == "deleted":
			alreadyGone = append(alreadyGone, r)
		default:
			removable = append(removable, r)
		}
	}
	return running, addresses, alreadyGone, removable
}

// deletionOrderCmd orders resources dependents-first. It mirrors the package
// helper but is reachable from the cmd layer.
func deletionOrderCmd(resources []aws.ManagedResource) []aws.ManagedResource {
	return aws.DeletionOrder(resources)
}

// openCleanupLog opens ~/.spawn/cleanup-<ts>.log for append; returns ("",nil) if
// it can't (logging is best-effort and must not block cleanup).
func openCleanupLog() (string, *os.File) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	dir := filepath.Join(home, ".spawn")
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return "", nil
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	path := filepath.Join(dir, fmt.Sprintf("cleanup-%s.log", ts))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", nil
	}
	return path, f
}

func writeCleanupLog(f *os.File, line string) {
	if f == nil {
		return
	}
	_, _ = f.WriteString(strings.TrimRight(line, "\n") + "\n")
}
