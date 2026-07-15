package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var autoscaleCmd = &cobra.Command{
	Use:   "autoscale",
	Short: "Manage auto-scaling job arrays",
	Long:  "Launch and manage auto-scaling job arrays that maintain target capacity",
}

var (
	autoscaleTerminateYes      bool
	autoscaleRemoveScheduleYes bool

	// Launch flags
	autoscaleName           string
	autoscaleJobArrayID     string
	autoscaleDesired        int
	autoscaleMin            int
	autoscaleMax            int
	autoscaleInstanceType   string
	autoscaleAMI            string
	autoscaleSpot           bool
	autoscaleKeyName        string
	autoscaleSubnetID       string
	autoscaleSecurityGroups []string
	autoscaleIAMProfile     string
	autoscaleUserData       string
	autoscaleTags           map[string]string // deprecated --tags (key=value map)
	autoscaleTagList        []string          // canonical --tag (repeatable key=value)

	// Update flags
	autoscaleNewDesired int
	autoscaleNewMin     int
	autoscaleNewMax     int

	// Scaling policy flags
	scalingPolicy             string
	queueURL                  string    // Single queue (backward compat)
	queueURLs                 []string  // Multi-queue support
	queueWeights              []float64 // Weights for multi-queue
	targetMessagesPerInstance int
	scaleUpCooldown           int
	scaleDownCooldown         int
	removePolicyFlag          bool

	// Metric policy flags
	metricPolicy    string
	metricName      string
	metricNamespace string
	metricStatistic string
	targetValue     float64
	periodSeconds   int

	// Schedule flags
	autoscaleScheduleName       string
	autoscaleScheduleExpression string
	autoscaleScheduleDesired    int
	autoscaleScheduleMin        int
	autoscaleScheduleMax        int
	autoscaleScheduleTimezone   string
	autoscaleScheduleEnabled    bool

	// Global flags
	autoscaleTableName string
	autoscaleEnv       string
)

var autoscaleLaunchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Launch an auto-scaling job array",
	Long:  "Launch a new auto-scaling job array with specified capacity and launch template",
	RunE:  runAutoscaleLaunch,
}

var autoscaleUpdateCmd = &cobra.Command{
	Use:   "update <group-name>",
	Short: "Update auto-scaling group capacity",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleUpdate,
}

var autoscaleStatusCmd = &cobra.Command{
	Use:   "status [group-name]",
	Short: "Show auto-scaling group status",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runAutoscaleStatus,
}

var autoscaleListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all active auto-scaling groups",
	Args:    cobra.NoArgs,
	RunE:    runAutoscaleList,
}

var autoscaleHealthCmd = &cobra.Command{
	Use:   "health <group-name>",
	Short: "Show instance health for auto-scaling group",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleHealth,
}

var autoscalePauseCmd = &cobra.Command{
	Use:   "pause <group-name>",
	Short: "Pause auto-scaling (stop reconciliation)",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscalePause,
}

var autoscaleResumeCmd = &cobra.Command{
	Use:   "resume <group-name>",
	Short: "Resume auto-scaling",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleResume,
}

var autoscaleTerminateCmd = &cobra.Command{
	Use:   "terminate <group-name>",
	Short: "Terminate auto-scaling group and all instances",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleTerminate,
}

var autoscaleSetPolicyCmd = &cobra.Command{
	Use: "set-scaling-policy <group-name>",
	// Renamed from `set-policy` to pair symmetrically with `set-metric-policy`
	// (and the `scaling-activity`/`metric-activity` pair); `set-policy` kept as a
	// deprecated alias (#307).
	Aliases: []string{"set-policy"},
	Short:   "Set or update scaling policy for an autoscale group",
	Args:    cobra.ExactArgs(1),
	RunE:    runAutoscaleSetPolicy,
}

var autoscaleScalingActivityCmd = &cobra.Command{
	Use:   "scaling-activity <group-name>",
	Short: "Show recent scaling activity for an autoscale group",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleScalingActivity,
}

var autoscaleSetMetricPolicyCmd = &cobra.Command{
	Use:   "set-metric-policy <group-name>",
	Short: "Set or update metric-based scaling policy",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleSetMetricPolicy,
}

var autoscaleMetricActivityCmd = &cobra.Command{
	Use:   "metric-activity <group-name>",
	Short: "Show recent metric-based scaling activity",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleMetricActivity,
}

