package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	GitCommit = "unknown"
	BuildDate = "unknown"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Long:  `Display version, build date, and git commit information for spawn.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("🌱 Spawn - EC2 Instance Lifecycle Manager\n\n")
		fmt.Printf("Version:    %s\n", Version)
		fmt.Printf("Git Commit: %s\n", GitCommit)
		fmt.Printf("Build Date: %s\n", BuildDate)
		fmt.Printf("\nProject:    https://spore.host\n")
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
