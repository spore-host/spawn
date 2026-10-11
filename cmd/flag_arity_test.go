package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Flag arity: a flag must be writable the way the tool documents it (#808).
//
// `--nist-800-171` was registered as a StringVar, so pflag REQUIRED a value — but
// nothing read it, because the mode was decided by Changed(). Three consequences,
// all of which shipped:
//
//  1. The command's own Example, `spawn validate --nist-800-171`, failed with
//     "flag needs an argument".
//  2. `spawn validate --nist-800-171 -o json` ate "-o" as the flag's value and
//     "json" as a positional, then printed a human table and exited 0 — the
//     documented JSON path silently producing the wrong format.
//  3. The error message the command prints as the remedy,
//     "use --nist-800-171 or --nist-800-53=<low|moderate|high>", did not work
//     either.
//
// The #807 gate (TestEveryFlagVariableIsReadSomewhere) could not catch this:
// both flags bound to ONE variable, so it had three references — a declaration
// and two registrations — and looked read. That is the specific trap gated below.

// aliasTargetRe matches a usage string that declares the flag an alias of
// another, e.g. "Alias for --dry-run".
var aliasTargetRe = regexp.MustCompile(`(?i)\balias (?:for|of) --[a-z0-9-]+`)

// optionalValueFlag reports whether a flag can be written with no value: a bool,
// or any flag given a NoOptDefVal.
func optionalValueFlag(f *pflag.Flag) bool {
	return f.Value.Type() == "bool" || f.NoOptDefVal != ""
}

// Every flag written without a value in a command's own Example must accept
// being written that way.
//
// This is the arity defect stated as a property. The #806 gate checks that a
// named flag EXISTS; it cannot tell that the flag demands an argument the example
// does not supply. Resolving against pflag's own notion of arity is what closes
// that gap.
func TestExampleFlagsCanBeWrittenAsDocumented(t *testing.T) {
	flagNameRe := regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

	checked := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, block := range []string{c.Example, c.Long} {
			for _, line := range strings.Split(block, "\n") {
				line = strings.TrimSpace(line)
				// Example lines start with the binary name; prose does not.
				if !strings.HasPrefix(line, "spawn ") {
					continue
				}
				// Resolve which command the line is about, so a flag is checked
				// against the command that actually owns it.
				words := strings.Fields(line)[1:]
				var path []string
				for _, w := range words {
					if strings.HasPrefix(w, "-") {
						break
					}
					path = append(path, w)
				}
				target, problem := resolvePrinted(path)
				if problem != "" {
					continue // #806's gates report unresolvable commands
				}

				// Walk TOKENS, not regex positions. The first version computed
				// "was a value supplied?" from whether any later flag existed
				// anywhere on the line, which made `--stack my-stack --region x`
				// look like --stack had no value — 40-odd false positives. The
				// question is only ever about the immediately following token.
				toks := strings.Fields(line)
				for i, tok := range toks {
					if !strings.HasPrefix(tok, "--") {
						continue
					}
					name := strings.TrimPrefix(tok, "--")
					if eq := strings.Index(name, "="); eq >= 0 {
						continue // value supplied inline; arity satisfied
					}
					if !flagNameRe.MatchString(name) {
						continue
					}
					f := target.Flags().Lookup(name)
					if f == nil {
						f = target.InheritedFlags().Lookup(name)
					}
					if f == nil {
						continue // #806's flag gate reports this
					}
					suppliedValue := i+1 < len(toks) &&
						!strings.HasPrefix(toks[i+1], "-") &&
						toks[i+1] != "\\" && toks[i+1] != "--"

					checked++
					if !suppliedValue && !optionalValueFlag(f) {
						t.Errorf("%q documents `--%s` with no value, but the flag is a %s and pflag "+
							"requires one — the example fails with \"flag needs an argument\", and the "+
							"flag silently eats the next token (#808).\n  line: %s",
							target.CommandPath(), name, f.Value.Type(), line)
					}
				}
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)

	if checked == 0 {
		t.Fatal("checked no value-less example flags — the matcher has stopped matching, which is " +
			"not the same as every example being writable")
	}
}

// No two flags on one command may bind to the same variable.
//
// This is what hid #808 from the #807 gate: --nist-800-171 and --nist-800-53 both
// bound to validateComplianceMode, giving it a declaration and two registrations,
// so a reference count of three made it look read. It also makes the flags
// clobber each other — whichever is parsed last wins — which is never what two
// differently-named flags should do unless one is a deprecated alias.
//
// Deprecated aliases are the legitimate case and are exempt: the repo uses that
// pattern deliberately (`collect --output` for `--output-dir`, `--spored-profile`
// for `--spored-role`).
func TestNoTwoLiveFlagsShareOneVariable(t *testing.T) {
	checked := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		byValue := map[pflag.Value][]*pflag.Flag{}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			byValue[f.Value] = append(byValue[f.Value], f)
		})
		for _, group := range byValue {
			if len(group) < 2 {
				continue
			}
			checked++
			var live []string
			for _, f := range group {
				if f.Deprecated != "" {
					continue // a deprecated alias is the documented pattern
				}
				// A LIVE alias is legitimate too, and is declared the same way the
				// repo declares one: `--print-config` on launch says "Alias for
				// --dry-run" and is deliberately not deprecated. The first version
				// of this gate exempted only deprecated flags and reported it —
				// the exemption has to come from the code's own statement of
				// intent, as the MarkDeprecated one does, not from a hand list.
				if aliasTargetRe.MatchString(f.Usage) {
					continue
				}
				live = append(live, "--"+f.Name)
			}
			if len(live) > 1 {
				t.Errorf("%q binds %s to the same variable. They clobber each other — whichever is "+
					"parsed last wins — and a shared variable inflates its reference count, which is "+
					"how #808's arity bug hid from the unread-flag gate. If one is an alias, mark it "+
					"deprecated.", c.CommandPath(), strings.Join(live, " and "))
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)

	if checked == 0 {
		t.Fatal("found no flags sharing a variable at all, not even the deprecated aliases this repo " +
			"deliberately uses — the detection has broken")
	}
}

// The compliance flags specifically.
func TestComplianceFlagArity(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"validate"})
	if err != nil {
		t.Fatalf("find validate: %v", err)
	}

	f171 := c.Flags().Lookup("nist-800-171")
	if f171 == nil {
		t.Fatal("validate has no --nist-800-171")
	}
	if f171.Value.Type() != "bool" {
		t.Errorf("--nist-800-171 is a %s; there is no value to supply, so demanding one makes the "+
			"documented `spawn validate --nist-800-171` fail and swallows the next argv token",
			f171.Value.Type())
	}

	f53 := c.Flags().Lookup("nist-800-53")
	if f53 == nil {
		t.Fatal("validate has no --nist-800-53")
	}
	if f53.Value.Type() == "bool" {
		t.Error("--nist-800-53 is a bool; the baseline (low|moderate|high) is meaningful and must be " +
			"accepted")
	}
	if f53.NoOptDefVal == "" {
		t.Error("--nist-800-53 has no NoOptDefVal, so a bare `--nist-800-53` fails even though the " +
			"handler defaults the baseline to low")
	}
	if f53.NoOptDefVal != nist53DefaultBaseline {
		t.Errorf("--nist-800-53 NoOptDefVal is %q, want %q so the flag and the handler agree",
			f53.NoOptDefVal, nist53DefaultBaseline)
	}

	// The help text must name the accepted values, since an invalid one is now
	// rejected by name.
	for _, b := range nist53Baselines {
		if !strings.Contains(f53.Usage, b) {
			t.Errorf("--nist-800-53 usage does not mention the %q baseline: %q", b, f53.Usage)
		}
	}
}

