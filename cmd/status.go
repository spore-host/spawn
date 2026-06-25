package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/libs/update"
	"github.com/spore-host/spawn/pkg/aws"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/sweep"
)

var (
	statusSweepID       string
	statusJSON          bool // deprecated: use --output json
	statusCheckComplete bool
)

var statusCmd = &cobra.Command{
	Use:  "status <instance-id>",
	RunE: runStatus,
	Args: cobra.MaximumNArgs(1),
	// Short and Long will be set after i18n initialization
}

func init() {
	rootCmd.AddCommand(statusCmd)

	statusCmd.Flags().StringVar(&statusSweepID, "sweep-id", "", "Check parameter sweep status instead of instance status")
	_ = statusCmd.Flags().MarkDeprecated("sweep-id", "use 'spawn sweep status <sweep-id>' instead")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output status as JSON")
	_ = statusCmd.Flags().MarkDeprecated("json", "use --output json instead")
	statusCmd.Flags().BoolVar(&statusCheckComplete, "check-complete", false, "Check completion status and exit with standardized codes (0=complete, 1=failed, 2=running, 3=error)")

	// Register completion for instance ID argument
	statusCmd.ValidArgsFunction = completeInstanceID
}

func runStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Check if sweep status requested (deprecated path; prefer 'spawn sweep status')
	if statusSweepID != "" {
		return runSweepStatus(ctx, statusSweepID, statusJSON || getOutputFormat() == "json", statusCheckComplete)
	}

	// Instance status mode (original behavior)
	if len(args) == 0 {
		return fmt.Errorf("instance ID or name required")
	}

	instanceIdentifier := args[0]

	// Create AWS client
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Resolve instance (by ID or name)
	instance, err := resolveInstance(ctx, client, instanceIdentifier)
	if err != nil {
		// In --check-complete mode, "can't reach the instance yet" is exit 3
		// (error/unknown), NOT a generic exit 1 — exit 1 means "task failed".
		// A just-launched instance fails to resolve for ~1s (EC2 eventual
		// consistency, InvalidInstanceID.NotFound); callers polling for
		// completion must keep polling, not conclude failure (#31).
		if statusCheckComplete {
			fmt.Fprintf(os.Stderr, "status: %v\n", err)
			os.Exit(3)
		}
		return err
	}

	// Build the remote spored command, forwarding --check-complete so spored
	// reports completion via standardized exit codes (#26).
	remoteCmd := "sudo /usr/local/bin/spored status 2>&1"
	if statusCheckComplete {
		remoteCmd = "sudo /usr/local/bin/spored status --check-complete"
	}

	// Find SSH key. A lagotto/cohort-launched instance is keyless (SSM-only by
	// design, #130), so there's no local key — status must not hard-fail there.
	// When no key resolves, run `spored status` over SSM instead (status needs
	// only Describe + SSM, never SSH). (#222)
	keyPath, keyErr := findSSHKey(instance.KeyName)
	if keyErr != nil {
		return runStatusOverSSM(ctx, client, instance, statusCheckComplete)
	}

	sshArgs := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
		fmt.Sprintf("ec2-user@%s", instance.PublicIP),
		remoteCmd,
	}

	sshCmd := exec.Command("ssh", sshArgs...)

	// In check-complete mode, propagate spored's exit code (0/1/2/3) as spawn's
	// own exit code so callers can poll it. SSH passes through the remote exit
	// status; SSH's own connection failures use 255, which we map to 3 (error).
	if statusCheckComplete {
		output, err := sshCmd.CombinedOutput()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code := exitErr.ExitCode()
				if code == 255 {
					// SSH-level failure (couldn't reach the instance): error.
					fmt.Fprintf(os.Stderr, "ssh: %s\n", string(output))
					os.Exit(3)
				}
				os.Exit(code)
			}
			fmt.Fprintf(os.Stderr, "failed to run status: %v\n", err)
			os.Exit(3)
		}
		os.Exit(0)
	}

	output, err := sshCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to get status: %w\nOutput: %s", err, string(output))
	}

	fmt.Print(string(output))
	fmt.Print(sporedUpgradeNotice(instance.Tags["spawn:spored-version"], string(output), instance.InstanceID))
	return nil
}

