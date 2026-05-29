package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/scheduler"
	"github.com/spore-host/spawn/pkg/staging"
	"gopkg.in/yaml.v3"
)

var (
	scheduleAt       string
	scheduleCron     string
	scheduleTimezone string
	scheduleName     string
	scheduleMaxExec  int
	scheduleEndAfter string
	scheduleStatus   string
	scheduleRegion   string
)

var scheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Manage scheduled parameter sweeps",
	Long: `Create, list, and manage scheduled executions of parameter sweeps.

Schedules run parameter sweeps at specified times via EventBridge Scheduler.
No CLI running required - sweeps launch automatically at scheduled times.

Examples:
  # One-time execution
  spawn schedule create params.yaml --at "2026-01-23T02:00:00" --timezone "America/New_York"

  # Recurring daily at 2 AM
  spawn schedule create params.yaml --cron "0 2 * * *" --name "nightly-training"

  # Recurring with execution limit
  spawn schedule create params.yaml --cron "0 */4 * * *" --max-executions 100

  # List all schedules
  spawn schedule list

  # Cancel a schedule
  spawn schedule cancel <schedule-id>
`,
}

var scheduleCreateCmd = &cobra.Command{
	Use:   "create <params-file>",
	Short: "Create a new scheduled execution",
	Long: `Create a new scheduled execution of a parameter sweep.

Either --at (one-time) or --cron (recurring) is required.

Time formats:
  --at:       ISO 8601 format (2026-01-23T02:00:00)
  --cron:     Standard cron expression (minute hour day month weekday)

Examples:
  # One-time at specific time
  spawn schedule create params.yaml --at "2026-01-23T14:30:00"

  # Every day at 2 AM Eastern
  spawn schedule create params.yaml --cron "0 2 * * *" --timezone "America/New_York"

  # Every 6 hours for 30 days
  spawn schedule create params.yaml --cron "0 */6 * * *" --max-executions 120

  # Weekdays only at 9 AM
  spawn schedule create params.yaml --cron "0 9 * * 1-5"
`,
	Args: cobra.ExactArgs(1),
	RunE: runScheduleCreate,
}

var scheduleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List scheduled executions",
	Long: `List all scheduled executions for the current user.

Examples:
  # List all schedules
  spawn schedule list

  # List only active schedules
  spawn schedule list --status active
`,
	RunE: runScheduleList,
}

var scheduleDescribeCmd = &cobra.Command{
	Use:   "describe <schedule-id>",
	Short: "Show details about a schedule",
	Long: `Show detailed information about a scheduled execution including
configuration, execution history, and next run time.

Examples:
  spawn schedule describe sched-20260122-140530
`,
	Args: cobra.ExactArgs(1),
	RunE: runScheduleDescribe,
}

var scheduleCancelCmd = &cobra.Command{
	Use:   "cancel <schedule-id>",
	Short: "Cancel a scheduled execution",
	Long: `Cancel a scheduled execution. This will delete the EventBridge schedule
and update the DynamoDB record. No further executions will occur.

Examples:
  spawn schedule cancel sched-20260122-140530
`,
	Args: cobra.ExactArgs(1),
	RunE: runScheduleCancel,
}

var schedulePauseCmd = &cobra.Command{
	Use:   "pause <schedule-id>",
	Short: "Pause a scheduled execution",
	Long: `Pause a scheduled execution temporarily. The schedule remains but
executions are disabled. Use 'resume' to re-enable.

Examples:
  spawn schedule pause sched-20260122-140530
`,
	Args: cobra.ExactArgs(1),
	RunE: runSchedulePause,
}

var scheduleResumeCmd = &cobra.Command{
	Use:   "resume <schedule-id>",
	Short: "Resume a paused schedule",
	Long: `Resume a paused scheduled execution. Executions will continue
according to the schedule.

Examples:
  spawn schedule resume sched-20260122-140530
`,
	Args: cobra.ExactArgs(1),
	RunE: runScheduleResume,
}

