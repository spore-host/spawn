package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/spotrequest"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
)

var (
	extendJobArrayID   string
	extendJobArrayName string
)

var extendCmd = &cobra.Command{
	Use:  "extend <instance-id-or-name> <duration>",
	RunE: runExtend,
	Args: cobra.RangeArgs(0, 2),
	// Short and Long will be set after i18n initialization
}

var (
	extendCostLimit     float64
	extendKeepCostLimit bool
)

func init() {
	rootCmd.AddCommand(extendCmd)

	// spawn:cost-limit fires independently of the TTL ("first-to-fire wins"), so
	// moving only the TTL left extend a no-op for anyone whose cap was sized to it
	// (#639). By default the cap is raised to whatever the new deadline needs.
	extendCmd.Flags().Float64Var(&extendCostLimit, "cost-limit", 0,
		"Set the instance's cost limit to this amount in USD, instead of raising it to what the new TTL needs")
	extendCmd.Flags().BoolVar(&extendKeepCostLimit, "keep-cost-limit", false,
		"Extend the TTL and leave the cost limit alone. spored will still terminate the instance when the existing cap is reached, which may be before the new deadline.")

	// Register completion for instance ID argument
	extendCmd.ValidArgsFunction = completeInstanceID

	// Add job array flags
	extendCmd.Flags().StringVar(&extendJobArrayID, "job-array-id", "", "Extend TTL for all instances in job array by ID")
	extendCmd.Flags().StringVar(&extendJobArrayName, "job-array-name", "", "Extend TTL for all instances in job array by name")
}

func runExtend(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Get user identity for audit logging
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	userID, err := aws.NewClientFromConfig(cfg).GetAccountID(ctx)
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	correlationID := uuid.New().String()
	auditLog := newAuditLogger(userID, correlationID)

	// Determine if extending job array or single instance
	if extendJobArrayID != "" || extendJobArrayName != "" {
		// Job array mode
		if len(args) != 1 {
			return fmt.Errorf("job array mode requires exactly 1 argument: <duration>")
		}
		return extendJobArrayWithAudit(ctx, args[0], auditLog)
	}

	// Single instance mode
	if len(args) != 2 {
		return fmt.Errorf("single instance mode requires 2 arguments: <instance-id-or-name> <duration>")
	}

	instanceIdentifier := args[0]
	newTTL := args[1]

	// Parse the duration before any AWS call, with the shared parser spored
	// and the reaper use (#990), so "1d" works and "1w" fails in the shell.
	extendDuration, err := parseExtendDuration(newTTL)
	if err != nil {
		return fmt.Errorf("invalid TTL format: %w", err)
	}

	// Create AWS client
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Resolve instance (by ID or name)
	instance, err := resolveInstance(ctx, client, instanceIdentifier)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Found instance in %s (current TTL: %s)\n", instance.Region, instance.TTL)

	// Log TTL extension initiation
	auditLog.LogOperationWithData("extend_ttl", instance.InstanceID, "initiated",
		map[string]interface{}{
			"old_ttl": instance.TTL,
			"new_ttl": newTTL,
		}, nil)

	// Extend the absolute deadline, anchored to the original launch time.
	// TTL is always relative to first launch — extending adds to the current deadline,
	// not to the current clock time.
	tags, newDeadline, costDecision := extensionTags(instance, newTTL, extendDuration, time.Now(), extendCostLimit, extendKeepCostLimit)

	// A persistent Spot request's end date was fixed at launch to the old
	// deadline (#950), and EC2 cannot move it. Say so before extending past it.
	if w := spotrequest.ExtendWarning(instance.Tags, newDeadline); w != "" {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: %s\n", w)
	}

	fmt.Fprintf(os.Stderr, "Extending TTL deadline to %s...\n", newDeadline.UTC().Format("2006-01-02 15:04 UTC"))
	err = client.UpdateInstanceTags(ctx, instance.Region, instance.InstanceID, tags)
	if err != nil {
		auditLog.LogOperationWithRegion("extend_ttl", instance.InstanceID, instance.Region, "failed", err)
		return fmt.Errorf("failed to update TTL: %w", err)
	}

	auditLog.LogOperationWithRegion("extend_ttl", instance.InstanceID, instance.Region, "success", nil)

	_, _ = fmt.Fprintf(os.Stdout, "\n✅ TTL extended successfully!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   Instance:     %s\n", instance.InstanceID)
	_, _ = fmt.Fprintf(os.Stdout, "   Extended by:  %s\n", newTTL)
	_, _ = fmt.Fprintf(os.Stdout, "   New TTL:      %s (total from launch)\n", tags["spawn:ttl"])
	_, _ = fmt.Fprintf(os.Stdout, "   New deadline: %s\n", newDeadline.UTC().Format("2006-01-02 15:04 UTC"))
	_, _ = fmt.Fprint(os.Stdout, renderCostLimitDecision(costDecision))

	// Trigger reload on instance
	fmt.Fprintf(os.Stderr, "\nTriggering configuration reload on instance...\n")
	if err := triggerReload(instance); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to trigger reload: %v\n", err)
		fmt.Fprintf(os.Stderr, "   You may need to manually run: ssh %s@%s 'sudo spored reload'\n",
			resolveSSHUser("", instance), instance.PublicIP)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "✓ Configuration reloaded on instance\n")
	}

	return nil
}

