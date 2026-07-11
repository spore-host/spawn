package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdaTypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/autoscaler"
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
	Use:   "set-policy <group-name>",
	Short: "Set or update scaling policy for an autoscale group",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleSetPolicy,
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

var autoscaleAddScheduleCmd = &cobra.Command{
	Use:   "add-schedule <group-name>",
	Short: "Add a scheduled action to an autoscale group",
	Args:  cobra.ExactArgs(1),
	RunE:  runAutoscaleAddSchedule,
}

var autoscaleRemoveScheduleCmd = &cobra.Command{
	Use:   "remove-schedule <group-name> <schedule-name>",
	Short: "Remove a scheduled action from an autoscale group",
	Args:  cobra.ExactArgs(2),
	RunE:  runAutoscaleRemoveSchedule,
}

var autoscaleListSchedulesCmd = &cobra.Command{
	Use:   "list-schedules <group-name>",
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
	autoscaleCmd.AddCommand(autoscaleAddScheduleCmd)
	autoscaleCmd.AddCommand(autoscaleRemoveScheduleCmd)
	autoscaleRemoveScheduleCmd.Flags().BoolVarP(&autoscaleRemoveScheduleYes, "yes", "y", false, "Skip the confirmation prompt")
	autoscaleCmd.AddCommand(autoscaleListSchedulesCmd)

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
}

func getAutoscaler(ctx context.Context) (*autoscaler.AutoScaler, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	// Build table name with environment suffix
	tableName := fmt.Sprintf("%s-%s", autoscaleTableName, autoscaleEnv)

	ec2Client := ec2.NewFromConfig(cfg)
	dynamoClient := dynamodb.NewFromConfig(cfg)
	sqsClient := sqs.NewFromConfig(cfg)
	cloudwatchClient := cloudwatch.NewFromConfig(cfg)

	return autoscaler.NewAutoScaler(&autoscaler.Config{
		EC2Client:        ec2Client,
		DynamoClient:     dynamoClient,
		SQSClient:        sqsClient,
		CloudWatchClient: cloudwatchClient,
		TableName:        tableName,
		RegistryTable:    "spawn-hybrid-registry",
	}), nil
}

