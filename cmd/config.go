package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/security"
)

var configCmd = &cobra.Command{
	Use:     "instance-config <instance-id> <action> [key] [value]",
	Short:   "Read or write runtime config on a running instance via SSH",
	Aliases: []string{"config"},
	RunE:    runConfig,
	Args:    cobra.MinimumNArgs(2),
}

func init() {
	rootCmd.AddCommand(configCmd)

	// Register completion for instance ID argument
	configCmd.ValidArgsFunction = completeInstanceID
}

func runConfig(cmd *cobra.Command, args []string) error {
	instanceIdentifier := args[0]
	action := args[1]
	ctx := context.Background()

	// Validate action
	if action != "get" && action != "set" && action != "list" {
		return fmt.Errorf("invalid action: %s (must be get, set, or list)", action)
	}

	// Validate arguments based on action
	if action == "get" && len(args) < 3 {
		return fmt.Errorf("config get requires a key")
	}
	if action == "set" && len(args) < 4 {
		return fmt.Errorf("config set requires a key and value")
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

	// Find SSH key
	keyPath, err := findSSHKey(instance.KeyName)
	if err != nil {
		return fmt.Errorf("failed to find SSH key: %w", err)
	}

	// Build spored command
	var sporedCmd string
	switch action {
	// ShellQuote, not ShellEscape (#680). These arguments come straight from argv
	// and the result is executed on the instance. ShellEscape is strconv.Quote —
	// Go/C escaping inside DOUBLE quotes — where a POSIX shell still expands
	// $VAR, $(...) and backticks, so `spawn config set k '$(id)'` ran `id` on the
	// instance instead of storing the literal string. Single quotes suppress
	// every expansion.
	//
	// The caller already owns the instance, so this is self-inflicted rather than
	// a privilege boundary — but it also means a value containing $ simply could
	// not be stored, which is a plain correctness bug.
	case "get":
		sporedCmd = fmt.Sprintf("sudo /usr/local/bin/spored config get %s 2>&1", security.ShellQuote(args[2]))
	case "set":
		sporedCmd = fmt.Sprintf("sudo /usr/local/bin/spored config set %s %s 2>&1", security.ShellQuote(args[2]), security.ShellQuote(args[3]))
	case "list":
		sporedCmd = "sudo /usr/local/bin/spored config list 2>&1"
	}

	// Run command via SSH, as the instance's resolved login user (not a hardcoded
	// ec2-user, which fails on non-AL2023 AMIs — #581).
	sshArgs := append([]string{"-i", keyPath}, sporedSSHOptions()...)
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", resolveSSHUser("", instance), instance.PublicIP), sporedCmd)

	sshCmd := exec.Command("ssh", sshArgs...)
	output, err := sshCmd.CombinedOutput()
	if err != nil {
		// Check if it's just a non-zero exit code from spored
		outputStr := string(output)
		if strings.Contains(outputStr, "Error:") {
			fmt.Print(outputStr)
			return nil
		}
		return fmt.Errorf("failed to run config command: %w\nOutput: %s", err, outputStr)
	}

	fmt.Print(string(output))
	return nil
}
