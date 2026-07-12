package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

var (
	stopJobArrayID        string
	stopJobArrayName      string
	stopYes               bool
	hibernateJobArrayID   string
	hibernateJobArrayName string
	startJobArrayID       string
	startJobArrayName     string
)

// stop command
var stopCmd = &cobra.Command{
	Use:   "stop [instance-id-or-name]",
	Short: "Stop a running instance (preserves EBS volumes)",
	RunE:  runStop,
	Args:  cobra.RangeArgs(0, 1),
}

// hibernate command
var hibernateCmd = &cobra.Command{
	Use:   "hibernate [instance-id-or-name]",
	Short: "Hibernate an instance to disk (saves RAM state)",
	RunE:  runHibernate,
	Args:  cobra.RangeArgs(0, 1),
}

// start command
var startCmd = &cobra.Command{
	Use:   "start [instance-id-or-name]",
	Short: "Start a stopped or hibernated instance",
	RunE:  runStart,
	Args:  cobra.RangeArgs(0, 1),
}

func init() {
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(hibernateCmd)
	rootCmd.AddCommand(startCmd)

	// Register completion for instance ID arguments
	stopCmd.ValidArgsFunction = completeInstanceID
	hibernateCmd.ValidArgsFunction = completeInstanceID
	startCmd.ValidArgsFunction = completeInstanceID

	// Add job array flags
	stopCmd.Flags().StringVar(&stopJobArrayID, "job-array-id", "", "Stop all instances in job array by ID")
	stopCmd.Flags().StringVar(&stopJobArrayName, "job-array-name", "", "Stop all instances in job array by name")
	stopCmd.Flags().BoolVarP(&stopYes, "yes", "y", false, "Skip the confirmation prompt")
	hibernateCmd.Flags().StringVar(&hibernateJobArrayID, "job-array-id", "", "Hibernate all instances in job array by ID")
	hibernateCmd.Flags().StringVar(&hibernateJobArrayName, "job-array-name", "", "Hibernate all instances in job array by name")
	startCmd.Flags().StringVar(&startJobArrayID, "job-array-id", "", "Start all instances in job array by ID")
	startCmd.Flags().StringVar(&startJobArrayName, "job-array-name", "", "Start all instances in job array by name")
}

func runStop(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Determine if stopping job array or single instance
	if stopJobArrayID != "" || stopJobArrayName != "" {
		// Job array mode
		if len(args) != 0 {
			return fmt.Errorf("job array mode does not accept instance ID argument")
		}
		return stopOrHibernateJobArray(ctx, false)
	}

	// Single instance mode
	if len(args) != 1 {
		return fmt.Errorf("single instance mode requires 1 argument: <instance-id-or-name>")
	}
	return stopOrHibernate(args[0], false, stopYes)
}

func runHibernate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Determine if hibernating job array or single instance
	if hibernateJobArrayID != "" || hibernateJobArrayName != "" {
		// Job array mode
		if len(args) != 0 {
			return fmt.Errorf("job array mode does not accept instance ID argument")
		}
		return stopOrHibernateJobArray(ctx, true)
	}

	// Single instance mode
	if len(args) != 1 {
		return fmt.Errorf("single instance mode requires 1 argument: <instance-id-or-name>")
	}
	// hibernate keeps its existing no-prompt behavior (skipConfirm=true); the
	// confirmation prompt was scoped to `spawn stop`.
	return stopOrHibernate(args[0], true, true)
}