// autoscaleScheduleCmd groups the scheduled-action verbs under
// `autoscale schedule` (#306).
var autoscaleScheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Manage scheduled scaling actions for an autoscale group",
}

var autoscaleAddScheduleCmd = &cobra.Command{
	Use:   "add <group-name>",
	Short: "Add a scheduled action to an autoscale group",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleAddSchedule,
}

var autoscaleRemoveScheduleCmd = &cobra.Command{
	Use:   "remove <group-name> <schedule-name>",
	Short: "Remove a scheduled action from an autoscale group",
	Args:  cobra.ExactArgs(2),
	RunE:  runAutoscaleRemoveSchedule,
}

var autoscaleListSchedulesCmd = &cobra.Command{
	Use:   "list <group-name>",
	Short: "List all scheduled actions for an autoscale group",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleListSchedules,
}

func init() {
	rootCmd.AddCommand(autoscaleCmd)
	autoscaleCmd.AddCommand(autoscaleLaunchCmd)
	autoscaleCmd.AddCommand(autoscaleUpdateCmd)
	autoscaleCmd.AddCommand(autoscaleStatusCmd)
	autoscaleCmd.AddCommand(autoscaleListCmd)
	autoscaleCmd.AddCommand(autoscaleHealthCmd)
	autoscaleCmd.AddCommand(autoscalePauseCmd)
	autoscaleCmd.AddCommand(autoscaleResumeCmd)
	autoscaleCmd.AddCommand(autoscaleTerminateCmd)
	autoscaleTerminateCmd.Flags().BoolVarP(&autoscaleTerminateYes, "yes", "y", false, "Skip the confirmation prompt")
	autoscaleCmd.AddCommand(autoscaleSetPolicyCmd)
	autoscaleCmd.AddCommand(autoscaleScalingActivityCmd)
	autoscaleCmd.AddCommand(autoscaleSetMetricPolicyCmd)
	autoscaleCmd.AddCommand(autoscaleMetricActivityCmd)
	// Scheduled-action verbs now live under `autoscale schedule <verb>` (#306).
	autoscaleCmd.AddCommand(autoscaleScheduleCmd)
	autoscaleScheduleCmd.AddCommand(autoscaleAddScheduleCmd)
	autoscaleScheduleCmd.AddCommand(autoscaleRemoveScheduleCmd)
	autoscaleRemoveScheduleCmd.Flags().BoolVarP(&autoscaleRemoveScheduleYes, "yes", "y", false, "Skip the confirmation prompt")
	autoscaleScheduleCmd.AddCommand(autoscaleListSchedulesCmd)

	// Global flags
	autoscaleCmd.PersistentFlags().StringVar(&autoscaleTableName, "table", "spawn-autoscale-groups", "DynamoDB table name")
	autoscaleCmd.PersistentFlags().StringVar(&autoscaleEnv, "env", "production", "Environment (production or staging)")

	// Launch flags
	autoscaleLaunchCmd.Flags().StringVar(&autoscaleName, "name", "", "Group name (required)")
	autoscaleLaunchCmd.Flags().StringVar(&autoscaleJobArrayID, "job-array-id", "", "Job array ID (auto-generated if not specified)")
	autoscaleLaunchCmd.Flags().IntVar(&autoscaleDesired, "desired-capacity", 0, "Desired instance count (required)")
	autoscaleLaunchCmd.Flags().IntVar(&autoscaleMin, "min-capacity", 0, "Minimum instance count (default: 0)")
	autoscaleLaunchCmd.Flags().IntVar(&autoscaleMax, "max-capacity", 0, "Maximum instance count (default: desired * 2)")
	autoscaleLaunchCmd.Flags().StringVar(&autoscaleInstanceType, "instance-type", "", "EC2 instance type (required)")
	autoscaleLaunchCmd.Flags().StringVar(&autoscaleAMI, "ami", "", "AMI ID (required)")
	autoscaleLaunchCmd.Flags().BoolVar(&autoscaleSpot, "spot", false, "Use spot instances")
	autoscaleLaunchCmd.Flags().StringVar(&autoscaleKeyName, "key-name", "", "SSH key name")
	autoscaleLaunchCmd.Flags().StringVar(&autoscaleSubnetID, "subnet-id", "", "Subnet ID")
	autoscaleLaunchCmd.Flags().StringSliceVar(&autoscaleSecurityGroups, "security-group-ids", nil, "Security group IDs (comma-separated or repeated)")
	// Deprecated alias for --security-group-ids (bound to the same var).
	autoscaleLaunchCmd.Flags().StringSliceVar(&autoscaleSecurityGroups, "security-groups", nil, "Security group IDs")
	_ = autoscaleLaunchCmd.Flags().MarkDeprecated("security-groups", "use --security-group-ids instead")
	autoscaleLaunchCmd.Flags().StringVar(&autoscaleIAMProfile, "iam-profile", "", "IAM instance profile")
	autoscaleLaunchCmd.Flags().StringVar(&autoscaleUserData, "user-data", "", "User data script (base64 encoded)")
	autoscaleLaunchCmd.Flags().StringArrayVar(&autoscaleTagList, "tag", nil, "Additional tag key=value (repeatable)")
	// Deprecated alias for --tag (key=value map form).
	autoscaleLaunchCmd.Flags().StringToStringVar(&autoscaleTags, "tags", nil, "Additional tags (key=value)")
	_ = autoscaleLaunchCmd.Flags().MarkDeprecated("tags", "use --tag key=value (repeatable) instead")

	autoscaleLaunchCmd.Flags().StringVar(&scalingPolicy, "scaling-policy", "",
		"Scaling policy type: 'queue-depth' (empty = manual mode)")
	autoscaleLaunchCmd.Flags().StringVar(&queueURL, "queue-url", "",
		"SQS queue URL for queue-depth policy (required if --scaling-policy=queue-depth)")
	autoscaleLaunchCmd.Flags().IntVar(&targetMessagesPerInstance, "target-messages-per-instance", 10,
		"Target messages per instance for queue-depth scaling")
	autoscaleLaunchCmd.Flags().IntVar(&scaleUpCooldown, "scale-up-cooldown", 60,
		"Scale-up cooldown in seconds")
	autoscaleLaunchCmd.Flags().IntVar(&scaleDownCooldown, "scale-down-cooldown", 300,
		"Scale-down cooldown in seconds")
	autoscaleLaunchCmd.Flags().StringVar(&metricPolicy, "metric-policy", "",
		"Metric policy type: 'cpu', 'memory', or 'custom'")
	autoscaleLaunchCmd.Flags().Float64Var(&targetValue, "target-value", 0,
		"Target metric value (e.g., 70.0 for 70% CPU)")
	autoscaleLaunchCmd.Flags().StringVar(&metricName, "metric-name", "",
		"CloudWatch metric name (for custom metrics)")
	autoscaleLaunchCmd.Flags().StringVar(&metricNamespace, "metric-namespace", "",
		"CloudWatch namespace (for custom metrics)")
	autoscaleLaunchCmd.Flags().StringVar(&metricStatistic, "metric-statistic", "Average",
		"Metric statistic: Average, Maximum, or Minimum")
	autoscaleLaunchCmd.Flags().IntVar(&periodSeconds, "metric-period", 300,
		"Metric evaluation period in seconds")

	_ = autoscaleLaunchCmd.MarkFlagRequired("name")
	_ = autoscaleLaunchCmd.MarkFlagRequired("desired-capacity")
	_ = autoscaleLaunchCmd.MarkFlagRequired("instance-type")
	_ = autoscaleLaunchCmd.MarkFlagRequired("ami")

	// Update flags
	autoscaleUpdateCmd.Flags().IntVar(&autoscaleNewDesired, "desired-capacity", -1, "New desired capacity")
	autoscaleUpdateCmd.Flags().IntVar(&autoscaleNewMin, "min-capacity", -1, "New minimum capacity")
	autoscaleUpdateCmd.Flags().IntVar(&autoscaleNewMax, "max-capacity", -1, "New maximum capacity")

	// Set-policy flags
	autoscaleSetPolicyCmd.Flags().StringVar(&scalingPolicy, "scaling-policy", "",
		"Scaling policy type: 'queue-depth'")
	autoscaleSetPolicyCmd.Flags().StringVar(&queueURL, "queue-url", "",
		"SQS queue URL for single-queue policy (deprecated: use --queue)")
	autoscaleSetPolicyCmd.Flags().StringSliceVar(&queueURLs, "queue", []string{},
		"SQS queue URL (can be specified multiple times for multi-queue)")
	autoscaleSetPolicyCmd.Flags().Float64SliceVar(&queueWeights, "queue-weight", []float64{},
		"Queue weight 0.0-1.0 (must match number of --queue flags)")
	autoscaleSetPolicyCmd.Flags().IntVar(&targetMessagesPerInstance, "target-messages-per-instance", 10,
		"Target messages per instance for queue-depth scaling")
	autoscaleSetPolicyCmd.Flags().IntVar(&scaleUpCooldown, "scale-up-cooldown", 60,
		"Scale-up cooldown in seconds")
	autoscaleSetPolicyCmd.Flags().IntVar(&scaleDownCooldown, "scale-down-cooldown", 300,
		"Scale-down cooldown in seconds")
	autoscaleSetPolicyCmd.Flags().BoolVar(&removePolicyFlag, "none", false,
		"Remove scaling policy (revert to manual mode)")

	// Set-metric-policy flags
	autoscaleSetMetricPolicyCmd.Flags().StringVar(&metricPolicy, "metric-policy", "",
		"Metric policy type: 'cpu', 'memory', or 'custom'")
	autoscaleSetMetricPolicyCmd.Flags().Float64Var(&targetValue, "target-value", 0,
		"Target metric value (e.g., 70.0 for 70% CPU)")
	autoscaleSetMetricPolicyCmd.Flags().StringVar(&metricName, "metric-name", "",
		"CloudWatch metric name (for custom metrics)")
	autoscaleSetMetricPolicyCmd.Flags().StringVar(&metricNamespace, "metric-namespace", "",
		"CloudWatch namespace (for custom metrics)")
	autoscaleSetMetricPolicyCmd.Flags().StringVar(&metricStatistic, "metric-statistic", "Average",
		"Metric statistic: Average, Maximum, or Minimum")
	autoscaleSetMetricPolicyCmd.Flags().IntVar(&periodSeconds, "metric-period", 300,
		"Metric evaluation period in seconds")
	autoscaleSetMetricPolicyCmd.Flags().BoolVar(&removePolicyFlag, "none", false,
		"Remove metric policy")

	// add-schedule flags
	autoscaleAddScheduleCmd.Flags().StringVar(&autoscaleScheduleName, "name", "",
		"Schedule name (required)")
	autoscaleAddScheduleCmd.Flags().StringVar(&autoscaleScheduleExpression, "schedule", "",
		"Cron expression: 'second minute hour day month weekday' (required)")
	autoscaleAddScheduleCmd.Flags().IntVar(&autoscaleScheduleDesired, "desired-capacity", 0,
		"Desired capacity (required)")
	autoscaleAddScheduleCmd.Flags().IntVar(&autoscaleScheduleMin, "min-capacity", 0,
		"Minimum capacity override (optional)")
	autoscaleAddScheduleCmd.Flags().IntVar(&autoscaleScheduleMax, "max-capacity", 0,
		"Maximum capacity override (optional)")
	autoscaleAddScheduleCmd.Flags().StringVar(&autoscaleScheduleTimezone, "timezone", "UTC",
		"Timezone (e.g., America/New_York)")
	autoscaleAddScheduleCmd.Flags().BoolVar(&autoscaleScheduleEnabled, "enabled", true,
		"Enable the schedule immediately")

	// Back-compat: these verbs used to be flat-hyphenated (`autoscale
	// add-schedule`, etc.). Keep hidden, deprecated shims that share the real
	// commands' RunE and flags so existing scripts keep working while
	// `autoscale schedule <verb>` is canonical (#306).
	registerScheduleAlias(autoscaleAddScheduleCmd, "add-schedule")
	registerScheduleAlias(autoscaleRemoveScheduleCmd, "remove-schedule")
	registerScheduleAlias(autoscaleListSchedulesCmd, "list-schedules")
}

// registerScheduleAlias adds a hidden, deprecated flat-name shim under
// `autoscale` that delegates to the canonical `autoscale schedule <verb>`
// command, sharing its flag set (same underlying package vars).
func registerScheduleAlias(target *cobra.Command, oldName string) {
	shim := &cobra.Command{
		Use:        oldName,
		Short:      target.Short,
		Hidden:     true,
		Deprecated: fmt.Sprintf("use `spawn autoscale schedule %s` instead", target.Name()),
		Args:       target.Args,
		RunE:       target.RunE,
	}
	shim.Flags().AddFlagSet(target.Flags())
	autoscaleCmd.AddCommand(shim)
}
