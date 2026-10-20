package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spore-host/libs/catalog"
)

// helpExample is one `spawn …` command line found in a command's help.
type helpExample struct {
	owner string // the command whose help shows it
	line  string
}

// discoverHelpExamples walks the real command tree and returns every spawn
// invocation in Long or Example help: a line that, trimmed (and without a
// leading "$ "), starts with "spawn " followed by a subcommand or a flag — so
// prose such as "spawn holds the tunnel …" is not one. A trailing backslash joins
// the next line; two or more spaces end the command (the column a listing's
// description, or a "# comment", starts in).
//
// Help assigned from spore-host/libs i18n (updateCommandDescriptions) is not
// checked here: it is fixed in libs, and its broken examples are
// spore-host/libs#59.
func discoverHelpExamples(t *testing.T) []helpExample {
	t.Helper()
	ensureI18nInitialized()
	updateCommandDescriptions()
	libsOwned := libsOwnedHelp(t)
	var out []helpExample
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, text := range []string{c.Long, c.Example} {
			if _, ok := libsOwned[text]; ok {
				continue
			}
			lines := strings.Split(text, "\n")
			for i := 0; i < len(lines); i++ {
				l := strings.TrimPrefix(strings.TrimSpace(lines[i]), "$ ")
				for strings.HasSuffix(l, "\\") && i+1 < len(lines) {
					i++
					l = strings.TrimSuffix(l, "\\") + " " + strings.TrimSpace(lines[i])
				}
				if !strings.HasPrefix(l, "spawn ") {
					continue
				}
				if j := strings.Index(l, "  "); j > 0 {
					l = l[:j]
				}
				next := strings.Fields(l)[1]
				if (!strings.HasPrefix(next, "-") || next == "-") && findSubcommand(rootCmd, next) == nil {
					continue // prose that starts with the word "spawn"
				}
				out = append(out, helpExample{owner: c.CommandPath(), line: l})
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
	if len(out) < 20 {
		t.Fatalf("found only %d spawn examples in help text — the discovery is broken", len(out))
	}
	return out
}

// exampleWords splits an example into shell words up to a "#" comment or a
// shell operator. Quotes group words; their content is not interpreted.
func exampleWords(line string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flush()
	for i, w := range words {
		switch w {
		case "|", "||", "&&", ";", ">", ">>", "<", "2>&1", "&":
			return words[:i]
		}
		if strings.HasPrefix(w, "#") {
			return words[:i]
		}
	}
	return words
}

// splitWorkload separates spawn's own words from what follows "--", which
// belongs to the workload: cobra hands it to the command as positional
// arguments, uninterpreted.
func splitWorkload(words []string) (own, workload []string) {
	for i, w := range words {
		if w == "--" {
			return words[:i], words[i+1:]
		}
	}
	return words, nil
}

// lookupExampleFlag finds a flag the way cobra parses it on c: local, then
// inherited persistent flags; by name for "--x", by shorthand for "-x".
func lookupExampleFlag(c *cobra.Command, word string) *pflag.Flag {
	if strings.HasPrefix(word, "--") {
		name := strings.TrimPrefix(word, "--")
		name, _, _ = strings.Cut(name, "=")
		if f := c.Flags().Lookup(name); f != nil {
			return f
		}
		return c.InheritedFlags().Lookup(name)
	}
	short := strings.TrimPrefix(word, "-")
	if short == "" {
		return nil
	}
	if f := c.Flags().ShorthandLookup(short[:1]); f != nil {
		return f
	}
	return c.InheritedFlags().ShorthandLookup(short[:1])
}

// resolveHelpExample resolves an example's words against the command tree and
// returns the command it names, its positional arguments, the flags it set, and
// an error naming what does not resolve: an unknown subcommand, a flag the
// resolved command does not have, or positional arguments its Args rejects.
func resolveHelpExample(words []string) (*cobra.Command, []string, map[string]string, error) {
	if len(words) == 0 || words[0] != "spawn" {
		return nil, nil, nil, fmt.Errorf("not a spawn invocation")
	}
	words, workload := splitWorkload(words)
	cmd := rootCmd
	var positional []string
	flags := map[string]string{}
	for i := 1; i < len(words); i++ {
		w := words[i]
		if strings.HasPrefix(w, "[") || w == "..." || strings.HasSuffix(w, "...") || strings.HasPrefix(w, "<") || strings.HasPrefix(w, "{") {
			// A placeholder: positional (<id>) or optional ([flags], [args...]).
			if strings.HasPrefix(w, "<") || strings.HasPrefix(w, "{") {
				positional = append(positional, w)
			}
			continue
		}
		if strings.HasPrefix(w, "-") && w != "-" {
			f := lookupExampleFlag(cmd, w)
			if f == nil {
				return cmd, nil, nil, fmt.Errorf("%s has no flag %s", cmd.CommandPath(), w)
			}
			val := ""
			if _, v, ok := strings.Cut(w, "="); ok {
				val = v
			} else if f.NoOptDefVal == "" && i+1 < len(words) {
				// A flag that takes a value consumes the next word.
				i++
				val = words[i]
			}
			flags[f.Name] = val
			continue
		}
		if len(positional) == 0 {
			if sub := findSubcommand(cmd, w); sub != nil {
				cmd = sub
				continue
			}
			if cmd.HasAvailableSubCommands() && !cmd.Runnable() {
				return cmd, nil, nil, fmt.Errorf("%s has no subcommand %q", cmd.CommandPath(), w)
			}
		}
		positional = append(positional, w)
	}
	positional = append(positional, workload...)
	if !cmd.Runnable() {
		return cmd, nil, nil, fmt.Errorf("%s is a command group, not a command", cmd.CommandPath())
	}
	if cmd.Args != nil {
		if err := cmd.Args(cmd, positional); err != nil {
			return cmd, nil, nil, fmt.Errorf("%s: %v", cmd.CommandPath(), err)
		}
	}
	return cmd, positional, flags, nil
}

func findSubcommand(c *cobra.Command, name string) *cobra.Command {
	for _, sub := range c.Commands() {
		if sub.Name() == name || sub.HasAlias(name) {
			return sub
		}
	}
	return nil
}

// TestHelpExamplesResolve runs every `spawn …` example in the help text through
// the real command tree: each word must be a subcommand, a flag of the command it
// resolved to, or a positional argument that command accepts (#1153). Help
// examples are what people paste.
func TestHelpExamplesResolve(t *testing.T) {
	for _, ex := range discoverHelpExamples(t) {
		if _, _, _, err := resolveHelpExample(exampleWords(ex.line)); err != nil {
			t.Errorf("help of %q: %q does not resolve: %v", ex.owner, ex.line, err)
		}
	}
}

// TestHelpAppLaunchExamplesAreLaunchable: a `spawn app launch <app>` example
// with no --image must name an app that launches out of the box. paraview and
// igv were shown this way, and both are refused (#1153).
func TestHelpAppLaunchExamplesAreLaunchable(t *testing.T) {
	embeddedCatalogOnly(t)
	seen := 0
	for _, ex := range discoverHelpExamples(t) {
		cmd, pos, flags, err := resolveHelpExample(exampleWords(ex.line))
		if err != nil || cmd != appLaunchCmd || len(pos) != 1 || strings.HasPrefix(pos[0], "<") {
			continue
		}
		seen++
		if _, ok := flags["image"]; ok {
			continue
		}
		entry, ok := catalog.Lookup(pos[0])
		if !ok {
			t.Errorf("help of %q: %q names an app not in the catalog, with no --image", ex.owner, ex.line)
			continue
		}
		if show, status := classifyForList(entry, ""); !show || status != appStatusLaunchable {
			t.Errorf("help of %q: %q launches %s, which is %q (not launchable without --image)", ex.owner, ex.line, entry.Name, status)
		}
	}
	if seen == 0 {
		t.Fatal("found no `spawn app launch <app>` examples — the discovery is broken")
	}
}

func TestExampleWords(t *testing.T) {
	got := exampleWords(`spawn service --addr-args "--listen 127.0.0.1:0" --ttl 1h -- ./srv --port 0 # c`)
	want := []string{"spawn", "service", "--addr-args", "--listen 127.0.0.1:0", "--ttl", "1h", "--", "./srv", "--port", "0"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("exampleWords = %q, want %q", got, want)
	}
}