func stopOrHibernate(identifier string, hibernate bool, skipConfirm bool) error {
	ctx := context.Background()

	// Create AWS client
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Resolve instance
	instance, err := resolveInstance(ctx, client, identifier)
	if err != nil {
		return err
	}

	// Check current state
	if instance.State == "stopped" {
		return fmt.Errorf("instance %s is already stopped", instance.InstanceID)
	}

	if instance.State != "running" {
		return fmt.Errorf("instance %s is not running (state: %s)", instance.InstanceID, instance.State)
	}

	action := "stop"
	actionTitle := "Stop"
	if hibernate {
		action = "hibernate"
		actionTitle = "Hibernate"
	}

	fmt.Fprintf(os.Stderr, "Found instance in %s (state: %s)\n", instance.Region, instance.State)

	label := instance.InstanceID
	if instance.Name != "" {
		label = fmt.Sprintf("%s (%s)", instance.Name, instance.InstanceID)
	}
	if !confirmYes(skipConfirm, fmt.Sprintf("%s instance %s?", actionTitle, label)) {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Requesting %s for instance %s...\n", action, instance.InstanceID)

	// Calculate remaining TTL if set
	remainingTTL := ""
	if instance.TTL != "" {
		// Parse the TTL duration
		ttlDuration, err := parseDuration(instance.TTL)
		if err == nil {
			// Calculate how long the instance has been running
			uptime := time.Since(instance.LaunchTime)
			remaining := ttlDuration - uptime

			if remaining > 0 {
				// Store remaining time so it can be restored on start
				remainingTTL = formatDuration(remaining)
				fmt.Fprintf(os.Stderr, "Saving remaining TTL: %s (was: %s, uptime: %s)\n",
					remainingTTL, instance.TTL, uptime.Round(time.Minute))
			}
		}
	}

	// Note: we deliberately do NOT run plugin local deprovision here — stop and
	// hibernate are resumable, so tearing down a plugin's controller-side
	// footprint (e.g. a mutagen sync session) would be wrong. The session parks
	// while the instance is down; `spawn start` reconciles it (a stop/start
	// reassigns the public IP, which the pinned session can't follow on its own).
	// Local deprovision runs only on `spawn plugin remove` / `spawn terminate`.
	err = client.StopInstance(ctx, instance.Region, instance.InstanceID, hibernate)
	if err != nil {
		return fmt.Errorf("failed to %s instance: %w", action, err)
	}

	// Tag the instance with the stop reason and remaining TTL
	stopReason := "user-stopped"
	if hibernate {
		stopReason = "user-hibernated"
	}
	tags := map[string]string{
		"spawn:last-stop-reason": stopReason,
		"spawn:last-stop-time":   time.Now().UTC().Format(time.RFC3339),
	}
	if remainingTTL != "" {
		tags["spawn:ttl-remaining"] = remainingTTL
	}
	_ = client.UpdateInstanceTags(ctx, instance.Region, instance.InstanceID, tags)

	if hibernate {
		_, _ = fmt.Fprintf(os.Stdout, "\n✅ Hibernate request sent!\n")
		_, _ = fmt.Fprintf(os.Stdout, "   Instance: %s\n", instance.InstanceID)
		_, _ = fmt.Fprintf(os.Stdout, "   Region:   %s\n", instance.Region)
		_, _ = fmt.Fprintf(os.Stdout, "\nThe instance will hibernate (save RAM to disk) and stop.\n")
		_, _ = fmt.Fprintf(os.Stdout, "TTL countdown will pause until the instance is started again.\n")
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "\n✅ Stop request sent!\n")
		_, _ = fmt.Fprintf(os.Stdout, "   Instance: %s\n", instance.InstanceID)
		_, _ = fmt.Fprintf(os.Stdout, "   Region:   %s\n", instance.Region)
		_, _ = fmt.Fprintf(os.Stdout, "\nThe instance will stop (EBS preserved).\n")
		_, _ = fmt.Fprintf(os.Stdout, "TTL countdown will pause until the instance is started again.\n")
	}

	return nil
}

func stopOrHibernateJobArray(ctx context.Context, hibernate bool) error {
	action := "stop"
	if hibernate {
		action = "hibernate"
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
	jobArrayID := stopJobArrayID
	jobArrayName := stopJobArrayName
	if hibernate {
		jobArrayID = hibernateJobArrayID
		jobArrayName = hibernateJobArrayName
	}

	for _, inst := range instances {
		if jobArrayID != "" && inst.JobArrayID == jobArrayID {
			jobArrayInstances = append(jobArrayInstances, inst)
		} else if jobArrayName != "" && inst.JobArrayName == jobArrayName {
			jobArrayInstances = append(jobArrayInstances, inst)
		}
	}

	if len(jobArrayInstances) == 0 {
		if jobArrayID != "" {
			return fmt.Errorf("no instances found with job-array-id: %s", jobArrayID)
		}
		return fmt.Errorf("no instances found with job-array-name: %s", jobArrayName)
	}

	// Display summary
	arrayName := jobArrayInstances[0].JobArrayName
	if arrayName == "" {
		arrayName = "unnamed"
	}
	arrayIDVal := jobArrayInstances[0].JobArrayID

	fmt.Fprintf(os.Stderr, "Found job array: %s (%d instances)\n", arrayName, len(jobArrayInstances))
	fmt.Fprintf(os.Stderr, "Array ID: %s\n", arrayIDVal)
	fmt.Fprintf(os.Stderr, "\n%s all instances...\n", action)

	// Process each instance
	successCount := 0
	failedInstances := []string{}

	for _, inst := range jobArrayInstances {
		// Skip already stopped instances
		if inst.State == "stopped" || inst.State == "stopping" {
			fmt.Fprintf(os.Stderr, "⏭  Skipping %s (already %s)\n", inst.InstanceID, inst.State)
			continue
		}

		if inst.State != "running" {
			fmt.Fprintf(os.Stderr, "⏭  Skipping %s (state: %s)\n", inst.InstanceID, inst.State)
			continue
		}

		// Calculate remaining TTL if set
		remainingTTL := ""
		if inst.TTL != "" {
			ttlDuration, err := parseDuration(inst.TTL)
			if err == nil {
				uptime := time.Since(inst.LaunchTime)
				remaining := ttlDuration - uptime
				if remaining > 0 {
					remainingTTL = formatDuration(remaining)
				}
			}
		}

		// Stop/hibernate the instance
		err := client.StopInstance(ctx, inst.Region, inst.InstanceID, hibernate)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to %s %s: %v\n", action, inst.InstanceID, err)
			failedInstances = append(failedInstances, inst.InstanceID)
			continue
		}

		// Tag the instance
		stopReason := "user-stopped"
		if hibernate {
			stopReason = "user-hibernated"
		}
		tags := map[string]string{
			"spawn:last-stop-reason": stopReason,
			"spawn:last-stop-time":   time.Now().UTC().Format(time.RFC3339),
		}
		if remainingTTL != "" {
			tags["spawn:ttl-remaining"] = remainingTTL
		}
		_ = client.UpdateInstanceTags(ctx, inst.Region, inst.InstanceID, tags)

		successCount++
		fmt.Fprintf(os.Stderr, "✓ %s %s\n", action, inst.InstanceID)
	}

	// Display results
	_, _ = fmt.Fprintf(os.Stdout, "\n✅ Job array %s request sent!\n", action)
	_, _ = fmt.Fprintf(os.Stdout, "   Array:     %s\n", arrayName)
	_, _ = fmt.Fprintf(os.Stdout, "   Processed: %d/%d instances\n", successCount, len(jobArrayInstances))

	if len(failedInstances) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Failed to %s %d instances:\n", action, len(failedInstances))
		for _, id := range failedInstances {
			fmt.Fprintf(os.Stderr, "   - %s\n", id)
		}
	}

	return nil
}

