package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The follow-up gate (#1543, extending #1540's launch gate): every
// `spawn status` and `spawn sweep …` invocation in examples/ must resolve to a
// real command, parse with its flags, use no deprecated flag, and pass its
// argument count. The launch gate checked how the examples start a sweep;
// nothing checked how they then watch it, and eleven files did it with
// `spawn status <sweep-id>`, which looks the ID up as an instance:
//
//	$ spawn status sweep-20261105-123456 --check-complete
//	Looking up instance sweep-20261105-123456...
//
// so --check-complete polled for an instance that does not exist. A sweep is
// `spawn sweep status <sweep-id>`. The gate refuses a positional `spawn status`
// argument that is sweep-shaped: a literal sweep-… ID, or a placeholder that
// names a sweep ($SWEEP_ID, <sweep-id>, $(cat /tmp/sweep_id.txt), the Python
// variable sweep_id). Placeholders are recognised as in the launch gate.

var (
	shellFollowup  = regexp.MustCompile(`\bspawn\s+((?:status|sweep)\b)`)
	pyListFollowup = regexp.MustCompile(`\[\s*['"]spawn['"]\s*,\s*(['"](?:status|sweep)['"])`)
)

func findExampleFollowups(t *testing.T, root string) []exampleLaunch {
	t.Helper()
	return findExampleInvocations(t, root, invocationPattern{shell: shellFollowup, py: pyListFollowup, quotedInPython: true})
}

// sweepShaped reports whether an argument stands for a sweep ID.
func sweepShaped(arg string) bool {
	return strings.Contains(strings.ToLower(arg), "sweep")
}

// checkExampleFollowup resolves l.argv (which starts at the subcommand, e.g.
// [sweep status <id> --check-complete]) against spawn's command tree and
// checks it. It never runs the command.
func checkExampleFollowup(l exampleLaunch) error {
	cmd, rest, err := rootCmd.Find(l.argv)
	if err != nil {
		return fmt.Errorf("spawn %q: %v", l.argv, err)
	}
	if !cmd.Runnable() {
		var subs []string
		for _, c := range cmd.Commands() {
			subs = append(subs, c.Name())
		}
		return fmt.Errorf("spawn %q: %q is a command group with no action of its own; name one of: %s",
			l.argv, cmd.CommandPath(), strings.Join(subs, ", "))
	}
	positional, used, err := parseExampleFollowupArgv(cmd, rest)
	if err != nil && l.partial() && strings.Contains(err.Error(), "flag needs an argument") {
		positional, used, err = parseExampleFollowupArgv(cmd, append(rest, "…"))
	}
	if err != nil {
		return fmt.Errorf("%s rejects the flags %q: %v", cmd.CommandPath(), rest, err)
	}
	if cmd == statusCmd {
		for _, a := range positional {
			if sweepShaped(a) {
				return fmt.Errorf("spawn status %q: %q is a sweep ID, but spawn status looks its argument up "+
					"as an instance: use spawn sweep status <sweep-id>", rest, a)
			}
		}
		bySweepID := false
		for _, f := range used {
			bySweepID = bySweepID || f.Name == "sweep-id"
		}
		if len(positional) == 0 && !bySweepID && !l.partial() {
			return fmt.Errorf("spawn status %q: names no instance", rest)
		}
	}
	for _, f := range used {
		if f.Deprecated != "" {
			return fmt.Errorf("%s %q: --%s is deprecated: %s", cmd.CommandPath(), rest, f.Name, f.Deprecated)
		}
	}
	if cmd.Args != nil && !l.partial() {
		if err := cmd.Args(cmd, positional); err != nil {
			return fmt.Errorf("%s %q: %v", cmd.CommandPath(), rest, err)
		}
	}
	return nil
}

// parseExampleFollowupArgv parses argv against a copy of cmd's flag
// definitions (its own and inherited), so no global is set. Every non-bool
// flag is taken as a string: a placeholder value need not parse as an int.
// It returns the positional arguments and the flags given.
func parseExampleFollowupArgv(cmd *cobra.Command, argv []string) ([]string, []*pflag.Flag, error) {
	clone := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	clone.SetOutput(io.Discard)
	orig := map[string]*pflag.Flag{}
	define := func(f *pflag.Flag) {
		if clone.Lookup(f.Name) != nil {
			return
		}
		orig[f.Name] = f
		if f.Value.Type() == "bool" {
			clone.BoolP(f.Name, f.Shorthand, false, "")
		} else {
			clone.StringArrayP(f.Name, f.Shorthand, nil, "")
		}
	}
	cmd.Flags().VisitAll(define)
	cmd.InheritedFlags().VisitAll(define)
	if err := clone.Parse(argv); err != nil {
		return nil, nil, err
	}
	var used []*pflag.Flag
	clone.Visit(func(c *pflag.Flag) { used = append(used, orig[c.Name]) })
	return clone.Args(), used, nil
}

// TestExampleFollowupInvocationsParse is the gate.
func TestExampleFollowupInvocationsParse(t *testing.T) {
	calls := findExampleFollowups(t, filepath.Join("..", "examples"))
	if len(calls) < 20 {
		t.Fatalf("found %d `spawn status`/`spawn sweep` invocations in examples/, want at least 20: the scan is broken", len(calls))
	}
	for _, l := range calls {
		t.Run(l.where, func(t *testing.T) {
			if err := checkExampleFollowup(l); err != nil {
				t.Errorf("%s: %v", l.where, err)
			}
		})
	}
}

// TestExampleFollowupGateCatchesSweepAsInstance: the gate fails each form
// #1543 found, so a revert of the fix to the examples fails CI.
func TestExampleFollowupGateCatchesSweepAsInstance(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Each line below is one rejected invocation.
	write("README.md", "```bash\nspawn status <sweep-id> --check-complete\n"+ // a sweep as an instance
		"spawn status sweep-20261105-123456\n"+ // a literal sweep ID
		"spawn sweep status\n```\n"+ // no sweep ID
		"Poll with `spawn status --sweep-id … --json`.\n") // deprecated --json
	write("argo.yaml", "args:\n- |\n  spawn status $SWEEP_ID --check-complete\n"+
		"#   spawn sweep --file sweep.yaml --detach\n") // spawn sweep has no action
	write("spawn_task.wdl", "command <<<\n  spawn status \"$SWEEP_ID\" --json > s.json\n>>>\n")
	write("dag.py", "\"\"\"Launch a spawn sweep and poll it.\"\"\"\n"+ // prose: not extracted
		"r = subprocess.run(['spawn', 'status', sweep_id, '--check-complete'])\n"+
		"r = subprocess.run(['spawn', 'status', sweep_id,\n                    '--json'])\n"+
		"s = BashSensor(bash_command='spawn status $(cat /tmp/sweep_id.txt) --check-complete')\n")

	calls := findExampleFollowups(t, dir)
	if len(calls) != 10 {
		t.Fatalf("found %d invocations, want 10: %+v", len(calls), calls)
	}
	for _, l := range calls {
		t.Run(l.where, func(t *testing.T) {
			err := checkExampleFollowup(l)
			if err == nil {
				t.Fatalf("the gate accepted spawn %q", l.argv)
			}
			t.Log(err)
		})
	}
}

// TestExampleFollowupGateAcceptsSweepStatus: the forms the examples now use
// pass, so the gate is not refusing everything.
func TestExampleFollowupGateAcceptsSweepStatus(t *testing.T) {
	for _, argv := range [][]string{
		{"sweep", "status", "{expr:sweep_id}", "--check-complete"},
		{"sweep", "status", "$SWEEP_ID", "--output", "json"},
		{"sweep", "collect", "<sweep-id>", "--metric", "accuracy", "--best", "5"},
		{"status", "<instance-id>"},
	} {
		if err := checkExampleFollowup(exampleLaunch{where: "t", argv: argv}); err != nil {
			t.Errorf("spawn %q: %v", argv, err)
		}
	}
}