func runAutoscaleLaunch(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Validate inputs
	if autoscaleDesired < 1 {
		return fmt.Errorf("desired-capacity must be at least 1")
	}

	// Validate scaling policy flags
	if scalingPolicy != "" {
		if scalingPolicy != "queue-depth" {
			return fmt.Errorf("invalid scaling policy: %s (only 'queue-depth' supported)", scalingPolicy)
		}
		if queueURL == "" {
			return fmt.Errorf("--queue-url required when --scaling-policy is set")
		}
	}

	// Set defaults
	if autoscaleMin < 0 {
		autoscaleMin = 0
	}
	if autoscaleMax <= 0 {
		autoscaleMax = autoscaleDesired * 2
	}
	if autoscaleJobArrayID == "" {
		autoscaleJobArrayID = fmt.Sprintf("%s-%d", autoscaleName, time.Now().Unix())
	}

	// Validate capacity ranges
	if autoscaleMin > autoscaleDesired {
		return fmt.Errorf("min-capacity cannot exceed desired-capacity")
	}
	if autoscaleMax < autoscaleDesired {
		return fmt.Errorf("max-capacity cannot be less than desired-capacity")
	}

	// Decode user data if provided
	userData := autoscaleUserData
	if userData != "" {
		if decoded, err := base64.StdEncoding.DecodeString(userData); err == nil {
			userData = string(decoded)
		}
	}

	// Merge tags from the canonical --tag (repeatable key=value) and the
	// deprecated --tags (key=value map). --tag wins on conflict.
	tags, err := parseKVTags(autoscaleTagList)
	if err != nil {
		return err
	}
	for k, v := range autoscaleTags {
		if _, ok := tags[k]; !ok {
			tags[k] = v
		}
	}

	// Create autoscaler
	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// Create group
	groupID := fmt.Sprintf("asg-%s-%d", autoscaleName, time.Now().Unix())
	group := &autoscaler.AutoScaleGroup{
		AutoScaleGroupID: groupID,
		GroupName:        autoscaleName,
		JobArrayID:       autoscaleJobArrayID,
		DesiredCapacity:  autoscaleDesired,
		MinCapacity:      autoscaleMin,
		MaxCapacity:      autoscaleMax,
		Status:           "active",
		LaunchTemplate: autoscaler.LaunchTemplate{
			InstanceType:       autoscaleInstanceType,
			AMI:                autoscaleAMI,
			Spot:               autoscaleSpot,
			KeyName:            autoscaleKeyName,
			SubnetID:           autoscaleSubnetID,
			SecurityGroups:     autoscaleSecurityGroups,
			IAMInstanceProfile: autoscaleIAMProfile,
			UserData:           userData,
			Tags:               tags,
		},
		HealthCheckInterval: 60 * time.Second,
		ReplacementStrategy: "immediate",
	}

	// Add scaling policy if specified
	if scalingPolicy == "queue-depth" {
		group.ScalingPolicy = &autoscaler.ScalingPolicy{
			PolicyType:                "queue-depth",
			QueueURL:                  queueURL,
			TargetMessagesPerInstance: targetMessagesPerInstance,
			ScaleUpCooldownSeconds:    scaleUpCooldown,
			ScaleDownCooldownSeconds:  scaleDownCooldown,
		}
	}

	// Add metric policy if specified
	if metricPolicy != "" {
		policy, err := buildMetricPolicy(metricPolicy, targetValue, metricName, metricNamespace, metricStatistic, periodSeconds)
		if err != nil {
			return err
		}
		group.MetricPolicy = policy
	}

	if err := as.CreateGroup(ctx, group); err != nil {
		return fmt.Errorf("create group: %w", err)
	}

	fmt.Printf("Created autoscale group: %s\n", groupID)
	fmt.Printf("Group name: %s\n", autoscaleName)
	fmt.Printf("Job array ID: %s\n", autoscaleJobArrayID)
	fmt.Printf("Desired capacity: %d\n", autoscaleDesired)
	fmt.Printf("Min/Max: %d/%d\n", autoscaleMin, autoscaleMax)

	// Trigger Lambda immediately
	if err := triggerLambda(ctx, groupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
		fmt.Println("\nGroup created but Lambda not triggered. Instances will launch on next scheduled run (within 1 minute).")
	} else {
		fmt.Println("\nTriggered immediate reconciliation. Instances will launch shortly.")
	}

	return nil
}

func runAutoscaleUpdate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	changed := false
	if autoscaleNewDesired >= 0 {
		group.DesiredCapacity = autoscaleNewDesired
		changed = true
	}
	if autoscaleNewMin >= 0 {
		group.MinCapacity = autoscaleNewMin
		changed = true
	}
	if autoscaleNewMax >= 0 {
		group.MaxCapacity = autoscaleNewMax
		changed = true
	}

	if !changed {
		return fmt.Errorf("no changes specified")
	}

	// Validate
	if group.MinCapacity > group.DesiredCapacity {
		return fmt.Errorf("min-capacity cannot exceed desired-capacity")
	}
	if group.MaxCapacity < group.DesiredCapacity {
		return fmt.Errorf("max-capacity cannot be less than desired-capacity")
	}

	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Updated group %s\n", groupName)
	fmt.Printf("New capacity: desired=%d, min=%d, max=%d\n",
		group.DesiredCapacity, group.MinCapacity, group.MaxCapacity)

	// Trigger Lambda
	if err := triggerLambda(ctx, group.AutoScaleGroupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
	} else {
		fmt.Println("Triggered immediate reconciliation.")
	}

	return nil
}

func runAutoscaleStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// If group name specified, show just that group
	if len(args) > 0 {
		group, err := as.GetGroupByName(ctx, args[0])
		if err != nil {
			return fmt.Errorf("get group: %w", err)
		}

		printGroupStatus(group)
		return nil
	}

	// Otherwise list all active groups (same view as `autoscale list`).
	return listAutoscaleGroups(ctx, as)
}

func runAutoscaleList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	return listAutoscaleGroups(ctx, as)
}

// listAutoscaleGroups prints a table of all active autoscale groups.
func listAutoscaleGroups(ctx context.Context, as *autoscaler.AutoScaler) error {
	groups, err := as.ListActiveGroups(ctx)
	if err != nil {
		return fmt.Errorf("list groups: %w", err)
	}

	if len(groups) == 0 {
		fmt.Println("No active autoscale groups")
		return nil
	}

	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "NAME\tSTATUS\tDESIRED\tMIN\tMAX\tCREATED")
	for _, group := range groups {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\n",
			group.GroupName,
			group.Status,
			group.DesiredCapacity,
			group.MinCapacity,
			group.MaxCapacity,
			group.CreatedAt.Format("2006-01-02 15:04"),
		)
	}
	return w.Flush()
}

func runAutoscaleHealth(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	// Get instances
	cfg, _ := config.LoadDefaultConfig(ctx)
	ec2Client := ec2.NewFromConfig(cfg)

	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:spawn:autoscale-group"),
				Values: []string{group.AutoScaleGroupID},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("describe instances: %w", err)
	}

	instanceIDs := make([]string, 0)
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			if instance.InstanceId != nil {
				instanceIDs = append(instanceIDs, aws.ToString(instance.InstanceId))
			}
		}
	}

	if len(instanceIDs) == 0 {
		fmt.Println("No instances found")
		return nil
	}

	// Check health
	dynamoClient := dynamodb.NewFromConfig(cfg)
	healthChecker := autoscaler.NewHealthChecker(ec2Client, dynamoClient, "spawn-hybrid-registry")

	health, err := healthChecker.CheckInstances(ctx, group.JobArrayID, instanceIDs)
	if err != nil {
		return fmt.Errorf("check health: %w", err)
	}

	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "INSTANCE\tSTATE\tHEARTBEAT\tSPOT\tHEALTH")
	for _, h := range health {
		spotStr := "no"
		if h.SpotInterruption {
			spotStr = "yes"
		}

		heartbeatStr := "N/A"
		if h.HeartbeatAge > 0 {
			heartbeatStr = fmt.Sprintf("%v ago", h.HeartbeatAge.Round(time.Second))
		}

		healthStr := "✓ healthy"
		if !h.Healthy {
			healthStr = fmt.Sprintf("✗ %s", h.Reason)
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			h.InstanceID, h.EC2State, heartbeatStr, spotStr, healthStr)
	}
	return w.Flush()
}

func runAutoscalePause(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	group.Status = "paused"
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Paused group %s (instances preserved)\n", groupName)
	return nil
}

func runAutoscaleResume(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	group.Status = "active"
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Resumed group %s\n", groupName)

	// Trigger Lambda
	if err := triggerLambda(ctx, group.AutoScaleGroupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
	}

	return nil
}

func runAutoscaleTerminate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	if !confirmYes(autoscaleTerminateYes, fmt.Sprintf("Terminate auto-scaling group %q and all its instances? This cannot be undone.", groupName)) {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return nil
	}

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	// Terminate all instances
	cfg, _ := config.LoadDefaultConfig(ctx)
	ec2Client := ec2.NewFromConfig(cfg)

	result, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:spawn:autoscale-group"),
				Values: []string{group.AutoScaleGroupID},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running", "stopping", "stopped"},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("describe instances: %w", err)
	}

	instanceIDs := make([]string, 0)
	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			if instance.InstanceId != nil {
				instanceIDs = append(instanceIDs, aws.ToString(instance.InstanceId))
			}
		}
	}

	if len(instanceIDs) > 0 {
		_, err = ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: instanceIDs,
		})
		if err != nil {
			return fmt.Errorf("terminate instances: %w", err)
		}
		fmt.Printf("Terminated %d instances\n", len(instanceIDs))
	}

	// Delete group
	if err := as.DeleteGroup(ctx, group.AutoScaleGroupID); err != nil {
		return fmt.Errorf("delete group: %w", err)
	}

	fmt.Printf("Terminated group %s\n", groupName)
	return nil
}

