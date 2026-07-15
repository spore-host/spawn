package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
)

var (
	// Instance config
	instanceType string
	region       string
	az           string
	ami          string
	osFlag       string
	nestedVirt   bool
	allowCIDR    string

	// Network (empty = auto-create)
	vpcID    string
	subnetID string
	sgIDs    []string

	// SSH key
	keyPair string

	// Behavior
	spot               bool
	spotMaxPrice       string
	useReservation     bool
	reservationID      string
	capacityBlock      bool
	hibernate          bool
	ttl                string
	idleTimeout        string
	hibernateOnIdle    bool
	onIdle             string
	preStop            string
	preStopTimeout     string
	spotWebhookURL     string
	webhookCorrelation string
	webhookTimeout     string
	onComplete         string
	completionFile     string
	completionDelay    string
	sessionTimeout     string

	// Meta
	name             string
	userData         string
	userDataFile     string
	dnsName          string
	dnsDomain        string
	dnsAPIEndpoint   string
	noTimeout        bool
	slackWorkspaceID string // for lifecycle notifications via spore-bot
	notifyPlatform   string // chat platform for lifecycle notifications: slack (default) / teams / discord (#2)
	activePorts      string // comma-separated ports to monitor for active connections (e.g. "8787,8888")
	activeProcesses  string // comma-separated process names to monitor (e.g. "rsession,jupyter")

	// Job array
	count          int
	jobArrayName   string
	instanceNames  string
	command        string
	reconcilerMode string // job-array launch engine: "legacy" (default) or "cohort"

	// MPI
	mpiEnabled            bool
	mpiProcessesPerNode   int
	mpiCommand            string
	mpiSkipInstall        bool
	mpiPlacementGroup     string
	mpiAutoPlacementGroup bool
	efaEnabled            bool

	// Shared storage
	efsID           string
	efsMountPoint   string
	efsProfile      string
	efsMountOptions string

	// Attached EBS data volumes from snapshots (#144)
	attachVolumes []string

	// FSx Lustre
	fsxCreate          bool
	fsxLifecycle       string // ephemeral | durable — REQUIRED with --fsx-create (#193)
	fsxTTL             string // required when --fsx-lifecycle=durable
	fsxID              string
	fsxSkipValidate    bool
	fsxRecall          string
	fsxStorageCapacity int32
	fsxThroughput      int32
	fsxS3Bucket        string
	fsxImportPath      string
	fsxExportPath      string
	fsxMountPoint      string

	// Parameter sweep
	paramFile              string
	params                 string
	cartesian              bool
	maxConcurrent          int
	maxConcurrentPerRegion int
	launchDelay            string
	detach                 bool
	noDetach               bool
	sweepName              string
	estimateOnly           bool
	autoYes                bool
	distributionMode       string
	budget                 float64
	costLimit              float64

	// Region constraints
	regionsInclude    []string
	regionsExclude    []string
	regionsGeographic []string
	proximityFrom     string
	costTier          string

	// Batch queue
	batchQueueFile string
	queueTemplate  string
	templateVars   map[string]string

	// IAM
	iamRole            string
	iamPolicy          []string
	iamManagedPolicies []string
	iamPolicyFile      string
	iamTrustServices   []string
	iamRoleTags        []string

	// Mode
	interactive      bool
	quiet            bool
	waitForRunning   bool
	waitForSSH       bool
	skipRegionCheck  bool
	terminateOnError bool

	// Workflow integration
	outputIDFile string
	wait         bool
	waitTimeout  string

	// Compliance (parsed from flags)
	complianceMode   string
	complianceStrict bool

	// Team sharing
	launchTeamID string
	launchTags   []string

	// Plugin declarations
	launchConfigFile string
	launchPlugins    []string

	// Strata software environment
	strataFormation string
	strataProfile   string
	strataRegistry  string

	// EBS root volume
	launchVolumeSize int32
)

var launchCmd = &cobra.Command{
	Use:     "launch <name>",
	Args:    cobra.ExactArgs(1),
	RunE:    runLaunch,
	Aliases: []string{"", "run", "create"},
	// Short and Long will be set after i18n initialization
}