// runStatusOverSSM gets `spored status` from an instance we hold no SSH key for
// (keyless/SSM-only, e.g. lagotto/cohort-launched, #222). It runs the same
// command via SSM RunShellScript — no SSH, no key, no public IP needed. The SSM
// agent runs commands as root, so the `sudo` the SSH path uses is unnecessary
// (and harmless to drop). In --check-complete mode it maps spored's exit code
// (carried back as the SSM ResponseCode) to spawn's own exit code, mirroring the
// SSH path's 0/1/2/3 contract; an SSM-level failure is exit 3 (error/unknown).
func runStatusOverSSM(ctx context.Context, client *aws.Client, instance *aws.InstanceInfo, checkComplete bool) error {
	// The SSH variant pipes 2>&1 into the human output; over SSM stdout/stderr are
	// separate fields, so drop the redirect and combine them ourselves below.
	cmd := "/usr/local/bin/spored status"
	if checkComplete {
		cmd = "/usr/local/bin/spored status --check-complete"
	}

	res, err := client.RunShellScript(ctx, instance.Region, instance.InstanceID, cmd, 60*time.Second)
	if err != nil {
		// Couldn't reach the instance over SSM (no agent / no profile / timeout).
		if checkComplete {
			fmt.Fprintf(os.Stderr, "status (ssm): %v\n", err)
			os.Exit(3)
		}
		return fmt.Errorf("failed to get status over SSM (instance is keyless; SSM required): %w", err)
	}

	if checkComplete {
		// spored's 0/1/2/3 comes back as the SSM ResponseCode.
		os.Exit(int(res.ResponseCode))
	}

	out := res.Stdout
	if res.Stderr != "" {
		out += res.Stderr
	}
	fmt.Print(out)
	fmt.Print(sporedUpgradeNotice(instance.Tags["spawn:spored-version"], out, instance.InstanceID))
	return nil
}

// sporedUpgradeNotice returns a one-line "upgrade available" annotation for the
// running spored, or "" when it's current / can't be determined (#234). It's
// best-effort and offline-tolerant: if the latest release can't be fetched
// (GitHub unreachable, CI) it returns "" rather than failing status. The running
// version comes from the spawn:spored-version tag when present (written by spored
// on boot, #232), else it's parsed from the `spored: vX.Y.Z` line in the status
// output so older agents that predate the tag are still annotated.
func sporedUpgradeNotice(tagVersion, statusOutput, instanceID string) string {
	running := tagVersion
	if running == "" {
		running = parseSporedVersion(statusOutput)
	}
	if running == "" {
		return ""
	}
	res := update.CheckNow("spawn", running)
	if res == nil || !res.HasUpdate() {
		return ""
	}
	return fmt.Sprintf("\n%s spored upgrade available: v%s → v%s — run 'spawn upgrade-spored %s'\n",
		i18n.Symbol("info"), res.CurrentVersion, res.LatestVersion, instanceID)
}

// parseSporedVersion extracts the version from the `spored: vX.Y.Z` line that
// spored status prints (cmd/spored/main.go), returning a bare semver or "".
func parseSporedVersion(statusOutput string) string {
	for _, line := range strings.Split(statusOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "spored:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return ""
		}
		return strings.TrimPrefix(fields[len(fields)-1], "v")
	}
	return ""
}

