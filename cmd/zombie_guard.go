package cmd

import (
	"fmt"
	"os"

	"github.com/spore-host/spawn/pkg/aws"
)

// Zombie-instance guard (2026-06 audit, M-health): a single source of truth for
// the "no TTL and no idle timeout → default to 1h idle" safety default and the
// paired --no-timeout warning. Previously copy-pasted across the single,
// batch-queue, and sweep launch paths (with a doubled comment block).

// applyIdleTimeoutDefault sets a 1h idle timeout on cfg when neither a TTL nor an
// idle timeout is set and --no-timeout was not passed, preventing an instance
// from running indefinitely if the CLI disconnects. It reports whether the
// default was applied. It does NOT print — callers decide how to surface it
// (once per instance, or once for a whole sweep).
func applyIdleTimeoutDefault(cfg *aws.LaunchConfig) bool {
	if cfg.TTL == "" && cfg.IdleTimeout == "" && !noTimeout {
		cfg.IdleTimeout = "1h"
		return true
	}
	return false
}

// warnIdleTimeoutDefault prints the notice that a 1h idle timeout was auto-applied.
func warnIdleTimeoutDefault() {
	fmt.Fprintf(os.Stderr, "\n⚠️  Auto-setting --idle-timeout=1h to prevent zombie instances\n")
	fmt.Fprintf(os.Stderr, "   Instance will terminate after 1 hour of inactivity.\n")
	fmt.Fprintf(os.Stderr, "   Override with --ttl, --idle-timeout, or --no-timeout\n")
	fmt.Fprintf(os.Stderr, "   See: https://github.com/spore-host/spawn/blob/main/docs/lifecycle.md\n\n")
}

// warnNoTimeout prints the zombie-risk warning when --no-timeout is set.
func warnNoTimeout() {
	fmt.Fprintf(os.Stderr, "\n⚠️  WARNING: --no-timeout specified\n")
	fmt.Fprintf(os.Stderr, "   Instance will run indefinitely until manually terminated.\n")
	fmt.Fprintf(os.Stderr, "   If CLI disconnects, you must track and terminate manually.\n")
	fmt.Fprintf(os.Stderr, "   This can result in unexpected costs from zombie instances.\n\n")
}

// guardZombieInstance applies the idle-timeout default to a single launch config
// and prints the appropriate notice/warning. Used by the single-instance and
// batch-queue launch paths. (The sweep path applies the default across many
// configs and prints once — it calls applyIdleTimeoutDefault directly.)
//
// When --no-timeout is set it now REQUIRES confirmation (2026-06 audit, L-ergo):
// disabling the cost guardrails is an explicit, acknowledged choice rather than a
// warning the user can blow past. --yes (autoYes) satisfies it non-interactively;
// a declined or non-interactive prompt without --yes aborts the launch.
func guardZombieInstance(cfg *aws.LaunchConfig) error {
	if applyIdleTimeoutDefault(cfg) {
		warnIdleTimeoutDefault()
		return nil
	}
	if noTimeout {
		warnNoTimeout()
		if !confirmYes(autoYes, "Launch with NO automatic timeout (zombie/cost risk)?") {
			return fmt.Errorf("aborted: --no-timeout not confirmed (pass --yes to acknowledge, or set --ttl/--idle-timeout)")
		}
	}
	return nil
}