func init() {
	rootCmd.AddCommand(launchCmd)

	// Instance config
	launchCmd.Flags().StringVar(&instanceType, "instance-type", "", "Instance type")
	launchCmd.Flags().StringVar(&region, "region", "", "AWS region")
	launchCmd.Flags().StringVar(&az, "az", "", "Availability zone")
	launchCmd.Flags().StringVar(&ami, "ami", "", "AMI ID (ami-...); omit or use 'auto' to auto-detect the latest AL2023")
	launchCmd.Flags().StringVar(&osFlag, "os", "", "Target OS: windows or linux. Omit to auto-detect from the AMI. Use to force the OS for a custom AMI whose platform metadata is unset.")
	launchCmd.Flags().BoolVar(&nestedVirt, "nested-virtualization", false, "Enable nested virtualization (run KVM/Hyper-V inside the instance). Requires a C8i/M8i/R8i instance type.")
	launchCmd.Flags().StringVar(&allowCIDR, "allow-cidr", "", "CIDR allowed to reach the managed Windows security group (RDP 3389 + SSH 22); default 0.0.0.0/0")
	launchCmd.Flags().Int32Var(&launchVolumeSize, "volume-size", 0, "Root EBS volume size in GiB (0 = use AMI default)")
	launchCmd.Flags().StringArrayVar(&attachVolumes, "attach-volume", nil, "Attach an EBS volume from a snapshot, mounted at a path: snap-xxx:/mount/point[:ro]. Repeatable. Read-only is the common case for shared reference data.")

	// Network
	launchCmd.Flags().StringVar(&vpcID, "vpc", "", "VPC ID")
	launchCmd.Flags().StringVar(&subnetID, "subnet-id", "", "Subnet ID")
	// Deprecated alias for --subnet-id (bound to the same var).
	launchCmd.Flags().StringVar(&subnetID, "subnet", "", "Subnet ID")
	_ = launchCmd.Flags().MarkDeprecated("subnet", "use --subnet-id instead")
	launchCmd.Flags().StringSliceVar(&sgIDs, "security-group-ids", nil, "Security group IDs (comma-separated or repeated)")
	// Deprecated alias for --security-group-ids (single value, appended to the slice).
	launchCmd.Flags().StringSliceVar(&sgIDs, "security-group", nil, "Security group ID")
	_ = launchCmd.Flags().MarkDeprecated("security-group", "use --security-group-ids instead")

	// SSH
	launchCmd.Flags().StringVar(&keyPair, "key-name", "", "SSH key pair name (EC2 KeyName)")
	// Deprecated alias for --key-name (bound to the same var).
	launchCmd.Flags().StringVar(&keyPair, "key-pair", "", "SSH key pair name")
	_ = launchCmd.Flags().MarkDeprecated("key-pair", "use --key-name instead")

	// Capacity
	launchCmd.Flags().BoolVar(&spot, "spot", false, "Launch as Spot instance")
	launchCmd.Flags().StringVar(&spotMaxPrice, "spot-max-price", "", "Max Spot price")
	launchCmd.Flags().BoolVar(&useReservation, "use-reservation", false, "Use capacity reservation")
	launchCmd.Flags().StringVar(&reservationID, "reservation-id", "", "Capacity Reservation / Capacity Block ID to launch into (fs-/cr-...) — instance must be in the reservation's AZ (#216)")
	launchCmd.Flags().BoolVar(&capacityBlock, "capacity-block", false, "The --reservation-id is a Capacity Block for ML (sets MarketType=capacity-block); mutually exclusive with --spot (#216)")

	// Behavior
	launchCmd.Flags().BoolVar(&hibernate, "hibernate", false, "Enable hibernation")
	launchCmd.Flags().StringVar(&ttl, "ttl", "", "Auto-terminate after duration (e.g., 8h, defaults to 1h idle if not set)")
	launchCmd.Flags().StringVar(&idleTimeout, "idle-timeout", "", "Auto-terminate if idle (defaults to 1h if neither --ttl nor --idle-timeout set)")
	launchCmd.Flags().BoolVar(&noTimeout, "no-timeout", false, "Disable automatic timeout (NOT RECOMMENDED: creates zombie risk)")
	launchCmd.Flags().StringVar(&onIdle, "on-idle", "", "Action when the instance goes idle: stop (default) or hibernate. Mirrors --on-complete. NOTE: a stopped/hibernated instance keeps billing for its EBS volumes (and any attached Elastic IP) — for batch/headless work prefer --on-complete terminate so cost is fully bounded")
	launchCmd.Flags().BoolVar(&hibernateOnIdle, "hibernate-on-idle", false, "Hibernate instead of stop when idle")
	_ = launchCmd.Flags().MarkDeprecated("hibernate-on-idle", "use --on-idle hibernate")
	launchCmd.Flags().StringVar(&preStop, "pre-stop", "", "Shell command to run on the instance before any lifecycle-triggered stop/terminate (e.g., \"aws s3 sync /results s3://bucket/\")")
	launchCmd.Flags().StringVar(&preStopTimeout, "pre-stop-timeout", "", "Max time to wait for --pre-stop command (default: 5m, spot: 90s)")
	launchCmd.Flags().StringVar(&spotWebhookURL, "spot-webhook-url", "", "On spot interruption, spored POSTs a fire-once, best-effort notice to this URL within the ~2-min window (off-node consumers; empty = disabled)")
	launchCmd.Flags().StringVar(&webhookCorrelation, "webhook-correlation", "", "Opaque blob echoed verbatim in the spot-webhook payload so a consumer can correlate the event to its own record (never parsed by spawn)")
	launchCmd.Flags().StringVar(&webhookTimeout, "webhook-timeout", "", "Hard cap on the spot-webhook POST so it can't eat the reclamation window (default: 2s)")
	launchCmd.Flags().StringVar(&onComplete, "on-complete", "", "Action when workload signals completion: terminate, stop, hibernate. Use 'terminate' for batch/headless workloads — 'stop' leaves EBS (and any attached EIP) billing indefinitely, which is easy to forget in accounts without a hosted reaper")
	launchCmd.Flags().StringVar(&completionFile, "completion-file", "/tmp/SPAWN_COMPLETE", "File to watch for completion signal")
	launchCmd.Flags().StringVar(&completionDelay, "completion-delay", "30s", "Grace period after completion signal")
	launchCmd.Flags().StringVar(&sessionTimeout, "session-timeout", "30m", "Auto-logout idle shells (0 to disable)")

	// Meta
	launchCmd.Flags().StringVar(&name, "name", "", "Name your spore, required (sets Name tag, DNS, and hostname)")
	launchCmd.Flags().StringVar(&userData, "user-data", "", "User data (@file or inline)")
	launchCmd.Flags().StringVar(&userDataFile, "user-data-file", "", "User data file")
	launchCmd.Flags().StringVar(&dnsName, "dns", "", "Override DNS name if different from --name (advanced)")
	launchCmd.Flags().StringVar(&slackWorkspaceID, "slack-workspace", "", "Slack workspace ID for lifecycle notifications (e.g. T03NE3GTY)")
	launchCmd.Flags().StringVar(&notifyPlatform, "notify-platform", "", "Chat platform for lifecycle notifications: slack (default), teams, or discord")
	launchCmd.Flags().StringVar(&activePorts, "active-ports", "", "TCP ports to monitor for active connections, prevents idle termination (e.g. '8787' for RStudio, '8787,8888' for RStudio+Jupyter)")
	launchCmd.Flags().StringVar(&activeProcesses, "active-processes", "", "Process names to monitor, prevents idle termination while any are running (e.g. 'rsession' for RStudio, 'rsession,jupyter' for multiple)")
	launchCmd.Flags().StringVar(&dnsDomain, "dns-domain", "", "Custom DNS domain (overrides default)")
	launchCmd.Flags().StringVar(&dnsAPIEndpoint, "dns-api-endpoint", "", "Custom DNS API endpoint (overrides default)")

	// Job array
	launchCmd.Flags().IntVar(&count, "count", 1, "Number of instances to launch (job array)")
	launchCmd.Flags().StringVar(&jobArrayName, "job-array-name", "", "Job array group name (required if --count > 1)")
	launchCmd.Flags().StringVar(&instanceNames, "instance-names", "", "Instance name template (e.g., 'worker-{index}', default: '{job-array-name}-{index}')")
	launchCmd.Flags().StringVar(&command, "command", "", "Command to run on all instances (executed after spored setup)")
	// Experimental: route a job-array launch through the cohort reconciler
	// (all-or-nothing barrier + leak-free drain) instead of the hand-rolled loop.
	// Hidden until it's the supported default; fully functional/testable.
	launchCmd.Flags().StringVar(&reconcilerMode, "reconciler", "legacy", "Job-array launch engine: legacy or cohort (experimental)")
	_ = launchCmd.Flags().MarkHidden("reconciler")

	// MPI
	launchCmd.Flags().BoolVar(&mpiEnabled, "mpi", false, "Enable MPI cluster setup (requires --count > 1)")
	launchCmd.Flags().IntVar(&mpiProcessesPerNode, "mpi-processes-per-node", 0, "MPI processes per node (default: vCPU count)")
	launchCmd.Flags().StringVar(&mpiCommand, "mpi-command", "", "Command to run via mpirun (alternative to --command)")
	launchCmd.Flags().BoolVar(&mpiSkipInstall, "skip-mpi-install", false, "Skip MPI installation (use with custom AMIs that have MPI pre-installed)")
	launchCmd.Flags().StringVar(&mpiPlacementGroup, "placement-group", "", "AWS Placement Group for MPI instances (auto-created if not specified)")
	launchCmd.Flags().BoolVar(&mpiAutoPlacementGroup, "auto-placement-group", true, "Automatically create placement group for MPI job arrays (default: true)")
	launchCmd.Flags().BoolVar(&efaEnabled, "efa", false, "Enable Elastic Fabric Adapter for ultra-low latency MPI (requires supported instance types)")

	// Shared storage
	launchCmd.Flags().StringVar(&efsID, "efs-id", "", "EFS filesystem ID to mount (fs-xxx)")
	launchCmd.Flags().StringVar(&efsMountPoint, "efs-mount-point", "/efs", "EFS mount point (default: /efs)")
	launchCmd.Flags().StringVar(&efsProfile, "efs-profile", "general", "EFS performance profile: general, max-io, max-throughput, burst")
	launchCmd.Flags().StringVar(&efsMountOptions, "efs-mount-options", "", "Custom EFS mount options (overrides profile)")

	// FSx Lustre
	launchCmd.Flags().BoolVar(&fsxCreate, "fsx-create", false, "Create new FSx Lustre filesystem with S3 backing (requires --fsx-lifecycle)")
	launchCmd.Flags().StringVar(&fsxLifecycle, "fsx-lifecycle", "", "FSx lifetime (REQUIRED with --fsx-create): 'ephemeral' (reaped when this instance terminates) or 'durable' (persists; requires --fsx-ttl)")
	launchCmd.Flags().StringVar(&fsxTTL, "fsx-ttl", "", "FSx time-to-live, required for --fsx-lifecycle=durable (e.g. 7d, 720h) — the filesystem is reaped this long after creation once no instance is using it")
	launchCmd.Flags().StringVar(&fsxID, "fsx-id", "", "Existing FSx Lustre filesystem ID to mount (fs-xxx)")
	launchCmd.Flags().BoolVar(&fsxSkipValidate, "fsx-skip-validate", false, "Skip FSx filesystem validation (for testing)")
	launchCmd.Flags().StringVar(&fsxRecall, "fsx-recall", "", "Recall FSx filesystem by stack name (recreate from S3)")
	launchCmd.Flags().Int32Var(&fsxStorageCapacity, "fsx-storage-capacity", 1200, "FSx storage capacity in GB (1200, 2400, or increments of 2400)")
	launchCmd.Flags().Int32Var(&fsxThroughput, "fsx-throughput", 125, "FSx PERSISTENT_2 throughput in MB/s/TiB (125, 250, 500, or 1000; default: 125)")
	launchCmd.Flags().StringVar(&fsxS3Bucket, "fsx-s3-bucket", "", "S3 bucket for FSx import/export (required with --fsx-create)")
	launchCmd.Flags().StringVar(&fsxImportPath, "fsx-import-path", "", "S3 path to import from (e.g., s3://bucket/prefix)")
	launchCmd.Flags().StringVar(&fsxExportPath, "fsx-export-path", "", "S3 path to export to (e.g., s3://bucket/prefix)")
	launchCmd.Flags().StringVar(&fsxMountPoint, "fsx-mount-point", "/fsx", "FSx mount point (default: /fsx)")

	// Parameter sweep
	launchCmd.Flags().StringVar(&paramFile, "param-file", "", "Path to parameter sweep file (JSON/YAML/CSV)")
	launchCmd.Flags().StringVar(&params, "params", "", "Inline JSON parameters for sweep")
	launchCmd.Flags().BoolVar(&cartesian, "cartesian", false, "Generate cartesian product of parameter lists")
	launchCmd.Flags().IntVar(&maxConcurrent, "max-concurrent", 0, "Max instances running simultaneously (0 = unlimited)")
	launchCmd.Flags().IntVar(&maxConcurrentPerRegion, "max-concurrent-per-region", 0, "Max instances running simultaneously per region (0 = unlimited)")
	launchCmd.Flags().StringVar(&launchDelay, "launch-delay", "0s", "Delay between instance launches (e.g., 5s)")
	launchCmd.Flags().BoolVar(&detach, "detach", false, "Run sweep orchestration in Lambda (auto-enabled for parameter sweeps)")
	launchCmd.Flags().BoolVar(&noDetach, "no-detach", false, "Disable auto-detach for parameter sweeps (requires --ttl or --idle-timeout)")
	launchCmd.Flags().StringVar(&sweepName, "sweep-name", "", "Human-readable sweep identifier (auto-generated if empty)")
	launchCmd.Flags().Float64Var(&budget, "budget", 0, "Budget limit in dollars for parameter sweeps (0 = no limit)")
	launchCmd.Flags().Float64Var(&costLimit, "cost-limit", 0, "Terminate/stop when compute spend reaches this amount in USD (compute cost only; 0 = disabled)")
	launchCmd.Flags().BoolVar(&estimateOnly, "estimate-only", false, "Show cost estimate and exit without launching")
	launchCmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "Auto-approve cost estimate (skip confirmation)")
	launchCmd.Flags().StringVar(&distributionMode, "mode", "balanced", "Distribution mode: balanced (fair share) or opportunistic (prioritize available regions)")

	// Region constraints
	launchCmd.Flags().StringSliceVar(&regionsInclude, "regions-include", []string{}, "Only use these regions (supports wildcards: us-*, eu-*)")
	launchCmd.Flags().StringSliceVar(&regionsExclude, "regions-exclude", []string{}, "Exclude these regions (supports wildcards: us-*, eu-*)")
	launchCmd.Flags().StringSliceVar(&regionsGeographic, "regions-geographic", []string{}, "Geographic constraints: us, eu, ap, north-america, europe, asia-pacific")
	launchCmd.Flags().StringVar(&proximityFrom, "proximity-from", "", "Prefer regions close to this region (e.g., us-east-1)")
	launchCmd.Flags().StringVar(&costTier, "cost-tier", "", "Prefer cost tier: low, standard, premium")

	// Batch queue
	launchCmd.Flags().StringVar(&batchQueueFile, "batch-queue", "", "Batch job queue file (JSON) for sequential execution")
	launchCmd.Flags().StringVar(&queueTemplate, "queue-template", "", "Queue template name (use 'spawn queue template list' to see options)")
	launchCmd.Flags().StringToStringVar(&templateVars, "template-var", nil, "Template variables (key=value)")

	// IAM
	launchCmd.Flags().StringVar(&iamRole, "iam-role", "", "IAM role name (creates if doesn't exist)")
	launchCmd.Flags().StringSliceVar(&iamPolicy, "iam-policy", []string{}, "Service-level policies (e.g., s3:ReadOnly,dynamodb:WriteOnly)")
	launchCmd.Flags().StringSliceVar(&iamManagedPolicies, "iam-managed-policies", []string{}, "AWS managed policy ARNs")
	launchCmd.Flags().StringVar(&iamPolicyFile, "iam-policy-file", "", "Custom IAM policy JSON file")
	launchCmd.Flags().StringSliceVar(&iamTrustServices, "iam-trust-services", []string{"ec2"}, "Services that can assume role")
	launchCmd.Flags().StringSliceVar(&iamRoleTags, "iam-role-tags", []string{}, "Tags for IAM role (key=value format)")

	// Mode
	launchCmd.Flags().BoolVar(&interactive, "interactive", false, "Force interactive wizard")
	launchCmd.Flags().BoolVar(&quiet, "quiet", false, "Minimal output")
	launchCmd.Flags().BoolVar(&waitForRunning, "wait-for-running", true, "Wait until running")
	launchCmd.Flags().BoolVar(&waitForSSH, "wait-for-ssh", true, "Wait until SSH is ready")
	launchCmd.Flags().BoolVar(&skipRegionCheck, "skip-region-check", false, "Skip data locality region mismatch warnings")
	launchCmd.Flags().BoolVar(&terminateOnError, "terminate-on-error", false, "If post-launch verification fails (e.g. spored didn't come up), terminate the instance instead of leaving it running")

	// Compliance
	launchCmd.Flags().Bool("nist-800-171", false, "Enable NIST 800-171 Rev 3 compliance mode")
	launchCmd.Flags().String("nist-800-53", "", "Enable NIST 800-53 compliance (low, moderate, high)")
	launchCmd.Flags().Bool("compliance-strict", false, "Strict mode: fail on warnings (default: show warnings only)")

	// Workflow integration
	launchCmd.Flags().StringVar(&outputIDFile, "output-id", "", "Write sweep/instance ID to file for scripting")
	launchCmd.Flags().BoolVar(&wait, "wait", false, "Wait for sweep/launch to complete (requires --detach)")
	launchCmd.Flags().StringVar(&waitTimeout, "wait-timeout", "0", "Timeout for --wait (e.g., 2h, 30m, 0=no timeout)")

	// Team sharing
	launchCmd.Flags().StringVar(&launchTeamID, "team", "", "Team ID: tag instance with spawn:team-id for team-shared access")
	launchCmd.Flags().StringArrayVar(&launchTags, "tag", nil, "Custom tag key=value on the instance and its created volumes (repeatable). The spawn: prefix is reserved.")

	// Plugin declarations
	launchCmd.Flags().StringVar(&launchConfigFile, "config", "", "Launch config YAML file (supports plugins: list)")
	launchCmd.Flags().StringArrayVar(&launchPlugins, "plugin", nil, "Plugin to install at launch (ref[@version], repeatable)")

	// Strata software environment
	launchCmd.Flags().StringVar(&strataFormation, "strata-formation", "", "Strata formation to activate (e.g. r-research@2024.03)")
	launchCmd.Flags().StringVar(&strataProfile, "strata-profile", "", "Path to a Strata profile YAML file")
	launchCmd.Flags().StringVar(&strataRegistry, "strata-registry", "s3://strata-registry", "Strata registry S3 URL")

	// Register completions for flags
	_ = launchCmd.RegisterFlagCompletionFunc("region", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeRegion(cmd, args, toComplete)
	})
	_ = launchCmd.RegisterFlagCompletionFunc("instance-type", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeInstanceType(cmd, args, toComplete)
	})
}

