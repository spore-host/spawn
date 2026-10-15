package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1022: Windows launches accepted --command and --user-data and dropped them,
// so the workload never ran and --on-complete never fired. The rule these tests
// hold: every launch flag is either honoured on Windows or refused.

// launchFlagNames discovers every flag `spawn launch` accepts, local and
// inherited, from the cobra command itself.
func launchFlagNames(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	launchCmd.Flags().VisitAll(func(f *pflag.Flag) { seen[f.Name] = true })
	launchCmd.InheritedFlags().VisitAll(func(f *pflag.Flag) { seen[f.Name] = true })
	var names []string
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	// A discovery that found (almost) nothing has broken; it has not proven
	// every flag classified.
	if len(names) < 50 {
		t.Fatalf("discovered only %d launch flags; the discovery is broken", len(names))
	}
	return names
}

func TestEveryLaunchFlagHasAWindowsRule(t *testing.T) {
	names := launchFlagNames(t)
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
		rule, ok := windowsLaunchFlags[n]
		if !ok {
			t.Errorf("--%s has no entry in windowsLaunchFlags: decide whether a Windows launch honours it "+
				"or refuses it. Accepting and dropping it is #1022", n)
			continue
		}
		if strings.TrimSpace(rule.why) == "" {
			t.Errorf("--%s: the rule records no reason", n)
		}
	}
	for n := range windowsLaunchFlags {
		if !known[n] {
			t.Errorf("windowsLaunchFlags has an entry for --%s, which `spawn launch` no longer has", n)
		}
	}
}

// changedOnly returns a FlagSet holding copies of launch's flags with only the
// named ones marked as set. Copies, and Changed rather than Set, so no test
// mutates the package globals the flags are bound to.
func changedOnly(names ...string) *pflag.FlagSet {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	fs := pflag.NewFlagSet("launch-copy", pflag.ContinueOnError)
	add := func(f *pflag.Flag) {
		if fs.Lookup(f.Name) != nil {
			return
		}
		c := *f
		c.Changed = want[f.Name]
		fs.AddFlag(&c)
	}
	launchCmd.Flags().VisitAll(add)
	launchCmd.InheritedFlags().VisitAll(add)
	return fs
}

// The issue's own flags, plus the others confirmed (by reading the Windows
// path) to be dropped, stated outright: the table-driven test below reads the
// same table as the guard, so it cannot catch one of these being reclassified
// as honoured.
func TestWindowsRefusesCommandAndUserData(t *testing.T) {
	for _, n := range []string{
		"command", "user-data", "user-data-file", // #1022
		"plugin", "fsx-create", "efs-id", "attach-volume", "mpi", "params", "batch-queue",
	} {
		err := windowsUnsupportedFlags("windows", changedOnly(n))
		if err == nil {
			t.Errorf("--%s was accepted for Windows; the Windows bootstrap drops it (#1022)", n)
			continue
		}
		if !strings.Contains(err.Error(), "--"+n+":") {
			t.Errorf("--%s: the refusal does not name the flag: %v", n, err)
		}
	}
}

func TestWindowsFlagRulesAreEnforced(t *testing.T) {
	for _, n := range launchFlagNames(t) {
		rule, ok := windowsLaunchFlags[n]
		if !ok {
			continue // TestEveryLaunchFlagHasAWindowsRule reports it
		}
		fs := changedOnly(n)
		winErr := windowsUnsupportedFlags("windows", fs)
		switch {
		case rule.rejected && winErr == nil:
			t.Errorf("--%s is marked refused on Windows but the guard accepted it", n)
		case rule.rejected && !strings.Contains(winErr.Error(), "--"+n+":"):
			t.Errorf("--%s: the refusal does not name the flag: %v", n, winErr)
		case !rule.rejected && winErr != nil:
			t.Errorf("--%s is marked honoured on Windows but the guard refused it: %v", n, winErr)
		}
		// Linux is never affected.
		if err := windowsUnsupportedFlags("linux", fs); err != nil {
			t.Errorf("--%s: a Linux launch was refused: %v", n, err)
		}
	}
}

