package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/cost"
)

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Cost tracking and breakdown",
	Long: `View cost breakdowns and budget status for parameter sweeps.

Examples:
  # View cost breakdown by region and instance type
  spawn cost breakdown <sweep-id>

  # View budget status
  spawn cost breakdown <sweep-id>
`,
}

var costBreakdownCmd = &cobra.Command{
	Use:   "breakdown <sweep-id>",
	Short: "Show cost breakdown for a sweep",
	Long: `Display detailed cost breakdown by region and instance type.

Shows:
- Resource costs (compute, storage, network)
- Cloud economics (effective cost/hr, utilization, savings)
- Time breakdown (running vs stopped hours)
- Budget status (if budget was set)
- Cost by region and instance type

Examples:
  spawn cost breakdown sweep-20260124-140530
`,
	Args: cobra.ExactArgs(1),
	RunE: runCostBreakdown,
}

func init() {
	rootCmd.AddCommand(costCmd)
	costCmd.AddCommand(costBreakdownCmd)
}

func runCostBreakdown(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	sweepID := args[0]

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	dbClient := dynamodb.NewFromConfig(cfg)
	costClient := cost.NewClient(dbClient)

	breakdown, err := costClient.GetCostBreakdown(ctx, sweepID)
	if err != nil {
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

	return nil
}