// The baseline set must be validated, and the rejection must name the options.
func TestNIST53BaselineValidation(t *testing.T) {
	for _, good := range nist53Baselines {
		if !isNIST53Baseline(good) {
			t.Errorf("%q is in nist53Baselines but isNIST53Baseline rejects it", good)
		}
	}
	for _, bad := range []string{"", "LOW", "nonsense", "low ", "medium", "moderate-high"} {
		if isNIST53Baseline(bad) {
			t.Errorf("isNIST53Baseline accepts %q", bad)
		}
	}
	if !isNIST53Baseline(nist53DefaultBaseline) {
		t.Errorf("the default baseline %q is not in the accepted set", nist53DefaultBaseline)
	}
}

// The baseline check must be WIRED, not merely defined.
//
// TestNIST53BaselineValidation exercises isNIST53Baseline directly, so it keeps
// passing when the call site is removed — a revert to
// `if !isNIST53Baseline(val) && false` left the function correct and the handler
// unguarded, and the invalid baseline went back to surfacing twelve frames down
// as "unknown compliance mode: nist-800-53-nonsense". Testing a validator is not
// testing that anything validates.
func TestValidateCallsTheBaselineCheck(t *testing.T) {
	src := readCmdSource(t, "validate.go")

	i := strings.Index(src, "if !isNIST53Baseline(val)")
	if i < 0 {
		t.Fatal("the --nist-800-53 handler does not call isNIST53Baseline; an invalid baseline would " +
			"surface from inside the config loader naming an internal string rather than the flag (#808)")
	}
	line := src[i:]
	if j := strings.Index(line, "\n"); j > 0 {
		line = line[:j]
	}
	if strings.Contains(line, "&&") || strings.Contains(line, "||") {
		t.Errorf("the baseline guard is compound (%q); it must fire on every invalid value",
			strings.TrimSpace(line))
	}
	// And the two frameworks must be mutually exclusive rather than one silently
	// winning.
	if !strings.Contains(src, "mutually exclusive") {
		t.Error("passing both --nist-800-171 and --nist-800-53 is not refused; 800-171 would silently win")
	}
}

func readCmdSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRootFromCmd(t), "cmd", name)) //nolint:gosec // fixed path
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
