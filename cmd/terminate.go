package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
)

var (
	terminateJobArrayID   string
	terminateJobArrayName string
	terminateYes          bool
)

// terminate command — permanently destroys an instance and its instance-store /
// non-persisted EBS volumes. Unlike stop/hibernate this is irreversible, so it
// confirms by default unless --yes is given.
var terminateCmd = &cobra.Command{
	Use:   "terminate [instance-id-or-name]",
	Short: "Terminate an instance (permanent — destroys the instance)",
	Long: `Permanently terminate an instance. This is irreversible: the instance is
destroyed and any non-persisted volumes are deleted. Use stop or hibernate to
keep EBS volumes.

Terminate a single instance by ID or name, or an entire job array:

  spawn terminate i-0abc123
  spawn terminate my-instance --yes
  spawn terminate --job-array-name training`,
	RunE: runTerminate,
	Args: cobra.RangeArgs(0, 1),
}

func init() {
	rootCmd.AddCommand(terminateCmd)
	terminateCmd.ValidArgsFunction = completeInstanceID
	terminateCmd.Flags().StringVar(&terminateJobArrayID, "job-array-id", "", "Terminate all instances in job array by ID")
	terminateCmd.Flags().StringVar(&terminateJobArrayName, "job-array-name", "", "Terminate all instances in job array by name")
	terminateCmd.Flags().BoolVarP(&terminateYes, "yes", "y", false, "Skip the confirmation prompt")
}

func runTerminate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	if terminateJobArrayID != "" || terminateJobArrayName != "" {
		if len(args) != 0 {
			return fmt.Errorf("job array mode does not accept an instance ID argument")
		}
		return terminateJobArray(ctx)
	}

	if len(args) != 1 {
		return fmt.Errorf("single instance mode requires 1 argument: <instance-id-or-name>")
	}
	return terminateSingle(ctx, args[0])
}

func terminateSingle(ctx context.Context, identifier string) error {
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	instance, err := resolveInstance(ctx, client, identifier)
	if err != nil {
		return err
	}

	if instance.State == "terminated" {
		return fmt.Errorf("instance %s is already terminated", instance.InstanceID)
	}
	if instance.State == "shutting-down" {
		return fmt.Errorf("instance %s is already shutting down", instance.InstanceID)
	}

	fmt.Fprintf(os.Stderr, "Found instance in %s (state: %s)\n", instance.Region, instance.State)

	label := instance.InstanceID
	if instance.Name != "" {
		label = fmt.Sprintf("%s (%s)", instance.Name, instance.InstanceID)
	}
	if !confirmTerminate(fmt.Sprintf("Permanently terminate %s?", label)) {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	// Tear down any controller-side plugin footprint (mutagen sync, Globus
	// endpoint, …) before the instance goes away. Best-effort; keyed on both the
	// instance ID and its Name since either may have been used at install time.
	deprovisionAllLocalPlugins(ctx, instance.InstanceID, instance.Name)
	// Also drop any spawn-managed ssh_config identity block for this IP, even if
	// no plugin left a deprovision record (e.g. tailscale writes an identity for
	// its local mint step but has nothing to deprovision).
	removeHostIdentityForIP(instance.PublicIP)

	fmt.Fprintf(os.Stderr, "Requesting termination for instance %s...\n", instance.InstanceID)
	if err := client.Terminate(ctx, instance.Region, instance.InstanceID); err != nil {
		return fmt.Errorf("failed to terminate instance: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "\n✅ Terminate request sent!\n")
	_, _ = fmt.Fprintf(os.Stdout, "   Instance: %s\n", instance.InstanceID)
	_, _ = fmt.Fprintf(os.Stdout, "   Region:   %s\n", instance.Region)
	_, _ = fmt.Fprintf(os.Stdout, "\nThe instance is shutting down and will be destroyed.\n")
	return nil
}

func terminateJobArray(ctx context.Context) error {
	client, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	instances, err := client.ListInstances(ctx, "", "")
	if err != nil {
		return fmt.Errorf("failed to list instances: %w", err)
	}

	var arrayInstances []aws.InstanceInfo
	for _, inst := range instances {
		if terminateJobArrayID != "" && inst.JobArrayID == terminateJobArrayID {
			arrayInstances = append(arrayInstances, inst)
		} else if terminateJobArrayName != "" && inst.JobArrayName == terminateJobArrayName {
			arrayInstances = append(arrayInstances, inst)
		}
	}

	if len(arrayInstances) == 0 {
		if terminateJobArrayID != "" {
			return fmt.Errorf("no instances found with job-array-id: %s", terminateJobArrayID)
		}
		return fmt.Errorf("no instances found with job-array-name: %s", terminateJobArrayName)
	}

	arrayName := arrayInstances[0].JobArrayName
	if arrayName == "" {
		arrayName = "unnamed"
	}
	fmt.Fprintf(os.Stderr, "Found job array: %s (%d instances)\n", arrayName, len(arrayInstances))

	if !confirmTerminate(fmt.Sprintf("Permanently terminate all %d instances in job array %q?", len(arrayInstances), arrayName)) {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	successCount := 0
	var failedInstances []string
	for _, inst := range arrayInstances {
		if inst.State == "terminated" || inst.State == "shutting-down" {
			fmt.Fprintf(os.Stderr, "⏭  Skipping %s (already %s)\n", inst.InstanceID, inst.State)
			continue
		}
		deprovisionAllLocalPlugins(ctx, inst.InstanceID, inst.Name)
		removeHostIdentityForIP(inst.PublicIP)
		if err := client.Terminate(ctx, inst.Region, inst.InstanceID); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to terminate %s: %v\n", inst.InstanceID, err)
			failedInstances = append(failedInstances, inst.InstanceID)
			continue
		}
		fmt.Fprintf(os.Stderr, "✅ Terminating %s\n", inst.InstanceID)
		successCount++
	}

	_, _ = fmt.Fprintf(os.Stdout, "\nTerminated %d of %d instances.\n", successCount, len(arrayInstances))
	if len(failedInstances) > 0 {
		return fmt.Errorf("failed to terminate %d instance(s): %s", len(failedInstances), strings.Join(failedInstances, ", "))
	}
	return nil
}

// confirmTerminate prompts for confirmation unless --yes was passed.
func confirmTerminate(prompt string) bool {
	return confirmYes(terminateYes, prompt)
}