func runStart(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Determine if starting job array or single instance
	if startJobArrayID != "" || startJobArrayName != "" {
		// Job array mode
		if len(args) != 0 {
			return fmt.Errorf("job array mode does not accept instance ID argument")
		}
		return startJobArray(ctx)
	}

	// Single instance mode
	if len(args) != 1 {
		return fmt.Errorf("single instance mode requires 1 argument: <instance-id-or-name>")
	}
	identifier := args[0]

	// Create AWS client
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Resolve instance
	instance, err := resolveInstance(ctx, client, identifier)
	if err != nil {
		return err
	}

	// Check current state
	if instance.State == "running" {
		return fmt.Errorf("instance %s is already running", instance.InstanceID)
	}

	if instance.State != "stopped" {
		return fmt.Errorf("instance %s cannot be started (state: %s)", instance.InstanceID, instance.State)
	}

	fmt.Fprintf(os.Stderr, "Found instance in %s (state: %s)\n", instance.Region, instance.State)
	fmt.Fprintf(os.Stderr, "Starting instance %s...\n", instance.InstanceID)

	// Start the instance
	err = client.StartInstance(ctx, instance.Region, instance.InstanceID)
	if err != nil {
		return fmt.Errorf("failed to start instance: %w", err)
	}

	// Check if there's a saved TTL to restore
	tags := map[string]string{
		"spawn:last-start-time": time.Now().UTC().Format(time.RFC3339),
	}

	if ttlRemaining, exists := instance.Tags["spawn:ttl-remaining"]; exists && ttlRemaining != "" {
		// Restore the remaining TTL
		tags["spawn:ttl"] = ttlRemaining
		// Clear the saved remaining TTL (will be set in subsequent update)
		fmt.Fprintf(os.Stderr, "Restoring TTL: %s (was paused during stop)\n", ttlRemaining)
	}

	// Tag the instance with the start event and restored TTL
	_ = client.UpdateInstanceTags(ctx, instance.Region, instance.InstanceID, tags)

	_, _ = fmt.Fprintf(os.Stdout, "\n✅ Start request sent!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   Instance: %s\n", instance.InstanceID)
	_, _ = fmt.Fprintf(os.Stdout, "   Region:   %s\n", instance.Region)
	_, _ = fmt.Fprintf(os.Stdout, "\nThe instance is starting up...\n")
	_, _ = fmt.Fprintf(os.Stdout, "TTL countdown will resume once the instance is running.\n")

	// Wait for instance to be running AND to have a public IP. The public IP is
	// often not populated the instant the state flips to "running", and a
	// stop/start reassigns it — we need the new address to re-point any local
	// plugin footprint (spore-sync's mutagen session), so keep polling until the
	// IP appears rather than returning on the first "running".
	fmt.Fprintf(os.Stderr, "\nWaiting for instance to reach running state...")
	running := false
	newIP := ""
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)

		instances, err := client.ListInstances(ctx, instance.Region, "")
		if err != nil {
			break
		}

		for _, inst := range instances {
			if inst.InstanceID == instance.InstanceID {
				if inst.State == "running" {
					if !running {
						fmt.Fprintf(os.Stderr, " running!\n")
						running = true
					}
					newIP = inst.PublicIP
				}
				break
			}
		}
		if running && newIP != "" {
			break
		}
		fmt.Fprintf(os.Stderr, ".")
	}

	if running && newIP != "" {
		// A stop/start reassigns the public IP; re-point any local plugin
		// footprint (e.g. spore-sync's mutagen session) that declares reconcile
		// steps, so it follows the new address.
		reconcileAllLocalPlugins(ctx, newIP, instance.InstanceID, instance.Name)
		_, _ = fmt.Fprintf(os.Stdout, "\n🔌 Connect: spawn connect %s\n", instance.InstanceID)
		return nil
	}

	fmt.Fprintf(os.Stderr, " (taking longer than expected)\n")
	_, _ = fmt.Fprintf(os.Stdout, "\nUse 'spawn list' to check the current state.\n")

	return nil
}