// A flag left at its default is not "set": --session-timeout defaults to 30m
// and --efs-mount-point to /efs, and neither may refuse a plain Windows launch.
func TestWindowsGuardIgnoresDefaults(t *testing.T) {
	if err := windowsUnsupportedFlags("windows", changedOnly()); err != nil {
		t.Fatalf("a Windows launch with no flags set was refused: %v", err)
	}
}

// The refusal must come from the path both a real launch and --dry-run take
// (ensureAMIAndPreflight, through windowsLifecycleGuard), including when the
// OS is decided there rather than by an explicit --os.
func TestWindowsDryRunRefusesCommand(t *testing.T) {
	env := testutil.SubstrateServer(t)
	seedAL2023AMIParams(t, env)
	client := spawnaws.NewClientFromConfig(env.AWSConfig)
	stubDryRunPrice(t, 0.2)

	f := launchCmd.Flags().Lookup("command")
	prevOS, prevChanged := osFlag, f.Changed
	t.Cleanup(func() { osFlag, f.Changed = prevOS, prevChanged })
	f.Changed = true

	newCfg := func() *spawnaws.LaunchConfig {
		return &spawnaws.LaunchConfig{
			Name: "w", InstanceType: "m7i.xlarge", Region: "us-east-1",
			AMI: "ami-0abc123", TTL: "4h", SecurityGroupIDs: []string{"sg-0123456789abcdef0"},
		}
	}

	osFlag = "windows"
	var out bytes.Buffer
	err := runLaunchDryRun(context.Background(), &out, client, newCfg())
	if err == nil || !strings.Contains(err.Error(), "--command:") {
		t.Fatalf("a Windows --dry-run with --command was not refused for it: err=%v\n%s", err, out.String())
	}
	if out.Len() != 0 {
		t.Errorf("a refused launch still printed a plan:\n%s", out.String())
	}

	osFlag = "linux"
	if err := runLaunchDryRun(context.Background(), &bytes.Buffer{}, client, newCfg()); err != nil {
		t.Fatalf("a Linux --dry-run with --command was refused: %v", err)
	}
}

// An explicit --os windows is refused at the top of runLaunch, before the
// batch-queue and sweep dispatches, which never resolve a target OS.
func TestRunLaunchRefusesWindowsSweepBeforeDispatch(t *testing.T) {
	f := launchCmd.Flags().Lookup("params")
	prevOS, prevParams, prevChanged, prevDry := osFlag, params, f.Changed, launchDryRun
	t.Cleanup(func() { osFlag, params, f.Changed, launchDryRun = prevOS, prevParams, prevChanged, prevDry })
	osFlag, params, f.Changed = "windows", "x=1", true
	// --dry-run makes the sweep dispatch return its own refusal without
	// touching AWS, so a reverted guard fails this test on the message
	// instead of attempting a launch.
	launchDryRun = true

	err := runLaunch(launchCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--params:") {
		t.Fatalf("a Windows sweep reached dispatch; want the #1022 refusal naming --params, got: %v", err)
	}
}

// The backstop where the inputs would be dropped: buildUserData refuses rather
// than emit a Windows bootstrap without them.
func TestBuildUserDataRefusesWindowsWorkload(t *testing.T) {
	dir := t.TempDir()
	pub := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAAtest spawn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plat := &platform.Platform{OS: "linux", HomeDir: dir, SSHPubKeyPath: pub}

	if _, err := buildUserData(plat, &spawnaws.LaunchConfig{TargetOS: "windows", JobArrayCommand: "run.ps1"}, "", nil); err == nil {
		t.Error("buildUserData emitted a Windows bootstrap that drops --command")
	}

	prev := userData
	t.Cleanup(func() { userData = prev })
	userData = "echo hi"
	if _, err := buildUserData(plat, &spawnaws.LaunchConfig{TargetOS: "windows"}, "", nil); err == nil {
		t.Error("buildUserData emitted a Windows bootstrap that drops --user-data")
	}

	userData = ""
	if _, err := buildUserData(plat, &spawnaws.LaunchConfig{TargetOS: "windows"}, "", nil); err != nil {
		t.Errorf("a plain Windows bootstrap was refused: %v", err)
	}
}
