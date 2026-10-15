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
	"github.com/spore-host/spawn/pkg/spotrequest"
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
	Long:  "Stop a running instance (preserves EBS volumes).\n\n" + ttlDeadlineDoesNotPause,
	RunE:  runStop,
	Args:  cobra.RangeArgs(0, 1),
}

// hibernate command
var hibernateCmd = &cobra.Command{
	Use:   "hibernate [instance-id-or-name]",
	Short: "Hibernate an instance to disk (saves RAM state)",
	Long:  "Hibernate an instance to disk (saves RAM state).\n\n" + ttlDeadlineDoesNotPause,
	RunE:  runHibernate,
	Args:  cobra.RangeArgs(0, 1),
}

// start command
var startCmd = &cobra.Command{
	Use:   "start [instance-id-or-name]",
	Short: "Start a stopped or hibernated instance",
	Long:  "Start a stopped or hibernated instance.\n\n" + ttlDeadlineDoesNotPause,
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
	return stopOrHibernateInstance(ctx, client, instance, hibernate, skipConfirm)
}

// stopOrHibernateInstance stops or hibernates an already-resolved instance.
func stopOrHibernateInstance(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo, hibernate, skipConfirm bool) error {
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

	// Before anything is printed or saved: EC2 refuses to stop an instance from a
	// one-time Spot request, and this used to print "Saving remaining TTL" and
	// then fail with UnsupportedOperation (#922).
	if err := oneTimeSpotStopError(instance, action); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Found instance in %s (state: %s)\n", instance.Region, instance.State)

	label := instance.InstanceID
	if instance.Name != "" {
		label = fmt.Sprintf("%s (%s)", instance.Name, instance.InstanceID)
	}
	if !confirmYes(skipConfirm, fmt.Sprintf("%s instance %s?", actionTitle, label)) {
		return newAborted("aborted: %s was left unchanged", label)
	}

	fmt.Fprintf(os.Stderr, "Requesting %s for instance %s...\n", action, instance.InstanceID)

	// Note: we deliberately do NOT run plugin local deprovision here — stop and
	// hibernate are resumable, so tearing down a plugin's controller-side
	// footprint (e.g. a mutagen sync session) would be wrong. The session parks
	// while the instance is down; `spawn start` reconciles it (a stop/start
	// reassigns the public IP, which the pinned session can't follow on its own).
	// Local deprovision runs only on `spawn plugin remove` / `spawn terminate`.
	err := client.StopInstance(ctx, instance.Region, instance.InstanceID, hibernate)
	if err != nil {
		return fmt.Errorf("failed to %s instance: %w", action, err)
	}

	// Record why and when it stopped. No TTL is saved: the deadline does not
	// pause, so there is nothing to restore at start (#928).
	_ = client.UpdateInstanceTags(ctx, instance.Region, instance.InstanceID, stopTags(hibernate, time.Now()))

	ttlNote := ttlDeadlineNotice(instance, "stop", statusLocation(), time.Now())
	if hibernate {
		_, _ = fmt.Fprintf(os.Stdout, "\n✅ Hibernate request sent!\n")
		_, _ = fmt.Fprintf(os.Stdout, "   Instance: %s\n", instance.InstanceID)
		_, _ = fmt.Fprintf(os.Stdout, "   Region:   %s\n", instance.Region)
		_, _ = fmt.Fprintf(os.Stdout, "\nThe instance will hibernate (save RAM to disk) and stop.\n")
		_, _ = fmt.Fprint(os.Stdout, ttlNote)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "\n✅ Stop request sent!\n")
		_, _ = fmt.Fprintf(os.Stdout, "   Instance: %s\n", instance.InstanceID)
		_, _ = fmt.Fprintf(os.Stdout, "   Region:   %s\n", instance.Region)
		_, _ = fmt.Fprintf(os.Stdout, "\nThe instance will stop (EBS preserved).\n")
		_, _ = fmt.Fprint(os.Stdout, ttlNote)
	}

	// A stopped instance stops billing for compute but its FSx lease does not:
	// the filesystem keeps running (and keeps the reaper's refcount above zero),
	// so a "stopped to save money" instance can still be accruing ~$174/month of
	// storage. Say so (spawn#613). Informational only — stop deletes nothing.
	if notice := fsxLeaseNoticeFor(ctx, client, *instance, action); notice != "" {
		_, _ = fmt.Fprint(os.Stdout, notice)
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

		if err := oneTimeSpotStopError(&inst, action); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Cannot %s %s: one-time Spot instance (EC2 cannot stop it)\n", action, inst.InstanceID)
			failedInstances = append(failedInstances, inst.InstanceID)
			continue
		}

		// Stop/hibernate the instance
		err := client.StopInstance(ctx, inst.Region, inst.InstanceID, hibernate)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to %s %s: %v\n", action, inst.InstanceID, err)
			failedInstances = append(failedInstances, inst.InstanceID)
			continue
		}

		// Tag the instance (no TTL saved: the deadline does not pause, #928).
		_ = client.UpdateInstanceTags(ctx, inst.Region, inst.InstanceID, stopTags(hibernate, time.Now()))

		successCount++
		fmt.Fprintf(os.Stderr, "✓ %s %s\n", action, inst.InstanceID)
	}

	// Display results
	_, _ = fmt.Fprintf(os.Stdout, "\n✅ Job array %s request sent!\n", action)
	_, _ = fmt.Fprintf(os.Stdout, "   Array:     %s\n", arrayName)
	_, _ = fmt.Fprintf(os.Stdout, "   Processed: %d/%d instances\n", successCount, len(jobArrayInstances))
	_, _ = fmt.Fprintf(os.Stdout, "\nEach instance's TTL deadline still applies while it is stopped.\n")

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
		return startFailedError(instance, err, time.Now())
	}

	// Record the start. The TTL is NOT touched: spawn:ttl is the total from
	// launch (pkg/ttl.Tags) and spawn:ttl-deadline is absolute, so there is
	// nothing to "restore" (#983, #928). An instance stopped by an older spawn
	// may still carry spawn:ttl-remaining; say plainly that it was not restored
	// (see startDoesNotRestoreTTLNotice).
	if notice := startDoesNotRestoreTTLNotice(instance); notice != "" {
		fmt.Fprint(os.Stderr, notice)
	}
	_ = client.UpdateInstanceTags(ctx, instance.Region, instance.InstanceID, startTags(time.Now()))

	_, _ = fmt.Fprintf(os.Stdout, "\n✅ Start request sent!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   Instance: %s\n", instance.InstanceID)
	_, _ = fmt.Fprintf(os.Stdout, "   Region:   %s\n", instance.Region)
	_, _ = fmt.Fprintf(os.Stdout, "\nThe instance is starting up...\n")
	_, _ = fmt.Fprint(os.Stdout, ttlDeadlineNotice(instance, "start", statusLocation(), time.Now()))

	// Wait for instance to be running AND to have a public IP. The public IP is
	// often not populated the instant the state flips to "running", and a
	// stop/start reassigns it — we need the new address to re-point any local
	// plugin footprint (spore-sync's mutagen session), so keep polling until the
	// IP appears rather than returning on the first "running".
	// Two waiters rather than one hand-rolled loop (#752): the SDK's
	// instance-running waiter, then a poll for the IP. This is the
	// cmd/launch_single.go:862→871 pattern.
	//
	// What it replaces polled a whole-region describe every 2s, and on a
	// ListInstances error it `break`ed out — silently reporting "taking longer
	// than expected" for what was an API failure. It also had no ctx.Done()
	// case, so Ctrl-C was ignored for the full minute.
	fmt.Fprintf(os.Stderr, "\nWaiting for instance to reach running state...")
	newIP := ""
	running := false
	if err := client.WaitForRunning(ctx, instance.Region, instance.InstanceID, 2*time.Minute); err != nil {
		fmt.Fprintf(os.Stderr, "\n   %v\n", err)
	} else {
		running = true
		fmt.Fprintf(os.Stderr, " running!\n")
		ip, ipErr := client.WaitForPublicIP(ctx, instance.Region, instance.InstanceID, time.Minute)
		if ipErr != nil {
			fmt.Fprintf(os.Stderr, "   no public IP yet: %v\n", ipErr)
		}
		newIP = ip
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
			fmt.Fprintf(os.Stderr, "⚠️  Failed to start %s: %v\n", inst.InstanceID, startFailedError(&inst, err, time.Now()))
			failedInstances = append(failedInstances, inst.InstanceID)
			continue
		}

		// Tag the instance with the start time. The TTL is left alone: the
		// deadline is absolute (see the single-instance path, #928, #983).
		if notice := startDoesNotRestoreTTLNotice(&inst); notice != "" {
			fmt.Fprint(os.Stderr, notice)
		}
		_ = client.UpdateInstanceTags(ctx, inst.Region, inst.InstanceID, startTags(time.Now()))

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

