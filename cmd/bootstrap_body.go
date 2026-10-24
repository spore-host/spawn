package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/launcher"
)

// bootstrapBodyCmd prints the static Linux bootstrap body (#1274), for the
// release workflow to sign and publish as spawn/versions/<version>/bootstrap.sh
// in every spawn-binaries bucket. Hidden: it is release tooling, not a feature.
//
// A subcommand of the released binary rather than a separate generator, so the
// published bytes come from the very build users install: the stub that build
// puts in user data refuses any body whose sha256 differs from
// launcher.BootstrapBody() as compiled into it, and running that same binary is
// the one way to be certain the two agree.
var bootstrapBodyCmd = &cobra.Command{
	Use:    "bootstrap-body",
	Short:  "Print the static Linux bootstrap body the release publishes (internal)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := fmt.Fprint(cmd.OutOrStdout(), launcher.BootstrapBody())
		return err
	},
}

func init() {
	rootCmd.AddCommand(bootstrapBodyCmd)
}
