package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/launcher"
)

// awsCLIPinCmd prints the AWS CLI zips the Linux bootstrap installs on an
// image with no aws CLI, one line per architecture:
//
//	<arch> <version> <sha256>
//
// for the release workflow, which downloads each from awscli.amazonaws.com and
// requires its signature by the AWS CLI team's key and this sha256 before it
// publishes anything (scripts/verify-awscli-pin.sh). Hidden: it is release
// tooling. Read from this build, so the release checks exactly what it
// fetches. Nothing is copied anywhere: spawn does not mirror third-party
// artifacts (#1515).
var awsCLIPinCmd = &cobra.Command{
	Use:    "awscli-pin",
	Short:  "Print the pinned AWS CLI zips the release verifies (internal)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		for _, p := range launcher.AWSCLIPins() {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", p.Arch, p.Version, p.SHA256); err != nil {
				return err
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(awsCLIPinCmd)
}
