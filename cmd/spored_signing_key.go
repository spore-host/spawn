package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/sporedinstall"
)

// sporedSigningKeyCmd prints the spored signing public key compiled into this
// build. Hidden: it is release tooling. The release workflow verifies every
// spored signature KMS returns against it BEFORE publishing (#1415): a release
// spawn installs spored only if its .sig verifies against this key, so a
// signature made by any other key (a wrong SPORED_SIGNING_KEY_ID, a rotation
// half-done) would retire every launch that release makes.
var sporedSigningKeyCmd = &cobra.Command{
	Use:    "spored-signing-key",
	Short:  "Print the spored signing public key compiled into spawn (internal)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !sporedinstall.VerificationEnabled() {
			return fmt.Errorf("no spored signing key is compiled into this build")
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), sporedinstall.SigningKeyPEM)
		return err
	},
}

func init() {
	rootCmd.AddCommand(sporedSigningKeyCmd)
}
