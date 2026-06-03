package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestFlagConventions locks the suite-wide flag conventions (spawn#40) so they
// can't regress as commands are added:
//
//  1. No command may expose an UNDEPRECATED local --json flag. Structured
//     output is selected by the root persistent -o/--output flag; the few
//     historical --json bools are kept only as deprecated aliases.
//  2. Every destructive command (cancel/terminate/delete/remove/destroy) must
//     offer a --yes flag so it can be run non-interactively and, by symmetry,
//     prompts when it is absent.
//
// Walking rootCmd in-process keeps the gate exact (real flag state, not --help
// text) and cheap (plain unit test, no AWS, no build tag).
func TestFlagConventions(t *testing.T) {
	walk(rootCmd, func(c *cobra.Command) {
		if c.Name() == "help" || !c.Runnable() {
			return
		}
		path := c.CommandPath()

		// (1) local --json must be deprecated if present.
		if f := c.Flags().Lookup("json"); f != nil && f.Deprecated == "" {
			t.Errorf("%s: defines an undeprecated --json flag; use the root -o/--output "+
				"and MarkDeprecated(\"json\", ...) (spawn#40)", path)
		}

		// (2) destructive commands need --yes.
		if isDestructive(c) {
			if f := c.Flags().Lookup("yes"); f == nil {
				t.Errorf("%s: destructive command is missing a --yes confirmation flag (spawn#40)", path)
			}
		}
	})
}

// isDestructive reports whether a command performs an irreversible/mutating
// action that warrants a confirmation flag, keyed on its verb.
func isDestructive(c *cobra.Command) bool {
	switch c.Name() {
	case "cancel", "terminate", "delete", "remove", "destroy":
		// `notify`/bot workspace destroy uses --confirm by design; allow either.
		return c.Flags().Lookup("confirm") == nil
	default:
		return false
	}
}

func walk(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walk(sub, fn)
	}
}
