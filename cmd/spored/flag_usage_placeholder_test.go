package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestFlagUsagePlaceholderIsTypeName is spored's half of #1520's gate (the
// spawn half, with the rule's rationale, is in cmd/flag_usage_backticks_test.go).
// pflag takes the first back-quoted span in a usage string as the flag's value
// name, so a usage that quotes a command as code puts that command into --help
// where the placeholder should be. The value name must be the one pflag derives
// from the flag's type.
func TestFlagUsagePlaceholderIsTypeName(t *testing.T) {
	var walk func(*cobra.Command)
	flags := 0
	walk = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			flags++
			got, _ := pflag.UnquoteUsage(f)
			bare := *f
			bare.Usage = strings.ReplaceAll(f.Usage, "`", "")
			want, _ := pflag.UnquoteUsage(&bare)
			if got != want {
				t.Errorf("%s --%s: --help shows the value name %q, want the type name %q: the usage "+
					"back-quotes text pflag reads as the placeholder; drop the backticks", c.CommandPath(), f.Name, got, want)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRootCmd())
	if flags == 0 {
		t.Fatal("walked no flags: the command walk is broken, not the help clean")
	}
}
