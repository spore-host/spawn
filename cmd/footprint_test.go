package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
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

// #1161: the help and footer must not call EventBridge rules "schedules".
// #1219: Scheduler schedules ARE scanned now, so the help must say so and must
// no longer claim they are not.
func TestFootprintSaysRulesNotSchedules(t *testing.T) {
	long := strings.Join(strings.Fields(footprintCmd.Long), " ")
	if strings.Contains(long, "EventBridge schedules") {
		t.Error("footprint help says it reports \"EventBridge schedules\"; rules and Scheduler schedules are different things")
	}
	if !strings.Contains(long, "default event bus") {
		t.Error("footprint help does not say which buses the EventBridge rules come from")
	}
	if !strings.Contains(long, "EventBridge Scheduler schedules (what 'spawn schedule' creates") {
		t.Error("footprint help does not say it scans the Scheduler schedules 'spawn schedule' creates")
	}
	if strings.Contains(long, "not scanned") {
		t.Error("footprint help still says something is not scanned")
	}
}

// #1300: the footprint names who deletes the per-task spawn-instance-* roles,
// and counts only those: the shared spored-instance-role and the reaper's own
// role are not per-task and must not be counted as reapable.
func TestFootprintNamesTheDeleterOfPerTaskRoles(t *testing.T) {
	var b strings.Builder
	renderFootprint(&b, footprintReport{Global: []aws.ControlPlaneResource{
		{Service: "iam", Kind: "role", Name: "spawn-instance-af541989", Why: "name"},
		{Service: "iam", Kind: "instance-profile", Name: "spawn-instance-af541989", Why: "name"},
		{Service: "iam", Kind: "role", Name: "spored-instance-role", Why: "name"},
		{Service: "iam", Kind: "role", Name: "spawn-ttl-reaper-ec2", Why: "name"},
	}})
	out := b.String()
	if !strings.Contains(out, "1 per-task spawn-instance-* role(s)") {
		t.Errorf("footprint does not count exactly the one per-task role:\n%s", out)
	}
	if !strings.Contains(out, "spawn cleanup --instance-roles") || !strings.Contains(out, "REAPER_INSTANCE_ROLES") {
		t.Errorf("footprint does not name the deleters:\n%s", out)
	}
}
