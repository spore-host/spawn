package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/alerts"
)

var (
	alertEmail          string
	alertSlack          string
	alertSNS            string
	alertWebhook        string
	alertOnComplete     bool
	alertOnFailure      bool
	alertCostThreshold  float64
	alertLongRunning    int
	alertInstanceFailed bool
	alertSweepID        string
	alertScheduleID     string
	alertDeleteYes      bool
)

var alertsCmd = &cobra.Command{
	Use:   "alerts",
	Short: "Manage alerts for sweeps and schedules",
	Long: `Create, list, and manage alert notifications for parameter sweeps and schedules.

Get notified via email, Slack, SNS, or webhooks when sweeps complete, fail,
exceed cost thresholds, or encounter issues.

Examples:
  # Create alert for sweep completion
  spawn alerts create <sweep-id> --on-complete --email user@example.com

  # Create alert for failures with Slack
  spawn alerts create <sweep-id> --on-failure --slack https://hooks.slack.com/...

  # Create cost threshold alert
  spawn alerts create <sweep-id> --cost-threshold 100 --email user@example.com

  # List all alerts
  spawn alerts list

  # Delete alert
  spawn alerts delete <alert-id>
`,
}

var alertsCreateCmd = &cobra.Command{
	Use:   "create <sweep-id>",
	Short: "Create a new alert",
	Long: `Create a new alert for a parameter sweep or schedule.

At least one trigger (--on-complete, --on-failure, etc.) and one destination
(--email, --slack, --sns, --webhook) must be specified.

Examples:
  # Sweep completion via email
  spawn alerts create sweep-123 --on-complete --email user@example.com

  # Multiple triggers and destinations
  spawn alerts create sweep-123 \\
    --on-complete \\
    --on-failure \\
    --email user@example.com \\
    --slack https://hooks.slack.com/services/...

  # Cost threshold alert
  spawn alerts create sweep-123 \\
    --cost-threshold 100 \\
    --email finance@example.com

  # Long-running sweep alert (trigger after 2 hours)
  spawn alerts create sweep-123 \\
    --long-running 120 \\
    --email user@example.com

  # Schedule execution failure alert
  spawn alerts create --schedule-id sched-123 \\
    --on-failure \\
    --slack https://hooks.slack.com/...
`,
	Args: cobra.MaximumNArgs(1),
	RunE: runAlertsCreate,
}

var alertsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all alerts",
	Long: `List all alert configurations for the current user.

Optionally filter by sweep ID.

Examples:
  # List all alerts
  spawn alerts list

  # List alerts for specific sweep
  spawn alerts list --sweep-id sweep-123
`,
	RunE: runAlertsList,
}

var alertsDeleteCmd = &cobra.Command{
	Use:   "delete <alert-id>",
	Short: "Delete an alert",
	Long: `Delete an alert configuration.

Example:
  spawn alerts delete alert-abc123
`,
	Args: cobra.ExactArgs(1),
	RunE: runAlertsDelete,
}

var alertsHistoryCmd = &cobra.Command{
	Use:   "history <alert-id>",
	Short: "Show alert history",
	Long: `Show the history of notifications sent for an alert.

Example:
  spawn alerts history alert-abc123
`,
	Args: cobra.ExactArgs(1),
	RunE: runAlertsHistory,
}