// startDoesNotRestoreTTLNotice is what `spawn start` says about the TTL of a
// stopped instance that `spawn stop`/`hibernate` saved a spawn:ttl-remaining
// for. Empty when there is nothing saved.
//
// `spawn start` used to "restore" that value by writing spawn:ttl=<remaining>
// and nothing else (#983). That enforced nothing: spored and the TTL reaper
// both read spawn:ttl-deadline, which the start never moved, so the instance
// still died at its original deadline (and the reaper reaps stopped instances
// at it too). What the write did do was corrupt spawn:ttl, which every other
// reader takes as the total from launch, and on an older instance without a
// deadline tag it SHORTENED the TTL to launch+remaining. The deadline is
// fixed at launch by design ("stop and start cannot extend it"; #928 fixed
// the stop message that claimed a pause), so start reports the deadline that
// is actually enforced instead of claiming a restore. Since #928 stop no longer
// writes spawn:ttl-remaining, so this only fires for instances stopped by an
// older spawn.
func startDoesNotRestoreTTLNotice(instance *aws.InstanceInfo) string {
	remaining := instance.Tags["spawn:ttl-remaining"]
	if remaining == "" {
		return ""
	}
	deadline, ok := lifecycleDeadline(instance)
	if !ok {
		return ""
	}
	return fmt.Sprintf("Note: %s had %s of TTL left when it was stopped. That is not restored: the deadline keeps counting while stopped.\n"+
		"   It is still enforced at its deadline, %s. To give it more time: spawn extend %s <duration>\n",
		instance.InstanceID, remaining, deadline.UTC().Format("2006-01-02 15:04 UTC"), instance.InstanceID)
}