func extendJobArrayWithAudit(ctx context.Context, newTTL string, auditLog *audit.AuditLogger) error {
	// Parse before any AWS call and before anything is written (#990).
	extendDuration, err := parseExtendDuration(newTTL)
	if err != nil {
		return fmt.Errorf("invalid TTL format: %w", err)
	}

	// Create AWS client
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// List all instances
	instances, err := client.ListInstances(ctx, "", "")
	if err != nil {
		return fmt.Errorf("failed to list instances: %w", err)
	}

	// Filter instances by job array
	var jobArrayInstances []aws.InstanceInfo
	for _, inst := range instances {
		if extendJobArrayID != "" && inst.JobArrayID == extendJobArrayID {
			jobArrayInstances = append(jobArrayInstances, inst)
		} else if extendJobArrayName != "" && inst.JobArrayName == extendJobArrayName {
			jobArrayInstances = append(jobArrayInstances, inst)
		}
	}

	if len(jobArrayInstances) == 0 {
		if extendJobArrayID != "" {
			return fmt.Errorf("no instances found with job-array-id: %s", extendJobArrayID)
		}
		return fmt.Errorf("no instances found with job-array-name: %s", extendJobArrayName)
	}

	// Display summary
	arrayName := jobArrayInstances[0].JobArrayName
	if arrayName == "" {
		arrayName = "unnamed"
	}
	arrayID := jobArrayInstances[0].JobArrayID

	fmt.Fprintf(os.Stderr, "Found job array: %s (%d instances)\n", arrayName, len(jobArrayInstances))
	fmt.Fprintf(os.Stderr, "Array ID: %s\n", arrayID)
	fmt.Fprintf(os.Stderr, "\nExtending TTL by %s for all instances...\n", newTTL)

	// Log job array TTL extension initiation
	auditLog.LogOperationWithData("extend_job_array_ttl", arrayID, "initiated",
		map[string]interface{}{
			"array_name":     arrayName,
			"instance_count": len(jobArrayInstances),
			"new_ttl":        newTTL,
		}, nil)

	// Each instance goes through extensionTags, exactly as the single-instance
	// path does: spawn:ttl-deadline (what spored and the TTL reaper enforce) is
	// pushed forward from THAT instance's own deadline, spawn:ttl stays the total
	// from launch, and the cost limit is raised per instance when its cap would
	// otherwise fire first (#639). Before #983 this path wrote only
	// spawn:ttl=<raw argument>, so it reported success while every instance kept
	// its original, enforced deadline.
	//
	// The per-instance accounting lives in extendJobArrayInstances so that a
	// reload failure is tracked and reported, not discarded (spawn#512), and is
	// unit-testable without a real AWS client or SSH.
	updateTags := jobArrayTagUpdater(
		func(region, instanceID string, tags map[string]string) error {
			return client.UpdateInstanceTags(ctx, region, instanceID, tags)
		},
		newTTL, extendDuration, time.Now(), extendCostLimit, extendKeepCostLimit, os.Stderr)
	successCount, failedInstances, reloadFailedInstances := extendJobArrayInstances(
		jobArrayInstances, updateTags, triggerReload, os.Stderr)

	// Display results
	_, _ = fmt.Fprintf(os.Stdout, "\n✅ Job array TTL extended!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   Array:     %s\n", arrayName)
	_, _ = fmt.Fprintf(os.Stdout, "   Extended:  by %s (each instance's deadline moved from its own current deadline)\n", newTTL)
	_, _ = fmt.Fprintf(os.Stdout, "   Updated:   %d/%d instances\n", successCount, len(jobArrayInstances))
	if len(reloadFailedInstances) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "   Reloaded:  %d/%d instances (%d tag-updated but not confirmed reloaded — see warnings above)\n",
			successCount-len(reloadFailedInstances), successCount, len(reloadFailedInstances))
	}

	if len(failedInstances) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Failed to update %d instances:\n", len(failedInstances))
		for _, id := range failedInstances {
			fmt.Fprintf(os.Stderr, "   - %s\n", id)
		}
		auditLog.LogOperationWithData("extend_job_array_ttl", arrayID, "partial_success",
			map[string]interface{}{
				"success_count":       successCount,
				"failed_count":        len(failedInstances),
				"reload_failed_count": len(reloadFailedInstances),
			}, nil)
	} else if len(reloadFailedInstances) > 0 {
		auditLog.LogOperationWithData("extend_job_array_ttl", arrayID, "partial_success",
			map[string]interface{}{
				"success_count":       successCount,
				"reload_failed_count": len(reloadFailedInstances),
			}, nil)
	} else {
		auditLog.LogOperationWithData("extend_job_array_ttl", arrayID, "success",
			map[string]interface{}{
				"instance_count": successCount,
			}, nil)
	}

	return nil
}