func init() {
	rootCmd.AddCommand(alertsCmd)

	alertsCmd.AddCommand(alertsCreateCmd)
	alertsCmd.AddCommand(alertsListCmd)
	alertsCmd.AddCommand(alertsDeleteCmd)
	alertsDeleteCmd.Flags().BoolVarP(&alertDeleteYes, "yes", "y", false, "Skip the confirmation prompt")
	alertsCmd.AddCommand(alertsHistoryCmd)

	// Create flags
	alertsCreateCmd.Flags().StringVar(&alertEmail, "email", "", "Email address for notifications")
	alertsCreateCmd.Flags().StringVar(&alertSlack, "slack", "", "Slack webhook URL for notifications")
	alertsCreateCmd.Flags().StringVar(&alertSNS, "sns", "", "SNS topic ARN for notifications")
	alertsCreateCmd.Flags().StringVar(&alertWebhook, "webhook", "", "Webhook URL for notifications")
	alertsCreateCmd.Flags().BoolVar(&alertOnComplete, "on-complete", false, "Alert when sweep/schedule completes")
	alertsCreateCmd.Flags().BoolVar(&alertOnFailure, "on-failure", false, "Alert when sweep/schedule fails")
	alertsCreateCmd.Flags().Float64Var(&alertCostThreshold, "cost-threshold", 0, "Alert when cost exceeds threshold (dollars)")
	alertsCreateCmd.Flags().IntVar(&alertLongRunning, "long-running", 0, "Alert when sweep runs longer than N minutes")
	alertsCreateCmd.Flags().BoolVar(&alertInstanceFailed, "instance-failed", false, "Alert when any instance fails")
	alertsCreateCmd.Flags().StringVar(&alertScheduleID, "schedule-id", "", "Schedule ID (alternative to sweep-id)")

	// List flags
	alertsListCmd.Flags().StringVar(&alertSweepID, "sweep-id", "", "Filter by sweep ID")
}

func runAlertsCreate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Get sweep ID from args or flag
	var sweepID string
	if len(args) > 0 {
		sweepID = args[0]
	}

	// Validate inputs
	if sweepID == "" && alertScheduleID == "" {
		return fmt.Errorf("either sweep-id or --schedule-id is required")
	}

	if sweepID != "" && alertScheduleID != "" {
		return fmt.Errorf("cannot specify both sweep-id and --schedule-id")
	}

	// Collect triggers
	var triggers []alerts.TriggerType
	if alertOnComplete {
		triggers = append(triggers, alerts.TriggerComplete)
	}
	if alertOnFailure {
		triggers = append(triggers, alerts.TriggerFailure)
	}
	if alertCostThreshold > 0 {
		triggers = append(triggers, alerts.TriggerCostThreshold)
	}
	if alertLongRunning > 0 {
		triggers = append(triggers, alerts.TriggerLongRunning)
	}
	if alertInstanceFailed {
		triggers = append(triggers, alerts.TriggerInstanceFailed)
	}

	if len(triggers) == 0 {
		return fmt.Errorf("at least one trigger is required (--on-complete, --on-failure, --cost-threshold, --long-running, --instance-failed)")
	}

	// Collect destinations
	var destinations []alerts.Destination
	if alertEmail != "" {
		destinations = append(destinations, alerts.Destination{
			Type:   alerts.DestinationEmail,
			Target: alertEmail,
		})
	}
	if alertSlack != "" {
		destinations = append(destinations, alerts.Destination{
			Type:   alerts.DestinationSlack,
			Target: alertSlack,
		})
	}
	if alertSNS != "" {
		destinations = append(destinations, alerts.Destination{
			Type:   alerts.DestinationSNS,
			Target: alertSNS,
		})
	}
	if alertWebhook != "" {
		destinations = append(destinations, alerts.Destination{
			Type:   alerts.DestinationWebhook,
			Target: alertWebhook,
		})
	}

	if len(destinations) == 0 {
		return fmt.Errorf("at least one destination is required (--email, --slack, --sns, --webhook)")
	}

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Get user ID (AWS account ID)
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("get caller identity: %w", err)
	}
	userID := *identity.Account

	// Create alert config
	alertConfig := &alerts.AlertConfig{
		SweepID:         sweepID,
		ScheduleID:      alertScheduleID,
		UserID:          userID,
		Triggers:        triggers,
		Destinations:    destinations,
		CostThreshold:   alertCostThreshold,
		DurationMinutes: alertLongRunning,
	}

	// Create alerts client
	dbClient := dynamodb.NewFromConfig(cfg)
	alertsClient := alerts.NewClient(dbClient)

	// Create alert
	if err := alertsClient.CreateAlert(ctx, alertConfig); err != nil {
		return fmt.Errorf("create alert: %w", err)
	}

	// Print summary
	fmt.Printf("Alert created: %s\n\n", alertConfig.AlertID)
	if sweepID != "" {
		fmt.Printf("Sweep ID:    %s\n", sweepID)
	}
	if alertScheduleID != "" {
		fmt.Printf("Schedule ID: %s\n", alertScheduleID)
	}
	fmt.Printf("Triggers:    %s\n", formatTriggers(triggers))
	fmt.Printf("Destinations: %s\n", formatDestinations(destinations))
	if alertCostThreshold > 0 {
		fmt.Printf("Cost Threshold: $%.2f\n", alertCostThreshold)
	}
	if alertLongRunning > 0 {
		fmt.Printf("Duration Threshold: %d minutes\n", alertLongRunning)
	}

	return nil
}