func init() {
	// Create subcommand flags
	scheduleCreateCmd.Flags().StringVar(&scheduleAt, "at", "", "One-time execution time (ISO 8601 format)")
	scheduleCreateCmd.Flags().StringVar(&scheduleCron, "cron", "", "Cron expression for recurring execution")
	scheduleCreateCmd.Flags().StringVar(&scheduleTimezone, "timezone", "UTC", "IANA timezone (e.g., America/New_York)")
	scheduleCreateCmd.Flags().StringVar(&scheduleName, "name", "", "Friendly name for this schedule")
	scheduleCreateCmd.Flags().IntVar(&scheduleMaxExec, "max-executions", 0, "Maximum number of executions (0 = unlimited)")
	scheduleCreateCmd.Flags().StringVar(&scheduleEndAfter, "end-after", "", "Stop executing after this time (ISO 8601 format)")
	scheduleCreateCmd.Flags().StringVar(&scheduleRegion, "region", "us-east-1", "AWS region for sweep execution")

	// List subcommand flags
	scheduleListCmd.Flags().StringVar(&scheduleStatus, "status", "", "Filter by status (active|paused|cancelled)")

	// Add subcommands
	scheduleCmd.AddCommand(scheduleCreateCmd)
	scheduleCmd.AddCommand(scheduleListCmd)
	scheduleCmd.AddCommand(scheduleDescribeCmd)
	scheduleCmd.AddCommand(scheduleCancelCmd)
	scheduleCmd.AddCommand(schedulePauseCmd)
	scheduleCmd.AddCommand(scheduleResumeCmd)

	rootCmd.AddCommand(scheduleCmd)
}

func runScheduleCreate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	paramsFile := args[0]

	// Validate flags
	if scheduleAt == "" && scheduleCron == "" {
		return fmt.Errorf("must specify either --at or --cron")
	}
	if scheduleAt != "" && scheduleCron != "" {
		return fmt.Errorf("cannot specify both --at and --cron")
	}

	fmt.Fprintf(os.Stderr, "\n📅 Creating Scheduled Execution\n\n")

	// Load AWS config for spore-host-infra
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get account ID
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	accountID := *identity.Account

	// Parse parameter file
	params, err := parseParamsFile(paramsFile)
	if err != nil {
		return fmt.Errorf("failed to parse parameter file: %w", err)
	}

	// Generate schedule ID
	scheduleID := scheduler.GenerateScheduleID()

	// Upload parameter file to S3
	fmt.Fprintf(os.Stderr, "📤 Uploading parameter file...\n")
	stagingClient := staging.NewClient(cfg, accountID)
	_, s3Key, size, sha256, err := stagingClient.UploadScheduleParams(ctx, paramsFile, scheduleID, scheduleRegion)
	if err != nil {
		return fmt.Errorf("failed to upload parameter file: %w", err)
	}
	fmt.Fprintf(os.Stderr, "   Uploaded: %s (%.2f MB, SHA256: %s...)\n", s3Key, float64(size)/(1024*1024), sha256[:16])

	// Build schedule expression
	scheduleExpr := buildScheduleExpression()

	// Determine schedule type
	scheduleType := scheduler.ScheduleTypeOneTime
	if scheduleCron != "" {
		scheduleType = scheduler.ScheduleTypeRecurring
	}

	// Parse end_after if specified
	var endAfter time.Time
	if scheduleEndAfter != "" {
		endAfter, err = time.Parse(time.RFC3339, scheduleEndAfter)
		if err != nil {
			return fmt.Errorf("invalid end-after time format (use ISO 8601): %w", err)
		}
	}

	// Calculate next execution time
	nextExecTime := calculateNextExecution(scheduleExpr, scheduleTimezone)

	// Determine sweep name
	sweepName := scheduleName
	if sweepName == "" {
		sweepName = params.getSweepName()
	}

	// Create schedule record
	record := &scheduler.ScheduleRecord{
		ScheduleID:         scheduleID,
		UserID:             accountID,
		ScheduleName:       scheduleName,
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
		ScheduleExpression: scheduleExpr,
		ScheduleType:       string(scheduleType),
		Timezone:           scheduleTimezone,
		NextExecutionTime:  nextExecTime,
		S3ParamsKey:        s3Key,
		SweepName:          sweepName,
		MaxConcurrent:      params.getMaxConcurrent(),
		LaunchDelay:        params.getLaunchDelay(),
		Region:             scheduleRegion,
		Status:             string(scheduler.ScheduleStatusActive),
		ExecutionCount:     0,
		MaxExecutions:      scheduleMaxExec,
		EndAfter:           endAfter,
	}

	// Create EventBridge schedule
	fmt.Fprintf(os.Stderr, "\n⏰ Creating EventBridge schedule...\n")
	lambdaARN := fmt.Sprintf("arn:aws:lambda:%s:%s:function:scheduler-handler", scheduleRegion, accountID)
	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/SpawnSchedulerExecutionRole", accountID)

	schedulerClient := scheduler.NewClient(cfg, lambdaARN, roleARN, accountID)
	scheduleARN, err := schedulerClient.CreateSchedule(ctx, record)
	if err != nil {
		return fmt.Errorf("failed to create schedule: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n✅ Schedule created successfully!\n\n")
	fmt.Fprintf(os.Stderr, "   Schedule ID:     %s\n", scheduleID)
	fmt.Fprintf(os.Stderr, "   Name:            %s\n", scheduleName)
	fmt.Fprintf(os.Stderr, "   Type:            %s\n", scheduleType)
	fmt.Fprintf(os.Stderr, "   Expression:      %s\n", scheduleExpr)
	fmt.Fprintf(os.Stderr, "   Timezone:        %s\n", scheduleTimezone)
	fmt.Fprintf(os.Stderr, "   Next execution:  %s\n", nextExecTime.Format(time.RFC3339))
	if scheduleMaxExec > 0 {
		fmt.Fprintf(os.Stderr, "   Max executions:  %d\n", scheduleMaxExec)
	}
	if !endAfter.IsZero() {
		fmt.Fprintf(os.Stderr, "   End after:       %s\n", endAfter.Format(time.RFC3339))
	}
	fmt.Fprintf(os.Stderr, "   ARN:             %s\n", scheduleARN)
	fmt.Fprintf(os.Stderr, "\n")

	return nil
}

