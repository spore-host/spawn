package cmd

import (
	"strings"
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

// typePlaceholder is the value name --help shows for f when its usage carries
// no back-quoted span: pflag's own type-derived name ("string", "int",
// "duration", "strings", ...; "" for a bool). It asks pflag rather than
// keeping a list, so a new flag type cannot trip the gate by being unlisted.
func typePlaceholder(f *pflag.Flag) string {
	bare := *f
	bare.Usage = strings.ReplaceAll(f.Usage, "`", "")
	name, _ := pflag.UnquoteUsage(&bare)
	return name
}

// TestFlagUsagePlaceholderIsTypeName is #1520's gate, the generalisation of
// #1441's bool-only one above to every flag of every command. pflag takes the
// first back-quoted span in a usage string as the flag's value name, so a help
// line that quoted a command as code (`spawn launch <name>`, `spawn terminate`)
// was printed as `--name spawn launch <name>`.
//
// The rule: the value name --help prints must be the one pflag derives from
// the flag's type. That fails on ANY back-quoted override, not only on one
// with a space in it: a one-word code span (`spored`) becomes a placeholder
// just as wrongly, and a space-only rule would let it through. It has no
// false positives in this CLI: not one flag sets a deliberate placeholder (the
// convention is the type name), and flag help must not use backticks at all.
// A name with a space is reported separately because that is always this bug.
func TestFlagUsagePlaceholderIsTypeName(t *testing.T) {
	flags := 0
	walk(rootCmd, func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			flags++
			got, _ := pflag.UnquoteUsage(f)
			want := typePlaceholder(f)
			switch {
			case strings.ContainsAny(got, " \t"):
				t.Errorf("%s --%s: --help shows the value name %q, which has a space in it: the usage "+
					"back-quotes text pflag reads as the placeholder; drop the backticks", c.CommandPath(), f.Name, got)
			case got != want:
				t.Errorf("%s --%s: --help shows the value name %q, want the type name %q: the usage "+
					"back-quotes text pflag reads as the placeholder; drop the backticks", c.CommandPath(), f.Name, got, want)
			}
		})
	})
	if flags < 100 {
		t.Fatalf("walked only %d flags: the command walk is broken, not the help clean", flags)
	}
}
