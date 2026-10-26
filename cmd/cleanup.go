package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/bucketsweep"
	"github.com/spore-host/spawn/pkg/instancerole"
)

var (
	cleanupRegion     string
	cleanupAllRegions bool
	cleanupForce      bool
	cleanupDryRun     bool
	cleanupYes        bool
	cleanupAll        bool
	cleanupElasticIPs bool
	cleanupInstRoles  bool
	cleanupBuckets    bool
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove spawn-managed AWS infrastructure (security groups, placement groups, key pairs, IAM, …)",
	Long: `Remove the shared AWS resources spore.host created (tagged spawn:managed),
in dependency order. Running instances are NEVER removed — stop or terminate
them first. A cluster placement group that still has members is reported and
skipped, since EC2 refuses to delete one — re-run once those instances are gone.

Preview what would be removed with --dry-run; otherwise cleanup prompts for
confirmation (skip with --yes) and then deletes. By default it acts only on
resources you created; --all widens to every principal in the account.

--elastic-ips releases only the Elastic IPs spawn allocated for multi-interface
EFA launches whose instance is gone (terminated, or no longer known to EC2) and
that are attached to nothing. Each one bills and counts toward the region's
Elastic IP limit. This mode touches nothing else, so running instances do not
block it. spawn list and spawn status warn about these addresses and print the
command.

--buckets removes only spawn's own spawn-staging-<account>-<region> and
spawn-results-<account>-<region> buckets, and only once they have been empty
for 7 days. It never deletes an object: a bucket holding anything at all
(objects, old versions, delete markers, an unfinished multipart upload) is
kept. S3 records no "empty since", so the first run that finds a bucket empty
tags it spawn:empty-since and a later run deletes it once that tag is 7 days
old; a run that finds it in use removes the tag, and so does a launch that
uses it. A results bucket with lifecycle rules (spawn results lifecycle
--apply) is kept, since a re-created one would not get them back. The
staging bucket is never listed (no spawn role may list it): S3 itself refuses
to delete it if it holds anything. spawn
re-creates either bucket the next time it needs it. Every call names your
account as the expected bucket owner. --dry-run writes nothing, not even the
tag. The TTL reaper can do the same on its own schedule (REAPER_EMPTY_BUCKETS).

--instance-roles deletes only the per-task spawn-instance-<hash> IAM roles and
instance profiles that 'spawn task run' and --iam-policy launches create, one
per distinct set of permissions. A role goes only when it and its profile are
tagged spawn:managed=true, no instance in any enabled region (pending through
stopped) has the profile attached, the role and profile are at least 24 hours
old, and the role is unused for 24 hours. The shared spored-instance-role is never touched. IAM is
global, so this always checks every enabled region whatever --region says, and
covers every principal's roles. The TTL reaper applies the same rule
(REAPER_INSTANCE_ROLES).

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
	cleanupCmd.Flags().BoolVar(&cleanupElasticIPs, "elastic-ips", false, "Release only orphaned spawn launch Elastic IPs (unassociated, instance gone); nothing else is touched")
	cleanupCmd.Flags().BoolVar(&cleanupInstRoles, "instance-roles", false, "Delete only unused per-task spawn-instance-* IAM roles and instance profiles (no instance uses them, 24h old and unused); nothing else is touched")
	cleanupCmd.Flags().BoolVar(&cleanupBuckets, "buckets", false, "Delete only spawn's own staging and results buckets that have been empty for "+bucketGraceDays()+"; no object is ever deleted and nothing else is touched")
}

func runCleanup(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	client, err := aws.NewClient(ctx)
	if err != nil {
		return err
	}
	if cleanupInstRoles {
		// IAM is global: a role is in use if an instance in ANY enabled
		// region has its profile, so the in-use check is never narrowed to
		// --region (instancerole.EnabledRegions, as the reaper does).
		iamc, ec2s, err := instancerole.NewClients(ctx, client.Config())
		if err != nil {
			return fmt.Errorf("list enabled regions (needed to check which roles instances use): %w", err)
		}
		return runCleanupInstanceRoles(ctx, cmd, iamc, ec2s, time.Now())
	}

	regions, err := resolveCleanupRegions(ctx, client, cleanupRegion, cleanupAllRegions)
	if err != nil {
		return err
	}
	onlyMine := !cleanupAll

	if cleanupElasticIPs && cleanupBuckets {
		return fmt.Errorf("--elastic-ips and --buckets each select one kind of resource; run them separately")
	}
	if cleanupElasticIPs {
		return runCleanupElasticIPs(ctx, cmd, client, regions, onlyMine)
	}
	if cleanupBuckets {
		account, err := client.GetAccountID(ctx)
		if err != nil {
			return fmt.Errorf("resolve your account (the expected owner of every bucket): %w", err)
		}
		return runCleanupBuckets(ctx, cmd, account, regions, bucketsweep.ClientsFor(client.Config()), time.Now())
	}

	var found []aws.ManagedResource
	for _, region := range regions {
		rs, _, derr := client.DiscoverManagedResources(ctx, aws.DiscoverOptions{Region: region, OnlyMine: onlyMine})
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

	running, addresses, alreadyGone, notYet, removable := splitCleanupResources(found)

	printResourceTable(cmd, found)

	if len(addresses) > 0 {
		fmt.Fprintf(os.Stderr, "\nℹ️  %d Elastic IP(s) shown are user-owned — spawn never releases an EIP it did not allocate. If unneeded, release each yourself:\n", len(addresses))
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

	if len(notYet) > 0 {
		fmt.Fprintf(os.Stderr, "\nℹ️  %d resource(s) are reported but not removed:\n", len(notYet))
		for _, r := range notYet {
			// Say WHY per item: the two reasons are unrelated, and a single
			// blanket sentence was wrong for one of them.
			reason := "still in use"
			switch {
			case aws.IsSharedSporedIdentity(r):
				reason = "shared infrastructure reused by every launch — never removed"
			case r.ResourceType == "placement-group":
				reason = "still has members; deletable once those instances are gone"
			case r.Service == "s3":
				reason = "an S3 bucket; never removed here (spawn cleanup --buckets removes spawn's staging and results buckets once empty for " + bucketGraceDays() + ")"
			case aws.IsSpawnAllocatedAddress(r):
				reason = launchAddressNotYetReason(r)
			}
			fmt.Fprintf(os.Stderr, "    %s (%s) — %s\n", r.ID, r.Region, reason)
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
		return newAborted("aborted: nothing was cleaned up (re-run with --yes)")
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
//   - addresses: user-owned Elastic IPs — spawn never releases them (#262). An
//     Elastic IP spawn allocated for a multi-interface EFA launch (#661) is
//     removable once orphaned (aws.IsOrphanedSpawnAddress: unassociated, its
//     instance gone), and notYet while unassociated with its instance alive.
//   - alreadyGone: resources whose State is already "deleted" — Resource Groups
//     Tagging API tag mappings outlive the resources they describe, so a
//     discovery sweep routinely returns ids for things no longer there.
//     Counting these as "removable" overstates both the dry-run preview and
//     the real confirmation prompt (spawn#516) — they must be split out, not
//     folded into removable just because they aren't running or an address.
//   - notYet: resources that exist but cannot be removed yet — currently only a
//     placement group that still has members. EC2 refuses to delete one, so
//     offering it is a wait ending in a failure. Kept OUT of `running`, which
//     aborts the whole cleanup: a group whose only members are stopped
//     instances must not block cleanup from terminating those very instances
//     (#685). Reported, not acted on; the next sweep picks it up.
//   - removable: everything else, the actual candidates for RemoveResource.
func splitCleanupResources(found []aws.ManagedResource) (running, addresses, alreadyGone, notYet, removable []aws.ManagedResource) {
	for _, r := range found {
		switch {
		case r.IsRunningInstance():
			running = append(running, r)
		case aws.IsOrphanedSpawnAddress(r):
			// An Elastic IP a multi-interface EFA launch allocated whose instance
			// is gone (#661): spawn's, billing, and released here.
			removable = append(removable, r)
		case aws.IsSpawnAllocatedAddress(r) && r.State == "unassociated":
			// Spawn's, unassociated, but its instance still exists (or could not
			// be looked up): a launch between allocating and associating it, or
			// a briefly detached address. Not an orphan, so not released (#1232).
			notYet = append(notYet, r)
		case r.ResourceType == "address":
			addresses = append(addresses, r)
		case r.ResourceType == "placement-group" && r.State == "in-use":
			notYet = append(notYet, r)
		case r.Service == "s3":
			// A bucket may hold data, and RemoveResource cannot delete one, so
			// offering it was a promise ending in "don't know how to remove".
			// --buckets removes spawn's EMPTY staging and results buckets (#1301).
			notYet = append(notYet, r)
		case aws.IsSharedSporedIdentity(r):
			// The shared spored role/profile are created once and reused by every
			// launch, so cleanup must never offer them. Latent until #708: the
			// --mine scope filtered on a tag they never carry, so they were
			// invisible here and this was unreachable. Fixing the scope made them
			// deletable — a dry run offered both — so one bug had been hiding the
			// other (#713).
			notYet = append(notYet, r)
		case r.State == "deleted":
			alreadyGone = append(alreadyGone, r)
		default:
			removable = append(removable, r)
		}
	}
	return running, addresses, alreadyGone, notYet, removable
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

func launchAddressNotYetReason(r aws.ManagedResource) string {
	inst := r.Tags[aws.ElasticIPInstanceTagKey]
	if r.LaunchInstanceState == "" {
		return fmt.Sprintf("spawn's, but its instance %s could not be checked; not released", inst)
	}
	return fmt.Sprintf("spawn's, but its instance %s is still %s; not released", inst, r.LaunchInstanceState)
}

// cleanupEIPAPI is what `spawn cleanup --elastic-ips` needs; *aws.Client is it.
type cleanupEIPAPI interface {
	FindOrphanedSpawnAddresses(ctx context.Context, region string) ([]aws.ManagedResource, error)
	GetCallerIdentityInfo(ctx context.Context) (string, string, error)
	RemoveResource(ctx context.Context, r aws.ManagedResource) error
}

// runCleanupElasticIPs releases the orphaned launch Elastic IPs in regions
// (aws.IsOrphanedSpawnAddress, the definition `spawn list`/`status` warn by) and
// nothing else (#1232). It is not gated on running instances: releasing an
// address no instance holds cannot affect one.
func runCleanupElasticIPs(ctx context.Context, cmd *cobra.Command, api cleanupEIPAPI, regions []string, onlyMine bool) error {
	out := cmd.OutOrStdout()
	var callerARN string
	if onlyMine {
		_, arn, err := api.GetCallerIdentityInfo(ctx)
		if err != nil {
			return fmt.Errorf("resolve caller identity for the default scope (or pass --all): %w", err)
		}
		callerARN = arn
	}

	var orphans []aws.ManagedResource
	hidden, unchecked := 0, 0
	for _, region := range regions {
		rs, err := api.FindOrphanedSpawnAddresses(ctx, region)
		if err != nil {
			unchecked++
			fmt.Fprintf(cmd.ErrOrStderr(), "%s  %s: %v\n", i18n.Symbol("warning"), region, err)
			continue
		}
		for _, r := range rs {
			if onlyMine && !r.OwnedBy(callerARN) {
				hidden++
				continue
			}
			orphans = append(orphans, r)
		}
	}

	if len(orphans) == 0 {
		fmt.Fprintf(out, "No orphaned spawn Elastic IPs in %s.", displayCleanupRegions(regions))
		if hidden > 0 {
			fmt.Fprintf(out, " %d launched by other principals were skipped (--all includes them).", hidden)
		}
		fmt.Fprintln(out)
		if unchecked > 0 {
			return fmt.Errorf("%d region(s) could not be checked", unchecked)
		}
		return nil
	}

	fmt.Fprintf(out, "%d orphaned spawn Elastic IP(s) (unassociated, instance gone):\n", len(orphans))
	for _, r := range orphans {
		fmt.Fprintf(out, "  %s  %s  %s  instance %s %s\n", r.ID, r.PublicIP, r.Region, r.Tags[aws.ElasticIPInstanceTagKey], r.LaunchInstanceState)
	}
	if hidden > 0 {
		fmt.Fprintf(out, "%d more launched by other principals were skipped (--all includes them).\n", hidden)
	}

	if cleanupDryRun {
		fmt.Fprintf(out, "\nDry run: %d Elastic IP(s) would be released. Re-run without --dry-run to release.\n", len(orphans))
		return nil
	}
	if !confirmYes(cleanupYes, fmt.Sprintf("Release %d Elastic IP(s)?", len(orphans))) {
		return newAborted("aborted: no Elastic IP was released (re-run with --yes)")
	}

	logPath, logFile := openCleanupLog()
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}
	released, failed := 0, 0
	for _, r := range orphans {
		if err := api.RemoveResource(ctx, r); err != nil {
			failed++
			fmt.Fprintf(cmd.ErrOrStderr(), "  ✗ %s (%s): %v\n", r.ID, r.Region, err)
			writeCleanupLog(logFile, fmt.Sprintf("FAILED %s\t%s\taddress\t%v", r.ID, r.Region, err))
			continue
		}
		released++
		fmt.Fprintf(out, "  ✓ released %s (%s)\n", r.ID, r.Region)
		writeCleanupLog(logFile, fmt.Sprintf("REMOVED %s\t%s\taddress", r.ID, r.Region))
	}
	fmt.Fprintf(out, "\nReleased %d Elastic IP(s)", released)
	if failed > 0 {
		fmt.Fprintf(out, ", %d failed", failed)
	}
	if logPath != "" {
		fmt.Fprintf(out, " — log: %s", logPath)
	}
	fmt.Fprintln(out, ".")
	switch {
	case failed > 0:
		return fmt.Errorf("%d Elastic IP(s) could not be released", failed)
	case unchecked > 0:
		return fmt.Errorf("%d region(s) could not be checked", unchecked)
	}
	return nil
}

// bucketGraceDays is bucketsweep.Grace in days, for help text.
func bucketGraceDays() string {
	return fmt.Sprintf("%d days", int(bucketsweep.Grace.Hours()/24))
}

// runCleanupBuckets is `spawn cleanup --buckets` (#1301): spawn's staging and
// results buckets for account in each region, through bucketsweep.
//
// Two passes. The first may write only the spawn:empty-since tag (and nothing
// under --dry-run) and reports what qualifies; the second, after the
// confirmation, re-runs the whole check on each qualifying bucket with deletion
// allowed, so a bucket that changed in between is judged on what it is now.
func runCleanupBuckets(ctx context.Context, cmd *cobra.Command, account string, regions []string, s3For func(region string) bucketsweep.API, now time.Time) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	writes := !cleanupDryRun
	var due []bucketsweep.Result
	failed := 0
	for _, region := range regions {
		api := s3For(region)
		for _, b := range bucketsweep.Buckets(account, region) {
			res := bucketsweep.Sweep(ctx, api, b, bucketsweep.Options{Account: account, Now: now, Writes: writes})
			printBucketResult(out, errOut, res, writes, now)
			switch res.Action {
			case bucketsweep.WouldDelete:
				due = append(due, res)
			case bucketsweep.Failed:
				failed++
			}
		}
	}

	if cleanupDryRun || len(due) == 0 {
		if cleanupDryRun {
			fmt.Fprintf(out, "\nDry run: %d empty bucket(s) would be deleted; nothing was changed.\n", len(due))
		} else {
			fmt.Fprintf(out, "\nNo bucket has been empty for %s yet; nothing was deleted.\n", bucketGraceDays())
		}
		if failed > 0 {
			return fmt.Errorf("%d bucket(s) could not be checked", failed)
		}
		return nil
	}

	if !confirmYes(cleanupYes, fmt.Sprintf("Delete %d empty bucket(s)?", len(due))) {
		return newAborted("aborted: no bucket was deleted (re-run with --yes)")
	}
	logPath, logFile := openCleanupLog()
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}
	deleted := 0
	for _, d := range due {
		res := bucketsweep.Sweep(ctx, s3For(d.Bucket.Region), d.Bucket, bucketsweep.Options{Account: account, Now: now, Writes: true, Delete: true})
		switch res.Action {
		case bucketsweep.Deleted:
			deleted++
			fmt.Fprintf(out, "  ✓ deleted %s (%s)\n", d.Bucket.Name, d.Bucket.Region)
			writeCleanupLog(logFile, fmt.Sprintf("REMOVED arn:aws:s3:::%s\t%s\tbucket", d.Bucket.Name, d.Bucket.Region))
		case bucketsweep.Failed:
			failed++
			fmt.Fprintf(errOut, "  ✗ %s (%s): %s: %v\n", d.Bucket.Name, d.Bucket.Region, res.Reason, res.Err)
			writeCleanupLog(logFile, fmt.Sprintf("FAILED arn:aws:s3:::%s\t%s\tbucket\t%v", d.Bucket.Name, d.Bucket.Region, res.Err))
		default:
			fmt.Fprintf(out, "  - kept %s (%s): %s %s\n", d.Bucket.Name, d.Bucket.Region, res.Action, res.Reason)
		}
	}
	fmt.Fprintf(out, "\nDeleted %d bucket(s)", deleted)
	if logPath != "" {
		fmt.Fprintf(out, " — log: %s", logPath)
	}
	fmt.Fprintln(out, ".")
	if failed > 0 {
		return fmt.Errorf("%d bucket(s) could not be checked or deleted", failed)
	}
	return nil
}

func printBucketResult(out, errOut io.Writer, res bucketsweep.Result, writes bool, now time.Time) {
	b := res.Bucket
	would := ""
	if !writes {
		would = "would be "
	}
	switch res.Action {
	case bucketsweep.Absent:
		return
	case bucketsweep.Kept:
		fmt.Fprintf(out, "  %s (%s): kept, %s\n", b.Name, b.Region, res.Reason)
	case bucketsweep.InUse:
		fmt.Fprintf(out, "  %s (%s): in use (%s)\n", b.Name, b.Region, res.Reason)
	case bucketsweep.Stamped:
		fmt.Fprintf(out, "  %s (%s): empty; %stagged %s=%s, deletable after %s\n", b.Name, b.Region, would,
			bucketsweep.EmptySinceTagKey, res.EmptySince.Format(time.RFC3339), res.EmptySince.Add(bucketsweep.Grace).Format("2006-01-02"))
	case bucketsweep.Waiting:
		fmt.Fprintf(out, "  %s (%s): empty since %s; deletable after %s\n", b.Name, b.Region,
			res.EmptySince.Format(time.RFC3339), res.EmptySince.Add(bucketsweep.Grace).Format("2006-01-02"))
	case bucketsweep.WouldDelete:
		fmt.Fprintf(out, "  %s (%s): empty since %s (%d days); will be deleted\n", b.Name, b.Region,
			res.EmptySince.Format(time.RFC3339), int(now.Sub(res.EmptySince).Hours()/24))
	case bucketsweep.Failed:
		fmt.Fprintf(errOut, "%s  %s (%s): %s: %v\n", i18n.Symbol("warning"), b.Name, b.Region, res.Reason, res.Err)
	}
}

// runCleanupInstanceRoles deletes the per-task spawn-instance-* roles and
// profiles that instancerole.Decide says are no longer needed (#1300): the
// rule the reaper applies, from the same package. It previews first, asks,
// then sweeps again with reap on, so each role is re-decided at the moment it
// is deleted rather than trusted from the preview.
func runCleanupInstanceRoles(ctx context.Context, cmd *cobra.Command, iamc instancerole.API, ec2s map[string]instancerole.EC2Describer, now time.Time) error {
	out := cmd.OutOrStdout()
	inUse, err := instancerole.ProfilesInUse(ctx, ec2s)
	if err != nil {
		return fmt.Errorf("could not list instances, so no role can be shown to be unused: %w", err)
	}
	preview, err := instancerole.Sweep(ctx, iamc, inUse, now, false)
	if err != nil {
		return err
	}
	scanned, _, would, _ := preview.Count()
	if scanned == 0 {
		fmt.Fprintf(out, "No %s* IAM roles in this account.\n", instancerole.NamePrefix)
		return nil
	}
	fmt.Fprintf(out, "%d %s* role(s), checked against instances in %d region(s):\n", scanned, instancerole.NamePrefix, len(ec2s))
	tw := newTableWriter(out)
	fmt.Fprintf(tw, "  ROLE\tACTION\tCREATED\tLAST USED\tWHY KEPT\n")
	for _, d := range preview.Decisions {
		action, why := "keep", d.Reason
		if d.Err != nil {
			why = fmt.Sprintf("%s: %v", d.Reason, d.Err)
		}
		if d.Delete {
			action, why = "delete", ""
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", d.Name, action, roleDay(d.Created), roleDay(d.LastUsed), why)
	}
	tw.Flush()

	if would == 0 {
		fmt.Fprintln(out, "\nNothing to delete.")
		return nil
	}
	if cleanupDryRun {
		fmt.Fprintf(out, "\nDry run: %d role(s) and their instance profiles would be deleted. Re-run without --dry-run to delete.\n", would)
		return nil
	}
	if !confirmYes(cleanupYes, fmt.Sprintf("Delete %d IAM role(s) and their instance profiles?", would)) {
		return newAborted("aborted: no IAM role was deleted (re-run with --yes)")
	}

	res, err := instancerole.Sweep(ctx, iamc, inUse, now, true)
	if err != nil {
		return err
	}
	logPath, logFile := openCleanupLog()
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}
	_, deleted, _, _ := res.Count()
	failed := 0
	for _, d := range res.Decisions {
		switch {
		case d.Deleted:
			fmt.Fprintf(out, "  ✓ deleted role and instance profile %s\n", d.Name)
			writeCleanupLog(logFile, fmt.Sprintf("REMOVED %s\tiam\trole+instance-profile", d.Name))
		case d.Delete && d.Err != nil:
			failed++
			fmt.Fprintf(cmd.ErrOrStderr(), "  ✗ %s: %v\n", d.Name, d.Err)
			writeCleanupLog(logFile, fmt.Sprintf("FAILED %s\tiam\trole+instance-profile\t%v", d.Name, d.Err))
		}
	}
	fmt.Fprintf(out, "\nDeleted %d role(s)", deleted)
	if failed > 0 {
		fmt.Fprintf(out, ", %d failed", failed)
	}
	if logPath != "" {
		fmt.Fprintf(out, " — log: %s", logPath)
	}
	fmt.Fprintln(out, ".")
	if failed > 0 {
		return fmt.Errorf("%d IAM role(s) could not be deleted", failed)
	}
	return nil
}

// roleDay is a date for the role table; "never" for a zero time.
func roleDay(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02")
}