func runScheduleList(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	// Load AWS config for spore-host-infra
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get account ID
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	accountID := *identity.Account

	schedulerClient := scheduler.NewClient(cfg, "", "", accountID)
	schedules, err := schedulerClient.ListSchedulesByUser(ctx, accountID)
	if err != nil {
		return fmt.Errorf("failed to list schedules: %w", err)
	}

	// Filter by status if specified
	if scheduleStatus != "" {
		filtered := []scheduler.ScheduleRecord{}
		for _, s := range schedules {
			if s.Status == scheduleStatus {
				filtered = append(filtered, s)
			}
		}
		schedules = filtered
	}

	if len(schedules) == 0 {
		fmt.Fprintf(os.Stderr, "No schedules found.\n")
		return nil
	}

	fmt.Fprintf(os.Stderr, "\n📅 Scheduled Executions\n\n")
	fmt.Fprintf(os.Stderr, "%-25s %-20s %-12s %-12s %-20s\n",
		"SCHEDULE ID", "NAME", "STATUS", "TYPE", "NEXT EXECUTION")
	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 100))

	for _, s := range schedules {
		nextExec := s.NextExecutionTime.Format("2006-01-02 15:04 MST")
		name := s.ScheduleName
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(os.Stderr, "%-25s %-20s %-12s %-12s %-20s\n",
			s.ScheduleID, name, s.Status, s.ScheduleType, nextExec)
	}
	fmt.Fprintf(os.Stderr, "\n")

	return nil
}

func runScheduleDescribe(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	scheduleID := args[0]

	// Load AWS config for spore-host-infra
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get account ID
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	accountID := *identity.Account

	schedulerClient := scheduler.NewClient(cfg, "", "", accountID)

	// Get schedule details
	schedule, err := schedulerClient.GetSchedule(ctx, scheduleID)
	if err != nil {
		return fmt.Errorf("failed to get schedule: %w", err)
	}

	// Get execution history
	history, err := schedulerClient.GetExecutionHistory(ctx, scheduleID, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to get execution history: %v\n", err)
	}

	// Display schedule details
	fmt.Fprintf(os.Stderr, "\n📅 Schedule Details\n\n")
	fmt.Fprintf(os.Stderr, "Schedule ID:        %s\n", schedule.ScheduleID)
	fmt.Fprintf(os.Stderr, "Name:               %s\n", schedule.ScheduleName)
	fmt.Fprintf(os.Stderr, "Status:             %s\n", schedule.Status)
	fmt.Fprintf(os.Stderr, "Type:               %s\n", schedule.ScheduleType)
	fmt.Fprintf(os.Stderr, "Expression:         %s\n", schedule.ScheduleExpression)
	fmt.Fprintf(os.Stderr, "Timezone:           %s\n", schedule.Timezone)
	fmt.Fprintf(os.Stderr, "Next execution:     %s\n", schedule.NextExecutionTime.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "Created:            %s\n", schedule.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "Updated:            %s\n", schedule.UpdatedAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "\nSweep Configuration:\n")
	fmt.Fprintf(os.Stderr, "  Sweep name:       %s\n", schedule.SweepName)
	fmt.Fprintf(os.Stderr, "  Region:           %s\n", schedule.Region)
	fmt.Fprintf(os.Stderr, "  Max concurrent:   %d\n", schedule.MaxConcurrent)
	fmt.Fprintf(os.Stderr, "  Launch delay:     %s\n", schedule.LaunchDelay)
	fmt.Fprintf(os.Stderr, "\nExecution Stats:\n")
	fmt.Fprintf(os.Stderr, "  Total executions: %d\n", schedule.ExecutionCount)
	if schedule.MaxExecutions > 0 {
		fmt.Fprintf(os.Stderr, "  Max executions:   %d\n", schedule.MaxExecutions)
	}
	if !schedule.EndAfter.IsZero() {
		fmt.Fprintf(os.Stderr, "  End after:        %s\n", schedule.EndAfter.Format(time.RFC3339))
	}
	if schedule.LastSweepID != "" {
		fmt.Fprintf(os.Stderr, "  Last sweep ID:    %s\n", schedule.LastSweepID)
	}

	// Display execution history
	if len(history) > 0 {
		fmt.Fprintf(os.Stderr, "\nRecent Executions (last %d):\n", len(history))
		fmt.Fprintf(os.Stderr, "%-20s %-30s %-10s\n", "EXECUTION TIME", "SWEEP ID", "STATUS")
		fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 70))
		for _, h := range history {
			fmt.Fprintf(os.Stderr, "%-20s %-30s %-10s\n",
				h.ExecutionTime.Format("2006-01-02 15:04:05"),
				h.SweepID,
				h.Status)
			if h.ErrorMessage != "" {
				fmt.Fprintf(os.Stderr, "  Error: %s\n", h.ErrorMessage)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "\n")
	return nil
}

