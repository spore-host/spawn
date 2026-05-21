package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

var defaultsCmd = &cobra.Command{
	Use:   "defaults",
	Short: "Manage default launch settings (~/.spawn/config.yaml)",
	Long: `Manage default values for spawn launch flags.

Defaults are stored in ~/.spawn/config.yaml and applied whenever the
corresponding flag is not explicitly provided on the command line.

Valid keys:
  slack-workspace    Slack workspace ID for lifecycle notifications (e.g. T03NE3GTY)
  active-processes   Process names to monitor for idle detection (e.g. rsession)
  active-ports       TCP ports to monitor for active connections (e.g. 8787)
  idle-timeout       Default idle timeout duration (e.g. 30m, 1h)
  hibernate-on-idle  Hibernate instead of terminating on idle (true/false)

Examples:
  spawn defaults set slack-workspace T03NE3GTY
  spawn defaults set active-processes rsession
  spawn defaults set idle-timeout 1h
  spawn defaults list
  spawn defaults unset active-processes`,
}

var defaultsSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a default launch value",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key, value := args[0], args[1]
		if err := spawnconfig.SetLaunchDefault(key, value); err != nil {
			return err
		}
		fmt.Printf("Default set: %s = %s\n", key, value)
		return nil
	},
}

var defaultsUnsetCmd = &cobra.Command{
	Use:   "unset <key>",
	Short: "Remove a default launch value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := args[0]
		if err := spawnconfig.UnsetLaunchDefault(key); err != nil {
			return err
		}
		fmt.Printf("Default cleared: %s\n", key)
		return nil
	},
}

var defaultsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all default launch values",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := spawnconfig.LoadLaunchDefaults()
		if err != nil {
			return err
		}
		any := false
		for _, key := range spawnconfig.KnownDefaultKeys() {
			val := spawnconfig.GetDefaultValue(d, key)
			if val != "" {
				fmt.Printf("%-20s %s\n", key, val)
				any = true
			}
		}
		if !any {
			fmt.Println("No defaults set. Use `spawn defaults set <key> <value>` to configure.")
		}
		return nil
	},
}

func init() {
	defaultsCmd.AddCommand(defaultsSetCmd)
	defaultsCmd.AddCommand(defaultsUnsetCmd)
	defaultsCmd.AddCommand(defaultsListCmd)
	rootCmd.AddCommand(defaultsCmd)
}
