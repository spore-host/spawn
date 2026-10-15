package cmd

import (
	"context"

	"github.com/spf13/cobra"
)

// sweepGroupCmd is the parent for all parameter sweep subcommands.
// Use: spawn sweep <subcommand>
var sweepGroupCmd = &cobra.Command{
	Use:   "sweep",
	Short: "Manage parameter sweeps",
	Long: `Manage parameter sweeps: list, check status, cancel, resume, and collect results.

Examples:
  spawn sweep list
  spawn sweep status sweep-20260116-abc123
  spawn sweep cancel sweep-20260116-abc123
  spawn sweep resume sweep-20260116-abc123 --max-concurrent 5
  spawn sweep collect sweep-20260116-abc123 --output-file results.json`,
}

// ── sweep list ──────────────────────────────────────────────────────────────

var sweepListCmd = &cobra.Command{
	Use:   "list",
	Short: "List parameter sweeps",
	RunE:  runListSweeps,
}

// ── sweep status ────────────────────────────────────────────────────────────

var (
	sweepStatusJSON          bool
	sweepStatusCheckComplete bool
)

var sweepStatusCmd = &cobra.Command{
	Use:   "status <sweep-id>",
	Short: "Show parameter sweep status and progress",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		jsonOut := sweepStatusJSON || getOutputFormat() == "json"
		return runSweepStatus(ctx, args[0], jsonOut, sweepStatusCheckComplete)
	},
}

// ── sweep cancel ────────────────────────────────────────────────────────────

var sweepCancelCmd = &cobra.Command{
	Use:   "cancel <sweep-id>",
	Short: "Cancel a running parameter sweep and terminate its instances",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cancelSweepID = args[0]
		return runCancel(cmd, args)
	},
}

// ── sweep resume ────────────────────────────────────────────────────────────

var sweepResumeCmd = &cobra.Command{
	Use:   "resume <sweep-id>",
	Short: "Resume an interrupted parameter sweep from checkpoint",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resumeSweepID = args[0]
		return runResume(cmd, args)
	},
}

// ── sweep collect ───────────────────────────────────────────────────────────

var sweepCollectCmd = &cobra.Command{
	Use:   "collect <sweep-id>",
	Short: "Download and aggregate results from a completed sweep",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		collectSweepID = args[0]
		return runCollectResults(cmd, args)
	},
}

func init() {
	rootCmd.AddCommand(sweepGroupCmd)
	sweepGroupCmd.AddCommand(sweepListCmd, sweepStatusCmd, sweepCancelCmd, sweepResumeCmd, sweepCollectCmd)

	// sweep list shares flags with listSweepsCmd
	sweepListCmd.Flags().StringVar(&listSweepsStatus, "status", "", "Filter by status (RUNNING, COMPLETED, FAILED, CANCELLED)")
	sweepListCmd.Flags().IntVar(&listSweepsLast, "last", 20, "Show last N sweeps")
	sweepListCmd.Flags().StringVar(&listSweepsSince, "since", "", "Show sweeps created after date (YYYY-MM-DD)")
	sweepListCmd.Flags().StringVar(&listSweepsRegion, "region", "", "Filter by region")

	// sweep cancel shares the --yes confirmation with the top-level cancel (it
	// delegates to runCancel, which honors cancelYes).
	sweepCancelCmd.Flags().BoolVarP(&cancelYes, "yes", "y", false, "Skip the confirmation prompt")

	// sweep status flags
	sweepStatusCmd.Flags().BoolVar(&sweepStatusJSON, "json", false, "Output as JSON")
	_ = sweepStatusCmd.Flags().MarkDeprecated("json", "use --output json instead")
	sweepStatusCmd.Flags().BoolVar(&sweepStatusCheckComplete, "check-complete", false, "Exit with standardized codes: 0=complete 1=failed 2=running 3=error")

	// sweep resume flags: the same set the deprecated top-level resume declares
	// (they share runResume and its vars), so neither spelling can lose one (#935).
	registerResumeFlags(sweepResumeCmd)

	// sweep collect flags: the same set the deprecated collect-results declares
	// (they share runCollectResults and its vars) (#935).
	registerCollectFlags(sweepCollectCmd)

	// Deprecate the old top-level sweep commands
	cancelCmd.Deprecated = "use 'spawn sweep cancel <sweep-id>' instead"
	listSweepsCmd.Deprecated = "use 'spawn sweep list' instead"
	resumeCmd.Deprecated = "use 'spawn sweep resume <sweep-id>' instead"
	collectCmd.Deprecated = "use 'spawn sweep collect <sweep-id>' instead"
}
