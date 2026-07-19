package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"
	"github.com/spore-host/libs/docgen"
)

var gendocsOut string

// gendocsCmd regenerates the command/flag reference fragments consumed by the
// docs site (docs/gen/spawn/). It is hidden — an internal tooling command, not a
// user feature — and is the source of truth the docs drift gate checks: CI runs
// `spawn gen-docs` and fails if the committed docs-gen/ differs. Because it's a
// normal subcommand, root's PersistentPreRunE (i18n init) runs first, so command
// Short/Long descriptions are populated before the tree is walked.
var gendocsCmd = &cobra.Command{
	Use:    "gen-docs",
	Short:  "Regenerate the docs reference fragments (internal)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		frags, err := docgen.Generate(rootCmd, docgen.Options{CLIName: "spawn"})
		if err != nil {
			return fmt.Errorf("generate docs: %w", err)
		}
		if err := os.MkdirAll(gendocsOut, 0755); err != nil {
			return fmt.Errorf("create out dir: %w", err)
		}
		names := make([]string, 0, len(frags))
		for name := range frags {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(gendocsOut, name)
			if err := os.WriteFile(path, frags[name], 0644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %d reference fragments to %s\n", len(frags), gendocsOut)
		return nil
	},
}

func init() {
	gendocsCmd.Flags().StringVar(&gendocsOut, "out", "docs-gen", "Output directory for generated reference fragments")
	rootCmd.AddCommand(gendocsCmd)
}