func runSweepStatus(ctx context.Context, sweepID string, jsonOut bool, checkComplete bool) error {
	// Load AWS SDK config for spore-host-infra (where DynamoDB table lives)
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Query sweep status
	if !jsonOut && !checkComplete {
		fmt.Fprintf(os.Stderr, "🔍 Querying sweep status...\n\n")
	}
	status, err := sweep.QuerySweepStatus(ctx, cfg, sweepID)
	if err != nil {
		if checkComplete {
			os.Exit(3) // Error querying status
		}
		return fmt.Errorf("failed to query sweep status: %w", err)
	}

	// If check-complete mode, exit with standardized code
	if checkComplete {
		switch status.Status {
		case "COMPLETED":
			os.Exit(0) // Complete
		case "FAILED", "CANCELLED":
			os.Exit(1) // Failed
		case "RUNNING", "INITIALIZING":
			os.Exit(2) // Still running
		default:
			os.Exit(3) // Unknown/error
		}
	}

	// If JSON output requested, marshal and print
	if jsonOut {
		jsonBytes, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal status to JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	// Display sweep information
	_, _ = fmt.Fprintf(os.Stdout, "╔═══════════════════════════════════════════════════════════════╗\n")
	_, _ = fmt.Fprintf(os.Stdout, "║  Parameter Sweep Status                                      ║\n")
	_, _ = fmt.Fprintf(os.Stdout, "╚═══════════════════════════════════════════════════════════════╝\n\n")

	_, _ = fmt.Fprintf(os.Stdout, "Sweep ID:          %s\n", status.SweepID)
	_, _ = fmt.Fprintf(os.Stdout, "Sweep Name:        %s\n", status.SweepName)
	_, _ = fmt.Fprintf(os.Stdout, "Status:            %s\n", colorizeStatus(status.Status))

	// Display region information
	if status.MultiRegion && len(status.RegionStatus) > 0 {
		regions := make([]string, 0, len(status.RegionStatus))
		for region := range status.RegionStatus {
			regions = append(regions, region)
		}
		_, _ = fmt.Fprintf(os.Stdout, "Type:              Multi-Region\n")
		_, _ = fmt.Fprintf(os.Stdout, "Regions:           %d (%v)\n", len(regions), regions)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "Region:            %s\n", status.Region)
	}
	_, _ = fmt.Fprintf(os.Stdout, "\n")

	// Display timestamps
	createdAt, _ := time.Parse(time.RFC3339, status.CreatedAt)
	updatedAt, _ := time.Parse(time.RFC3339, status.UpdatedAt)
	_, _ = fmt.Fprintf(os.Stdout, "Created:           %s\n", createdAt.Format("2006-01-02 15:04:05 MST"))
	_, _ = fmt.Fprintf(os.Stdout, "Last Updated:      %s\n", updatedAt.Format("2006-01-02 15:04:05 MST"))

	if status.CompletedAt != "" {
		completedAt, _ := time.Parse(time.RFC3339, status.CompletedAt)
		_, _ = fmt.Fprintf(os.Stdout, "Completed:         %s\n", completedAt.Format("2006-01-02 15:04:05 MST"))
		duration := completedAt.Sub(createdAt)
		_, _ = fmt.Fprintf(os.Stdout, "Duration:          %s\n", formatDuration(duration))
	}
	_, _ = fmt.Fprintf(os.Stdout, "\n")

	// Display progress
	_, _ = fmt.Fprintf(os.Stdout, "Progress (Global):\n")
	_, _ = fmt.Fprintf(os.Stdout, "  Total Parameters:  %d\n", status.TotalParams)
	_, _ = fmt.Fprintf(os.Stdout, "  Launched:          %d (%.1f%%)\n", status.Launched, float64(status.Launched)/float64(status.TotalParams)*100)

	if !status.MultiRegion {
		// Only show NextToLaunch for single-region sweeps (legacy)
		_, _ = fmt.Fprintf(os.Stdout, "  Next to Launch:    %d\n", status.NextToLaunch)
	}
	_, _ = fmt.Fprintf(os.Stdout, "  Failed:            %d\n", status.Failed)

	// Calculate and display estimated completion time
	if status.Status == "RUNNING" && status.Launched > 0 {
		elapsed := updatedAt.Sub(createdAt)
		avgTimePerLaunch := elapsed / time.Duration(status.Launched)

		var remaining int
		if status.MultiRegion {
			// Count remaining from RegionStatus
			for _, rs := range status.RegionStatus {
				remaining += len(rs.NextToLaunch)
			}
		} else {
			remaining = status.TotalParams - status.NextToLaunch
		}

		if remaining > 0 {
			// Account for max concurrent limiting
			remainingBatches := (remaining + status.MaxConcurrent - 1) / status.MaxConcurrent
			estimatedRemaining := time.Duration(remainingBatches) * avgTimePerLaunch * time.Duration(status.MaxConcurrent)
			estimatedCompletion := time.Now().Add(estimatedRemaining)

			_, _ = fmt.Fprintf(os.Stdout, "  Est. Completion:   %s (in %s)\n",
				estimatedCompletion.Format("3:04 PM MST"),
				formatDuration(estimatedRemaining))
		}
	}
	_, _ = fmt.Fprintf(os.Stdout, "\n")

	// Display regional breakdown for multi-region sweeps
	if status.MultiRegion && len(status.RegionStatus) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "Regional Breakdown:\n")

		// Sort regions for consistent display
		regions := make([]string, 0, len(status.RegionStatus))
		for region := range status.RegionStatus {
			regions = append(regions, region)
		}
		// Simple sort using a loop (avoiding imports)
		for i := 0; i < len(regions); i++ {
			for j := i + 1; j < len(regions); j++ {
				if regions[i] > regions[j] {
					regions[i], regions[j] = regions[j], regions[i]
				}
			}
		}

		totalCost := 0.0
		for _, region := range regions {
			rs := status.RegionStatus[region]
			total := len(rs.NextToLaunch) + rs.Launched + rs.Failed
			pending := len(rs.NextToLaunch)

			_, _ = fmt.Fprintf(os.Stdout, "  %-13s  %d/%d launched, %d active, %d pending, %d failed\n",
				region+":",
				rs.Launched,
				total,
				rs.ActiveCount,
				pending,
				rs.Failed,
			)

			// Show costs if available
			if rs.TotalInstanceHours > 0 || rs.EstimatedCost > 0 {
				_, _ = fmt.Fprintf(os.Stdout, "  %-13s  Cost: $%.2f (%.1f instance-hours)\n",
					"",
					rs.EstimatedCost,
					rs.TotalInstanceHours,
				)
			}

			totalCost += rs.EstimatedCost
		}

		// Show total cost if any
		if totalCost > 0 {
			_, _ = fmt.Fprintf(os.Stdout, "\n  Total Estimated Cost: $%.2f\n", totalCost)
			if status.Budget > 0 {
				remaining := status.Budget - totalCost
				if remaining < 0 {
					_, _ = fmt.Fprintf(os.Stdout, "  Budget:              $%.2f (EXCEEDED by $%.2f)\n", status.Budget, -remaining)
				} else {
					_, _ = fmt.Fprintf(os.Stdout, "  Budget:              $%.2f (%.1f%% used)\n", status.Budget, (totalCost/status.Budget)*100)
				}
			}
		}

		_, _ = fmt.Fprintf(os.Stdout, "\n")
	}

	// Display configuration
	_, _ = fmt.Fprintf(os.Stdout, "Configuration:\n")
	_, _ = fmt.Fprintf(os.Stdout, "  Max Concurrent:    %d\n", status.MaxConcurrent)
	_, _ = fmt.Fprintf(os.Stdout, "  Launch Delay:      %s\n", status.LaunchDelay)
	_, _ = fmt.Fprintf(os.Stdout, "\n")

	// Calculate active instances
	activeCount := 0
	completedCount := 0
	failedCount := 0
	for _, inst := range status.Instances {
		switch inst.State {
		case "pending", "running":
			activeCount++
		case "terminated", "stopped":
			completedCount++
		case "failed":
			failedCount++
		}
	}

	_, _ = fmt.Fprintf(os.Stdout, "Instances:\n")
	_, _ = fmt.Fprintf(os.Stdout, "  Active:            %d\n", activeCount)
	_, _ = fmt.Fprintf(os.Stdout, "  Completed:         %d\n", completedCount)
	_, _ = fmt.Fprintf(os.Stdout, "  Failed:            %d\n", failedCount)
	_, _ = fmt.Fprintf(os.Stdout, "\n")

	// Display error message if any
	if status.ErrorMessage != "" {
		_, _ = fmt.Fprintf(os.Stdout, "%s Error: %s\n\n", i18n.Symbol("warning"), status.ErrorMessage)
	}

	// Display instance details (limited to most recent 10)
	if len(status.Instances) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "Recent Instances (showing last 10):\n")

		if status.MultiRegion {
			// Show region column for multi-region sweeps
			_, _ = fmt.Fprintf(os.Stdout, "%-5s %-13s %-20s %-15s %-20s\n", "Index", "Region", "Instance ID", "State", "Launched At")
			_, _ = fmt.Fprintf(os.Stdout, "%-5s %-13s %-20s %-15s %-20s\n", "-----", "-------------", "--------------------", "---------------", "--------------------")
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "%-5s %-20s %-15s %-20s\n", "Index", "Instance ID", "State", "Launched At")
			_, _ = fmt.Fprintf(os.Stdout, "%-5s %-20s %-15s %-20s\n", "-----", "--------------------", "---------------", "--------------------")
		}

		// Show last 10 instances
		startIdx := 0
		if len(status.Instances) > 10 {
			startIdx = len(status.Instances) - 10
		}

		for _, inst := range status.Instances[startIdx:] {
			launchedAt, _ := time.Parse(time.RFC3339, inst.LaunchedAt)
			stateDisplay := colorizeInstanceState(inst.State)

			if status.MultiRegion {
				_, _ = fmt.Fprintf(os.Stdout, "%-5d %-13s %-20s %-15s %-20s\n",
					inst.Index,
					inst.Region,
					inst.InstanceID,
					stateDisplay,
					launchedAt.Format("2006-01-02 15:04:05"),
				)
			} else {
				_, _ = fmt.Fprintf(os.Stdout, "%-5d %-20s %-15s %-20s\n",
					inst.Index,
					inst.InstanceID,
					stateDisplay,
					launchedAt.Format("2006-01-02 15:04:05"),
				)
			}
		}
		_, _ = fmt.Fprintf(os.Stdout, "\n")
	}

	// Display failed launches if any
	if failedCount > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "Failed Launches:\n")
		for _, inst := range status.Instances {
			if inst.State == "failed" {
				_, _ = fmt.Fprintf(os.Stdout, "  [%d] %s\n", inst.Index, inst.ErrorMessage)
			}
		}
		_, _ = fmt.Fprintf(os.Stdout, "\n")
	}

	// Display next steps based on status
	switch status.Status {
	case "RUNNING":
		_, _ = fmt.Fprintf(os.Stdout, "The sweep is currently running in Lambda.\n")
		_, _ = fmt.Fprintf(os.Stdout, "Re-run this command to see updated progress.\n")
	case "COMPLETED":
		_, _ = fmt.Fprintf(os.Stdout, "%s Sweep completed successfully!\n", i18n.Symbol("success"))
	case "FAILED":
		_, _ = fmt.Fprintf(os.Stdout, "%s Sweep failed. Check error message above.\n", i18n.Symbol("error"))
		_, _ = fmt.Fprintf(os.Stdout, "\nTo resume:\n")
		_, _ = fmt.Fprintf(os.Stdout, "  spawn resume --sweep-id %s --detach\n", status.SweepID)
	}

	return nil
}

func colorizeStatus(status string) string {
	switch status {
	case "INITIALIZING":
		return i18n.Symbol("progress") + " " + status
	case "RUNNING":
		return i18n.Symbol("progress") + " " + status
	case "COMPLETED":
		return i18n.Symbol("success") + " " + status
	case "FAILED":
		return i18n.Symbol("error") + " " + status
	case "CANCELLED":
		return i18n.Symbol("warning") + " " + status
	default:
		return status
	}
}

func colorizeInstanceState(state string) string {
	switch state {
	case "pending":
		return i18n.Symbol("pending") + " " + state
	case "running":
		return i18n.Symbol("success") + " " + state
	case "terminated", "stopped":
		return i18n.Symbol("pause") + " " + state
	case "failed":
		return i18n.Symbol("error") + " " + state
	default:
		return state
	}
}