// parseIAMRoleTags parses IAM role tags from key=value format
func parseIAMRoleTags(tags []string) map[string]string {
	result := make(map[string]string)
	for _, tagStr := range tags {
		parts := strings.SplitN(tagStr, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

// applyLaunchDefaults loads ~/.spawn/config.yaml defaults and applies them to
// package-level flag variables that were not explicitly set on the command line.
func applyLaunchDefaults(cmd *cobra.Command) {
	d, err := spawnconfig.LoadLaunchDefaults()
	if err != nil || d == nil {
		return
	}
	if !cmd.Flags().Changed("slack-workspace") && d.SlackWorkspace != "" {
		slackWorkspaceID = d.SlackWorkspace
	}
	if !cmd.Flags().Changed("active-processes") && d.ActiveProcesses != "" {
		activeProcesses = d.ActiveProcesses
	}
	if !cmd.Flags().Changed("active-ports") && d.ActivePorts != "" {
		activePorts = d.ActivePorts
	}
	if !cmd.Flags().Changed("idle-timeout") && d.IdleTimeout != "" {
		idleTimeout = d.IdleTimeout
	}
	if !cmd.Flags().Changed("on-idle") && !cmd.Flags().Changed("hibernate-on-idle") && d.HibernateOnIdle != nil {
		hibernateOnIdle = *d.HibernateOnIdle
	}
}

// validateOnIdle checks the --on-idle enum. Empty (unset) is valid and means the
// default idle action (stop). Only "stop" and "hibernate" are accepted — the idle
// daemon never terminates (that's --on-complete's job), so "terminate" is rejected
// with a pointer to the right flag (#316).
func validateOnIdle(v string) error {
	switch v {
	case "", "stop", "hibernate":
		return nil
	case "terminate":
		return fmt.Errorf("--on-idle does not accept 'terminate' (the idle daemon only stops or hibernates); use --on-complete terminate to terminate on completion")
	default:
		return fmt.Errorf("invalid --on-idle %q: must be 'stop' or 'hibernate'", v)
	}
}
