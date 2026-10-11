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
		// Remove fragments for commands that no longer exist (#795).
		//
		// Writing without pruning made `make check-docs` pass while shipping docs
		// for a DELETED command: the drift check is
		// `git diff --exit-code docs-gen/`, and an orphaned file is neither
		// rewritten nor removed, so there is no diff to detect. docs-gen/alerts.md
		// survived the removal of `spawn alerts` and would have kept describing
		// four subcommands that do not exist.
		//
		// The floor is not defensiveness. If Generate returned almost nothing —
		// a broken walk, an i18n failure leaving every Short empty — pruning would
		// delete the whole reference and the git diff would then dutifully report
		// it, loudly but too late. A discovery that finds almost nothing is broken,
		// not a CLI that shrank.
		const minPlausibleFragments = 10
		if len(frags) < minPlausibleFragments {
			return fmt.Errorf("generated only %d fragments (expected at least %d) — "+
				"refusing to prune %s, because a discovery that finds almost nothing is broken",
				len(frags), minPlausibleFragments, gendocsOut)
		}

		existing, err := filepath.Glob(filepath.Join(gendocsOut, "*.md"))
		if err != nil {
			return fmt.Errorf("list %s: %w", gendocsOut, err)
		}
		pruned := 0
		for _, path := range existing {
			if _, generated := frags[filepath.Base(path)]; generated {
				continue
			}
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("prune %s: %w", path, err)
			}
			// Named, not just counted: a silent deletion is indistinguishable
			// from a file that was never there.
			fmt.Fprintf(cmd.OutOrStdout(), "pruned %s (no such command)\n", filepath.Base(path))
			pruned++
		}

		fmt.Fprintf(cmd.OutOrStdout(), "wrote %d reference fragments to %s", len(frags), gendocsOut)
		if pruned > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), " (pruned %d stale)", pruned)
		}
		fmt.Fprintln(cmd.OutOrStdout())
		return nil
	},
}

func init() {
	gendocsCmd.Flags().StringVar(&gendocsOut, "out", "docs-gen", "Output directory for generated reference fragments")
	rootCmd.AddCommand(gendocsCmd)
}