// extendJobArrayInstances applies updateTags to every instance in the job
// array and, for each one whose tag write succeeds, calls reload to trigger
// the on-instance config refresh. It returns the count of successful tag
// writes, the IDs of instances whose tag write failed, and the IDs of
// instances whose tag write succeeded but whose reload call failed.
//
// The reload failure must be tracked, not just printed and discarded — before
// spawn#512, a reload error was thrown away with `_ = triggerReload(...)`, so
// a caller extending a job array of N instances had no way to tell which (if
// any) actually picked up the new TTL on-instance vs. which are running on
// stale config until the next periodic tag refresh (every 5 minutes in
// production). This mirrors the single-instance path in runExtend, which
// warns and prints a manual SSH fallback on a reload failure.
func extendJobArrayInstances(
	instances []aws.InstanceInfo,
	updateTags func(inst *aws.InstanceInfo) error,
	reload func(*aws.InstanceInfo) error,
	stderr io.Writer,
) (successCount int, failedInstances, reloadFailedInstances []string) {
	for _, inst := range instances {
		if err := updateTags(&inst); err != nil {
			fmt.Fprintf(stderr, "⚠️  Failed to update %s: %v\n", inst.InstanceID, err)
			failedInstances = append(failedInstances, inst.InstanceID)
			continue
		}
		successCount++
		if err := reload(&inst); err != nil {
			fmt.Fprintf(stderr, "⚠️  Tag updated for %s, but failed to trigger reload: %v\n", inst.InstanceID, err)
			fmt.Fprintf(stderr, "   Until the next periodic tag refresh, it is running on stale config. Manual fallback: ssh %s@%s 'sudo spored reload'\n",
				resolveSSHUser("", &inst), inst.PublicIP)
			reloadFailedInstances = append(reloadFailedInstances, inst.InstanceID)
		}
	}
	return successCount, failedInstances, reloadFailedInstances
}

