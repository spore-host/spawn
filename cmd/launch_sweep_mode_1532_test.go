package cmd

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spore-host/spawn/pkg/aws"
)

// stubStdinTTY makes runLaunch see stdin as a terminal (tty) or a pipe.
func stubStdinTTY(t *testing.T, tty bool) {
	t.Helper()
	prev := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = prev })
	stdinIsTerminal = func() bool { return tty }
}

const wizardRefusal = "not supported by the interactive wizard"

// runModeLaunch runs runLaunch against the recording AWS fake, with the
// sweep cross-account role absent, stdin a TTY and the given flags only: no
// --name and no --instance-type unless flags sets them.
func runModeLaunch(t *testing.T, flags map[string]string) (string, *roleIAMFake, error) {
	t.Helper()
	serveTagLimitAWS(t)
	// The sweep writes audit records: keep ~/.spawn/audit.log in a temp dir
	// and close it, or Windows cannot remove the open file.
	withLogDir(t, false)
	t.Setenv("SPAWN_COMPUTE_PROFILE", "")
	t.Setenv("SPAWN_INFRA_PROFILE", "")
	stubTeamName(t)
	stubStdinTTY(t, true)
	role := &roleIAMFake{err: errNoCrossAccountRole}
	stubSweepRoleIAM(t, role)
	for k, v := range flags {
		setLaunchFlag(t, k, v)
	}
	var err error
	out := captureStderr(t, func() { err = runLaunch(launchCmd, nil) })
	return out, role, err
}

