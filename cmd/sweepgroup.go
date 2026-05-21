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
  spawn sweep collect sweep-20260116-abc123 --output results.json`,
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

var sweepResumeMaxConcurrent int
var sweepResumeDetach bool

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

var sweepCollectOutputFile string
var sweepCollectFormat string

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

	// sweep status flags
	sweepStatusCmd.Flags().BoolVar(&sweepStatusJSON, "json", false, "Output as JSON")
	_ = sweepStatusCmd.Flags().MarkDeprecated("json", "use --output json instead")
	sweepStatusCmd.Flags().BoolVar(&sweepStatusCheckComplete, "check-complete", false, "Exit with standardized codes: 0=complete 1=failed 2=running 3=error")

	// sweep resume flags
	sweepResumeCmd.Flags().IntVar(&resumeMaxConcurrent, "max-concurrent", 0, "Override max concurrent instances (0 = use original)")
	sweepResumeCmd.Flags().BoolVar(&resumeDetach, "detach", false, "Run sweep orchestration in Lambda")

	// sweep collect flags (reuse collect vars)
	sweepCollectCmd.Flags().StringVarP(&collectOutputFile, "output-file", "f", "results.json", "Output file path")
	sweepCollectCmd.Flags().StringVar(&collectFormat, "format", "json", "Output format: json, csv, jsonl")
	sweepCollectCmd.Flags().StringVar(&collectS3Prefix, "s3-prefix", "", "Custom S3 prefix for results (default: auto-detect)")
	sweepCollectCmd.Flags().StringVar(&collectMetric, "metric", "", "Metric to rank results by (e.g. accuracy, loss)")
	sweepCollectCmd.Flags().IntVar(&collectBestN, "best", 0, "Show only top N results by metric (0 = all)")
	sweepCollectCmd.Flags().StringVar(&collectRegions, "regions", "", "Comma-separated list of regions to collect from")

	// Deprecate the old top-level sweep commands
	cancelCmd.Deprecated = "use 'spawn sweep cancel <sweep-id>' instead"
	listSweepsCmd.Deprecated = "use 'spawn sweep list' instead"
	resumeCmd.Deprecated = "use 'spawn sweep resume <sweep-id>' instead"
	collectCmd.Deprecated = "use 'spawn sweep collect <sweep-id>' instead"
}