// jobArrayTagUpdater returns the per-instance tag update the job-array path
// hands to extendJobArrayInstances. It computes each instance's tags with
// extensionTags — the same function the single-instance path uses — and writes
// them in one call, so spawn:ttl never moves without spawn:ttl-deadline (#983).
// Each instance's new deadline, and any cost-limit change, is printed to out.
func jobArrayTagUpdater(
	write func(region, instanceID string, tags map[string]string) error,
	newTTL string, extendDuration time.Duration, now time.Time,
	explicitCostLimit float64, keepCostLimit bool, out io.Writer,
) func(inst *aws.InstanceInfo) error {
	return func(inst *aws.InstanceInfo) error {
		tags, newDeadline, costDecision := extensionTags(inst, newTTL, extendDuration, now, explicitCostLimit, keepCostLimit)
		if err := write(inst.Region, inst.InstanceID, tags); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ %s: deadline %s\n", inst.InstanceID, newDeadline.UTC().Format("2006-01-02 15:04 UTC"))
		_, _ = fmt.Fprint(out, renderCostLimitDecision(costDecision))
		return nil
	}
}

// extensionTags is the complete tag set `spawn extend` writes for one
// instance, shared by the single-instance and job-array paths so the two
// cannot drift (#983): the TTL pair from computeExtendedTTLTags plus, when the
// cost limit would otherwise fire before the new deadline, a raised
// spawn:cost-limit. The cost limit is a SECOND, independent deadline; leave it
// alone and the extension silently fails for every instance whose cap was
// sized to its TTL (#639).
func extensionTags(instance *aws.InstanceInfo, newTTL string, extendDuration time.Duration, now time.Time, explicitCostLimit float64, keepCostLimit bool) (map[string]string, time.Time, costLimitDecision) {
	tags, newDeadline := computeExtendedTTLTags(instance, newTTL, extendDuration, now)
	costDecision := decideCostLimit(instance, newDeadline, now, explicitCostLimit, keepCostLimit)
	if costDecision.Changed() {
		tags["spawn:cost-limit"] = costLimitTagValue(costDecision.NewValue)
	}
	return tags, newDeadline, costDecision
}