func runAlertsList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Get user ID
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("get caller identity: %w", err)
	}
	userID := *identity.Account

	// Create alerts client
	dbClient := dynamodb.NewFromConfig(cfg)
	alertsClient := alerts.NewClient(dbClient)

	// List alerts
	alertsList, err := alertsClient.ListAlerts(ctx, userID, alertSweepID)
	if err != nil {
		return fmt.Errorf("list alerts: %w", err)
	}

	if len(alertsList) == 0 {
		fmt.Println("No alerts found.")
		return nil
	}

	// Print table
	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "ALERT ID\tSWEEP/SCHEDULE\tTRIGGERS\tDESTINATIONS\tCREATED")
	_, _ = fmt.Fprintln(w, strings.Repeat("─", 100))

	for _, alert := range alertsList {
		target := alert.SweepID
		if target == "" {
			target = alert.ScheduleID
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			alert.AlertID,
			target,
			formatTriggers(alert.Triggers),
			formatDestinations(alert.Destinations),
			alert.CreatedAt.Format("2006-01-02 15:04"),
		)
	}

	return w.Flush()
}

func runAlertsDelete(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	alertID := args[0]

	if !confirmYes(alertDeleteYes, fmt.Sprintf("Delete alert %s?", alertID)) {
		fmt.Println("Aborted.")
		return nil
	}

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Create alerts client
	dbClient := dynamodb.NewFromConfig(cfg)
	alertsClient := alerts.NewClient(dbClient)

	// Delete alert
	if err := alertsClient.DeleteAlert(ctx, alertID); err != nil {
		return fmt.Errorf("delete alert: %w", err)
	}

	fmt.Printf("Alert deleted: %s\n", alertID)

	return nil
}

func runAlertsHistory(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	alertID := args[0]

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	// Create alerts client
	dbClient := dynamodb.NewFromConfig(cfg)
	alertsClient := alerts.NewClient(dbClient)

	// Get alert history
	history, err := alertsClient.ListAlertHistory(ctx, alertID)
	if err != nil {
		return fmt.Errorf("list alert history: %w", err)
	}

	if len(history) == 0 {
		fmt.Println("No alert history found.")
		return nil
	}

	// Print table
	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "TIMESTAMP\tTRIGGER\tSUCCESS\tMESSAGE")
	_, _ = fmt.Fprintln(w, strings.Repeat("─", 100))

	for _, h := range history {
		success := "✓"
		if !h.Success {
			success = "✗"
		}

		message := h.Message
		if len(message) > 60 {
			message = message[:57] + "..."
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			h.Timestamp.Format("2006-01-02 15:04:05"),
			h.Trigger,
			success,
			message,
		)
	}

	return w.Flush()
}

func formatTriggers(triggers []alerts.TriggerType) string {
	var parts []string
	for _, t := range triggers {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ", ")
}

func formatDestinations(destinations []alerts.Destination) string {
	var parts []string
	for _, d := range destinations {
		parts = append(parts, string(d.Type))
	}
	return strings.Join(parts, ", ")
}