func runAutoscaleSetPolicy(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	// Handle --none flag (remove policy)
	if removePolicyFlag {
		group.ScalingPolicy = nil
		fmt.Printf("Removed scaling policy from %s (reverted to manual mode)\n", groupName)
	} else {
		// Validate and set policy
		if scalingPolicy == "" {
			return fmt.Errorf("--scaling-policy required (or use --none to remove)")
		}
		if scalingPolicy != "queue-depth" {
			return fmt.Errorf("invalid scaling policy: %s (only 'queue-depth' supported)", scalingPolicy)
		}

		// Build queue configuration (multi-queue or single queue)
		queues, err := buildQueueConfig(queueURLs, queueWeights, queueURL)
		if err != nil {
			return err
		}

		group.ScalingPolicy = &autoscaler.ScalingPolicy{
			PolicyType:                scalingPolicy,
			Queues:                    queues,
			TargetMessagesPerInstance: targetMessagesPerInstance,
			ScaleUpCooldownSeconds:    scaleUpCooldown,
			ScaleDownCooldownSeconds:  scaleDownCooldown,
		}

		if len(queues) == 1 {
			fmt.Printf("Updated scaling policy for %s (single queue)\n", groupName)
		} else {
			fmt.Printf("Updated scaling policy for %s (%d queues)\n", groupName, len(queues))
		}
	}

	// Update group in DynamoDB
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	// Trigger Lambda
	if err := triggerLambda(ctx, group.AutoScaleGroupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
	} else {
		fmt.Println("Triggered immediate reconciliation.")
	}

	return nil
}

func runAutoscaleScalingActivity(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	// Display scaling state
	if group.ScalingPolicy == nil {
		fmt.Println("No scaling policy configured (manual mode)")
		return nil
	}

	fmt.Printf("Scaling Policy: %s\n", group.ScalingPolicy.PolicyType)
	fmt.Printf("Queue: %s\n", group.ScalingPolicy.QueueURL)
	fmt.Printf("Target: %d messages/instance\n", group.ScalingPolicy.TargetMessagesPerInstance)
	fmt.Println()

	if group.ScalingState == nil {
		fmt.Println("No scaling activity yet")
		return nil
	}

	fmt.Printf("Last Queue Depth: %d messages\n", group.ScalingState.LastQueueDepth)
	fmt.Printf("Last Calculated Capacity: %d instances\n", group.ScalingState.LastCalculatedCapacity)

	if !group.ScalingState.LastScaleUp.IsZero() {
		fmt.Printf("Last Scale Up: %s (%s ago)\n",
			group.ScalingState.LastScaleUp.Format(time.RFC3339),
			time.Since(group.ScalingState.LastScaleUp).Round(time.Second))
	}
	if !group.ScalingState.LastScaleDown.IsZero() {
		fmt.Printf("Last Scale Down: %s (%s ago)\n",
			group.ScalingState.LastScaleDown.Format(time.RFC3339),
			time.Since(group.ScalingState.LastScaleDown).Round(time.Second))
	}

	return nil
}

func runAutoscaleSetMetricPolicy(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	// Handle --none flag (remove policy)
	if removePolicyFlag {
		group.MetricPolicy = nil
		fmt.Printf("Removed metric policy from %s\n", groupName)
	} else {
		// Validate and set policy
		if metricPolicy == "" {
			return fmt.Errorf("--metric-policy required (or use --none to remove)")
		}

		policy, err := buildMetricPolicy(metricPolicy, targetValue, metricName, metricNamespace, metricStatistic, periodSeconds)
		if err != nil {
			return err
		}

		group.MetricPolicy = policy
		fmt.Printf("Updated metric policy for %s\n", groupName)
	}

	// Update group in DynamoDB
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	// Trigger Lambda
	if err := triggerLambda(ctx, group.AutoScaleGroupID); err != nil {
		log.Printf("Warning: failed to trigger Lambda: %v", err)
	} else {
		fmt.Println("Triggered immediate reconciliation.")
	}

	return nil
}