// defaultsOnlySweepFile is #1532's param file: instance_type only under
// defaults:, so the command line carries no --instance-type.
func defaultsOnlySweepFile(t *testing.T) string {
	t.Helper()
	pf := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(pf, []byte("defaults:\n  instance_type: t4g.small\n  ttl: 1h\nparams:\n  - alpha: 1\n  - alpha: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return pf
}

// TestParamFileSweepWithoutInstanceTypeIsNotTheWizard is #1532. On a TTY, a
// --param-file sweep whose instance type is in the file's defaults: and not on
// the command line was judged a wizard launch by the pre-flight guards, and
// --no-public-ip was refused "by the interactive wizard", although runLaunch
// sends a sweep to the sweep path and never to the wizard. It must reach the
// sweep path: here the detached one, recognised by its look for
// SpawnSweepCrossAccountRole (absent, so it stops there, having created
// nothing). (--interactive on a sweep is refused by the sweep's own flag rules,
// #1366, before this guard: TestInteractiveSweepIsRefusedAsASweep.)
func TestParamFileSweepWithoutInstanceTypeIsNotTheWizard(t *testing.T) {
	for _, extra := range []map[string]string{
		{},
		{"no-public-ip": "true", "subnet-id": "subnet-0123456789abcdef0"},
	} {
		t.Run(strings.Join(sortedKeys(extra), ","), func(t *testing.T) {
			flags := map[string]string{
				"param-file": defaultsOnlySweepFile(t), "sweep-name": "swr", "region": "us-east-1",
				"yes": "true", "no-dns": "true", "ami": "ami-0123456789abcdef0", "os": "linux",
			}
			for k, v := range extra {
				flags[k] = v
			}
			out, role, err := runModeLaunch(t, flags)
			if err != nil && strings.Contains(err.Error(), wizardRefusal) {
				t.Fatalf("a --param-file sweep was refused as a wizard launch (#1532): %v", err)
			}
			if role.count() == 0 || !isRoleRefusal(err) {
				t.Fatalf("did not reach the sweep path: err = %v\n%s", err, out)
			}
			if !strings.Contains(out, "Orchestration: Lambda (detached)") {
				t.Errorf("no detached sweep summary:\n%s", out)
			}
		})
	}
}

// TestInteractiveLaunchStillGetsTheWizard: the other side of #1532. A launch
// that is not a sweep and has no --instance-type on a TTY, or passes
// --interactive, is a wizard launch, so --no-public-ip is still refused before
// any AWS call. (The refusal is how the test sees the wizard without running
// it.)
func TestInteractiveLaunchStillGetsTheWizard(t *testing.T) {
	for desc, flags := range map[string]map[string]string{
		"no --instance-type on a TTY": {"name": "box", "no-public-ip": "true", "subnet-id": "subnet-0123456789abcdef0"},
		"--interactive": {"name": "box", "interactive": "true", "instance-type": "t4g.small",
			"no-public-ip": "true", "subnet-id": "subnet-0123456789abcdef0"},
	} {
		t.Run(desc, func(t *testing.T) {
			_, role, err := runModeLaunch(t, flags)
			if err == nil || !strings.Contains(err.Error(), wizardRefusal) {
				t.Fatalf("err = %v, want the wizard's --no-public-ip refusal", err)
			}
			if role.count() != 0 {
				t.Error("a wizard launch reached the sweep path")
			}
			if got := currentLaunchMode(); got != modeWizard {
				t.Errorf("currentLaunchMode() = %d, want the wizard", got)
			}
		})
	}
}

// TestInteractiveSweepIsRefusedAsASweep: --interactive with --param-file is
// refused because a sweep never runs the wizard (#1366), not with the wizard's
// --no-public-ip refusal, which would say the launch is a wizard one.
func TestInteractiveSweepIsRefusedAsASweep(t *testing.T) {
	_, role, err := runModeLaunch(t, map[string]string{
		"param-file": defaultsOnlySweepFile(t), "region": "us-east-1", "yes": "true",
		"interactive": "true", "no-public-ip": "true", "subnet-id": "subnet-0123456789abcdef0",
	})
	if err == nil || !strings.Contains(err.Error(), "--interactive") || strings.Contains(err.Error(), wizardRefusal) {
		t.Fatalf("err = %v, want the sweep's --interactive refusal", err)
	}
	if role.count() != 0 {
		t.Error("an --interactive sweep got past the flag refusals")
	}
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return []string{"plain"}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSweepNameArgs is #1532's name rule, through launch's own Args
// validator: a parameter sweep needs no spore name (its members are named by
// their rows, else <sweep-name>-<index>); every other launch still does, and
// a sweep given two different names is still refused.
func TestSweepNameArgs(t *testing.T) {
	tests := []struct {
		desc    string
		args    []string
		flags   map[string]string
		wantErr string
	}{
		{desc: "sweep, no name", flags: map[string]string{"param-file": "p.yaml", "sweep-name": "swr"}},
		{desc: "sweep without --sweep-name, no name", flags: map[string]string{"param-file": "p.yaml"}},
		{desc: "inline --params sweep, no name", flags: map[string]string{"params": "[]"}},
		{desc: "sweep, positional name", args: []string{"swr"}, flags: map[string]string{"param-file": "p.yaml"}},
		{desc: "sweep, --name", flags: map[string]string{"param-file": "p.yaml", "name": "swr"}},
		{desc: "sweep, conflicting names", args: []string{"aaa"},
			flags: map[string]string{"param-file": "p.yaml", "name": "bbb"}, wantErr: "conflicting names"},
		{desc: "sweep, too many positionals", args: []string{"a", "b"},
			flags: map[string]string{"param-file": "p.yaml"}, wantErr: "too many arguments"},
		{desc: "single launch, no name", flags: map[string]string{"instance-type": "t4g.small"},
			wantErr: "a spore name is required"},
		{desc: "batch queue with a param file, no name",
			flags:   map[string]string{"param-file": "p.yaml", "batch-queue": "q.json"},
			wantErr: "a spore name is required"},
		{desc: "queue template with a param file, no name",
			flags:   map[string]string{"param-file": "p.yaml", "queue-template": "t"},
			wantErr: "a spore name is required"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			restoreLaunchFlagsAtCleanup(t)
			for k, v := range tt.flags {
				if err := launchCmd.Flags().Set(k, v); err != nil {
					t.Fatalf("set --%s: %v", k, err)
				}
			}
			err := launchNameArgs(launchCmd, tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected acceptance, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// restoreLaunchFlagsAtCleanup puts back, at cleanup, every launch flag the test sets.
func restoreLaunchFlagsAtCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		launchCmd.Flags().Visit(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	})
}

// TestExampleParamFileUsageLinesAreAccepted: the usage a param-file example
// prints in its header ("# Use with: spawn ...") is what users copy. Each one
// must resolve to a command, parse with that command's flags and pass its Args
// validator. A `spawn launch` one is also a flags launch on a TTY, not the
// wizard. #1532: simple-params.yaml's line has no spore name, which launch
// refused.
func TestExampleParamFileUsageLinesAreAccepted(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "examples", "*params*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	launches := 0
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			_, usage, ok := strings.Cut(line, "# Use with: spawn ")
			if !ok {
				continue
			}
			t.Run(filepath.Base(path), func(t *testing.T) {
				argv := splitShellWords(t, usage)
				// The usage names the file relative to examples/.
				for i, a := range argv {
					if a == filepath.Base(path) {
						argv[i] = path
					}
				}
				c, rest, err := rootCmd.Find(argv)
				if err != nil {
					t.Fatalf("%q does not resolve to a command: %v", usage, err)
				}
				t.Cleanup(func() {
					c.Flags().Visit(func(f *pflag.Flag) {
						_ = f.Value.Set(f.DefValue)
						f.Changed = false
					})
				})
				if c == launchCmd {
					launches++
				}
				if err := c.ParseFlags(rest); err != nil {
					t.Fatalf("%s rejects the flags in %q: %v", c.CommandPath(), usage, err)
				}
				if c.Args != nil {
					if err := c.Args(c, c.Flags().Args()); err != nil {
						t.Fatalf("%s rejects %q: %v", c.CommandPath(), usage, err)
					}
				}
				if c == launchCmd {
					stubStdinTTY(t, true)
					if got := currentLaunchMode(); got != modeFlags {
						t.Errorf("%q is mode %d on a TTY, want a flags launch", usage, got)
					}
					if _, err := parseParamFile(paramFile); err != nil {
						t.Errorf("%s: %v", path, err)
					}
				}
			})
		}
	}
	if launches == 0 {
		t.Fatal("found no `spawn launch` usage line in examples/*params*.yaml: the scan is broken")
	}
}

// splitShellWords splits a command line on spaces, honouring double quotes,
// which is all the example usage lines use.
func splitShellWords(t *testing.T, s string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inQuote, inWord := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote, inWord = !inQuote, true
		case r == ' ' && !inQuote:
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inQuote {
		t.Fatalf("unbalanced quote in %q", s)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// TestNamelessSweepMembersAreNamedBySweep: what a sweep without a spore name
// launches (#1532). Each member is named by its row's name:, else
// <sweep-name>-<index>; nothing is left unnamed.
func TestNamelessSweepMembersAreNamedBySweep(t *testing.T) {
	keepLaunchGlobals(t)
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"instance_type": "t4g.small", "ttl": "1h"},
		Params:   []map[string]interface{}{{"alpha": 1}, {"alpha": 2, "name": "own"}},
	}
	cfgs, _, err := foregroundSweepMembers(aws.LaunchConfig{Region: "us-east-1"}, pf, "swr-id", "swr")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{cfgs[0].Name, cfgs[1].Name}
	if got[0] != "swr-0" || got[1] != "own" {
		t.Errorf("member names = %q, want [swr-0 own]", got)
	}
}
