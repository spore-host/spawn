package cmd

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// replacementPathRe pulls the command path out of a cobra Deprecated message:
// "use 'spawn sweep resume <sweep-id>' instead" -> "sweep resume". Words stop at
// the first non-word token (a <placeholder>, a closing quote or backtick).
var replacementPathRe = regexp.MustCompile("[`']spawn((?: [a-z][a-z0-9-]*)+)")

// flagNameRe pulls the replacement flag out of a flag's Deprecated message:
// "use --output-file instead" -> "output-file".
var flagNameRe = regexp.MustCompile(`--([a-z][a-z0-9-]*)`)

// TestDeprecatedCommandFlagsExistOnReplacement is the #935 gate. A deprecated
// command tells its user to run another command instead; a user who does so
// must not lose an option the old spelling had. `spawn resume` pointed at
// `spawn sweep resume`, which lacked --max-concurrent-auto, so the #494 feature
// was reachable only through the spelling users are told to stop using.
//
// Deprecated commands and their replacements are discovered from the cobra
// tree (every command whose Deprecated message names a `spawn ...` path), not
// listed. For every flag defined on a deprecated command, the replacement
// must accept the same invocation with the same meaning:
//
//   - a flag the replacement takes positionally (its Use names <flag-name>,
//     as `spawn sweep resume <sweep-id>` does for --sweep-id) is satisfied by
//     the positional argument;
//   - a flag that is itself deprecated is satisfied when the flag its
//     deprecation message names exists on the replacement;
//   - any other flag must exist on the replacement under the same name and
//     shorthand, bound to the same variable — a same-named flag bound to
//     something else (such as the root -o/--output format flag) would parse
//     and do the wrong thing.
func TestDeprecatedCommandFlagsExistOnReplacement(t *testing.T) {
	pairs := 0
	walk(rootCmd, func(old *cobra.Command) {
		if old.Deprecated == "" {
			return
		}
		m := replacementPathRe.FindStringSubmatch(old.Deprecated)
		if m == nil {
			return // deprecated without naming a replacement command
		}
		path := strings.Fields(m[1])
		repl, rest, err := rootCmd.Find(path)
		if err != nil || len(rest) != 0 || repl == rootCmd || repl == old {
			t.Errorf("%s: deprecation message %q names `spawn %s`, which does not resolve to a command",
				old.CommandPath(), old.Deprecated, strings.Join(path, " "))
			return
		}
		pairs++

		old.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "help" {
				return
			}
			where := old.CommandPath() + " --" + f.Name + " -> " + repl.CommandPath()

			if strings.Contains(repl.Use, "<"+f.Name+">") {
				return
			}

			if f.Deprecated != "" {
				fm := flagNameRe.FindStringSubmatch(f.Deprecated)
				if fm == nil {
					return // deprecated with no replacement flag (e.g. inert)
				}
				if lookupFlag(repl, fm[1]) == nil {
					t.Errorf("%s: --%s is deprecated in favour of --%s, which the replacement does not have",
						where, f.Name, fm[1])
				}
				return
			}

			nf := lookupFlag(repl, f.Name)
			if nf == nil {
				t.Errorf("%s: flag is missing on the replacement (#935)", where)
				return
			}
			if nf.Shorthand != f.Shorthand {
				t.Errorf("%s: shorthand -%s on the deprecated command, -%s on the replacement",
					where, f.Shorthand, nf.Shorthand)
			}
			if a, b := boundAddr(f.Value), boundAddr(nf.Value); a == 0 || a != b {
				t.Errorf("%s: the replacement's --%s is bound to a different variable, so the same "+
					"invocation means something else", where, f.Name)
			}
		})
	})
	if pairs == 0 {
		t.Fatal("discovered no deprecated command naming a replacement; discovery is broken")
	}
}

func lookupFlag(c *cobra.Command, name string) *pflag.Flag {
	if f := c.Flags().Lookup(name); f != nil {
		return f
	}
	return c.InheritedFlags().Lookup(name)
}

// boundAddr returns the address of the variable a pflag.Value writes to.
// Scalar values are the variable's own pointer reinterpreted (*intValue is
// *int); slice values are a struct holding a pointer to the slice.
func boundAddr(v pflag.Value) uintptr {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return 0
	}
	if e := rv.Elem(); e.Kind() == reflect.Struct {
		for i := 0; i < e.NumField(); i++ {
			if e.Field(i).Kind() == reflect.Pointer {
				return e.Field(i).Pointer()
			}
		}
		return 0
	}
	return rv.Pointer()
}
