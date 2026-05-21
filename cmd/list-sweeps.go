package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

var (
	listSweepsStatus string
	listSweepsLast   int
	listSweepsSince  string
	listSweepsRegion string
	listSweepsJSON   bool
)

var listSweepsCmd = &cobra.Command{
	Use:   "list-sweeps",
	Short: "List parameter sweeps",
	Long: `List parameter sweeps from DynamoDB orchestration table.

Shows recent sweeps with their status, progress, and creation time.

Examples:
  # List recent sweeps
  spawn list-sweeps

  # Filter by status
  spawn list-sweeps --status RUNNING

  # Show last 5 sweeps
  spawn list-sweeps --last 5

  # Show sweeps since a date
  spawn list-sweeps --since 2026-01-15

  # JSON output
  spawn list-sweeps --json
`,
	RunE: runListSweeps,
}

func init() {
	rootCmd.AddCommand(listSweepsCmd)

	listSweepsCmd.Flags().StringVar(&listSweepsStatus, "status", "", "Filter by status (RUNNING, COMPLETED, FAILED, CANCELLED)")
	listSweepsCmd.Flags().IntVar(&listSweepsLast, "last", 20, "Show last N sweeps")
	listSweepsCmd.Flags().StringVar(&listSweepsSince, "since", "", "Show sweeps created after date (YYYY-MM-DD)")
	listSweepsCmd.Flags().StringVar(&listSweepsRegion, "region", "", "Filter by region")
	listSweepsCmd.Flags().BoolVar(&listSweepsJSON, "json", false, "Output as JSON")
}

type SweepSummary struct {
	SweepID       string    `json:"sweep_id"`
	SweepName     string    `json:"sweep_name"`
	Status        string    `json:"status"`
	TotalParams   int       `json:"total_params"`
	Launched      int       `json:"launched"`
	Failed        int       `json:"failed"`
	Region        string    `json:"region"`
	CreatedAt     time.Time `json:"created_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	EstimatedCost float64   `json:"estimated_cost,omitempty"`
}

func runListSweeps(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Load AWS config for spore-host-infra (where DynamoDB lives)
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get current user identity
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get user identity: %w", err)
	}
	userID := *identity.Arn

	// Query DynamoDB for user's sweeps
	dynamodbClient := dynamodb.NewFromConfig(cfg)

	// Scan table (no GSI on user_id yet, so we scan and filter)
	// TODO: Add GSI on user_id for better performance
	scanInput := &dynamodb.ScanInput{
		TableName: aws.String("spawn-sweep-orchestration"),
	}

	result, err := dynamodbClient.Scan(ctx, scanInput)
	if err != nil {
		return fmt.Errorf("failed to query sweeps: %w", err)
	}

	// Unmarshal and filter sweeps
	var sweeps []SweepSummary
	for _, item := range result.Items {
		var sweep struct {
			SweepID       string  `dynamodbav:"sweep_id"`
			SweepName     string  `dynamodbav:"sweep_name"`
			UserID        string  `dynamodbav:"user_id"`
			Status        string  `dynamodbav:"status"`
			TotalParams   int     `dynamodbav:"total_params"`
			Launched      int     `dynamodbav:"launched"`
			Failed        int     `dynamodbav:"failed"`
			Region        string  `dynamodbav:"region"`
			CreatedAt     string  `dynamodbav:"created_at"`
			CompletedAt   string  `dynamodbav:"completed_at,omitempty"`
			EstimatedCost float64 `dynamodbav:"estimated_cost,omitempty"`
		}

		if err := attributevalue.UnmarshalMap(item, &sweep); err != nil {
			continue
		}

		// Filter by user
		if sweep.UserID != userID {
			continue
		}

		// Filter by status
		if listSweepsStatus != "" && sweep.Status != listSweepsStatus {
			continue
		}

		// Filter by region
		if listSweepsRegion != "" && sweep.Region != listSweepsRegion {
			continue
		}

		// Parse timestamps
		createdAt, _ := time.Parse(time.RFC3339, sweep.CreatedAt)
		completedAt, _ := time.Parse(time.RFC3339, sweep.CompletedAt)

		// Filter by since date
		if listSweepsSince != "" {
			sinceDate, err := time.Parse("2006-01-02", listSweepsSince)
			if err != nil {
				return fmt.Errorf("invalid --since date format (use YYYY-MM-DD): %w", err)
			}
			if createdAt.Before(sinceDate) {
				continue
			}
		}

		sweeps = append(sweeps, SweepSummary{
			SweepID:       sweep.SweepID,
			SweepName:     sweep.SweepName,
			Status:        sweep.Status,
			TotalParams:   sweep.TotalParams,
			Launched:      sweep.Launched,
			Failed:        sweep.Failed,
			Region:        sweep.Region,
			CreatedAt:     createdAt,
			CompletedAt:   completedAt,
			EstimatedCost: sweep.EstimatedCost,
		})
	}

	// Sort by creation time (newest first)
	sort.Slice(sweeps, func(i, j int) bool {
		return sweeps[i].CreatedAt.After(sweeps[j].CreatedAt)
	})

	// Limit results
	if len(sweeps) > listSweepsLast {
		sweeps = sweeps[:listSweepsLast]
	}

	// Output
	if listSweepsJSON {
		data, err := json.MarshalIndent(sweeps, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	// Table output
	if len(sweeps) == 0 {
		fmt.Fprintf(os.Stderr, "No sweeps found.\n")
		return nil
	}

	fmt.Fprintf(os.Stderr, "\n📊 Parameter Sweeps\n\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "SWEEP ID\tNAME\tSTATUS\tPROGRESS\tREGION\tCREATED\n")

	for _, sweep := range sweeps {
		// Status icon
		statusIcon := getStatusIcon(sweep.Status)

		// Progress
		progress := fmt.Sprintf("%d/%d", sweep.Launched, sweep.TotalParams)
		if sweep.Failed > 0 {
			progress = fmt.Sprintf("%d/%d (%d failed)", sweep.Launched, sweep.TotalParams, sweep.Failed)
		}

		// Created time (relative)
		createdStr := formatRelativeTime(sweep.CreatedAt)

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s %s\t%s\t%s\t%s\n",
			sweep.SweepID,
			sweep.SweepName,
			statusIcon,
			sweep.Status,
			progress,
			sweep.Region,
			createdStr,
		)
	}

	_ = w.Flush()

	fmt.Fprintf(os.Stderr, "\nTotal: %d sweep(s)\n", len(sweeps))
	fmt.Fprintf(os.Stderr, "\nTip: Use 'spawn status --sweep-id <id>' for details\n\n")

	return nil
}

func getStatusIcon(status string) string {
	switch strings.ToUpper(status) {
	case "RUNNING":
		return "🚀"
	case "COMPLETED":
		return "✅"
	case "FAILED":
		return "❌"
	case "CANCELLED":
		return "⚠️"
	case "INITIALIZING":
		return "🔄"
	default:
		return "❓"
	}
}

func formatRelativeTime(t time.Time) string {
	now := time.Now()
	diff := now.Sub(t)

	if diff < time.Minute {
		return "just now"
	} else if diff < time.Hour {
		minutes := int(diff.Minutes())
		return fmt.Sprintf("%dm ago", minutes)
	} else if diff < 24*time.Hour {
		hours := int(diff.Hours())
		return fmt.Sprintf("%dh ago", hours)
	} else if diff < 7*24*time.Hour {
		days := int(diff.Hours() / 24)
		return fmt.Sprintf("%dd ago", days)
	} else {
		return t.Format("2006-01-02")
	}
}