func runScheduleCancel(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	scheduleID := args[0]

	fmt.Fprintf(os.Stderr, "\n🛑 Cancelling Schedule\n")
	fmt.Fprintf(os.Stderr, "   Schedule ID: %s\n\n", scheduleID)

	// Load AWS config for spore-host-infra
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get account ID
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	accountID := *identity.Account

	schedulerClient := scheduler.NewClient(cfg, "", "", accountID)

	// Delete schedule
	if err := schedulerClient.DeleteSchedule(ctx, scheduleID); err != nil {
		return fmt.Errorf("failed to delete schedule: %w", err)
	}

	fmt.Fprintf(os.Stderr, "✅ Schedule cancelled successfully!\n\n")
	return nil
}

func runSchedulePause(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	scheduleID := args[0]

	return updateScheduleStatusCmd(ctx, scheduleID, scheduler.ScheduleStatusPaused)
}

func runScheduleResume(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	scheduleID := args[0]

	return updateScheduleStatusCmd(ctx, scheduleID, scheduler.ScheduleStatusActive)
}

func updateScheduleStatusCmd(ctx context.Context, scheduleID string, status scheduler.ScheduleStatus) error {
	action := "Pausing"
	if status == scheduler.ScheduleStatusActive {
		action = "Resuming"
	}

	fmt.Fprintf(os.Stderr, "\n⏸️  %s Schedule\n", action)
	fmt.Fprintf(os.Stderr, "   Schedule ID: %s\n\n", scheduleID)

	// Load AWS config for spore-host-infra
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get account ID
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	accountID := *identity.Account

	schedulerClient := scheduler.NewClient(cfg, "", "", accountID)

	// Update schedule status
	if err := schedulerClient.UpdateScheduleStatus(ctx, scheduleID, status); err != nil {
		return fmt.Errorf("failed to update schedule status: %w", err)
	}

	fmt.Fprintf(os.Stderr, "✅ Schedule %s successfully!\n\n", strings.ToLower(string(status)))
	return nil
}

// Helper functions

type parsedParams struct {
	defaults map[string]interface{}
	params   []map[string]interface{}
}

func parseParamsFile(filename string) (*parsedParams, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	var result struct {
		Defaults map[string]interface{}   `yaml:"defaults"`
		Params   []map[string]interface{} `yaml:"params"`
	}

	if err := yaml.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	return &parsedParams{
		defaults: result.Defaults,
		params:   result.Params,
	}, nil
}

func (p *parsedParams) getSweepName() string {
	if name, ok := p.defaults["sweep_name"].(string); ok {
		return name
	}
	return "sweep"
}

func (p *parsedParams) getMaxConcurrent() int {
	if max, ok := p.defaults["max_concurrent"].(int); ok {
		return max
	}
	return 10
}

func (p *parsedParams) getLaunchDelay() string {
	if delay, ok := p.defaults["launch_delay"].(string); ok {
		return delay
	}
	return "10s"
}

func buildScheduleExpression() string {
	if scheduleAt != "" {
		// One-time execution: at(YYYY-MM-DDTHH:mm:ss)
		return fmt.Sprintf("at(%s)", scheduleAt)
	}
	// Recurring execution: cron expression
	return fmt.Sprintf("cron(%s)", scheduleCron)
}

func calculateNextExecution(expr, tz string) time.Time {
	// For one-time schedules, parse the at() expression
	if strings.HasPrefix(expr, "at(") {
		timeStr := strings.TrimPrefix(strings.TrimSuffix(expr, ")"), "at(")
		t, err := time.Parse(time.RFC3339, timeStr)
		if err == nil {
			return t
		}
	}

	// For recurring schedules, return current time (Lambda will calculate next)
	return time.Now()
}