// parseDuration parses a TTL string, accepting Go's native duration syntax plus
// a day unit (e.g. "2d", "1d12h"). It delegates to pkg/ttl.ParseDuration (via
// pkg/config), the one parser launch, spored and the TTL reaper share (#990).
func parseDuration(ttl string) (time.Duration, error) {
	return spawnconfig.ParseDurationE(ttl)
}

// oneTimeSpotStopError explains, before any work is done, why an instance from a
// one-time Spot request cannot be stopped or hibernated: EC2 only stops Spot
// instances from PERSISTENT requests, and rejects the call with
// UnsupportedOperation (#922). spawn launches --spot as persistent when the
// lifecycle needs a stop (an idle timeout, --on-complete stop|hibernate,
// --hibernate); a terminate-only --spot launch, and any launched before that, is
// one-time. nil when the stop can go ahead.
func oneTimeSpotStopError(instance *aws.InstanceInfo, action string) error {
	if !instance.SpotInstance || !spotrequest.IsOneTime(instance.Tags) {
		return nil
	}
	return fmt.Errorf("cannot %s %s: it is a Spot instance from a one-time Spot request, and EC2 can only "+
		"stop Spot instances from persistent requests. Nothing was changed.\n"+
		"  To end it:      spawn terminate %s\n"+
		"  To get a stoppable Spot instance, launch with a stop-based lifecycle — --idle-timeout, "+
		"--on-complete stop|hibernate, or --hibernate — which makes spawn request persistent Spot",
		action, instance.InstanceID, instance.InstanceID)
}

