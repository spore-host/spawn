package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/update"
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

		// Explicit, user-initiated check — report whether a newer release exists.
		// CheckNow is synchronous and ungated (unlike the incidental CheckAsync
		// notice on other commands); nil means we couldn't reach GitHub.
		if res := update.CheckNow("spawn", Version); res == nil {
			fmt.Printf("\n(couldn't check for updates)\n")
		} else if res.HasUpdate() {
			fmt.Printf("\n⬆️  A newer version is available: %s → %s\n    %s\n",
				res.CurrentVersion, res.LatestVersion, res.UpdateURL)
		} else {
			fmt.Printf("\n✓ You're on the latest version.\n")
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
