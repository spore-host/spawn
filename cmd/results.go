package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/provenance"
	"github.com/spore-host/spawn/pkg/taskproto"
)

var (
	resultsRetentionBucket  string
	resultsRetentionPrefix  string
	resultsRetentionRegion  string
	resultsRetentionObjects bool
)

var resultsCmd = &cobra.Command{
	Use:   "results",
	Short: "Inspect task results in S3",
}

var resultsRetentionCmd = &cobra.Command{
	Use:   "retention",
	Short: "Report what task results cost to keep and what retention by regeneration cost would do (read only)",
	Long: `Report on task results by what they would cost to make again (#1076).

Since v0.132.1, task outputs are tagged when they're uploaded. The tags give the
run's regeneration class: cheap is under $1 and under 1 hour, expensive is $25
or more, or 12 hours or more, and moderate is in between. Each output also gets
a provenance record of the recipe, the inputs and their ETags, and the cost.
This command reads a bucket and reports, run by run:

  - what is stored, and its monthly storage cost now;
  - what it would cost once the decided tiering rules had moved it. Cheap goes
    to Standard-IA at 30 days and Glacier IR at 90, and moderate goes to
    Standard-IA at 60. Expensive and unclassified outputs, and anything without
    spawn:retain=false, are never moved;
  - whether the deletion sweeper could prove the run regenerable today. That
    needs three things: the spec opted in (regenerable: true), the provenance
    record is readable, and every recorded input still exists with the same
    ETag and version. Each reason a run can't be proven is listed. An input you
    can't read counts as unproven, never as unchanged.

This command NEVER tiers, tags or deletes anything; it is a report. No tiering
rule or sweeper exists yet. They come in later phases, after reports like this
one have been read.

Storage prices are a static us-east-1 table, and every dollar figure says so.
Objects with no spawn:run-id tag are listed as untracked and kept: logs,
completion records, outputs written before v0.132.1, and outputs uploaded
through the untagged fallback.`,
	Example: `  # This account's default results bucket in the configured region
  spawn results retention

  # A caller bucket, one prefix, with every object listed
  spawn results retention --bucket my-lab-results --prefix project-x/ --objects

  # Machine-readable
  spawn results retention -o json`,
	Args: cobra.NoArgs,
	RunE: runResultsRetention,
}

func init() {
	resultsRetentionCmd.Flags().StringVar(&resultsRetentionBucket, "bucket", "", "Bucket to report on (default: spawn-results-<account>-<region>)")
	resultsRetentionCmd.Flags().StringVar(&resultsRetentionPrefix, "prefix", "", "Only objects under this key prefix")
	resultsRetentionCmd.Flags().StringVar(&resultsRetentionRegion, "region", "", "Region for the default bucket (default: configured region)")
	resultsRetentionCmd.Flags().BoolVar(&resultsRetentionObjects, "objects", false, "List every object with where tiering would put it")
	resultsCmd.AddCommand(resultsRetentionCmd)
	rootCmd.AddCommand(resultsCmd)
}

func runResultsRetention(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	var client *aws.Client
	var err error
	if resultsRetentionRegion != "" {
		client, err = aws.NewClientWithRegion(ctx, resultsRetentionRegion)
	} else {
		client, err = aws.NewClient(ctx)
	}
	if err != nil {
		return fmt.Errorf("initialize AWS client: %w", err)
	}
	cfg := client.Config()
	bucket := resultsRetentionBucket
	if bucket == "" {
		account, err := client.GetAccountID(ctx)
		if err != nil {
			return fmt.Errorf("resolve account for the default results bucket: %w", err)
		}
		bucket = taskproto.DefaultResultsBucket(account, cfg.Region)
	}

	rep, err := provenance.BuildReportFromConfig(ctx, cfg, provenance.ReportOptions{Bucket: bucket, Prefix: resultsRetentionPrefix})
	if err != nil {
		return err
	}
	if spawnOutputFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	renderRetention(os.Stdout, rep, resultsRetentionObjects)
	return nil
}