// computeExtendedTTLTags computes the tag set `spawn extend` writes, given
// the instance's current tags, the requested extend duration string, its
// parsed duration, and the current time (injected so this is testable
// without a real clock). Returns the tags and the resolved new deadline.
//
// spawn:ttl-deadline is pushed forward from the existing deadline (or, for an
// older instance without one, from LaunchTime + the current spawn:ttl), with
// a floor of now+extendDuration so a stale/expired deadline can't reap the
// instance the moment it's extended. This ADD-to-the-existing-deadline
// semantics is what makes `extend` monotonic-forward-only by construction —
// it is the mirror image of `spored config set ttl` (pkg/ttl.SetTags), which
// takes newTTL as an absolute new total-from-launch and can therefore move
// the deadline earlier (spawn#553). Once the deadline is resolved here, tag
// rendering itself is delegated to pkg/ttl.Tags — the same function
// `config set ttl` uses — so there is exactly one place that decides what
// spawn:ttl looks like relative to spawn:ttl-deadline.
//
// spawn:ttl ends up as the new deadline minus the ORIGINAL launch time
// (spawn:launch-time; EC2's LaunchTime resets on every start, #992) — the
// total duration from launch, matching what every other reader of this tag
// (spored's pre-deadline fallback, spawn stop/hibernate's remaining-TTL
// preservation) expects it to mean. Writing the raw extend argument here
// instead (spawn#506) meant the tag stopped describing the instance's TTL at
// all after the first extend call — a repeated `extend <id> 8h` looked
// unchanged even though the deadline had genuinely moved twice.
func computeExtendedTTLTags(instance *aws.InstanceInfo, newTTL string, extendDuration time.Duration, now time.Time) (map[string]string, time.Time) {
	var newDeadline time.Time
	if dl, ok := instance.Tags["spawn:ttl-deadline"]; ok {
		if parsed, err := time.Parse(time.RFC3339, dl); err == nil {
			newDeadline = parsed.Add(extendDuration)
		}
	}
	// The original launch, not EC2's LaunchTime, which resets on every start
	// (#992): spawn:ttl is the total from the original launch everywhere else.
	launch := originalLaunchTime(instance)
	if newDeadline.IsZero() {
		// Older instance without a deadline tag. Its current deadline is
		// launch + spawn:ttl: that is what spored synthesizes and what the TTL
		// reaper enforces (#990). This used to be now + spawn:ttl, which also
		// handed back all the time already elapsed since launch.
		if cur, err := ttlpkg.ParseDuration(instance.TTL); err == nil && cur > 0 && !launch.IsZero() {
			newDeadline = launch.Add(cur).Add(extendDuration)
		} else {
			newDeadline = now.Add(extendDuration)
		}
	}
	if floor := now.Add(extendDuration); newDeadline.Before(floor) {
		newDeadline = floor
	}

	if launch.IsZero() {
		// No launch time to anchor spawn:ttl to (older instance) — fall back to
		// the raw argument for that tag only; spawn:ttl-deadline (the field
		// enforcement actually reads) is unaffected either way.
		return map[string]string{
			"spawn:ttl-deadline": newDeadline.UTC().Format(time.RFC3339),
			"spawn:ttl":          newTTL,
		}, newDeadline
	}
	return ttlpkg.Tags(launch, newDeadline), newDeadline
}

// validateTTL reports whether s is a valid `spawn extend` duration.
func validateTTL(s string) error {
	_, err := parseExtendDuration(s)
	return err
}

// parseExtendDuration parses a `spawn extend` duration with pkg/ttl's shared
// parser, the one launch, spored and the TTL reaper use (#990): Go duration
// syntax plus days ("2h", "90m", "1d", "2d12h", "1.5h"), and it must be
// positive.
//
// This used to be a regex that accepted "1d" followed by time.ParseDuration,
// which does not, so `spawn extend <id> 1d` passed validation and then failed
// (and, on the job-array path before #983, wrote spawn:ttl=1d with no
// deadline).
func parseExtendDuration(s string) (time.Duration, error) {
	d, err := ttlpkg.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("TTL must be in format <number><unit> (e.g., 2h, 30m, 1d, 2d12h): %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("TTL must be greater than 0")
	}
	return d, nil
}

func triggerReload(instance *aws.InstanceInfo) error {
	// Find SSH key
	keyPath, err := findSSHKey(instance.KeyName)
	if err != nil {
		return fmt.Errorf("failed to find SSH key: %w", err)
	}

	sshArgs := buildReloadSSHArgs(keyPath, instance)

	cmd := exec.Command("ssh", sshArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, string(output))
	}

	return nil
}

// buildReloadSSHArgs constructs the ssh argv `spawn extend` uses to run
// `spored reload` on an instance. It is pure/testable (no exec, no key lookup)
// so the login-user resolution can be verified directly.
//
// The login user comes from resolveSSHUser, NOT a hardcoded ec2-user. Hardcoding
// ec2-user here was #581: on an Ubuntu AMI the login user is `ubuntu`, so the SSH
// failed with "Permission denied (publickey)", the reload never ran, and the box
// kept self-terminating at its ORIGINAL TTL — a silent no-op on `spawn extend`,
// a lifecycle-critical operation.
func buildReloadSSHArgs(keyPath string, instance *aws.InstanceInfo) []string {
	user := resolveSSHUser("", instance)
	sshArgs := append([]string{"-i", keyPath}, sporedSSHOptions()...)
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", user, instance.PublicIP), "sudo /usr/local/bin/spored reload")
	return sshArgs
}
