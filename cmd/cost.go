package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/cost"
	"github.com/spore-host/spawn/pkg/costmodel"
)

var costCmd = &cobra.Command{
	Use:   "cost <sweep-id | instance>",
	Short: "Show cost breakdown for a sweep or a single instance",
	Long: `Display a detailed cost breakdown.

For a parameter sweep or job array, shows the full breakdown:
- Resource costs (compute, storage, network)
- Cloud economics (effective cost/hr, utilization, savings)
- Time breakdown (running vs stopped hours)
- Budget status (if budget was set)
- Cost by region and instance type

For a single instance launched with 'spawn launch' (which has no sweep cost
record), shows an on-the-fly estimate of its cost so far: the on-demand rate
(spawn:price-per-hour tag) × its compute time across every stop and start
(spored's spawn:compute-seconds; compute bills only while running), plus EBS
over the time since first launch when spawn:ebs-hourly-cost is set (EBS bills
while stopped too), plus $0.005/hr for each public IPv4 address spored has
recorded (an Elastic IP bills while stopped too). The total is the figure
--cost-limit is enforced against.

Examples:
  spawn cost sweep-20260124-140530
  spawn cost my-devbox
`,
	Args: cobra.ExactArgs(1),
	RunE: runCostBreakdown,
}

// costBreakdownCmd preserves the previous `spawn cost breakdown <sweep-id>` form
// as a hidden alias; the breakdown is now reachable directly as `spawn cost
// <sweep-id>` (#308).
var costBreakdownCmd = &cobra.Command{
	Use:    "breakdown <sweep-id>",
	Short:  "Show cost breakdown for a sweep (deprecated: use `spawn cost <sweep-id>`)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE:   runCostBreakdown,
}

func init() {
	rootCmd.AddCommand(costCmd)
	costCmd.AddCommand(costBreakdownCmd)
}

