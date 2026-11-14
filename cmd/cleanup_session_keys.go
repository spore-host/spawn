package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/keypairsweep"
)

// runCleanupSessionKeys is `spawn cleanup --session-keys` (#1621): the same
// rule the TTL reaper applies (pkg/keypairsweep). It deletes only key pairs
// tagged spawn:purpose=ssh-key-session that no instance in the region uses and
// that are an hour old, whoever imported them. A workstation key
// (spawn:purpose=ssh-key) is never touched.
//
// A preview pass decides what is due and is shown; after the prompt, the
// sweep runs again and decides afresh, so a key a launch started using
// between the two is kept.
func runCleanupSessionKeys(ctx context.Context, cmd *cobra.Command, regions []string, apiFor func(region string) keypairsweep.API, now time.Time) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	var due []string
	for _, region := range regions {
		rs, err := keypairsweep.Sweep(ctx, apiFor(region), now, true)
		if err != nil {
			fmt.Fprintf(errOut, "⚠️  %s: %v\n", region, err)
			continue
		}
		for _, r := range rs {
			switch r.Outcome {
			case keypairsweep.WouldDelete:
				fmt.Fprintf(out, "  %s  %s (%s), unused, %s old\n", region, r.KeyName, r.KeyPairID, r.Age.Round(time.Minute))
				due = append(due, region)
			case keypairsweep.InUse:
				fmt.Fprintf(out, "  %s  %s kept: used by %s\n", region, r.KeyName, r.UsedBy)
			case keypairsweep.TooYoung:
				fmt.Fprintf(out, "  %s  %s kept: under %s old\n", region, r.KeyName, keypairsweep.Grace)
			}
		}
	}
	if len(due) == 0 {
		fmt.Fprintf(out, "No unused session key pairs in %s.\n", displayCleanupRegions(regions))
		return nil
	}
	if cleanupDryRun {
		fmt.Fprintf(out, "\n%d session key pair(s) would be deleted (dry run).\n", len(due))
		return nil
	}
	if !confirmYes(cleanupYes, fmt.Sprintf("Delete %d unused session key pair(s)?", len(due))) {
		return newAborted("aborted: no key pair was deleted")
	}
	deleted, failed := 0, 0
	seen := map[string]bool{}
	for _, region := range due {
		if seen[region] {
			continue
		}
		seen[region] = true
		rs, err := keypairsweep.Sweep(ctx, apiFor(region), now, false)
		if err != nil {
			fmt.Fprintf(errOut, "⚠️  %s: %v\n", region, err)
			failed++
			continue
		}
		for _, r := range rs {
			switch r.Outcome {
			case keypairsweep.Deleted:
				deleted++
				fmt.Fprintf(out, "Deleted key pair %s (%s) in %s\n", r.KeyName, r.KeyPairID, region)
			case keypairsweep.Failed:
				failed++
				fmt.Fprintf(errOut, "⚠️  %s: delete %s: %v\n", region, r.KeyName, r.Err)
			}
		}
	}
	fmt.Fprintf(out, "\nDeleted %d session key pair(s).\n", deleted)
	if failed > 0 {
		return fmt.Errorf("%d session key pair deletion(s) failed", failed)
	}
	return nil
}
