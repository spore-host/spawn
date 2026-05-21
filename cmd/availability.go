package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/availability"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

var (
	availabilityInstanceType string
	availabilityRegions      string
)

var availabilityCmd = &cobra.Command{
	Use:   "availability",
	Short: "Display availability statistics for instance types across regions",
	Long: `Display availability statistics based on historical launch success/failure data.

This helps identify regions with proven capacity for specific instance types.
Statistics are passively collected from actual launch attempts.`,
	RunE: runAvailability,
}

func init() {
	rootCmd.AddCommand(availabilityCmd)

	availabilityCmd.Flags().StringVar(&availabilityInstanceType, "instance-type", "", "Instance type to check (required)")
	availabilityCmd.Flags().StringVar(&availabilityRegions, "regions", "", "Comma-separated list of regions (default: common regions)")
	_ = availabilityCmd.MarkFlagRequired("instance-type")
}

func runAvailability(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Parse regions
	var regions []string
	if availabilityRegions != "" {
		regions = strings.Split(availabilityRegions, ",")
		for i := range regions {
			regions[i] = strings.TrimSpace(regions[i])
		}
	} else {
		// Default to common regions
		regions = []string{
			"us-east-1", "us-east-2", "us-west-1", "us-west-2",
			"eu-west-1", "eu-west-2", "eu-central-1",
			"ap-southeast-1", "ap-southeast-2", "ap-northeast-1",
		}
	}

	// Load AWS config (infra account for DynamoDB access)
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create DynamoDB client
	dynamoClient := dynamodb.NewFromConfig(cfg)

	// Fetch stats for all regions
	stats, err := availability.ListStatsByRegions(ctx, dynamoClient, regions, availabilityInstanceType)
	if err != nil {
		return fmt.Errorf("failed to fetch availability stats: %w", err)
	}

	// Display results
	fmt.Fprintf(os.Stderr, "\nAvailability Statistics for %s\n\n", availabilityInstanceType)

	w := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "Region\tSuccess Rate\tLast Success\tStatus")
	_, _ = fmt.Fprintln(w, "------\t------------\t------------\t------")

	for _, region := range regions {
		stat := stats[region]
		if stat == nil {
			continue
		}

		// Calculate success rate
		total := stat.SuccessCount + stat.FailureCount
		var successRate string
		if total == 0 {
			successRate = "No data"
		} else {
			rate := float64(stat.SuccessCount) / float64(total) * 100
			successRate = fmt.Sprintf("%.1f%% (%d/%d)", rate, stat.SuccessCount, total)
		}

		// Format last success
		var lastSuccess string
		if stat.LastSuccess == "" {
			lastSuccess = "Never"
		} else {
			ts, err := time.Parse(time.RFC3339, stat.LastSuccess)
			if err == nil {
				lastSuccess = ts.Format("2006-01-02 15:04")
			} else {
				lastSuccess = "Unknown"
			}
		}

		// Determine status
		status := getAvailabilityStatus(stat)

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", region, successRate, lastSuccess, status)
	}
	_ = w.Flush()

	fmt.Fprintln(os.Stderr)
	return nil
}

func getAvailabilityStatus(stat *availability.AvailabilityStats) string {
	// Check if in backoff
	if stat.BackoffUntil != "" {
		backoffUntil, err := time.Parse(time.RFC3339, stat.BackoffUntil)
		if err == nil && time.Now().Before(backoffUntil) {
			remaining := time.Until(backoffUntil).Round(time.Minute)
			return fmt.Sprintf("⏸️  Backoff (%s)", remaining)
		}
	}

	// Calculate success rate
	total := stat.SuccessCount + stat.FailureCount
	if total == 0 {
		return "❓ Unknown"
	}

	successRate := float64(stat.SuccessCount) / float64(total)
	switch {
	case successRate >= 0.9:
		return "✅ Available"
	case successRate >= 0.7:
		return "⚠️  Limited"
	default:
		return "❌ Constrained"
	}
}