func runCostBreakdown(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	sweepID := args[0]

	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// The sweep table and the on-demand pricer both use cfg, so --profile
	// reaches the pricing calls too (#967).
	costClient := cost.NewClient(cfg)

	breakdown, err := costClient.GetCostBreakdown(ctx, sweepID)
	if err != nil {
		// No sweep record: this is almost certainly a single `spawn launch`
		// instance, whose cost lives on its own tags, not the sweep table. Report
		// its compute cost from rate × runtime rather than leaking DynamoDB's raw
		// ResourceNotFoundException (#578).
		if errors.Is(err, cost.ErrNoSweepRecord) {
			if inst := resolveInstanceForCost(ctx, sweepID); inst != nil {
				fmt.Print(singleInstanceCostReport(inst, time.Now()))
				return nil
			}
			return fmt.Errorf("no cost record for %q: it is neither a known sweep/job-array "+
				"nor a resolvable spawn instance. A single instance's cost ≈ on-demand rate × "+
				"runtime — see `spawn status %s`", sweepID, sweepID)
		}
		return fmt.Errorf("get cost breakdown: %w", err)
	}

	fmt.Printf("\nCost Breakdown for %s\n", sweepID)
	fmt.Println(strings.Repeat("━", 60))
	fmt.Println()

	// Budget status
	if breakdown.Budget > 0 {
		fmt.Printf("Budget:           $%.2f\n", breakdown.Budget)
		fmt.Printf("Total Cost:       $%.2f\n", breakdown.TotalCost)
		if breakdown.BudgetExceeded {
			fmt.Printf("Budget Status:    EXCEEDED by $%.2f\n", -breakdown.BudgetRemaining)
		} else {
			fmt.Printf("Budget Remaining: $%.2f\n", breakdown.BudgetRemaining)
		}
		fmt.Println()
	} else {
		fmt.Printf("Total Cost:       $%.2f\n", breakdown.TotalCost)
		fmt.Println()
	}

	// Resource costs (only if we have component data)
	if breakdown.ComputeCost > 0 || breakdown.StorageCost > 0 || breakdown.NetworkCost > 0 {
		fmt.Println("Resource Costs:")
		fmt.Printf("  Compute (%.1fh running):  $%.2f\n", breakdown.RunningHours, breakdown.ComputeCost)
		fmt.Printf("  Storage (%.1fh total):    $%.2f\n", breakdown.TotalHours, breakdown.StorageCost)
		if breakdown.NetworkCost > 0 {
			fmt.Printf("  Network (IPv4):          $%.2f\n", breakdown.NetworkCost)
		}
		fmt.Printf("  %s\n", strings.Repeat("─", 35))
		fmt.Printf("  Total:                   $%.2f\n", breakdown.TotalCost)
		fmt.Println()
	}

	// Time breakdown
	if breakdown.TotalHours > 0 {
		fmt.Println("Time Breakdown:")
		totalH := int(breakdown.TotalHours)
		totalM := int((breakdown.TotalHours - float64(totalH)) * 60)
		runH := int(breakdown.RunningHours)
		runM := int((breakdown.RunningHours - float64(runH)) * 60)
		stopH := int(breakdown.StoppedHours)
		stopM := int((breakdown.StoppedHours - float64(stopH)) * 60)
		fmt.Printf("  Lifetime:  %dh %dm\n", totalH, totalM)
		if breakdown.TotalHours > 0 {
			fmt.Printf("  Running:   %dh %dm (%.1f%%)\n", runH, runM, (breakdown.RunningHours/breakdown.TotalHours)*100)
			fmt.Printf("  Stopped:   %dh %dm (%.1f%%)\n", stopH, stopM, (breakdown.StoppedHours/breakdown.TotalHours)*100)
		}
		fmt.Println()
	}

	// Cloud economics
	if breakdown.EffectiveCostPerHour > 0 {
		fmt.Println("Cloud Economics:")
		fmt.Printf("  Effective Cost/Hour:     $%.4f/hr\n", breakdown.EffectiveCostPerHour)
		if breakdown.Utilization > 0 {
			fmt.Printf("  Utilization:             %.1f%%\n", breakdown.Utilization)
		}
		fmt.Println()
	}

	// By region
	if len(breakdown.ByRegion) > 0 {
		fmt.Println("By Region:")
		fmt.Println()

		w := newTableWriter(os.Stdout)
		_, _ = fmt.Fprintln(w, "REGION\tINSTANCES\tRUNNING-HOURS\tCOST")
		_, _ = fmt.Fprintln(w, strings.Repeat("─", 60))

		for _, rc := range breakdown.ByRegion {
			_, _ = fmt.Fprintf(w, "%s\t%d\t%.1f\t$%.2f\n",
				rc.Region,
				rc.InstanceCount,
				rc.InstanceHours,
				rc.EstimatedCost,
			)
		}
		_ = w.Flush()
		fmt.Println()
	}

	// By instance type
	if len(breakdown.ByInstanceType) > 0 {
		fmt.Println("By Instance Type:")
		fmt.Println()

		w := newTableWriter(os.Stdout)
		_, _ = fmt.Fprintln(w, "INSTANCE TYPE\tINSTANCES\tRUNNING-HOURS\tCOST")
		_, _ = fmt.Fprintln(w, strings.Repeat("─", 60))

		for _, tc := range breakdown.ByInstanceType {
			_, _ = fmt.Fprintf(w, "%s\t%d\t%.1f\t$%.2f\n",
				tc.InstanceType,
				tc.InstanceCount,
				tc.InstanceHours,
				tc.EstimatedCost,
			)
		}
		_ = w.Flush()
		fmt.Println()
	}

	// Summary
	fmt.Println("Summary:")
	fmt.Printf("  Total Running-Hours: %.1f\n", breakdown.TotalInstanceHours)
	fmt.Printf("  Total Cost:          $%.2f\n", breakdown.TotalCost)
	if breakdown.Budget > 0 {
		percentUsed := (breakdown.TotalCost / breakdown.Budget) * 100
		fmt.Printf("  Budget Utilization:  %.1f%%\n", percentUsed)
	}
	fmt.Println()

	// #543: an instance type truffle could not price (live nor its own static
	// fallback) has its compute cost excluded above, not fabricated from a
	// guess — say so, since a Total Cost that quietly omits real spend can't be
	// trusted without this line.
	if len(breakdown.UnpricedInstanceTypes) > 0 {
		fmt.Println("⚠️  Unpriced (excluded from Total Cost above — pricing unavailable):")
		for _, t := range breakdown.UnpricedInstanceTypes {
			fmt.Printf("  - %s\n", t)
		}
		fmt.Println()
	}

	return nil
}

// resolveInstanceForCost best-effort resolves identifier to a single
// spawn-managed instance, for the single-instance fallback of `spawn cost`.
// Returns nil (never an error) when no AWS client can be built or the
// identifier doesn't name an instance — the caller then prints its own
// actionable "no cost record" message. resolveInstance already logs its lookup
// progress and any not-found detail to stderr.
func resolveInstanceForCost(ctx context.Context, identifier string) *aws.InstanceInfo {
	client, err := aws.NewClient(ctx)
	if err != nil {
		return nil
	}
	inst, err := resolveInstance(ctx, client, identifier)
	if err != nil {
		return nil
	}
	return inst
}

// instanceRuntime is what a single instance's "so far" figures are measured
// over, across every stop/start (#1029):
//
//   - Since is the wall clock since the ORIGINAL launch (originalLaunchTime —
//     the rule the TTL reaper enforces with). EBS bills on it, stopped time
//     included.
//   - Compute is the time the instance has actually RUN, which is all EC2 bills
//     compute for: spored's cumulative spawn:compute-seconds, the same total
//     --cost-limit is enforced against (pkg/agent accumulatedComputeCost). With
//     no usable tag (no spored, or an older one) it is Since, the most the
//     instance can have run, and ComputeFromTag says so.
//
// It used to be time.Since(EC2 LaunchTime) for both, which is the latest boot
// only: after a stop/start it dropped every earlier boot, and for a stopped
// instance it counted the stopped time as compute.
type instanceRuntime struct {
	Launch         time.Time
	Since          time.Duration
	Compute        time.Duration
	ComputeFromTag bool
}