func runAutoscaleMetricActivity(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	groupName := args[0]

	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	// Display metric policy
	if group.MetricPolicy == nil {
		fmt.Println("No metric policy configured")
		return nil
	}

	fmt.Printf("Metric Policy: %s\n", group.MetricPolicy.MetricType)
	fmt.Printf("Metric: %s (%s)\n", group.MetricPolicy.MetricName, group.MetricPolicy.Namespace)
	fmt.Printf("Statistic: %s\n", group.MetricPolicy.Statistic)
	fmt.Printf("Target: %.1f\n", group.MetricPolicy.TargetValue)
	fmt.Printf("Period: %ds\n", group.MetricPolicy.PeriodSeconds)
	fmt.Println()

	if group.ScalingState == nil {
		fmt.Println("No scaling activity yet")
		return nil
	}

	fmt.Printf("Last Calculated Capacity: %d instances\n", group.ScalingState.LastCalculatedCapacity)

	if !group.ScalingState.LastScaleUp.IsZero() {
		fmt.Printf("Last Scale Up: %s (%s ago)\n",
			group.ScalingState.LastScaleUp.Format(time.RFC3339),
			time.Since(group.ScalingState.LastScaleUp).Round(time.Second))
	}
	if !group.ScalingState.LastScaleDown.IsZero() {
		fmt.Printf("Last Scale Down: %s (%s ago)\n",
			group.ScalingState.LastScaleDown.Format(time.RFC3339),
			time.Since(group.ScalingState.LastScaleDown).Round(time.Second))
	}

	return nil
}

// buildQueueConfig creates queue configuration from CLI flags
func buildQueueConfig(queueURLs []string, queueWeights []float64, legacyQueueURL string) ([]autoscaler.QueueConfig, error) {
	// Multi-queue mode: --queue flags
	if len(queueURLs) > 0 {
		// Validate weights
		if len(queueWeights) > 0 && len(queueWeights) != len(queueURLs) {
			return nil, fmt.Errorf("number of --queue-weight (%d) must match number of --queue (%d)",
				len(queueWeights), len(queueURLs))
		}

		queues := make([]autoscaler.QueueConfig, len(queueURLs))
		for i, url := range queueURLs {
			weight := 1.0
			if i < len(queueWeights) {
				weight = queueWeights[i]
				if weight <= 0 || weight > 1.0 {
					return nil, fmt.Errorf("queue weight must be between 0.0 and 1.0, got %.2f", weight)
				}
			}
			queues[i] = autoscaler.QueueConfig{
				QueueURL: url,
				Weight:   weight,
			}
		}
		return queues, nil
	}

	// Single queue mode (backward compat): --queue-url flag
	if legacyQueueURL != "" {
		return []autoscaler.QueueConfig{
			{
				QueueURL: legacyQueueURL,
				Weight:   1.0,
			},
		}, nil
	}

	return nil, fmt.Errorf("--queue or --queue-url required when --scaling-policy is set")
}

