//go:build e2e_tier0

package e2e

import (
	"sort"
	"strings"
	"testing"
)

// TestTier0_CommandCoverageGate enforces a deliberate Tier 0 coverage decision
// for EVERY top-level command: each must be either exercised by the Tier 0
// suite or explicitly deferred with a reason (Tier 2/3 surface that needs a
// real instance, SSH, spored, or SQS — which Substrate can't emulate). A new
// command added without a classification fails this gate, so coverage decisions
// can't be silently skipped (mirrors the repo's "no untested source" rule, #23).
//
// The gate compares the binary's actual top-level command list against the
// union of the two maps below — it fails on BOTH directions: an unclassified
// command, and a stale classification for a command that no longer exists.
func TestTier0_CommandCoverageGate(t *testing.T) {
	// Commands with at least one Tier 0 behavioral test (this directory).
	covered := map[string]string{
		"launch":    "tier0_lifecycle_test.go",
		"list":      "tier0_lifecycle_test.go / output matrix",
		"stop":      "tier0_lifecycle_test.go (StateTransitions)",
		"start":     "tier0_lifecycle_test.go (StateTransitions)",
		"terminate": "tier0_lifecycle_test.go / output matrix",
		"hibernate": "tier0_lifecycle_test.go",
		"defaults":  "tier0_statecmds_test.go",
		"team":      "tier0_statecmds_test.go",
		"sweep":     "tier0_sweeps_test.go (list-sweeps query path)",
		"status":    "output matrix (negative) + Tier 2 for SSH path",
		"version":   "output matrix / smoke",
	}

	// Commands deliberately deferred to Tier 2/3 (real instance / SSH / spored /
	// SQS / external integration that Substrate's control-plane emulation can't
	// faithfully serve), each with a reason.
	deferred := map[string]string{
		"connect":         "Tier 2: real SSH to a booted instance",
		"extend":          "Tier 2: SSH → spored reload on a live instance",
		"app":             "Tier 2: app streaming on a live instance",
		"instance-config": "Tier 2: config get/set over SSH",
		"queue":           "Tier 3: SQS-backed; SDK v2 SQS protocol mismatch in Substrate",
		"pipeline":        "Tier 3: multi-stage orchestration over the queue",
		"slurm":           "Tier 3: real Slurm submit/cluster",
		"burst":           "Tier 3: cross-account burst needs real dev-account identity",
		"stage":           "Tier 2/3: S3 data staging to/from a live run",
		"autoscale":       "Tier 3: scaling decisions against a real ASG/fleet",
		"alerts":          "Tier 1/3: Slack/SNS external delivery",
		"notify":          "Tier 1/3: external notification delivery",
		"schedule":        "Tier 3: EventBridge Scheduler end-to-end",
		"dns":             "Tier 2: Route53 + live instance public IP",
		"fsx":             "Tier 2/3: FSx lifecycle against a real filesystem",
		"plugin":          "Tier 2: plugin install/status on a live instance",
		"cost":            "Tier 1: real Cost Explorer / pricing data",
		"availability":    "Tier 1: real instance-type offerings / quotas",
		"ami":             "Tier 1/2: real AMI build/copy",
		"validate":        "covered by pkg/infrastructure substrate tests (not CLI Tier 0)",

		// cobra built-ins — no spawn behavior to cover.
		"help":       "cobra built-in",
		"completion": "cobra built-in (shell completion script)",
	}

	live := liveTopLevelCommands(t)

	var unclassified []string
	for _, cmd := range live {
		_, inCovered := covered[cmd]
		_, inDeferred := deferred[cmd]
		if !inCovered && !inDeferred {
			unclassified = append(unclassified, cmd)
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("commands with no Tier 0 coverage decision: %v\n"+
			"Add each to either `covered` (with a Tier 0 test) or `deferred` "+
			"(with a reason) in tier0_coverage_test.go.", unclassified)
	}

	// Reverse direction: a classification for a command the binary no longer
	// exposes is stale and should be removed.
	liveSet := map[string]bool{}
	for _, c := range live {
		liveSet[c] = true
	}
	var stale []string
	for c := range covered {
		if !liveSet[c] {
			stale = append(stale, "covered:"+c)
		}
	}
	for c := range deferred {
		if !liveSet[c] {
			stale = append(stale, "deferred:"+c)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("stale coverage classifications for commands no longer in the binary: %v", stale)
	}
}

// liveTopLevelCommands parses `spawn --help` and returns the top-level command
// names the binary actually exposes.
func liveTopLevelCommands(t *testing.T) []string {
	env := startSpawnSubstrate(t)
	out, _, _ := env.run("--help")

	var cmds []string
	inSection := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Available Commands:") {
			inSection = true
			continue
		}
		if inSection {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "Flags:") {
				break
			}
			fields := strings.Fields(line)
			if len(fields) > 0 {
				cmds = append(cmds, fields[0])
			}
		}
	}
	if len(cmds) == 0 {
		t.Fatalf("could not parse any commands from --help:\n%s", out)
	}
	return cmds
}
