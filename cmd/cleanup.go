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
	cleanupYes        bool
	cleanupAll        bool
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove spawn-managed AWS infrastructure (security groups, key pairs, IAM, …)",
	Long: `Remove the shared AWS resources spore.host created (tagged spawn:managed),
in dependency order. Running instances are NEVER removed — stop or terminate
them first.

By default cleanup previews what would be removed (a dry run) and removes
nothing. Pass --force to actually delete. By default it acts only on resources
you created; --all widens to every principal in the account.

A log of everything removed is written to ~/.spawn/cleanup-<timestamp>.log.`,
	RunE: runCleanup,
}

func init() {
	rootCmd.AddCommand(cleanupCmd)
	cleanupCmd.Flags().StringVar(&cleanupRegion, "region", "", "AWS region (default: current region from AWS config)")
	cleanupCmd.Flags().BoolVar(&cleanupAllRegions, "all-regions", false, "Clean up every enabled region")
	cleanupCmd.Flags().BoolVar(&cleanupForce, "force", false, "Actually delete (without this, runs as a dry-run preview)")
	cleanupCmd.Flags().BoolVarP(&cleanupYes, "yes", "y", false, "Skip the confirmation prompt (only with --force)")
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

	// Split running instances out — they're never removed and gate cleanup.
	var running, removable []aws.ManagedResource
	for _, r := range found {
		if r.IsRunningInstance() {
			running = append(running, r)
		} else {
			removable = append(removable, r)
		}
	}

	printResourceTable(cmd, found)

	if len(running) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  %d running/pending instance(s) will NOT be removed (stop or terminate them first):\n", len(running))
		for _, r := range running {
			fmt.Fprintf(os.Stderr, "    %s (%s)\n", r.ID, r.Region)
		}
		if !cleanupForce {
			// Preview mode: just report and stop.
		} else {
			fmt.Fprintln(os.Stderr, "\nRefusing to clean up shared infrastructure while instances are still running.")
			return fmt.Errorf("running instances present; stop/terminate them or wait for TTL, then re-run")
		}
	}

	if !cleanupForce {
		fmt.Fprintf(out, "\nDry run: %d resource(s) would be removed. Re-run with --force to delete.\n", len(removable))
		return nil
	}

	if len(removable) == 0 {
		fmt.Fprintln(out, "\nNo removable resources (only running instances present).")
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