// renderRetention writes the human report. Split out so the wording, which is
// most of the point, is testable without AWS.
func renderRetention(w io.Writer, rep *provenance.Report, objects bool) {
	where := "s3://" + rep.Bucket + "/" + rep.Prefix
	fmt.Fprintf(w, "spawn results retention: %s (a report; nothing is tiered, tagged or deleted)\n\n", where)

	if rep.Totals.Objects == 0 && len(rep.Errors) == 0 {
		fmt.Fprintf(w, "No objects under %s.\n", where)
		fmt.Fprintf(w, "Task outputs go where each spec's outputs[].destination says, which is often\n")
		fmt.Fprintf(w, "not this bucket. Use --bucket and --prefix to report on them there.\n")
		return
	}

	if len(rep.Runs) == 0 {
		fmt.Fprintf(w, "No run-tagged outputs here. Outputs are tagged only since v0.132.1, and only\n")
		fmt.Fprintf(w, "when the instance's spored uploaded them.\n\n")
	} else {
		fmt.Fprintf(w, "Runs (%d), largest first\n", len(rep.Runs))
		tw := newTableWriter(w)
		fmt.Fprintf(tw, "  RUN\tTASK\tCLASS\tREGEN COST\tOBJECTS\tSIZE\t$/MONTH NOW\t$/MONTH TIERED\tDELETABLE?\n")
		for _, r := range rep.Runs {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", shortRunID(r.RunID), dashIfEmpty(r.TaskID),
				classLabel(r.Class), regenLabel(r), r.Objects, sizeLabel(r.Bytes),
				usdLabel(r.MonthlyUSD), usdLabel(r.TieredMonthlyUSD), provableLabel(r.Provable))
		}
		tw.Flush()
		fmt.Fprintln(w)

		var notes []string
		for _, r := range rep.Runs {
			if !r.Provable {
				fmt.Fprintf(w, "  %s can't be proven regenerable:\n", shortRunID(r.RunID))
				for _, why := range r.WhyNot {
					fmt.Fprintf(w, "    - %s\n", why)
				}
			}
			if r.RateSource == provenance.RateSourceOnDemandStatic {
				notes = append(notes, shortRunID(r.RunID)+": its rate is STATIC: truffle's built-in table, used because the live AWS Price List lookup failed at launch; truffle doesn't expose the table's collection date (truffle#184)")
			}
			if r.LowerBound {
				notes = append(notes, shortRunID(r.RunID)+": Windows: the rate omits the Windows licence, so its regeneration cost is a lower bound (#884)")
			}
			if r.Unpriced > 0 {
				notes = append(notes, fmt.Sprintf("%s: %s is in a storage class the static price table doesn't cover and is not in the $ figures", shortRunID(r.RunID), sizeLabel(r.Unpriced)))
			}
		}
		fmt.Fprintln(w)
		for _, n := range notes {
			fmt.Fprintf(w, "  note: %s\n", n)
		}
		if len(notes) > 0 {
			fmt.Fprintln(w)
		}
	}

	if objects {
		for _, r := range rep.Runs {
			fmt.Fprintf(w, "Objects of %s\n", shortRunID(r.RunID))
			tw := newTableWriter(w)
			fmt.Fprintf(tw, "  KEY\tSIZE\tSTORAGE\tTIERED TO\tWHY NOT\n")
			for _, o := range r.ObjectSet {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", o.Key, sizeLabel(o.Bytes), o.StorageClass, dashIfEmpty(o.TierTarget), dashIfEmpty(o.NotTieredWhy))
			}
			tw.Flush()
			fmt.Fprintln(w)
		}
	}

	if rep.Untracked.Objects > 0 {
		fmt.Fprintf(w, "Untracked: %d object(s), %s, %s/month. No spawn:run-id tag, so no rule will\n", rep.Untracked.Objects, sizeLabel(rep.Untracked.Bytes), usdLabel(rep.Untracked.MonthlyUSD))
		fmt.Fprintf(w, "ever tier or delete them (logs, completion records, older or fallback uploads).\n\n")
	}

	fmt.Fprintf(w, "Total: %d object(s), %s. Storage %s/month now, %s/month once tiering has caught up.\n",
		rep.Totals.Objects, sizeLabel(rep.Totals.Bytes), usdLabel(rep.Totals.MonthlyUSD), usdLabel(rep.Totals.TieredMonthlyUSD))
	fmt.Fprintf(w, "Runs the deletion sweeper could prove regenerable today: %d of %d.\n\n", rep.Totals.ProvableRuns, len(rep.Runs))
	fmt.Fprintf(w, "Storage $ are %s.\n", rep.PriceSource)

	if len(rep.Errors) > 0 {
		fmt.Fprintf(w, "\nINCOMPLETE: %d object(s) couldn't be read and are in none of the figures above:\n", len(rep.Errors))
		for _, e := range rep.Errors {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
}

func shortRunID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func classLabel(c provenance.Class) string {
	if c == "" {
		return "unclassified"
	}
	return string(c)
}

func regenLabel(r provenance.RunSummary) string {
	if r.RegenUSD == "" {
		return "rate unknown"
	}
	s := "$" + r.RegenUSD + " / " + strings.TrimRight(strings.TrimRight(r.RegenHours, "0"), ".") + "h"
	if r.RateSource == provenance.RateSourceOnDemandStatic {
		s += " (static rate)"
	}
	if r.LowerBound {
		s += " (lower bound)"
	}
	return s
}

func provableLabel(ok bool) string {
	if ok {
		return "provable"
	}
	return "no"
}

func usdLabel(v float64) string {
	if v > 0 && v < 0.0001 {
		return "<$0.0001"
	}
	if v > 0 && v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func sizeLabel(b int64) string {
	const k = 1024
	switch {
	case b >= k*k*k*k:
		return fmt.Sprintf("%.1f TiB", float64(b)/(k*k*k*k))
	case b >= k*k*k:
		return fmt.Sprintf("%.1f GiB", float64(b)/(k*k*k))
	case b >= k*k:
		return fmt.Sprintf("%.1f MiB", float64(b)/(k*k))
	case b >= k:
		return fmt.Sprintf("%.1f KiB", float64(b)/k)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
