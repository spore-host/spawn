package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestBoolFlagUsageHasNoValueName is #1441's gate. pflag takes the first
// back-quoted span in a flag's usage as its value name, so a bool flag whose
// help quotes `spawn connect` was listed as `--no-public-ip spawn connect`, as
// though it took an argument. A bool flag takes none: walking every command's
// flags, UnquoteUsage must find no value name on any of them.
func TestBoolFlagUsageHasNoValueName(t *testing.T) {
	bools := 0
	walk(rootCmd, func(c *cobra.Command) {
		seen := func(f *pflag.Flag) {
			if f.Value.Type() != "bool" {
				return
			}
			bools++
			if name, _ := pflag.UnquoteUsage(f); name != "" {
				t.Errorf("%s --%s: a bool flag, but its usage back-quotes %q, which --help shows as a value name; "+
					"drop the backticks", c.CommandPath(), f.Name, name)
			}
		}
		c.LocalFlags().VisitAll(seen)
	})
	if bools == 0 {
		t.Fatal("found no bool flags: the command walk is broken, not the help clean")
	}
}
