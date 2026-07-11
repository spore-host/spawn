package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
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

func init() {
	rootCmd.AddCommand(extendCmd)

	// Register completion for instance ID argument
	extendCmd.ValidArgsFunction = completeInstanceID

	// Add job array flags
	extendCmd.Flags().StringVar(&extendJobArrayID, "job-array-id", "", "Extend TTL for all instances in job array by ID")
	extendCmd.Flags().StringVar(&extendJobArrayName, "job-array-name", "", "Extend TTL for all instances in job array by name")
}

func runExtend(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Get user identity for audit logging
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	userID := *identity.Account
	correlationID := uuid.New().String()
	auditLog := audit.NewLogger(os.Stderr, userID, correlationID)

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

	// Validate TTL format
	if err := validateTTL(newTTL); err != nil {
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
	extendDuration, err := time.ParseDuration(newTTL)
	if err != nil {
		return fmt.Errorf("invalid TTL duration %q: %w", newTTL, err)
	}

	tags := map[string]string{"spawn:ttl": newTTL}

	// Push the absolute deadline forward, keeping it anchored to the original launch time.
	var newDeadline time.Time
	if dl, ok := instance.Tags["spawn:ttl-deadline"]; ok {
		if parsed, err := time.Parse(time.RFC3339, dl); err == nil {
			newDeadline = parsed.Add(extendDuration)
		}
	}
	if newDeadline.IsZero() {
		// Older instance without deadline tag — best-effort from current TTL
		if cur, err := time.ParseDuration(instance.TTL); err == nil {
			newDeadline = time.Now().Add(cur).Add(extendDuration)
		} else {
			newDeadline = time.Now().Add(extendDuration)
		}
	}
	// Safety floor: never set a deadline earlier than the requested duration from
	// now. A past/expired existing spawn:ttl-deadline (or a stale launch anchor)
	// must not reap the instance the moment the user asks to extend it.
	if floor := time.Now().Add(extendDuration); newDeadline.Before(floor) {
		newDeadline = floor
	}
	tags["spawn:ttl-deadline"] = newDeadline.UTC().Format(time.RFC3339)

	fmt.Fprintf(os.Stderr, "Extending TTL deadline to %s...\n", newDeadline.UTC().Format("2006-01-02 15:04 UTC"))
	err = client.UpdateInstanceTags(ctx, instance.Region, instance.InstanceID, tags)
	if err != nil {
		auditLog.LogOperationWithRegion("extend_ttl", instance.InstanceID, instance.Region, "failed", err)
		return fmt.Errorf("failed to update TTL: %w", err)
	}

	auditLog.LogOperationWithRegion("extend_ttl", instance.InstanceID, instance.Region, "success", nil)

	_, _ = fmt.Fprintf(os.Stdout, "\n✅ TTL extended successfully!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   Instance: %s\n", instance.InstanceID)
	_, _ = fmt.Fprintf(os.Stdout, "   Old TTL:  %s\n", instance.TTL)
	_, _ = fmt.Fprintf(os.Stdout, "   New TTL:  %s\n", newTTL)

	// Trigger reload on instance
	fmt.Fprintf(os.Stderr, "\nTriggering configuration reload on instance...\n")
	if err := triggerReload(instance); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to trigger reload: %v\n", err)
		fmt.Fprintf(os.Stderr, "   You may need to manually run: ssh ec2-user@%s 'sudo spored reload'\n",
			instance.PublicIP)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "✓ Configuration reloaded on instance\n")
	}

	return nil
}

func extendJobArrayWithAudit(ctx context.Context, newTTL string, auditLog *audit.AuditLogger) error {
	// Validate TTL format
	if err := validateTTL(newTTL); err != nil {
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
	fmt.Fprintf(os.Stderr, "\nExtending TTL to %s for all instances...\n", newTTL)

	// Log job array TTL extension initiation
	auditLog.LogOperationWithData("extend_job_array_ttl", arrayID, "initiated",
		map[string]interface{}{
			"array_name":     arrayName,
			"instance_count": len(jobArrayInstances),
			"new_ttl":        newTTL,
		}, nil)

	// Update TTL for each instance
	successCount := 0
	failedInstances := []string{}

	for _, inst := range jobArrayInstances {
		err := client.UpdateInstanceTags(ctx, inst.Region, inst.InstanceID, map[string]string{
			"spawn:ttl": newTTL,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to update %s: %v\n", inst.InstanceID, err)
			failedInstances = append(failedInstances, inst.InstanceID)
		} else {
			successCount++
			// Try to trigger reload (non-fatal)
			_ = triggerReload(&inst)
		}
	}

	// Display results
	_, _ = fmt.Fprintf(os.Stdout, "\n✅ Job array TTL extended!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   Array:     %s\n", arrayName)
	_, _ = fmt.Fprintf(os.Stdout, "   New TTL:   %s\n", newTTL)
	_, _ = fmt.Fprintf(os.Stdout, "   Updated:   %d/%d instances\n", successCount, len(jobArrayInstances))

	if len(failedInstances) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Failed to update %d instances:\n", len(failedInstances))
		for _, id := range failedInstances {
			fmt.Fprintf(os.Stderr, "   - %s\n", id)
		}
		auditLog.LogOperationWithData("extend_job_array_ttl", arrayID, "partial_success",
			map[string]interface{}{
				"success_count": successCount,
				"failed_count":  len(failedInstances),
			}, nil)
	} else {
		auditLog.LogOperationWithData("extend_job_array_ttl", arrayID, "success",
			map[string]interface{}{
				"instance_count": successCount,
			}, nil)
	}

	return nil
}

func validateTTL(ttl string) error {
	// TTL format: <number><unit> where unit is s, m, h, or d
	// Also supports multiple components like "3h30m"
	pattern := regexp.MustCompile(`^(\d+[smhd])+$`)
	if !pattern.MatchString(ttl) {
		return fmt.Errorf("TTL must be in format <number><unit> (e.g., 2h, 30m, 1d)")
	}

	// Parse each component to ensure it's valid
	componentPattern := regexp.MustCompile(`(\d+)([smhd])`)
	matches := componentPattern.FindAllStringSubmatch(ttl, -1)

	if len(matches) == 0 {
		return fmt.Errorf("no valid duration components found")
	}

	totalSeconds := 0
	for _, match := range matches {
		value, err := strconv.Atoi(match[1])
		if err != nil {
			return fmt.Errorf("invalid number: %s", match[1])
		}

		unit := match[2]
		switch unit {
		case "s":
			totalSeconds += value
		case "m":
			totalSeconds += value * 60
		case "h":
			totalSeconds += value * 3600
		case "d":
			totalSeconds += value * 86400
		}
	}

	if totalSeconds <= 0 {
		return fmt.Errorf("TTL must be greater than 0")
	}

	return nil
}

// Helper function to format duration for display
func triggerReload(instance *aws.InstanceInfo) error {
	// Find SSH key
	keyPath, err := findSSHKey(instance.KeyName)
	if err != nil {
		return fmt.Errorf("failed to find SSH key: %w", err)
	}

	// Run spored reload via SSH
	sshArgs := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
		fmt.Sprintf("ec2-user@%s", instance.PublicIP),
		"sudo /usr/local/bin/spored reload",
	}

	cmd := exec.Command("ssh", sshArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, string(output))
	}

	return nil
}