func buildMetricPolicy(policyType string, target float64, name, namespace, statistic string, period int) (*autoscaler.MetricScalingPolicy, error) {
	var policy *autoscaler.MetricScalingPolicy

	switch policyType {
	case "cpu", "memory":
		policy = autoscaler.GetMetricPolicyDefaults(policyType)
		if target > 0 {
			policy.TargetValue = target
		}
	case "custom":
		if name == "" || namespace == "" {
			return nil, fmt.Errorf("--metric-name and --metric-namespace required for custom metrics")
		}
		if target == 0 {
			return nil, fmt.Errorf("--target-value required for custom metrics")
		}
		policy = &autoscaler.MetricScalingPolicy{
			MetricType:    "custom",
			MetricName:    name,
			Namespace:     namespace,
			Statistic:     statistic,
			TargetValue:   target,
			PeriodSeconds: period,
		}
	default:
		return nil, fmt.Errorf("invalid metric policy: %s (use 'cpu', 'memory', or 'custom')", policyType)
	}

	// Apply custom statistic and period if specified
	if statistic != "Average" {
		policy.Statistic = statistic
	}
	if period != 300 {
		policy.PeriodSeconds = period
	}

	return policy, nil
}

func triggerLambda(ctx context.Context, groupID string) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	lambdaClient := lambda.NewFromConfig(cfg)
	functionName := fmt.Sprintf("spawn-autoscale-orchestrator-%s", autoscaleEnv)

	payload := fmt.Sprintf(`{"group_id":"%s"}`, groupID)

	_, err = lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(functionName),
		InvocationType: lambdaTypes.InvocationTypeEvent,
		Payload:        []byte(payload),
	})

	return err
}

