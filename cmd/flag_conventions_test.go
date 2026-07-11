package cmd

import (
	"strings"
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
//  3. Canonical names for concepts that used to be spelled inconsistently
//     across commands (spawn#309/#311/#312/#313) may only appear under their
//     canonical spelling; the historical spellings are allowed ONLY as
//     deprecated aliases. This stops the drift that the 2026-07 audit fixed
//     from creeping back in.
//
// Walking rootCmd in-process keeps the gate exact (real flag state, not --help
// text) and cheap (plain unit test, no AWS, no build tag).
func TestFlagConventions(t *testing.T) {
	// Historical flag spellings that are now deprecated aliases for a canonical
	// name. If a command exposes one of these, it must be MarkDeprecated'd.
	deprecatedAliases := map[string]string{
		"subnet":            "subnet-id",
		"key-pair":          "key-name",
		"security-group":    "security-group-ids",
		"security-groups":   "security-group-ids",
		"security-group-id": "security-group-ids",
		"tags":              "tag",
	}

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

		// (3) historical flag spellings may exist only as deprecated aliases.
		for old, canonical := range deprecatedAliases {
			if f := c.Flags().Lookup(old); f != nil && f.Deprecated == "" {
				t.Errorf("%s: defines --%s undeprecated; use --%s and MarkDeprecated(%q, ...) (2026-07 audit)",
					path, old, canonical, old)
			}
		}
	})
}

// destructiveVerbs are the verbs that mark an irreversible/mutating action.
var destructiveVerbs = map[string]bool{
	"cancel": true, "terminate": true, "delete": true, "remove": true, "destroy": true,
}

// isDestructive reports whether a command performs an irreversible/mutating
// action that warrants a confirmation flag, keyed on its verb.
//
// cobra's Name() is the first whitespace token of Use, so hyphenated compound
// verbs like "workspace-remove" or "remove-schedule" arrive as a single token.
// We therefore check every hyphen-segment, not the whole Name(): otherwise a
// compound-verb command slips past the gate and can perform an irreversible
// delete with no --yes/prompt (spawn#285).
func isDestructive(c *cobra.Command) bool {
	found := false
	for _, seg := range strings.Split(c.Name(), "-") {
		if destructiveVerbs[seg] {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	// A command may satisfy the confirmation contract with --yes OR --confirm
	// (bot workspace destroy uses --confirm/dry-run by design).
	return c.Flags().Lookup("confirm") == nil
}

func walk(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walk(sub, fn)
	}
}