// startFailedError explains a failed `spawn start`. When the instance's
// persistent Spot request has passed the ValidUntil spawn gave it at launch
// (#950), that is the likely cause, and it is said, because EC2's own error
// doesn't mention the request.
func startFailedError(instance *aws.InstanceInfo, err error, now time.Time) error {
	if note := spotrequest.StartFailureNote(instance.Tags, now); note != "" {
		return fmt.Errorf("failed to start instance: %w\n  likely cause: %s", err, note)
	}
	return fmt.Errorf("failed to start instance: %w", err)
}

// ttlDeadlineDoesNotPause is the one statement of how a TTL behaves across a
// stop, shared by the stop/hibernate/start help and checked against the docs
// (TestStopStartAgreeWithTheDocsThatTheDeadlineDoesNotPause).
//
// `spawn stop` used to print "TTL countdown will pause until the instance is
// started again", and `spawn start` "Restoring TTL: … (was paused during stop)".
// Neither was true. The deadline is spawn:ttl-deadline, set once at launch and
// read by both spored and the reaper; the docs say it "never moves across
// stop/wake cycles", and a restarted instance in #928 showed exactly the
// original launch+1h deadline.
const ttlDeadlineDoesNotPause = "The TTL deadline is absolute: it keeps counting while the instance is " +
	"stopped, and stopping or starting does not move it. Only 'spawn extend' does."

// ttlDeadlineNotice is the TTL line `spawn stop`, `hibernate` and `start` print:
// the deadline in status's format (local time with UTC, #896) and the fact that
// it still applies. verb is "stop" or "start".
func ttlDeadlineNotice(instance *aws.InstanceInfo, verb string, loc *time.Location, now time.Time) string {
	deadline, ok := lifecycleDeadline(instance)
	if !ok {
		return "No TTL deadline is set on this instance.\n"
	}
	when := formatStatusTime(deadline, loc)
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		if verb == "start" {
			return fmt.Sprintf("The TTL deadline has passed: %s. The instance will terminate shortly after it boots; "+
				"run 'spawn extend' first if you need more time.\n", when)
		}
		return fmt.Sprintf("The TTL deadline has passed: %s. Starting the instance again will terminate it.\n", when)
	}
	if verb == "start" {
		return fmt.Sprintf("The TTL deadline still applies: %s, in %s. The stop did not extend it.\n",
			when, formatDuration(remaining))
	}
	return fmt.Sprintf("The TTL deadline still applies: %s, in %s. It keeps counting while the instance is stopped.\n",
		when, formatDuration(remaining))
}

// stopTags are the tags `spawn stop`/`hibernate` write: why and when. They
// used to include spawn:ttl-remaining, which start then wrote back over
// spawn:ttl as though the countdown had paused (#928).
func stopTags(hibernate bool, now time.Time) map[string]string {
	reason := "user-stopped"
	if hibernate {
		reason = "user-hibernated"
	}
	return map[string]string{
		"spawn:last-stop-reason": reason,
		"spawn:last-stop-time":   now.UTC().Format(time.RFC3339),
	}
}

// startTags are the tags `spawn start` writes. Never spawn:ttl or
// spawn:ttl-deadline: the deadline is absolute (#928).
func startTags(now time.Time) map[string]string {
	return map[string]string{"spawn:last-start-time": now.UTC().Format(time.RFC3339)}
}
