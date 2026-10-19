package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/spore-host/spawn/pkg/reaperconfig"
)

// parseReaperDeploy parses a real `spawn reaper deploy` command line through
// the same flag registration the command uses.
func parseReaperDeploy(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()
	saved := []string{reaperVersion, reaperArtifact, reaperBucket, reaperSchedule, reaperRegions,
		reaperSpotRequests, reaperNetResources, reaperMaxAge, reaperNotifyURL}
	t.Cleanup(func() {
		reaperVersion, reaperArtifact, reaperBucket, reaperSchedule, reaperRegions = saved[0], saved[1], saved[2], saved[3], saved[4]
		reaperSpotRequests, reaperNetResources, reaperMaxAge, reaperNotifyURL = saved[5], saved[6], saved[7], saved[8]
	})
	fs := pflag.NewFlagSet("deploy", pflag.ContinueOnError)
	registerReaperDeployFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return fs
}

// #1154: a plain re-deploy must not reset the scan regions to the deploy
// region or touch any setting it was not given.
func TestReaperDeployWithoutFlagsKeepsRegionsAndSettings(t *testing.T) {
	fs := parseReaperDeploy(t)
	opts, err := reaperDeployOptions(fs, "1", "us-east-1", "0.136.0", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Regions != "" {
		t.Errorf("Regions = %q without --regions; that overwrites the existing reaper's regions", opts.Regions)
	}
	if opts.DefaultRegions != "us-east-1" {
		t.Errorf("DefaultRegions = %q, want the deploy region for a new reaper", opts.DefaultRegions)
	}
	if len(opts.Settings) != 0 {
		t.Errorf("Settings = %v without any setting flag", opts.Settings)
	}
}

func TestReaperDeployFlagsBecomeSettings(t *testing.T) {
	fs := parseReaperDeploy(t, "--regions", "us-west-2,eu-west-1", "--spot-requests", "REAP",
		"--net-resources", "off", "--max-age", "14d", "--notify-url", "")
	opts, err := reaperDeployOptions(fs, "1", "us-east-1", "0.136.0", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Regions != "us-west-2,eu-west-1" {
		t.Errorf("Regions = %q", opts.Regions)
	}
	want := map[string]string{
		reaperconfig.EnvSpotRequests: "REAP",
		reaperconfig.EnvNetResources: "off",
		reaperconfig.EnvMaxAge:       "14d",
		reaperconfig.EnvNotifyURL:    "", // explicit empty: remove
	}
	for k, v := range want {
		got, ok := opts.Settings[k]
		if !ok || got != v {
			t.Errorf("Settings[%s] = %q (present %v), want %q", k, got, ok, v)
		}
	}
	// Every settable variable has a flag.
	for _, k := range reaperconfig.Settable() {
		if _, ok := reaperDeployFlags[k]; !ok {
			t.Errorf("%s is settable but spawn reaper deploy has no flag for it", k)
		}
	}
}

func TestReaperDeployRejectsBadValuesWithoutEchoingTheURL(t *testing.T) {
	for _, args := range [][]string{
		{"--spot-requests", "reep"},
		{"--max-age", "forever"},
		{"--notify-url", "ftp://s3cr3t@host/x"},
	} {
		fs := parseReaperDeploy(t, args...)
		_, err := reaperDeployOptions(fs, "1", "us-east-1", "0.136.0", strings.NewReader(""))
		if err == nil {
			t.Errorf("%v: want an error", args)
			continue
		}
		if !strings.Contains(err.Error(), args[0]) {
			t.Errorf("%v: error should name the flag: %v", args, err)
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%v: error leaks the notify URL: %v", args, err)
		}
	}
}

func TestReaperDeployNotifyURLFromStdin(t *testing.T) {
	fs := parseReaperDeploy(t, "--notify-url", "-")
	opts, err := reaperDeployOptions(fs, "1", "us-east-1", "0.136.0", strings.NewReader("https://hooks.example.invalid/x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Settings[reaperconfig.EnvNotifyURL] != "https://hooks.example.invalid/x" {
		t.Errorf("Settings = %v", opts.Settings)
	}
}

// --log-retention-days must reach the deploy (#1156). It is wired through
// reaperDeployOptions, which two PRs edited; without this nothing failed when
// the field was dropped.
func TestReaperDeployLogRetentionReachesTheDeploy(t *testing.T) {
	saved := reaperLogRetention
	t.Cleanup(func() { reaperLogRetention = saved })
	for args, want := range map[string]int32{"": 30, "90": 90} {
		var fs *pflag.FlagSet
		if args == "" {
			fs = parseReaperDeploy(t)
		} else {
			fs = parseReaperDeploy(t, "--log-retention-days", args)
		}
		opts, err := reaperDeployOptions(fs, "1", "us-east-1", "0.136.0", strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		if opts.LogRetentionDays != want {
			t.Errorf("--log-retention-days %q: LogRetentionDays = %d, want %d", args, opts.LogRetentionDays, want)
		}
	}
}
