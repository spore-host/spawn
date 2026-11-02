package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/launcher"
)

// awsCLIPinCmd prints the AWS CLI zips the Linux bootstrap installs on an
// image with no aws CLI, one line per architecture:
//
//	<arch> <version> <sha256> <mirror key>
//
// for the release workflow, which downloads each from awscli.amazonaws.com,
// verifies its signature and this sha256, and mirrors it at <mirror key> in
// every spawn-binaries-<region> bucket (#1494). Hidden: it is release tooling.
// Read from this build, so the release mirrors exactly what it fetches.
var awsCLIPinCmd = &cobra.Command{
	Use:    "awscli-pin",
	Short:  "Print the pinned AWS CLI zips the release mirrors (internal)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		for _, p := range launcher.AWSCLIPins() {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s %s\n", p.Arch, p.Version, p.SHA256, p.MirrorKey); err != nil {
				return err
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(awsCLIPinCmd)
}
