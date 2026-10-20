package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #1161: --all-regions scans the regions ENABLED in the account
// (resolveCleanupRegions → GetEnabledRegions), not "every region spawn
// supports". The flag's help must say what the resolver does; the resolver's
// call is checked too, so the two cannot drift apart unnoticed.
func TestFootprintAllRegionsHelpMatchesTheResolver(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRootFromCmd(t), "cmd", "resources.go")) //nolint:gosec // fixed repo path
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?s)func resolveCleanupRegions\(.*?if allRegions \{\s*regions, err := client\.GetEnabledRegions\(ctx\)`).Match(b) {
		t.Fatal("resolveCleanupRegions no longer resolves --all-regions with GetEnabledRegions; update this test and the flag help together")
	}
	usage := footprintCmd.Flags().Lookup("all-regions").Usage
	if !strings.Contains(usage, "enabled in this account") {
		t.Errorf("--all-regions help %q does not say it scans the account's enabled regions", usage)
	}
	if strings.Contains(footprintCmd.Example, "spawn knows about") || strings.Contains(usage, "spawn supports") {
		t.Error("footprint help still describes --all-regions as every region spawn supports")
	}
}

// #1161: the help and footer must not call default-bus rules "schedules".
func TestFootprintSaysRulesNotSchedules(t *testing.T) {
	if strings.Contains(footprintCmd.Long, "EventBridge schedules") {
		t.Error("footprint help says it reports EventBridge schedules; it lists default-bus rules")
	}
	if !strings.Contains(footprintCmd.Long, "default event bus") {
		t.Error("footprint help does not say the EventBridge rows are default-bus rules")
	}
}