func printGroupStatus(group *autoscaler.AutoScaleGroup) {
	fmt.Printf("Group: %s (%s)\n", group.AutoScaleGroupID, group.GroupName)
	fmt.Printf("Job Array ID: %s\n", group.JobArrayID)
	fmt.Printf("Status: %s\n", group.Status)
	fmt.Printf("Capacity: desired=%d, min=%d, max=%d\n",
		group.DesiredCapacity, group.MinCapacity, group.MaxCapacity)
	fmt.Printf("Created: %s\n", group.CreatedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("Updated: %s\n", group.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
	if !group.LastScaleEvent.IsZero() {
		fmt.Printf("Last Scale Event: %s\n", group.LastScaleEvent.Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Printf("Health Check Interval: %v\n", group.HealthCheckInterval)

	// Display scaling policy info
	if group.ScalingPolicy != nil {
		fmt.Printf("\nScaling Policy: %s\n", group.ScalingPolicy.PolicyType)

		// Display queues (multi-queue or single queue)
		if len(group.ScalingPolicy.Queues) > 1 {
			fmt.Printf("  Queues: %d (multi-queue)\n", len(group.ScalingPolicy.Queues))
			for i, q := range group.ScalingPolicy.Queues {
				weight := q.Weight
				if weight == 0 {
					weight = 1.0
				}
				fmt.Printf("    %d. %s (weight: %.1f)\n", i+1, q.QueueURL, weight)
			}
		} else if len(group.ScalingPolicy.Queues) == 1 {
			fmt.Printf("  Queue: %s\n", group.ScalingPolicy.Queues[0].QueueURL)
		} else if group.ScalingPolicy.QueueURL != "" {
			// Backward compat
			fmt.Printf("  Queue: %s\n", group.ScalingPolicy.QueueURL)
		}

		fmt.Printf("  Target: %d messages/instance\n", group.ScalingPolicy.TargetMessagesPerInstance)
		fmt.Printf("  Cooldowns: up=%ds, down=%ds\n",
			group.ScalingPolicy.ScaleUpCooldownSeconds,
			group.ScalingPolicy.ScaleDownCooldownSeconds)

		if group.ScalingState != nil {
			fmt.Printf("\nCurrent State:\n")
			fmt.Printf("  Queue Depth: %d messages\n", group.ScalingState.LastQueueDepth)
			if !group.ScalingState.LastScaleUp.IsZero() {
				fmt.Printf("  Last Scale Up: %s ago\n",
					time.Since(group.ScalingState.LastScaleUp).Round(time.Second))
			}
			if !group.ScalingState.LastScaleDown.IsZero() {
				fmt.Printf("  Last Scale Down: %s ago\n",
					time.Since(group.ScalingState.LastScaleDown).Round(time.Second))
			}
		}
	} else {
		fmt.Println("\nScaling Policy: Manual (no queue-based scaling)")
	}

	// Display metric policy info
	if group.MetricPolicy != nil {
		fmt.Printf("\nMetric Policy: %s\n", group.MetricPolicy.MetricType)
		fmt.Printf("  Metric: %s (%s)\n", group.MetricPolicy.MetricName, group.MetricPolicy.Namespace)
		fmt.Printf("  Statistic: %s\n", group.MetricPolicy.Statistic)
		fmt.Printf("  Target: %.1f\n", group.MetricPolicy.TargetValue)
		fmt.Printf("  Period: %ds\n", group.MetricPolicy.PeriodSeconds)
	}

	// Display schedule info
	if group.ScheduleConfig != nil && len(group.ScheduleConfig.Actions) > 0 {
		fmt.Printf("\nScheduled Actions: %d\n", len(group.ScheduleConfig.Actions))
		for _, action := range group.ScheduleConfig.Actions {
			status := "enabled"
			if !action.Enabled {
				status = "disabled"
			}
			fmt.Printf("  - %s (%s)\n", action.Name, status)
			fmt.Printf("    Schedule: %s\n", action.Schedule)
			fmt.Printf("    Desired: %d", action.DesiredCapacity)
			if action.MinCapacity > 0 || action.MaxCapacity > 0 {
				fmt.Printf(" (min: %d, max: %d)", action.MinCapacity, action.MaxCapacity)
			}
			fmt.Println()
			if action.Timezone != "" && action.Timezone != "UTC" {
				fmt.Printf("    Timezone: %s\n", action.Timezone)
			}
		}
	}
}

func runAutoscaleAddSchedule(cmd *cobra.Command, args []string) error {
	groupName := args[0]

	// Validate required flags
	if autoscaleScheduleName == "" {
		return fmt.Errorf("--name required")
	}
	if autoscaleScheduleExpression == "" {
		return fmt.Errorf("--schedule required")
	}
	if autoscaleScheduleDesired <= 0 {
		return fmt.Errorf("--desired-capacity must be > 0")
	}

	ctx := context.Background()
	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// Load group
	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	// Validate schedule expression
	evaluator := autoscaler.NewScheduleEvaluator()
	if err := evaluator.ValidateSchedule(autoscaleScheduleExpression); err != nil {
		return fmt.Errorf("invalid schedule expression: %w", err)
	}

	// Initialize schedule config if needed
	if group.ScheduleConfig == nil {
		group.ScheduleConfig = &autoscaler.ScheduleConfig{
			Actions: []autoscaler.ScheduledAction{},
		}
	}

	// Check if schedule with same name exists
	for i, action := range group.ScheduleConfig.Actions {
		if action.Name == autoscaleScheduleName {
			// Update existing schedule
			group.ScheduleConfig.Actions[i] = autoscaler.ScheduledAction{
				Name:            autoscaleScheduleName,
				Schedule:        autoscaleScheduleExpression,
				DesiredCapacity: autoscaleScheduleDesired,
				MinCapacity:     autoscaleScheduleMin,
				MaxCapacity:     autoscaleScheduleMax,
				Timezone:        autoscaleScheduleTimezone,
				Enabled:         autoscaleScheduleEnabled,
			}
			if err := as.UpdateGroup(ctx, group); err != nil {
				return fmt.Errorf("update group: %w", err)
			}
			fmt.Printf("Updated schedule %q for group %s\n", autoscaleScheduleName, groupName)

			// Show next trigger time
			nextTime, _ := evaluator.GetNextTriggerTime(autoscaleScheduleExpression, autoscaleScheduleTimezone)
			if !nextTime.IsZero() {
				fmt.Printf("Next trigger: %s (%s)\n", nextTime.Format(time.RFC3339), time.Until(nextTime).Round(time.Second))
			}
			return nil
		}
	}

	// Add new schedule
	group.ScheduleConfig.Actions = append(group.ScheduleConfig.Actions, autoscaler.ScheduledAction{
		Name:            autoscaleScheduleName,
		Schedule:        autoscaleScheduleExpression,
		DesiredCapacity: autoscaleScheduleDesired,
		MinCapacity:     autoscaleScheduleMin,
		MaxCapacity:     autoscaleScheduleMax,
		Timezone:        autoscaleScheduleTimezone,
		Enabled:         autoscaleScheduleEnabled,
	})

	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Added schedule %q to group %s\n", autoscaleScheduleName, groupName)

	// Show next trigger time
	nextTime, _ := evaluator.GetNextTriggerTime(autoscaleScheduleExpression, autoscaleScheduleTimezone)
	if !nextTime.IsZero() {
		fmt.Printf("Next trigger: %s (%s)\n", nextTime.Format(time.RFC3339), time.Until(nextTime).Round(time.Second))
	}

	return nil
}

func runAutoscaleRemoveSchedule(cmd *cobra.Command, args []string) error {
	groupName := args[0]
	scheduleName := args[1]

	ctx := context.Background()
	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// Load group
	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	if group.ScheduleConfig == nil || len(group.ScheduleConfig.Actions) == 0 {
		return fmt.Errorf("no schedules configured for group %s", groupName)
	}

	// Find and remove schedule
	found := false
	newActions := make([]autoscaler.ScheduledAction, 0)
	for _, action := range group.ScheduleConfig.Actions {
		if action.Name == scheduleName {
			found = true
		} else {
			newActions = append(newActions, action)
		}
	}

	if !found {
		return fmt.Errorf("schedule %q not found in group %s", scheduleName, groupName)
	}

	if !confirmYes(autoscaleRemoveScheduleYes, fmt.Sprintf("Remove schedule %q from group %s?", scheduleName, groupName)) {
		return fmt.Errorf("aborted")
	}

	group.ScheduleConfig.Actions = newActions
	if err := as.UpdateGroup(ctx, group); err != nil {
		return fmt.Errorf("update group: %w", err)
	}

	fmt.Printf("Removed schedule %q from group %s\n", scheduleName, groupName)
	return nil
}

func runAutoscaleListSchedules(cmd *cobra.Command, args []string) error {
	groupName := args[0]

	ctx := context.Background()
	as, err := getAutoscaler(ctx)
	if err != nil {
		return err
	}

	// Load group
	group, err := as.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("get group: %w", err)
	}

	if group.ScheduleConfig == nil || len(group.ScheduleConfig.Actions) == 0 {
		fmt.Printf("No scheduled actions for group %s\n", groupName)
		return nil
	}

	evaluator := autoscaler.NewScheduleEvaluator()

	fmt.Printf("Scheduled Actions for %s:\n\n", groupName)
	w := newTableWriter(os.Stdout)
	_, _ = fmt.Fprintln(w, "NAME\tSCHEDULE\tDESIRED\tMIN\tMAX\tTIMEZONE\tENABLED\tNEXT TRIGGER")

	for _, action := range group.ScheduleConfig.Actions {
		status := "yes"
		if !action.Enabled {
			status = "no"
		}

		minStr := "-"
		if action.MinCapacity > 0 {
			minStr = fmt.Sprintf("%d", action.MinCapacity)
		}

		maxStr := "-"
		if action.MaxCapacity > 0 {
			maxStr = fmt.Sprintf("%d", action.MaxCapacity)
		}

		tz := action.Timezone
		if tz == "" {
			tz = "UTC"
		}

		nextTrigger := "-"
		if action.Enabled {
			nextTime, err := evaluator.GetNextTriggerTime(action.Schedule, action.Timezone)
			if err == nil && !nextTime.IsZero() {
				nextTrigger = time.Until(nextTime).Round(time.Second).String()
			}
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			action.Name, action.Schedule, action.DesiredCapacity,
			minStr, maxStr, tz, status, nextTrigger)
	}

	return w.Flush()
}