func runtimeSoFar(inst *aws.InstanceInfo, now time.Time) instanceRuntime {
	r := instanceRuntime{Launch: originalLaunchTime(inst)}
	if !r.Launch.IsZero() && now.After(r.Launch) {
		r.Since = now.Sub(r.Launch)
	}
	r.Compute = r.Since
	if secs, ok := taggedComputeSeconds(inst); ok {
		r.Compute, r.ComputeFromTag = time.Duration(secs)*time.Second, true
	}
	return r
}

// singleInstanceCostReport renders the cost-so-far estimate for one instance:
// its on-demand rate (the spawn:price-per-hour tag truffle resolved at launch,
// #533) × its compute time across every boot (runtimeSoFar), plus EBS over the
// wall clock since first launch when the spawn:ebs-hourly-cost tag is present.
// A standalone instance has no DynamoDB cost record (that table is keyed by
// sweep id), so this is an on-the-fly estimate, clearly labelled, not a billing
// figure. now is a parameter so the output is deterministically testable.
func singleInstanceCostReport(inst *aws.InstanceInfo, now time.Time) string {
	var b strings.Builder
	title := inst.Name
	if title == "" {
		title = inst.InstanceID
	}
	fmt.Fprintf(&b, "\nCost for %s (%s)\n", title, inst.InstanceID)
	fmt.Fprintln(&b, strings.Repeat("━", 60))
	fmt.Fprintln(&b)

	rt := runtimeSoFar(inst, now)
	fmt.Fprintf(&b, "Type:            %s%s\n", inst.InstanceType, spotSuffix(inst.SpotInstance))
	fmt.Fprintf(&b, "State:           %s\n", inst.State)
	if rt.Since > 0 {
		fmt.Fprintf(&b, "Since launch:    %s (first launch, across stops and starts)\n", formatDuration(rt.Since))
	}
	if rt.ComputeFromTag {
		fmt.Fprintf(&b, "Compute time:    %s (running time across every boot, from spored's spawn:compute-seconds;\n", formatDuration(rt.Compute))
		fmt.Fprintln(&b, "                 up to 5 min behind while running)")
	} else if rt.Compute > 0 {
		fmt.Fprintf(&b, "Compute time:    at most %s (no spawn:compute-seconds tag, so this is the wall clock\n", formatDuration(rt.Compute))
		fmt.Fprintln(&b, "                 since first launch; compute bills only while running)")
	}

	rate := pricePerHour(inst)
	if rate <= 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "No spawn:price-per-hour tag on this instance, so its compute cost")
		fmt.Fprintln(&b, "cannot be estimated. Its cost ≈ on-demand rate × compute time.")
		return b.String()
	}

	fmt.Fprintf(&b, "Rate:            $%.4f/hr (spawn:price-per-hour)\n", rate)
	est := estimateInstanceCost(inst, rt.Compute)
	if rt.ComputeFromTag {
		fmt.Fprintf(&b, "Est. compute:    ~$%.2f (rate × compute time)\n", est)
	} else {
		fmt.Fprintf(&b, "Est. compute:    at most ~$%.2f (rate × wall clock)\n", est)
	}
	// The rest, and the total, through the function --cost-limit is enforced
	// with (costLimitSpend, pkg/costmodel), so this cannot disagree with it (#51).
	spend := costLimitSpend(inst, rt.Compute, rt.Since)
	fp := enforcedFootprint(inst)
	if fp.EBSPerHour > 0 && rt.Since > 0 {
		fmt.Fprintf(&b, "Est. storage:    ~$%.2f (spawn:ebs-hourly-cost × time since launch; EBS bills while stopped too)\n",
			spend.EBS)
	}
	switch {
	case !costmodel.IPv4Measured(inst.Tags, "spawn"):
		if inst.PublicIP != "" {
			fmt.Fprintln(&b, "Public IPv4:     not recorded by spored (it predates #51), so not counted; AWS bills $0.005/hr per address")
		}
	case fp.PublicIPv4+fp.ElasticIPv4 > 0:
		fmt.Fprintf(&b, "Public IPv4:     ~$%.2f (%d auto-assigned + %d Elastic × $%.3f/hr, static rate as of %s)\n",
			spend.PublicIPv4, fp.PublicIPv4, fp.ElasticIPv4, costmodel.PublicIPv4PerHour, costmodel.RatesAsOf)
	}
	fmt.Fprintf(&b, "Est. total:      ~$%.2f (what --cost-limit counts; excludes network)\n", spend.Total)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Note: a single instance has no sweep cost record — this is an on-the-fly")
	fmt.Fprintln(&b, "estimate from the instance's own tags, not a billing figure.")
	return b.String()
}