func startJobArray(ctx context.Context) error {
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
		if startJobArrayID != "" && inst.JobArrayID == startJobArrayID {
			jobArrayInstances = append(jobArrayInstances, inst)
		} else if startJobArrayName != "" && inst.JobArrayName == startJobArrayName {
			jobArrayInstances = append(jobArrayInstances, inst)
		}
	}

	if len(jobArrayInstances) == 0 {
		if startJobArrayID != "" {
			return fmt.Errorf("no instances found with job-array-id: %s", startJobArrayID)
		}
		return fmt.Errorf("no instances found with job-array-name: %s", startJobArrayName)
	}

	// Display summary
	arrayName := jobArrayInstances[0].JobArrayName
	if arrayName == "" {
		arrayName = "unnamed"
	}
	arrayID := jobArrayInstances[0].JobArrayID

	fmt.Fprintf(os.Stderr, "Found job array: %s (%d instances)\n", arrayName, len(jobArrayInstances))
	fmt.Fprintf(os.Stderr, "Array ID: %s\n", arrayID)
	fmt.Fprintf(os.Stderr, "\nStarting all instances...\n")

	// Process each instance
	successCount := 0
	failedInstances := []string{}

	for _, inst := range jobArrayInstances {
		// Skip already running instances
		if inst.State == "running" {
			fmt.Fprintf(os.Stderr, "⏭  Skipping %s (already running)\n", inst.InstanceID)
			continue
		}

		if inst.State != "stopped" {
			fmt.Fprintf(os.Stderr, "⏭  Skipping %s (state: %s)\n", inst.InstanceID, inst.State)
			continue
		}

		// Start the instance
		err := client.StartInstance(ctx, inst.Region, inst.InstanceID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to start %s: %v\n", inst.InstanceID, err)
			failedInstances = append(failedInstances, inst.InstanceID)
			continue
		}

		// Tag the instance with start time and restore TTL if available
		tags := map[string]string{
			"spawn:last-start-time": time.Now().UTC().Format(time.RFC3339),
		}
		if ttlRemaining, exists := inst.Tags["spawn:ttl-remaining"]; exists && ttlRemaining != "" {
			tags["spawn:ttl"] = ttlRemaining
		}
		_ = client.UpdateInstanceTags(ctx, inst.Region, inst.InstanceID, tags)

		successCount++
		fmt.Fprintf(os.Stderr, "✓ Started %s\n", inst.InstanceID)
	}

	// Display results
	_, _ = fmt.Fprintf(os.Stdout, "\n✅ Job array start request sent!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   Array:     %s\n", arrayName)
	_, _ = fmt.Fprintf(os.Stdout, "   Started:   %d/%d instances\n", successCount, len(jobArrayInstances))

	if len(failedInstances) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Failed to start %d instances:\n", len(failedInstances))
		for _, id := range failedInstances {
			fmt.Fprintf(os.Stderr, "   - %s\n", id)
		}
	}

	_, _ = fmt.Fprintf(os.Stdout, "\nInstances are starting up...\n")
	_, _ = fmt.Fprintf(os.Stdout, "Use 'spawn list --job-array-name %s' to check status.\n", arrayName)

	return nil
}

// parseDuration parses a TTL duration string (e.g., "2h", "30m", "1h30m")
// parseDuration parses a TTL string, accepting Go's native duration syntax plus
// a day unit (e.g. "2d", "1d12h"). It delegates to the shared day-aware parser
// in pkg/config so the CLI and the config-file path stay in lockstep.
func parseDuration(ttl string) (time.Duration, error) {
	return spawnconfig.ParseDurationE(ttl)
}
