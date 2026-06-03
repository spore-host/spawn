package main

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

// TestCLIContract locks the external command surface spawn and the bootstrap
// scripts depend on. These names/flags are load-bearing: spawn shells out to
// `spored status --check-complete`, `spored config get/set/list`,
// `spored reload`, `spored complete --status/--message`, the bootstrap runs the
// bare binary as the daemon, and install-spored.sh runs `spored version`.
func TestCLIContract(t *testing.T) {
	root := newRootCmd()

	// Root with no subcommand must be runnable (it is the daemon).
	if root.RunE == nil && root.Run == nil {
		t.Error("root command must be runnable (daemon mode for bare `spored`)")
	}

	byName := map[string]bool{}
	for _, c := range root.Commands() {
		byName[c.Name()] = true
	}
	for _, want := range []string{"run-queue", "run-pipeline-stage", "status", "reload", "config", "complete", "version"} {
		if !byName[want] {
			t.Errorf("missing required subcommand %q", want)
		}
	}

	// status --check-complete flag must exist (exit-code contract, #26).
	status := findCmd(root.Commands(), "status")
	if status == nil || status.Flags().Lookup("check-complete") == nil {
		t.Error("status must expose --check-complete")
	}

	// complete --status/--message/--file flags must exist.
	complete := findCmd(root.Commands(), "complete")
	for _, f := range []string{"status", "message", "file"} {
		if complete == nil || complete.Flags().Lookup(f) == nil {
			t.Errorf("complete must expose --%s", f)
		}
	}

	// config must have get/set/list subcommands.
	config := findCmd(root.Commands(), "config")
	if config == nil {
		t.Fatal("missing config command")
	}
	cfgSubs := map[string]bool{}
	for _, c := range config.Commands() {
		cfgSubs[c.Name()] = true
	}
	for _, want := range []string{"get", "set", "list"} {
		if !cfgSubs[want] {
			t.Errorf("config missing subcommand %q", want)
		}
	}

	// `spored version` must print the historical "spored version X" line.
	var out bytes.Buffer
	root.SetArgs([]string{"version"})
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("version subcommand: %v", err)
	}
}

func findCmd(cmds []*cobra.Command, name string) *cobra.Command {
	for _, c := range cmds {
		if c.Name() == name {
			return c
		}
	}
	return nil
}
