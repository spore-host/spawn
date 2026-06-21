package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/uuid"
	"github.com/scttfrdmn/strata/pkg/strata"
	"github.com/scttfrdmn/strata/spec"
	"github.com/spf13/cobra"
	"github.com/spore-host/libs/i18n"
	"github.com/spore-host/libs/pricing"
	"github.com/spore-host/spawn/pkg/audit"
	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/compliance"
	spawnconfig "github.com/spore-host/spawn/pkg/config"
	"github.com/spore-host/spawn/pkg/input"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/locality"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/queue"
	"github.com/spore-host/spawn/pkg/regions"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/sshkey"
	"github.com/spore-host/spawn/pkg/staging"
	"github.com/spore-host/spawn/pkg/storage"
	"github.com/spore-host/spawn/pkg/sweep"
	"github.com/spore-host/spawn/pkg/userdata"
	"github.com/spore-host/spawn/pkg/wizard"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
	"gopkg.in/yaml.v3"
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
	sgID     string

	// SSH key
	keyPair string

	// Behavior
	spot            bool
	spotMaxPrice    string
	useReservation  bool
	reservationID   string
	capacityBlock   bool
	hibernate       bool
	ttl             string
	idleTimeout     string
	hibernateOnIdle bool
	preStop         string
	preStopTimeout  string
	onComplete      string
	completionFile  string
	completionDelay string
	sessionTimeout  string

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
	count         int
	jobArrayName  string
	instanceNames string
	command       string

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
	interactive     bool
	quiet           bool
	waitForRunning  bool
	waitForSSH      bool
	skipRegionCheck bool

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
	launchCmd.Flags().StringVar(&subnetID, "subnet", "", "Subnet ID")
	launchCmd.Flags().StringVar(&sgID, "security-group", "", "Security group ID")

	// SSH
	launchCmd.Flags().StringVar(&keyPair, "key-pair", "", "SSH key pair name")

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
	launchCmd.Flags().BoolVar(&hibernateOnIdle, "hibernate-on-idle", false, "Hibernate instead of terminate when idle")
	launchCmd.Flags().StringVar(&preStop, "pre-stop", "", "Shell command to run on the instance before any lifecycle-triggered stop/terminate (e.g., \"aws s3 sync /results s3://bucket/\")")
	launchCmd.Flags().StringVar(&preStopTimeout, "pre-stop-timeout", "", "Max time to wait for --pre-stop command (default: 5m, spot: 90s)")
	launchCmd.Flags().StringVar(&onComplete, "on-complete", "", "Action when workload signals completion: terminate, stop, hibernate")
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

func runLaunch(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Get the account ID for audit logging. This is best-effort and resilient:
	// it uses the AWS client's GetAccountID, which falls back to IMDS if STS is
	// unreachable (e.g. an STS VPC endpoint with private DNS this subnet can't
	// route to, #33). A failure here must not block the launch — audit user_id
	// is non-critical metadata.
	identityClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	userID, err := identityClient.GetAccountID(ctx)
	if err != nil {
		userID = "unknown"
		fmt.Fprintf(os.Stderr, "⚠️  Could not determine account ID (STS/IMDS unavailable); continuing with audit user=unknown\n")
	}
	correlationID := uuid.New().String()
	auditLog := audit.NewLogger(os.Stderr, userID, correlationID)

	// Detect platform
	plat, err := platform.Detect()
	if err != nil {
		return i18n.Te("error.platform_detect_failed", err)
	}

	// Enable colors on Windows
	if plat.OS == "windows" {
		platform.EnableWindowsColors()
	}

	// Check for batch queue mode FIRST
	// Capture positional name argument early (before batch queue / sweep early returns)
	if len(args) > 0 && name == "" {
		name = args[0]
	}

	if batchQueueFile != "" || queueTemplate != "" {
		return launchWithBatchQueue(ctx, plat, auditLog)
	}

	// Check for parameter sweep mode (before wizard/config logic)
	if paramFile != "" || params != "" {
		// Parameter sweep launch path - config will be built inside launchParameterSweep
		// Create minimal config for sweep orchestration
		config := &aws.LaunchConfig{
			Region:       region,
			InstanceType: instanceType, // May be empty, that's ok for sweeps
		}
		return launchParameterSweep(ctx, config, plat, auditLog)
	}

	// Apply user defaults from ~/.spawn/config.yaml for flags not explicitly set.
	applyLaunchDefaults(cmd)

	// Positional arg takes precedence over --name flag.
	if len(args) > 0 {
		name = args[0]
	}

	var config *aws.LaunchConfig

	// Windows gets a non-burstable default instance type when --os windows is
	// explicit and --instance-type was omitted (#95). Apply it BEFORE launchMode,
	// which would otherwise treat an empty type as "go to the wizard/pipe" and
	// never reach the flags path.
	if instanceType == "" && strings.EqualFold(strings.TrimSpace(osFlag), "windows") {
		instanceType = defaultWindowsInstanceType
		fmt.Fprintf(os.Stderr, "ℹ️  No --instance-type given for Windows; defaulting to %s (non-burstable).\n", defaultWindowsInstanceType)
	}

	// Determine mode: wizard, pipe, or flags. See launchMode for the rules — the
	// key fix (#34): pipe mode requires no --instance-type, so explicit flags
	// never read stdin even when invoked with a piped (non-TTY) stdin, e.g. from
	// a Java/ProcessBuilder subprocess.
	switch launchMode(interactive, instanceType, isTerminal(os.Stdin)) {
	case modeWizard:
		wiz := wizard.NewWizard(plat)
		config, err = wiz.Run(ctx)
		if err != nil {
			return err
		}
	case modePipe:
		// Pipe mode (from truffle): no instance type given and stdin is piped.
		truffleInput, err := input.ParseFromStdin()
		if err != nil {
			return i18n.Te("error.input_parse_failed", err)
		}
		config, err = buildLaunchConfig(truffleInput)
		if err != nil {
			return err
		}
	default: // modeFlags
		// Explicit --instance-type; stdin is ignored (TTY or pipe).
		config, err = buildLaunchConfig(nil)
		if err != nil {
			return err
		}
	}

	// Apply custom --tag key=value tags to the instance + its created volumes,
	// so ephemeral spores and their --attach-volume data volumes are attributable
	// in Cost Explorer / cleanup scripts (#161). Set first so spawn-managed tags
	// (team, strata, the buildTags baseline) take precedence over user tags.
	if len(launchTags) > 0 {
		userTags, err := parseKVTags(launchTags)
		if err != nil {
			return err
		}
		if config.Tags == nil {
			config.Tags = make(map[string]string)
		}
		for k, v := range userTags {
			config.Tags[k] = v
		}
	}

	// Apply team tags if --team specified
	if launchTeamID != "" {
		if config.Tags == nil {
			config.Tags = make(map[string]string)
		}
		config.Tags["spawn:team-id"] = launchTeamID
		// Resolve team name from DynamoDB for the human-readable tag
		if teamName, err := resolveTeamName(ctx, launchTeamID); err == nil && teamName != "" {
			config.Tags["spawn:team-name"] = teamName
		}
	}

	// Strata software environment selection
	if strataFormation != "" || strataProfile != "" {
		fmt.Fprintf(os.Stderr, "Resolving Strata environment...\n")
		uri, err := resolveStrataEnvironment(ctx, strataFormation, strataProfile, strataRegistry)
		if err != nil {
			return fmt.Errorf("strata: %w", err)
		}
		config.Tags["strata:lockfile-s3-uri"] = uri
		fmt.Fprintf(os.Stderr, "Strata environment resolved: %s\n", uri)
	}

	// Validate
	if config.Name == "" {
		return fmt.Errorf("--name is required: give your spore a name (e.g. --name my-worker)")
	}
	if config.InstanceType == "" {
		// Windows with --os windows is defaulted to m7i.xlarge earlier (before
		// launchMode), so an empty type here is a genuine non-Windows omission.
		return i18n.Te("error.instance_type_required", nil)
	}

	// Auto-detect region if not specified
	if config.Region == "" {
		fmt.Fprintf(os.Stderr, "🌍 No region specified, auto-detecting closest region...\n")
		detectedRegion, err := detectBestRegion(ctx, config.InstanceType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not auto-detect region: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Using default: us-east-1\n")
			config.Region = "us-east-1"
		} else {
			fmt.Fprintf(os.Stderr, "✓ Selected region: %s\n", detectedRegion)
			config.Region = detectedRegion
		}
	}

	// Initialize AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return i18n.Te("error.aws_client_init", err)
	}

	// Determine compliance mode from flags
	if cmd.Flags().Changed("nist-800-171") {
		complianceMode = "nist-800-171"
	} else if cmd.Flags().Changed("nist-800-53") {
		val, _ := cmd.Flags().GetString("nist-800-53")
		if val == "" {
			val = "low"
		}
		complianceMode = fmt.Sprintf("nist-800-53-%s", val)
	}
	if cmd.Flags().Changed("compliance-strict") {
		complianceStrict, _ = cmd.Flags().GetBool("compliance-strict")
	}

	// Load compliance configuration and validate if enabled
	if complianceMode != "" {
		complianceConfig, err := spawnconfig.LoadComplianceConfig(ctx, complianceMode, complianceStrict)
		if err != nil {
			return fmt.Errorf("failed to load compliance config: %w", err)
		}

		infraConfig, err := spawnconfig.LoadInfrastructureConfig(ctx, "")
		if err != nil {
			return fmt.Errorf("failed to load infrastructure config: %w", err)
		}

		// Apply compliance enforcement to launch config
		validator := compliance.NewValidator(complianceConfig, infraConfig)
		if err := validator.EnforceLaunchConfig(config); err != nil {
			return fmt.Errorf("failed to enforce compliance: %w", err)
		}

		// Mark compliance mode in config for enforcement in aws client
		config.EBSEncrypted = complianceConfig.EnforceEncryptedEBS
		config.IMDSv2Enforced = complianceConfig.EnforceIMDSv2
		config.IMDSv2HopLimit = 1

		// Validate launch configuration
		result, err := validator.ValidateLaunchConfig(ctx, config)
		if err != nil {
			return fmt.Errorf("compliance validation failed: %w", err)
		}

		// Handle validation warnings
		if result.HasWarnings() {
			for _, warning := range result.Warnings {
				fmt.Fprintf(os.Stderr, "⚠️  %s\n", warning)
			}
		}

		// Handle validation violations
		if result.HasViolations() {
			if validator.IsStrictMode() {
				// Strict mode: fail on violations
				fmt.Fprintf(os.Stderr, "\n❌ Compliance validation failed (%d violations):\n", len(result.Violations))
				for _, violation := range result.Violations {
					fmt.Fprintf(os.Stderr, "  [%s] %s: %s\n", violation.ControlID, violation.ControlName, violation.Description)
				}
				return fmt.Errorf("compliance validation failed in strict mode")
			} else {
				// Non-strict mode: show warnings but continue
				fmt.Fprintf(os.Stderr, "\n⚠️  Compliance warnings (%d):\n", len(result.Violations))
				for _, violation := range result.Violations {
					fmt.Fprintf(os.Stderr, "  [%s] %s: %s\n", violation.ControlID, violation.ControlName, violation.Description)
				}
				fmt.Fprintf(os.Stderr, "\nContinuing launch with warnings. Use --compliance-strict to fail on violations.\n\n")
			}
		}

		// Show compliance summary
		if !quiet {
			fmt.Fprintf(os.Stderr, "\n✓ Compliance mode: %s\n", complianceConfig.GetModeDisplayName())
			if config.EBSEncrypted {
				fmt.Fprintf(os.Stderr, "✓ EBS encryption: enforced\n")
			}
			if config.IMDSv2Enforced {
				fmt.Fprintf(os.Stderr, "✓ IMDSv2: enforced\n")
			}
			fmt.Fprintf(os.Stderr, "\n")
		}
	}

	// CRITICAL SAFETY CHECK: Prevent zombie instances
	// If neither --ttl nor --idle-timeout are set, default to 1h idle timeout
	// This prevents instances from running indefinitely if CLI disconnects
	// CRITICAL SAFETY CHECK: Prevent zombie instances.
	// If neither --ttl nor --idle-timeout are set, default to 1h idle timeout so
	// an instance can't run indefinitely if the CLI disconnects.
	if config.TTL == "" && config.IdleTimeout == "" && !noTimeout {
		config.IdleTimeout = "1h"
		fmt.Fprintf(os.Stderr, "\n⚠️  Auto-setting --idle-timeout=1h to prevent zombie instances\n")
		fmt.Fprintf(os.Stderr, "   Instance will terminate after 1 hour of inactivity.\n")
		fmt.Fprintf(os.Stderr, "   Override with --ttl, --idle-timeout, or --no-timeout\n")
		fmt.Fprintf(os.Stderr, "   See: https://github.com/spore-host/spawn/blob/main/docs/lifecycle.md\n\n")
	} else if noTimeout {
		// User explicitly disabled timeout - warn about zombie risk
		fmt.Fprintf(os.Stderr, "\n⚠️  WARNING: --no-timeout specified\n")
		fmt.Fprintf(os.Stderr, "   Instance will run indefinitely until manually terminated.\n")
		fmt.Fprintf(os.Stderr, "   If CLI disconnects, you must track and terminate manually.\n")
		fmt.Fprintf(os.Stderr, "   This can result in unexpected costs from zombie instances.\n\n")
	}

	// --estimate-only: a true dry-run. Run the same pre-flight instance-type
	// constraint validation a real launch does (#110), BEFORE the cost estimate,
	// so a config that couldn't launch (e.g. --efa on a non-EFA type) surfaces the
	// same actionable error instead of a misleading "estimate complete" (#124).
	if estimateOnly {
		if err := preflightInstanceConstraints(ctx, awsClient, config, mpiEnabled, efaEnabled, hibernate || hibernateOnIdle); err != nil {
			return err
		}
		odPrice := pricing.GetEC2HourlyRate(config.Region, config.InstanceType)
		if odPrice == 0 {
			fmt.Fprintf(os.Stderr, "💰 Cost estimate: pricing data unavailable for %s in %s\n", config.InstanceType, config.Region)
		} else {
			fmt.Fprintf(os.Stderr, "💰 Cost estimate for %s in %s\n", config.InstanceType, config.Region)
			fmt.Fprintf(os.Stderr, "   On-demand:  $%.4f/hr\n", odPrice)
			if config.TTL != "" {
				if d, err := time.ParseDuration(config.TTL); err == nil {
					fmt.Fprintf(os.Stderr, "   TTL cost:   $%.2f (%.0f hr)\n", odPrice*d.Hours(), d.Hours())
				}
			}
		}
		fmt.Fprintf(os.Stderr, "✅ Estimate complete — no instances launched (--estimate-only)\n")
		return nil
	}

	// Launch with progress display
	return launchWithProgress(ctx, awsClient, config, plat, auditLog)
}

func launchParameterSweep(ctx context.Context, baseConfig *aws.LaunchConfig, plat *platform.Platform, auditLog *audit.AuditLogger) error {
	// Validate mutually exclusive flags
	if detach && noDetach {
		return fmt.Errorf("--detach and --no-detach are mutually exclusive")
	}

	// Validate workflow integration flags
	if wait && !detach {
		return fmt.Errorf("--wait requires --detach (only works with Lambda orchestration)")
	}

	// Parse parameter file
	var paramFormat *ParamFileFormat
	var err error

	if paramFile != "" {
		paramFormat, err = parseParamFile(paramFile)
		if err != nil {
			return fmt.Errorf("failed to parse parameter file: %w", err)
		}
	} else if params != "" {
		// TODO: Parse inline JSON params
		return fmt.Errorf("inline --params not yet implemented, use --param-file for now")
	} else {
		return fmt.Errorf("either --param-file or --params must be specified for parameter sweep")
	}

	// AUTO-ENABLE DETACHED MODE for parameter sweeps to prevent zombie instances
	// If the CLI disconnects (laptop sleep/shutdown), detached mode ensures:
	// - Sweep state persists in DynamoDB
	// - Lambda continues orchestration
	// - User can resume monitoring with 'spawn sweep status <sweep-id>'
	if !detach && !noDetach {
		detach = true
		fmt.Fprintf(os.Stderr, "\n⚠️  Auto-enabling --detach for parameter sweep\n")
		fmt.Fprintf(os.Stderr, "   This prevents zombie instances if CLI disconnects.\n")
		fmt.Fprintf(os.Stderr, "   Resume monitoring with: spawn sweep status <sweep-id>\n")

		// If maxConcurrent is 0 (launch all at once), set a reasonable default
		if maxConcurrent == 0 {
			// Default to number of params or 10, whichever is less
			defaultConcurrent := len(paramFormat.Params)
			if defaultConcurrent > 10 {
				defaultConcurrent = 10
			}
			maxConcurrent = defaultConcurrent
			fmt.Fprintf(os.Stderr, "   Setting --max-concurrent=%d for controlled launch\n", maxConcurrent)
			fmt.Fprintf(os.Stderr, "   (Override with --max-concurrent=N if needed)\n")
		}
		fmt.Fprintf(os.Stderr, "\n")
	} else if noDetach {
		// User explicitly disabled detached mode - warn about zombie instances
		fmt.Fprintf(os.Stderr, "\n⚠️  WARNING: --no-detach specified\n")
		fmt.Fprintf(os.Stderr, "   If CLI disconnects (laptop sleep/shutdown), instances may become zombies.\n")
		if ttl == "" && idleTimeout == "" {
			fmt.Fprintf(os.Stderr, "\n❌ ERROR: --no-detach requires --ttl or --idle-timeout to prevent zombie instances\n")
			return fmt.Errorf("--no-detach requires --ttl or --idle-timeout for safety")
		}
		fmt.Fprintf(os.Stderr, "   Using safeguards: ttl=%s, idle-timeout=%s\n\n", ttl, idleTimeout)
	}

	// Generate sweep ID
	name := sweepName
	if name == "" {
		name = "sweep"
	}
	sweepID := generateSweepID(name)

	// Write sweep ID to file for workflow integration
	if err := writeOutputID(sweepID, outputIDFile); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write sweep ID to file: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "\n🧪 Parameter Sweep: %s\n", sweepID)
	fmt.Fprintf(os.Stderr, "   Parameters: %d\n", len(paramFormat.Params))
	if maxConcurrent > 0 {
		fmt.Fprintf(os.Stderr, "   Max Concurrent: %d\n", maxConcurrent)
	} else {
		fmt.Fprintf(os.Stderr, "   Mode: All at once\n")
	}
	if detach {
		fmt.Fprintf(os.Stderr, "   Orchestration: Lambda (detached)\n")
	}
	fmt.Fprintf(os.Stderr, "\n")

	// Initialize AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}

	// Check for detached mode (Lambda orchestration)
	if detach && maxConcurrent > 0 {
		return launchSweepDetached(ctx, paramFormat, baseConfig, sweepID, name, maxConcurrent, launchDelay)
	}

	// Build launch configs for each parameter set
	launchConfigs := make([]*aws.LaunchConfig, 0, len(paramFormat.Params))
	for i, paramSet := range paramFormat.Params {
		config, err := buildLaunchConfigFromParams(paramFormat.Defaults, paramSet, sweepID, name, i, len(paramFormat.Params))
		if err != nil {
			return fmt.Errorf("failed to build launch config for parameter set %d: %w", i, err)
		}

		// Copy base config fields that weren't in params
		if config.Region == "" {
			config.Region = baseConfig.Region
		}
		if config.Name == "" {
			config.Name = fmt.Sprintf("%s-%d", name, i)
		}

		launchConfigs = append(launchConfigs, &config)
	}

	// Setup common resources (AMI, SSH key, IAM role) using first config as template.
	// In JSON mode, suppress the TUI so stdout carries only the JSON array (#21).
	var prog *progress.Progress
	if getOutputFormat() == "json" {
		prog = progress.NewQuietProgress()
	} else {
		prog = progress.NewProgress()
	}

	firstConfig := launchConfigs[0]

	// Auto-detect region if not specified
	if firstConfig.Region == "" {
		fmt.Fprintf(os.Stderr, "🌍 No region specified, auto-detecting closest region...\n")
		detectedRegion, err := detectBestRegion(ctx, firstConfig.InstanceType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not auto-detect region: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Using default: us-east-1\n")
			firstConfig.Region = "us-east-1"
		} else {
			fmt.Fprintf(os.Stderr, "✓ Selected region: %s\n", detectedRegion)
			firstConfig.Region = detectedRegion
		}
	}

	// Apply region to all configs
	for _, cfg := range launchConfigs {
		if cfg.Region == "" {
			cfg.Region = firstConfig.Region
		}
	}

	// Step 1: Detect AMI
	prog.Start("Detecting AMI")
	if firstConfig.AMI == "" {
		ami, err := awsClient.GetRecommendedAMI(ctx, firstConfig.Region, firstConfig.InstanceType)
		if err != nil {
			prog.Error("Detecting AMI", err)
			return err
		}
		// Apply AMI to all configs that don't have one
		for _, cfg := range launchConfigs {
			if cfg.AMI == "" {
				cfg.AMI = ami
			}
		}
	}
	prog.Complete("Detecting AMI")

	// Resolve target OS once and apply to every config in the sweep; enforce the
	// Windows lifecycle guard before launching any.
	targetOS := resolveTargetOS(ctx, awsClient, firstConfig.Region, firstConfig.AMI, osFlag)
	for _, cfg := range launchConfigs {
		cfg.TargetOS = targetOS
	}
	if err := windowsLifecycleGuard(firstConfig); err != nil {
		return err
	}

	// Step 2: Setup SSH key
	prog.Start("Setting up SSH key")
	if firstConfig.KeyName == "" {
		keyName, err := setupSSHKey(ctx, awsClient, firstConfig.Region, firstConfig.AMI, plat)
		if err != nil {
			prog.Error("Setting up SSH key", err)
			return err
		}
		// Apply key to all configs
		for _, cfg := range launchConfigs {
			if cfg.KeyName == "" {
				cfg.KeyName = keyName
			}
		}
	}
	prog.Complete("Setting up SSH key")

	// Step 3: Setup IAM role
	prog.Start("Setting up IAM role")
	if firstConfig.IamInstanceProfile == "" {
		instanceProfile, err := awsClient.SetupSporedIAMRole(ctx)
		if err != nil {
			prog.Error("Setting up IAM role", err)
			return err
		}
		// Apply IAM role to all configs
		for _, cfg := range launchConfigs {
			if cfg.IamInstanceProfile == "" {
				cfg.IamInstanceProfile = instanceProfile
			}
		}
	}
	prog.Complete("Setting up IAM role")

	// CRITICAL SAFETY CHECK: Apply timeout defaults to all sweep configs
	hasDefaultApplied := false
	for _, cfg := range launchConfigs {
		if cfg.TTL == "" && cfg.IdleTimeout == "" && !noTimeout {
			cfg.IdleTimeout = "1h"
			hasDefaultApplied = true
		}
	}
	if hasDefaultApplied {
		fmt.Fprintf(os.Stderr, "\n⚠️  Auto-setting --idle-timeout=1h for all sweep instances\n")
		fmt.Fprintf(os.Stderr, "   Instances will terminate after 1 hour of inactivity.\n")
		fmt.Fprintf(os.Stderr, "   Override with --ttl, --idle-timeout, or --no-timeout\n\n")
	} else if noTimeout {
		fmt.Fprintf(os.Stderr, "\n⚠️  WARNING: --no-timeout specified for sweep\n")
		fmt.Fprintf(os.Stderr, "   Instances will run indefinitely until manually terminated.\n\n")
	}

	// Build user-data for each config. (Storage mounts aren't wired through this
	// sweep path; attach-volume on sweeps would thread a storageScript here.)
	for _, cfg := range launchConfigs {
		userDataScript, err := buildUserData(plat, cfg, "")
		if err != nil {
			return fmt.Errorf("failed to build user data: %w", err)
		}
		cfg.UserData = encodeUserDataForOS(userDataScript, cfg.TargetOS)
	}

	// Launch instances with rolling queue or all at once
	var launchedInstances []*aws.LaunchResult
	var failures []string
	var successCount int

	if maxConcurrent > 0 && maxConcurrent < len(launchConfigs) {
		// Rolling queue mode
		launchedInstances, failures, successCount, err = launchWithRollingQueue(ctx, awsClient, launchConfigs, sweepID, name, maxConcurrent, launchDelay)
		if err != nil {
			return err
		}
	} else {
		// All-at-once mode (maxConcurrent == 0 or >= total params)
		fmt.Fprintf(os.Stderr, "\n🚀 Launching %d instances in parallel...\n\n", len(launchConfigs))
		launchedInstances, failures, successCount = launchAllAtOnce(ctx, awsClient, launchConfigs)
	}

	// Handle failures
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Some instances failed to launch:\n")
		for _, failure := range failures {
			fmt.Fprintf(os.Stderr, "   • %s\n", failure)
		}
		return fmt.Errorf("%d/%d instances failed to launch", len(failures), len(launchConfigs))
	}

	// Save sweep state
	state := &SweepState{
		SweepID:       sweepID,
		SweepName:     name,
		CreatedAt:     time.Now(),
		ParamFile:     paramFile,
		TotalParams:   len(paramFormat.Params),
		MaxConcurrent: maxConcurrent,
		LaunchDelay:   launchDelay,
		Completed:     0,
		Running:       successCount,
		Pending:       0,
		Failed:        0,
		Instances:     make([]InstanceState, 0, successCount),
	}

	for i, instance := range launchedInstances {
		if instance != nil {
			state.Instances = append(state.Instances, InstanceState{
				Index:      i,
				InstanceID: instance.InstanceID,
				State:      "running",
				LaunchedAt: time.Now(),
			})
		}
	}

	if err := saveSweepState(state); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to save sweep state: %v\n", err)
	}

	// Display success
	fmt.Fprintf(os.Stderr, "\n✅ Parameter sweep launched successfully!\n\n")
	fmt.Fprintf(os.Stderr, "Sweep ID:   %s\n", sweepID)
	fmt.Fprintf(os.Stderr, "Sweep Name: %s\n", name)
	fmt.Fprintf(os.Stderr, "Instances:  %d\n\n", successCount)

	fmt.Fprintf(os.Stderr, "Instances:\n")
	for _, instance := range launchedInstances {
		if instance != nil {
			fmt.Fprintf(os.Stderr, "  • %s (%s) - %s\n", instance.Name, instance.InstanceID, instance.State)
		}
	}

	fmt.Fprintf(os.Stderr, "\nTo view sweep status:\n")
	fmt.Fprintf(os.Stderr, "  spawn list --sweep-id %s\n", sweepID)

	return nil
}

// launchAllAtOnce launches all instances in parallel (no rolling queue)
func launchAllAtOnce(ctx context.Context, awsClient *aws.Client, launchConfigs []*aws.LaunchConfig) ([]*aws.LaunchResult, []string, int) {
	type launchResult struct {
		index  int
		result *aws.LaunchResult
		err    error
	}

	resultsChan := make(chan launchResult, len(launchConfigs))
	var wg sync.WaitGroup

	for i, cfg := range launchConfigs {
		wg.Add(1)
		go func(idx int, config *aws.LaunchConfig) {
			defer wg.Done()
			result, err := awsClient.Launch(ctx, *config)
			resultsChan <- launchResult{index: idx, result: result, err: err}
		}(i, cfg)
	}

	// Wait for all launches
	wg.Wait()
	close(resultsChan)

	// Collect results
	launchedInstances := make([]*aws.LaunchResult, len(launchConfigs))
	var failures []string
	successCount := 0

	for result := range resultsChan {
		if result.err != nil {
			failures = append(failures, fmt.Sprintf("Parameter set %d: %v", result.index, result.err))
		} else {
			launchedInstances[result.index] = result.result
			successCount++
			fmt.Fprintf(os.Stderr, "✓ Launched %s (parameter set %d/%d)\n", result.result.Name, result.index+1, len(launchConfigs))
		}
	}

	return launchedInstances, failures, successCount
}

// launchWithRollingQueue launches instances with rolling queue orchestration
func launchWithRollingQueue(ctx context.Context, awsClient *aws.Client, launchConfigs []*aws.LaunchConfig, sweepID, sweepName string, maxConcurrent int, launchDelay string) ([]*aws.LaunchResult, []string, int, error) {
	fmt.Fprintf(os.Stderr, "\n🚀 Launching parameter sweep with rolling queue...\n")
	fmt.Fprintf(os.Stderr, "   Max concurrent: %d\n", maxConcurrent)
	fmt.Fprintf(os.Stderr, "   Launch delay: %s\n\n", launchDelay)

	// Parse launch delay
	delay, err := time.ParseDuration(launchDelay)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("invalid launch delay %q: %w", launchDelay, err)
	}

	// Initialize tracking
	launchedInstances := make([]*aws.LaunchResult, len(launchConfigs))
	var failures []string
	successCount := 0

	// Track active instances (index -> instance ID)
	activeInstances := make(map[int]string)
	nextToLaunch := 0

	// Launch first batch
	initialBatch := maxConcurrent
	if initialBatch > len(launchConfigs) {
		initialBatch = len(launchConfigs)
	}

	fmt.Fprintf(os.Stderr, "Launching initial batch of %d instances...\n", initialBatch)
	for i := 0; i < initialBatch; i++ {
		result, err := awsClient.Launch(ctx, *launchConfigs[i])
		if err != nil {
			failures = append(failures, fmt.Sprintf("Parameter set %d: %v", i, err))
		} else {
			launchedInstances[i] = result
			activeInstances[i] = result.InstanceID
			successCount++
			fmt.Fprintf(os.Stderr, "✓ Launched %s (parameter set %d/%d)\n", result.Name, i+1, len(launchConfigs))
		}

		// Apply launch delay between initial launches
		if i < initialBatch-1 && delay > 0 {
			time.Sleep(delay)
		}
	}
	nextToLaunch = initialBatch

	// Save initial state
	state := &SweepState{
		SweepID:       sweepID,
		SweepName:     sweepName,
		CreatedAt:     time.Now(),
		ParamFile:     paramFile,
		TotalParams:   len(launchConfigs),
		MaxConcurrent: maxConcurrent,
		LaunchDelay:   launchDelay,
		Completed:     0,
		Running:       len(activeInstances),
		Pending:       len(launchConfigs) - nextToLaunch,
		Failed:        len(failures),
		Instances:     make([]InstanceState, 0, len(launchConfigs)),
	}

	for i, instance := range launchedInstances {
		if instance != nil {
			state.Instances = append(state.Instances, InstanceState{
				Index:      i,
				InstanceID: instance.InstanceID,
				State:      "running",
				LaunchedAt: time.Now(),
			})
		}
	}

	if err := saveSweepState(state); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to save sweep state: %v\n", err)
	}

	// Rolling queue: poll for completions and launch next
	if nextToLaunch < len(launchConfigs) {
		fmt.Fprintf(os.Stderr, "\nMonitoring instances and launching next in queue...\n")
		fmt.Fprintf(os.Stderr, "Press Ctrl-C to stop (sweep can be resumed later)\n\n")

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for nextToLaunch < len(launchConfigs) {
			select {
			case <-ctx.Done():
				fmt.Fprintf(os.Stderr, "\n⚠️  Interrupted. Progress saved to sweep state.\n")
				fmt.Fprintf(os.Stderr, "Resume with: spawn resume --sweep-id %s\n", sweepID)
				return launchedInstances, failures, successCount, ctx.Err()

			case <-ticker.C:
				// Query instance states
				instanceIDs := make([]string, 0, len(activeInstances))
				for _, id := range activeInstances {
					instanceIDs = append(instanceIDs, id)
				}

				if len(instanceIDs) == 0 {
					continue
				}

				// Get instance states
				instances, err := awsClient.ListInstances(ctx, launchConfigs[0].Region, "")
				if err != nil {
					fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to query instance states: %v\n", err)
					continue
				}

				// Build state map
				stateMap := make(map[string]string)
				for _, inst := range instances {
					stateMap[inst.InstanceID] = inst.State
				}

				// Check for terminated instances
				var toRemove []int
				for idx, instID := range activeInstances {
					state, exists := stateMap[instID]
					if !exists || state == "terminated" || state == "stopping" || state == "stopped" {
						toRemove = append(toRemove, idx)
					}
				}

				// Remove terminated instances and launch next
				for _, idx := range toRemove {
					delete(activeInstances, idx)

					// Wait launch delay if specified
					if delay > 0 {
						time.Sleep(delay)
					}

					// Launch next pending instance
					if nextToLaunch < len(launchConfigs) {
						result, err := awsClient.Launch(ctx, *launchConfigs[nextToLaunch])
						if err != nil {
							failures = append(failures, fmt.Sprintf("Parameter set %d: %v", nextToLaunch, err))
							fmt.Fprintf(os.Stderr, "✗ Failed to launch parameter set %d: %v\n", nextToLaunch+1, err)
						} else {
							launchedInstances[nextToLaunch] = result
							activeInstances[nextToLaunch] = result.InstanceID
							successCount++
							fmt.Fprintf(os.Stderr, "✓ Launched %s (parameter set %d/%d) [%d active, %d pending]\n",
								result.Name, nextToLaunch+1, len(launchConfigs),
								len(activeInstances), len(launchConfigs)-nextToLaunch-1)

							// Update state file
							state.Running = len(activeInstances)
							state.Pending = len(launchConfigs) - nextToLaunch - 1
							state.Failed = len(failures)
							state.Instances = append(state.Instances, InstanceState{
								Index:      nextToLaunch,
								InstanceID: result.InstanceID,
								State:      "running",
								LaunchedAt: time.Now(),
							})

							if err := saveSweepState(state); err != nil {
								fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to save sweep state: %v\n", err)
							}
						}
						nextToLaunch++
					}
				}
			}
		}

		fmt.Fprintf(os.Stderr, "\n✅ All instances launched. Waiting for final batch to complete...\n")
	}

	return launchedInstances, failures, successCount, nil
}

func launchWithProgress(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, plat *platform.Platform, auditLog *audit.AuditLogger) error {
	// In JSON mode, suppress the TUI so stdout carries only the JSON object (#21).
	var prog *progress.Progress
	if getOutputFormat() == "json" {
		prog = progress.NewQuietProgress()
	} else {
		prog = progress.NewProgress()
	}

	// Step 1: Detect AMI
	prog.Start("Detecting AMI")
	// "" or "auto" both mean auto-detect the latest AL2023 AMI (#342).
	if config.AMI == "" || strings.EqualFold(config.AMI, "auto") {
		ami, err := awsClient.GetRecommendedAMI(ctx, config.Region, config.InstanceType)
		if err != nil {
			prog.Error("Detecting AMI", err)
			return err
		}
		config.AMI = ami
	}
	prog.Complete("Detecting AMI")

	// Step 1b: Resolve target OS (--os override, else auto-detect from the AMI)
	// and enforce the Windows lifecycle guard before we spend on a launch.
	config.TargetOS = resolveTargetOS(ctx, awsClient, config.Region, config.AMI, osFlag)
	if err := windowsLifecycleGuard(config); err != nil {
		return err
	}
	// Reject burstable types for Windows (catches auto-detected Windows where the
	// early --os default didn't apply); spend nothing on a launch that'd crawl (#95).
	if err := guardWindowsInstanceType(config.TargetOS, config.InstanceType); err != nil {
		return err
	}

	// Reject --nested-virtualization on an instance type that can't do it, before
	// spending on a launch RunInstances would reject cryptically (#91).
	if config.NestedVirtualization {
		if err := awsClient.ValidateInstanceTypeForNestedVirtualization(ctx, config.InstanceType, config.Region); err != nil {
			return err
		}
	}

	// Pre-flight: validate instance-type feature constraints (MPI placement group,
	// EFA, hibernation) BEFORE creating any AWS resources (IAM role, security
	// group), so an unsupported combination fails fast with an actionable message
	// instead of cryptically after several API calls (#110).
	if err := preflightInstanceConstraints(ctx, awsClient, config, mpiEnabled, efaEnabled, hibernate || hibernateOnIdle); err != nil {
		return err
	}

	// Step 2: Setup SSH key
	prog.Start("Setting up SSH key")
	if config.KeyName == "" {
		keyName, err := setupSSHKey(ctx, awsClient, config.Region, config.AMI, plat)
		if err != nil {
			prog.Error("Setting up SSH key", err)
			return err
		}
		config.KeyName = keyName
	}
	prog.Complete("Setting up SSH key")

	// Step 3: Setup IAM instance profile
	prog.Start("Setting up IAM role")
	if config.IamInstanceProfile == "" {
		// Check if user specified custom IAM configuration
		if iamRole != "" || len(iamPolicy) > 0 || len(iamManagedPolicies) > 0 || iamPolicyFile != "" {
			// User-specified IAM configuration
			iamConfig := aws.IAMRoleConfig{
				RoleName:        iamRole,
				Policies:        iamPolicy,
				ManagedPolicies: iamManagedPolicies,
				PolicyFile:      iamPolicyFile,
				TrustServices:   iamTrustServices,
				Tags:            parseIAMRoleTags(iamRoleTags),
			}

			instanceProfile, err := awsClient.CreateOrGetInstanceProfile(ctx, iamConfig)
			if err != nil {
				prog.Error("Setting up IAM role", err)
				auditLog.LogOperation("create_iam_role", iamConfig.RoleName, "failed", err)
				return fmt.Errorf("failed to create IAM instance profile: %w", err)
			}
			config.IamInstanceProfile = instanceProfile
			auditLog.LogOperationWithData("create_iam_role", iamConfig.RoleName, "success",
				map[string]interface{}{
					"instance_profile": instanceProfile,
				}, nil)
		} else {
			// Default: use spored IAM role
			instanceProfile, err := awsClient.SetupSporedIAMRole(ctx)
			if err != nil {
				prog.Error("Setting up IAM role", err)
				auditLog.LogOperation("create_iam_role", "spored-instance-role", "failed", err)
				return err
			}
			config.IamInstanceProfile = instanceProfile
			auditLog.LogOperation("create_iam_role", "spored-instance-role", "success", nil)
		}
	}
	prog.Complete("Setting up IAM role")

	// Step 4: Security group (create for MPI if needed)
	if mpiEnabled {
		prog.Start("Creating MPI security group")
		// Get default VPC
		vpcID, err := awsClient.GetDefaultVPC(ctx, config.Region)
		if err != nil {
			prog.Error("Creating MPI security group", err)
			return fmt.Errorf("failed to get default VPC: %w", err)
		}

		// Create or get MPI security group
		sgName := fmt.Sprintf("spawn-mpi-%s", jobArrayName)
		sgID, err := awsClient.CreateOrGetMPISecurityGroup(ctx, config.Region, vpcID, sgName)
		if err != nil {
			prog.Error("Creating MPI security group", err)
			auditLog.LogOperationWithRegion("create_security_group", sgName, config.Region, "failed", err)
			return fmt.Errorf("failed to create MPI security group: %w", err)
		}

		config.SecurityGroupIDs = []string{sgID}
		auditLog.LogOperationWithData("create_security_group", sgName, "success",
			map[string]interface{}{
				"security_group_id": sgID,
				"region":            config.Region,
			}, nil)
		prog.Complete("Creating MPI security group")
	} else if config.TargetOS == "windows" && len(config.SecurityGroupIDs) == 0 {
		// Windows needs RDP (3389) + SSH (22); the default SG won't open 3389, so
		// RDP would be impossible (#95). Create a managed Windows SG.
		prog.Start("Creating Windows security group")
		vpcID, err := awsClient.GetDefaultVPC(ctx, config.Region)
		if err != nil {
			prog.Error("Creating Windows security group", err)
			return fmt.Errorf("failed to get default VPC: %w", err)
		}
		if allowCIDR == "" || allowCIDR == "0.0.0.0/0" {
			fmt.Fprintf(os.Stderr, "⚠️  Opening RDP (3389) + SSH (22) to 0.0.0.0/0; restrict with --allow-cidr <your-ip>/32.\n")
		}
		sgName := fmt.Sprintf("spawn-windows-%s", config.Name)
		sgID, err := awsClient.CreateOrGetWindowsSecurityGroup(ctx, config.Region, vpcID, sgName, allowCIDR)
		if err != nil {
			prog.Error("Creating Windows security group", err)
			auditLog.LogOperationWithRegion("create_security_group", sgName, config.Region, "failed", err)
			return fmt.Errorf("failed to create Windows security group: %w", err)
		}
		config.SecurityGroupIDs = []string{sgID}
		auditLog.LogOperationWithData("create_security_group", sgName, "success",
			map[string]interface{}{"security_group_id": sgID, "region": config.Region}, nil)
		prog.Complete("Creating Windows security group")
	} else {
		prog.Skip("Creating security group")
	}

	// Step 4.5: Create or get FSx Lustre filesystem
	var fsxInfo *aws.FSxInfo
	var err error

	// For an EPHEMERAL --fsx-create, the filesystem is created AFTER the launch
	// succeeds (#213) — a capacity-failed launch then never creates an FSx, so
	// there's no orphan and no create/teardown churn on retries (#210). We prepare
	// the resolved config here (validated up front) and carry it to the post-launch
	// step. Durable FSx is still created up front below (a deliberate persisted
	// resource). nil = no ephemeral FSx to create post-launch.
	var pendingFSxConfig *aws.FSxConfig
	var pendingFSxImportPath, pendingFSxExportPath, pendingFSxMountPoint string

	if fsxCreate {
		prog.Start("Creating FSx Lustre filesystem")

		// Generate stack name
		stackName := jobArrayName
		if stackName == "" {
			stackName = name
		}
		if stackName == "" {
			stackName = "fsx"
		}

		// Set import/export paths if not specified
		importPath := fsxImportPath
		if importPath == "" {
			importPath = fmt.Sprintf("s3://%s/", fsxS3Bucket)
		}

		exportPath := fsxExportPath
		if exportPath == "" {
			exportPath = fmt.Sprintf("s3://%s/", fsxS3Bucket)
		}

		throughput := fsxThroughput
		if throughput == 0 {
			throughput = 125 // PERSISTENT_2 minimum; valid values: 125, 250, 500, 1000
		}
		fsxConfig := aws.FSxConfig{
			StackName:                stackName,
			Region:                   config.Region,
			StorageCapacity:          fsxStorageCapacity,
			PerUnitStorageThroughput: throughput,
			S3Bucket:                 fsxS3Bucket,
			ImportPath:               importPath,
			ExportPath:               exportPath,
			AutoCreateBucket:         true,
			SecurityGroupIDs:         config.SecurityGroupIDs,
			Lifecycle:                fsxLifecycle,
		}
		// Co-locate the FSx with the instance's AZ (#194/#208). FSx Lustre is
		// single-AZ and a mounting instance MUST be in the same AZ as the
		// filesystem, so the FSx subnet has to match where the instance lands:
		//   1. An explicitly-pinned subnet wins.
		//   2. Else, if --az pinned the instance's AZ, resolve THAT AZ to a subnet
		//      — otherwise startFSxCreate falls back to subnets[0] of the default
		//      VPC, which silently ignores --az and can put the FSx in a different
		//      AZ than the instance (an unmountable cross-AZ FSx), and on accounts
		//      whose subnets[0] AZ doesn't offer PERSISTENT_2 makes every --az
		//      value fail identically with "not available in this availability
		//      zone" (#208).
		//   3. Else leave it empty: startFSxCreate's default-VPC fallback matches
		//      the instance's own unpinned placement.
		fsxConfig.SubnetID = config.SubnetID
		if aws.NeedsAZSubnetResolution(fsxConfig.SubnetID, config.AvailabilityZone) {
			subnetID, serr := awsClient.GetSubnetForAZ(ctx, config.Region, config.AvailabilityZone)
			if serr != nil {
				prog.Error("Creating FSx Lustre filesystem", serr)
				return fmt.Errorf("FSx create: could not find a subnet in --az %s to co-locate the filesystem with the instance: %w", config.AvailabilityZone, serr)
			}
			fsxConfig.SubnetID = subnetID
		}

		// Ensure Lustre ports on each SG used by both FSx and instances, up front
		// (needed by both the blocking and async paths). Without this the MGS
		// connection on 988 succeeds but follow-on dynamic-port traffic is blocked
		// ("client profile could not be read from MGS", #316).
		for _, sgID := range config.SecurityGroupIDs {
			if err := awsClient.EnsureLustrePorts(ctx, config.Region, sgID); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: could not ensure Lustre ports on %s: %v\n", sgID, err)
			}
		}

		if fsxLifecycle == "ephemeral" && count <= 1 {
			// Single-instance: DEFER creation until after the launch succeeds
			// (#213). Stash the resolved config and create the FSx post-launch, so a
			// capacity-failed launch never creates a filesystem (no orphan, no
			// per-retry churn — #210). The config is fully resolved/validated here;
			// only the CreateFileSystem call moves later. (Job arrays, count>1, keep
			// creating up front below — N instances share the one filesystem and the
			// array dispatch tags each via config.FSxPending, so it must exist before
			// dispatch.)
			cfgCopy := fsxConfig
			pendingFSxConfig = &cfgCopy
			pendingFSxImportPath = importPath
			pendingFSxExportPath = exportPath
			pendingFSxMountPoint = fsxMountPoint
			prog.Skip("Creating FSx Lustre filesystem (deferred until after launch)")
		} else if fsxLifecycle == "ephemeral" {
			// Job array (count>1): the shared ephemeral FSx must exist before the
			// array is dispatched (each instance is tagged spawn:fsx-pending from
			// config). Create it up front. (This path keeps the pre-#213 ordering;
			// the #210 reaper orphan-net is the backstop if the array dispatch fails.)
			prog.Start("Creating FSx Lustre filesystem (async)")
			fsID, cerr := awsClient.CreateFSxLustreFilesystemAsync(ctx, fsxConfig)
			if cerr != nil {
				prog.Error("Creating FSx Lustre filesystem (async)", cerr)
				return fmt.Errorf("failed to start FSx filesystem creation: %w", cerr)
			}
			config.FSxPending = fsID
			config.FSxImportPath = importPath
			config.FSxExportPath = exportPath
			if config.FSxMountPoint == "" {
				config.FSxMountPoint = fsxMountPoint
			}
			prog.Complete("Creating FSx Lustre filesystem (async)")
			fmt.Fprintf(os.Stderr, "   FSx %s is provisioning; array instances will mount it at %s once AVAILABLE (~10 min).\n", fsID, config.FSxMountPoint)
		} else {
			// durable: explicit death clock (#193), reaped on refcount-0 + TTL.
			if fsxLifecycle == "durable" && fsxTTL != "" {
				if d, derr := parseDuration(fsxTTL); derr == nil {
					fsxConfig.TTLDeadline = time.Now().Add(d)
				}
			}
			prog.Start("Creating FSx Lustre filesystem")
			fsxInfo, err = awsClient.CreateFSxLustreFilesystem(ctx, fsxConfig)
			if err != nil {
				prog.Error("Creating FSx Lustre filesystem", err)
				return fmt.Errorf("failed to create FSx filesystem: %w", err)
			}
			prog.Complete("Creating FSx Lustre filesystem")
		}

	} else if fsxID != "" && !fsxSkipValidate {
		prog.Start("Getting FSx filesystem info")

		fsxInfo, err = awsClient.GetFSxFilesystem(ctx, fsxID, config.Region)
		if err != nil {
			prog.Error("Getting FSx filesystem info", err)
			return fmt.Errorf("failed to get FSx info: %w", err)
		}

		prog.Complete("Getting FSx filesystem info")

	} else if fsxRecall != "" {
		prog.Start("Recalling FSx filesystem from S3")

		fsxInfo, err = awsClient.RecallFSxFilesystem(ctx, fsxRecall, config.Region)
		if err != nil {
			prog.Error("Recalling FSx filesystem", err)
			return fmt.Errorf("failed to recall FSx: %w", err)
		}

		prog.Complete("Recalling FSx filesystem from S3")
	} else {
		prog.Skip("FSx Lustre filesystem")
	}

	// Backfill FSx fields into config so buildTags writes spawn:fsx-id,
	// spawn:fsx-mount-name, and spawn:fsx-mount-point on every instance.
	// The --fsx-id path already sets config.FSxLustreID; the --fsx-create
	// and --fsx-recall paths only populated fsxInfo, not config (fixes #314).
	if fsxInfo != nil {
		config.FSxLustreID = fsxInfo.FileSystemID
		config.FSxMountName = fsxInfo.MountName
		if config.FSxMountPoint == "" {
			config.FSxMountPoint = fsxMountPoint
		}
	}

	// Step 4.6: Check data locality (region mismatches)
	if !skipRegionCheck && (efsID != "" || fsxID != "") {
		prog.Start("Checking data locality")

		fsxIDForCheck := ""
		if fsxID != "" {
			fsxIDForCheck = fsxID
		} else if fsxInfo != nil {
			fsxIDForCheck = fsxInfo.FileSystemID
		}

		awsCfg, err := awsClient.GetConfig(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to check data locality: %v\n", err)
			prog.Complete("Checking data locality")
		} else {
			warning, err := locality.CheckDataLocality(ctx, awsCfg, config.Region, efsID, fsxIDForCheck)
			if err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: Failed to check data locality: %v\n", err)
				prog.Complete("Checking data locality")
			} else if warning.HasMismatches {
				prog.Complete("Checking data locality")

				// Display warning
				fmt.Fprintf(os.Stderr, "%s", warning.FormatWarning())

				// Prompt for confirmation unless auto-approved
				if !autoYes {
					fmt.Fprintf(os.Stderr, "   Continue with cross-region launch? [y/N]: ")
					var response string
					_, _ = fmt.Scanln(&response)
					response = strings.ToLower(strings.TrimSpace(response))
					if response != "y" && response != "yes" {
						fmt.Fprintf(os.Stderr, "\n❌ Launch cancelled\n")
						return nil
					}
					fmt.Fprintf(os.Stderr, "\n")
				}
			} else {
				prog.Complete("Checking data locality")
			}
		}
	}

	// Build the storage-mount script (EFS / FSx / attached EBS volumes) FIRST, so
	// it can be injected BEFORE the user's script — the workload must see the
	// mounts already live; mounting after it finishes is useless (#166). Skip on
	// Windows: the storage user-data is Linux mount scripting and would corrupt
	// the <powershell> block. EFS/FSx on Windows is out of Phase 1 scope (#55).
	storageScript := ""
	if (efsID != "" || fsxInfo != nil || len(config.AttachVolumes) > 0) && config.TargetOS != "windows" {
		storageConfig := userdata.StorageConfig{}

		// EFS configuration
		if efsID != "" {
			mountOptions, err := getEFSMountOptions()
			if err != nil {
				return fmt.Errorf("failed to get EFS mount options: %w", err)
			}

			storageConfig.EFSEnabled = true
			storageConfig.EFSFilesystemDNS = aws.GetEFSDNSName(efsID, config.Region)
			storageConfig.EFSMountPoint = efsMountPoint
			storageConfig.EFSMountOptions = mountOptions
		}

		// FSx configuration
		if fsxInfo != nil {
			storageConfig.FSxLustreEnabled = true
			storageConfig.FSxFilesystemDNS = fsxInfo.DNSName
			storageConfig.FSxMountName = fsxInfo.MountName
			storageConfig.FSxMountPoint = fsxMountPoint
		}

		// Attached EBS data volumes from snapshots (#144)
		storageConfig.AttachedVolumes = attachedVolumesUserData(config.AttachVolumes)

		storageScript, err = userdata.GenerateStorageUserData(storageConfig)
		if err != nil {
			return fmt.Errorf("failed to generate storage user-data: %w", err)
		}
	}

	// Step 5: Build user data — the storage mount is injected before the user's
	// script (inside the bootstrap), not appended after it (#166).
	userDataScript, err := buildUserData(plat, config, storageScript)
	if err != nil {
		return fmt.Errorf("failed to build user data: %w", err)
	}

	config.UserData = encodeUserDataForOS(userDataScript, config.TargetOS)

	// Record the instance's primary user (Linux) so spored can run the pre-stop
	// hook as that user instead of root (#63). Windows has a single user and no
	// `su`, so it's left unset there.
	if config.TargetOS != "windows" {
		config.Username = plat.GetUsername()
	}

	// Validate MPI requirements
	if mpiEnabled {
		if count <= 1 {
			return fmt.Errorf("--mpi requires --count > 1 (need multiple nodes)")
		}
		if jobArrayName == "" {
			return fmt.Errorf("--mpi requires --job-array-name")
		}

		// Decide on a cluster placement group. HPC instance types (hpc6a/hpc7a/
		// hpc7g) don't support cluster placement groups — they get low-latency
		// networking from AWS HPC infrastructure — so --auto-placement-group
		// (on by default) must SKIP them gracefully rather than hard-fail (#104).
		// An explicitly requested --placement-group still errors if unsupported.
		tc := truffleaws.NewClientFromConfig(awsClient.Config())
		clusterPG := func() (bool, error) {
			caps, err := tc.GetCapabilities(ctx, config.InstanceType, config.Region)
			if err != nil {
				return false, err
			}
			return caps.ClusterPlacement, nil
		}
		if mpiPlacementGroup != "" {
			// Explicit request: must be supported (authoritative capability check).
			supported, err := clusterPG()
			if err != nil {
				return fmt.Errorf("placement group validation: %w", err)
			}
			if !supported {
				return fmt.Errorf("instance type %s does not support cluster placement groups (required for --placement-group)", config.InstanceType)
			}
		} else if mpiAutoPlacementGroup {
			supported, err := clusterPG()
			if err != nil {
				return fmt.Errorf("placement group validation: %w", err)
			}
			if supported {
				mpiPlacementGroup = fmt.Sprintf("spawn-mpi-%s", jobArrayName)
				fmt.Fprintf(os.Stderr, "Creating placement group: %s\n", mpiPlacementGroup)
				if err := awsClient.CreatePlacementGroup(ctx, mpiPlacementGroup, config.Region); err != nil {
					return fmt.Errorf("create placement group: %w", err)
				}
			} else {
				fmt.Fprintf(os.Stderr, "ℹ️  %s doesn't support cluster placement groups (HPC instance types use AWS HPC networking); skipping placement group.\n", config.InstanceType)
			}
		}

		// Set placement group in config
		if mpiPlacementGroup != "" {
			config.PlacementGroup = mpiPlacementGroup
		}

		// Validate EFA requirements
		if efaEnabled {
			// Validate in the launch region — some HPC instance types only exist
			// in specific regions and DescribeInstanceTypes returns InvalidInstanceType
			// when queried from a different region (fixes #307).
			if err := awsClient.ValidateInstanceTypeForEFAInRegion(ctx, config.InstanceType, config.Region); err != nil {
				return fmt.Errorf("EFA validation: %w", err)
			}

			// EFA works best with placement groups
			if !mpiAutoPlacementGroup && mpiPlacementGroup == "" {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: EFA works best with placement groups. Consider using --auto-placement-group\n")
			} else {
				fmt.Fprintf(os.Stderr, "✓ EFA enabled with placement group for optimal performance\n")
			}

			// Set EFA in config
			config.EFAEnabled = true
		}

		// Add MPI tags to config
		if config.Tags == nil {
			config.Tags = make(map[string]string)
		}
		config.Tags["spawn:mpi-enabled"] = "true"
		if mpiProcessesPerNode > 0 {
			config.Tags["spawn:mpi-processes-per-node"] = fmt.Sprintf("%d", mpiProcessesPerNode)
		}
	}

	// Check if job array mode (count > 1)
	if count > 1 {
		// Job array launch path
		if jobArrayName == "" {
			return fmt.Errorf("--job-array-name is required when --count > 1")
		}
		return launchJobArray(ctx, awsClient, config, plat, prog, fsxInfo, auditLog)
	}

	// Step 6: Launch instance
	prog.Start("Launching instance")
	auditLog.LogOperationWithData("launch_instance", "single", "initiated",
		map[string]interface{}{
			"instance_type": config.InstanceType,
			"region":        config.Region,
		}, nil)
	result, err := awsClient.Launch(ctx, *config)
	if err != nil {
		prog.Error("Launching instance", err)
		auditLog.LogOperationWithRegion("launch_instance", "single", config.Region, "failed", err)
		// No ephemeral-FSx teardown needed here (#213): the ephemeral FSx is now
		// created AFTER this launch succeeds (below), so a failed launch never
		// created one. A capacity failure leaves no billable resource by
		// construction — no orphan, no create/teardown churn on retries (#210).
		return err
	}
	auditLog.LogOperationWithData("launch_instance", result.InstanceID, "success",
		map[string]interface{}{
			"instance_type": config.InstanceType,
			"region":        config.Region,
		}, nil)
	prog.Complete("Launching instance")

	// Step 6.5: Now that the instance launched, create the ephemeral FSx and tag
	// the instance spawn:fsx-pending so spored waits → DRA → mounts once AVAILABLE
	// (#194). Created here (post-launch) so capacity failures cost nothing (#213).
	// If the create-or-tag fails, the FSx is torn down rather than leaked (#210
	// backstop); the reaper ephemeral-orphan net is the outer safety net.
	if pendingFSxConfig != nil {
		prog.Start("Creating FSx Lustre filesystem (async)")
		fsID, cerr := awsClient.CreateFSxLustreFilesystemAsync(ctx, *pendingFSxConfig)
		if cerr != nil {
			prog.Error("Creating FSx Lustre filesystem (async)", cerr)
			return fmt.Errorf("instance %s launched, but starting the ephemeral FSx failed: %w", result.InstanceID, cerr)
		}
		mountPoint := pendingFSxMountPoint
		if mountPoint == "" {
			mountPoint = "/fsx"
		}
		tags := map[string]string{
			"spawn:fsx-pending":     fsID,
			"spawn:fsx-mount-point": mountPoint,
		}
		if pendingFSxImportPath != "" {
			tags["spawn:fsx-s3-import-path"] = pendingFSxImportPath
		}
		if pendingFSxExportPath != "" {
			tags["spawn:fsx-s3-export-path"] = pendingFSxExportPath
		}
		if terr := awsClient.UpdateInstanceTags(ctx, config.Region, result.InstanceID, tags); terr != nil {
			// Tagging failed → spored will never see the pending FSx and the reaper
			// refcount will never count it, so delete it rather than leak it.
			if delErr := awsClient.DeleteFSxFilesystem(ctx, fsID, config.Region); delErr != nil {
				prog.Error("Creating FSx Lustre filesystem (async)", terr)
				return fmt.Errorf("instance %s launched but tagging it with pending FSx %s failed (%w) AND deleting the FSx failed — DELETE IT MANUALLY (aws fsx delete-file-system --file-system-id %s): %v", result.InstanceID, fsID, terr, fsID, delErr)
			}
			prog.Error("Creating FSx Lustre filesystem (async)", terr)
			return fmt.Errorf("instance %s launched but tagging it with pending FSx %s failed (deleted the FSx to avoid a leak): %w", result.InstanceID, fsID, terr)
		}
		config.FSxPending = fsID
		config.FSxImportPath = pendingFSxImportPath
		config.FSxExportPath = pendingFSxExportPath
		config.FSxMountPoint = mountPoint
		prog.Complete("Creating FSx Lustre filesystem (async)")
		fmt.Fprintf(os.Stderr, "   FSx %s is provisioning; the instance will mount it at %s once AVAILABLE (~10 min).\n", fsID, mountPoint)
	}

	// Write instance ID to file for workflow integration
	if err := writeOutputID(result.InstanceID, outputIDFile); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write instance ID to file: %v\n", err)
	}

	// Step 7: Wait for the instance to reach running. Use the SDK waiter
	// (returns as soon as it's running) rather than a fixed sleep. Gated by
	// --wait-for-running; the agent/user-data runs asynchronously on the
	// instance and is observed via spored, not by blocking here.
	prog.Start("Waiting for instance")
	if waitForRunning {
		if err := awsClient.WaitForRunning(ctx, config.Region, result.InstanceID, 5*time.Minute); err != nil {
			prog.Error("Waiting for instance", err)
			return err
		}
	}
	prog.Complete("Waiting for instance")

	// Step 9: Get public IP
	prog.Start("Getting public IP")
	publicIP, err := awsClient.GetInstancePublicIP(ctx, config.Region, result.InstanceID)
	if err != nil {
		prog.Error("Getting public IP", err)
		return err
	}
	result.PublicIP = publicIP
	prog.Complete("Getting public IP")

	// Step 10: Wait for the instance to be usable. For Windows, SSH (22) won't
	// answer until late and the real "usable" signal is the Administrator password
	// becoming available (after EC2Launch runs, post-Sysprep), so wait on that
	// instead of probing port 22 (#95). For Linux, probe SSH as before.
	if config.TargetOS == "windows" {
		prog.Start("Waiting for Windows (password available)")
		if waitForSSH {
			// WaitForPasswordData polls GetPasswordData; it's the earliest reliable
			// "Windows finished first boot" signal. Best-effort: don't fail the
			// launch if it's slow — the instance is up and self-managing via spored.
			if _, err := awsClient.WaitForPasswordData(ctx, config.Region, result.InstanceID, 12*time.Minute); err != nil {
				fmt.Fprintf(os.Stderr, "\n⚠️  Windows is still finishing first boot (Sysprep). The instance is up; `spawn connect %s` will wait for the password.\n", config.Name)
			}
		}
		prog.Complete("Waiting for Windows (password available)")
	} else {
		prog.Start("Waiting for SSH")
		if waitForSSH && result.PublicIP != "" {
			waitForSSHReady(ctx, result.PublicIP, 2*time.Minute)
		}
		prog.Complete("Waiting for SSH")
	}

	// Step 11: Register DNS (if requested).
	//
	// DNS registration is performed by SSHing into the instance, so it requires
	// SSH to be confirmed ready. When --wait-for-ssh=false the caller explicitly
	// declined to wait for SSH, so we cannot (and should not) register a record
	// pointing at an instance whose reachability is unconfirmed — skip it rather
	// than block on an SSH that may not yet be up (#56).
	var dnsRecord string
	if dnsName != "" && !waitForSSH {
		fmt.Fprintf(os.Stderr, "ℹ️  Skipping DNS registration (--wait-for-ssh=false); register later with: spawn dns register %s %s\n",
			result.InstanceID, dnsName)
	}
	if dnsName != "" && waitForSSH {
		// Load DNS configuration with precedence
		dnsConfig, err := spawnconfig.LoadDNSConfig(ctx, dnsDomain, dnsAPIEndpoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n⚠️  Failed to load DNS config: %v\n", err)
		} else {
			prog.Start("Registering DNS")
			fqdn, err := registerDNS(plat, result.KeyName, result.InstanceID, result.PublicIP, dnsName, dnsConfig.Domain, dnsConfig.APIEndpoint)
			if err != nil {
				prog.Error("Registering DNS", err)
				// Non-fatal: continue even if DNS registration fails
				fmt.Fprintf(os.Stderr, "\n⚠️  DNS registration failed: %v\n", err)
			} else {
				dnsRecord = fqdn
				prog.Complete("Registering DNS")
			}
		}
	}

	// Output: JSON array or TUI depending on --output flag
	if getOutputFormat() == "json" {
		out := []map[string]interface{}{
			{
				"instance_id":   result.InstanceID,
				"name":          config.Name,
				"instance_type": config.InstanceType,
				"region":        config.Region,
				"public_ip":     result.PublicIP,
				"state":         "running",
				"dns":           dnsRecord,
			},
		}
		jsonBytes, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	// Display success (TUI mode). Windows has no `ssh ec2-user@` path — connect
	// via `spawn connect`, which fetches the Administrator password and opens a
	// desktop (--rdp), a PowerShell-over-SSM session (default), or SSH (--ssh).
	var connectCmd string
	if config.TargetOS == "windows" {
		connectCmd = fmt.Sprintf("spawn connect %s --rdp     # graphical; or --ssh, or `spawn connect %s` (PowerShell over SSM)", config.Name, config.Name)
	} else {
		connectCmd = plat.GetSSHCommand("ec2-user", result.PublicIP)
	}
	prog.DisplaySuccess(result.InstanceID, result.PublicIP, connectCmd, config)

	// Show DNS info if registered
	if dnsRecord != "" {
		_, _ = fmt.Fprintf(os.Stdout, "\n🌐 DNS: %s\n", dnsRecord)
		if config.TargetOS == "windows" {
			_, _ = fmt.Fprintf(os.Stdout, "   Connect: spawn connect %s --rdp\n", config.Name)
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "   Connect: ssh %s@%s\n", plat.GetUsername(), dnsRecord)
		}
	}

	return nil
}

// parseAttachVolumes turns repeated --attach-volume values of the form
// "snap-xxx:/mount/point[:ro|:rw]" into AttachVolumeSpec. The mount point must
// be an absolute path; the optional trailing :ro / :rw sets read-only (default
// read-write). Mount paths don't contain ':' in practice, so we split on it (#144).
func parseAttachVolumes(raw []string) ([]aws.AttachVolumeSpec, error) {
	specs := make([]aws.AttachVolumeSpec, 0, len(raw))
	for _, r := range raw {
		parts := strings.Split(r, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("invalid --attach-volume %q: expected snap-xxx:/mount/point[:ro]", r)
		}
		snap := strings.TrimSpace(parts[0])
		mount := strings.TrimSpace(parts[1])
		readOnly := false
		if len(parts) == 3 {
			switch strings.ToLower(strings.TrimSpace(parts[2])) {
			case "ro":
				readOnly = true
			case "rw":
				readOnly = false
			default:
				return nil, fmt.Errorf("invalid --attach-volume %q: mode must be 'ro' or 'rw', got %q", r, parts[2])
			}
		}
		if !strings.HasPrefix(snap, "snap-") {
			return nil, fmt.Errorf("invalid --attach-volume %q: snapshot must look like snap-xxxx", r)
		}
		if !strings.HasPrefix(mount, "/") {
			return nil, fmt.Errorf("invalid --attach-volume %q: mount point must be an absolute path", r)
		}
		specs = append(specs, aws.AttachVolumeSpec{
			SnapshotID: snap,
			MountPoint: mount,
			ReadOnly:   readOnly,
		})
	}
	return specs, nil
}

// attachedVolumesUserData maps the launch config's attach-volume specs to the
// storage user-data's mount list, assigning each the same EC2 device name the
// block-device mapping used (aws.AttachDeviceName), so the mount resolves the
// right device (#144).
func attachedVolumesUserData(specs []aws.AttachVolumeSpec) []userdata.AttachedVolume {
	if len(specs) == 0 {
		return nil
	}
	vols := make([]userdata.AttachedVolume, 0, len(specs))
	for i, s := range specs {
		vols = append(vols, userdata.AttachedVolume{
			DeviceName: aws.AttachDeviceName(i),
			MountPoint: s.MountPoint,
			ReadOnly:   s.ReadOnly,
		})
	}
	return vols
}

func getEFSMountOptions() (string, error) {
	// Custom mount options override profile
	if efsMountOptions != "" {
		opts, err := storage.ParseCustomOptions(efsMountOptions)
		if err != nil {
			return "", fmt.Errorf("failed to parse custom mount options: %w", err)
		}
		return opts.ToMountString(), nil
	}

	// Validate profile
	if efsProfile != "" {
		if err := storage.ValidateProfile(efsProfile); err != nil {
			return "", err
		}
	}

	// Get mount options from profile
	opts, err := storage.GetEFSProfile(storage.EFSProfile(efsProfile))
	if err != nil {
		return "", fmt.Errorf("failed to get EFS profile: %w", err)
	}

	return opts.ToMountString(), nil
}

func buildLaunchConfig(truffleInput *input.TruffleInput) (*aws.LaunchConfig, error) {
	config := &aws.LaunchConfig{
		Tags: make(map[string]string),
	}

	// From truffle input
	if truffleInput != nil {
		config.InstanceType = truffleInput.InstanceType
		config.Region = truffleInput.Region
		config.AvailabilityZone = truffleInput.AvailabilityZone

		if truffleInput.Spot {
			config.Spot = true
			if truffleInput.SpotPrice > 0 {
				config.SpotMaxPrice = fmt.Sprintf("%.4f", truffleInput.SpotPrice)
			}
		}
		// Carry the reservation id from truffle input (#216). Previously dropped —
		// only type/region/AZ/spot were copied, so a piped reservation never
		// reached RunInstances (same silently-dropped-field class as lagotto#19).
		if truffleInput.ReservationID != "" {
			config.ReservationID = truffleInput.ReservationID
		}
	}

	// Override with flags
	if instanceType != "" {
		config.InstanceType = instanceType
	}
	if region != "" {
		config.Region = region
	}
	if az != "" {
		config.AvailabilityZone = az
	}
	// "auto" is an explicit synonym for "auto-detect" (empty); normalize so all
	// downstream AMI gates auto-detect (#342).
	if ami != "" && !strings.EqualFold(ami, "auto") {
		config.AMI = ami
	}
	if launchVolumeSize > 0 {
		config.RootVolumeSizeGiB = launchVolumeSize
	}
	if len(attachVolumes) > 0 {
		specs, err := parseAttachVolumes(attachVolumes)
		if err != nil {
			return nil, err
		}
		config.AttachVolumes = specs
	}
	if keyPair != "" {
		config.KeyName = keyPair
	}
	if spot {
		config.Spot = true
	}
	// Launch into a Capacity Reservation / Capacity Block (#216). The flag
	// overrides any truffle-supplied id.
	if reservationID != "" {
		config.ReservationID = reservationID
	}
	if capacityBlock {
		config.CapacityBlock = true
	}
	// A Capacity Block is consumed via MarketType=capacity-block, which is
	// mutually exclusive with Spot's market options — reject the combination
	// rather than silently dropping one.
	if config.CapacityBlock && config.Spot {
		return nil, fmt.Errorf("--capacity-block and --spot are mutually exclusive (a Capacity Block is not a Spot purchase)")
	}
	// --capacity-block only means something with a reservation to target.
	if config.CapacityBlock && config.ReservationID == "" {
		return nil, fmt.Errorf("--capacity-block requires --reservation-id (the Capacity Block id to launch into)")
	}
	if hibernate {
		config.Hibernate = true
	}
	if nestedVirt {
		config.NestedVirtualization = true
	}
	if ttl != "" {
		config.TTL = ttl
	}
	// --name implies DNS registration; --dns overrides the DNS portion only.
	if dnsName == "" && name != "" {
		dnsName = name
	} else if name == "" && dnsName != "" {
		name = dnsName
	}
	if dnsName != "" {
		config.DNSName = dnsName
	}
	if slackWorkspaceID != "" {
		config.SlackWorkspaceID = slackWorkspaceID
		// The spore-bot Lambda Function URL — hard-coded for hosted spore.host;
		// can be overridden via SPORE_BOT_NOTIFY_URL env var for self-hosted deployments.
		notifyURL := spawnconfig.GetNotifyURL()
		config.NotifyURL = notifyURL
		config.NotifyCommand = "/spore" // routes notifications to spore-bot workspace config
	}
	if notifyPlatform != "" {
		config.NotifyPlatform = notifyPlatform // slack (default) / teams / discord (#2)
	}
	if activePorts != "" {
		config.ActivePortsRaw = activePorts
	}
	if activeProcesses != "" {
		config.ActiveProcessesRaw = activeProcesses
	}
	if idleTimeout != "" {
		config.IdleTimeout = idleTimeout
	}
	if hibernateOnIdle {
		config.HibernateOnIdle = true
	}
	if preStop != "" {
		config.PreStop = preStop
	}
	if preStopTimeout != "" {
		config.PreStopTimeout = preStopTimeout
	}
	if onComplete != "" {
		config.OnComplete = onComplete
	}
	if completionFile != "" {
		config.CompletionFile = completionFile
	}
	if completionDelay != "" {
		config.CompletionDelay = completionDelay
	}
	if sessionTimeout != "" {
		config.SessionTimeout = sessionTimeout
	}
	if name != "" {
		config.Name = name
	}
	if efsID != "" {
		config.EFSID = efsID
	}
	if efsMountPoint != "" {
		config.EFSMountPoint = efsMountPoint
	}

	// FSx Lustre flags
	config.FSxLustreCreate = fsxCreate
	config.FSxLifecycle = fsxLifecycle
	config.FSxTTL = fsxTTL
	if fsxID != "" {
		config.FSxLustreID = fsxID
	}
	if fsxRecall != "" {
		config.FSxLustreRecall = fsxRecall
	}
	if fsxStorageCapacity > 0 {
		config.FSxStorageCapacity = fsxStorageCapacity
	}
	if fsxS3Bucket != "" {
		config.FSxS3Bucket = fsxS3Bucket
	}
	if fsxImportPath != "" {
		config.FSxImportPath = fsxImportPath
	}
	if fsxExportPath != "" {
		config.FSxExportPath = fsxExportPath
	}
	if fsxMountPoint != "" {
		config.FSxMountPoint = fsxMountPoint
	}

	if costLimit > 0 {
		config.CostLimit = costLimit
	}
	if command != "" {
		config.JobArrayCommand = command
	}

	// Validate FSx flags
	if fsxCreate && fsxID != "" {
		return nil, fmt.Errorf("cannot use --fsx-create and --fsx-id together")
	}
	if fsxCreate && fsxRecall != "" {
		return nil, fmt.Errorf("cannot use --fsx-create and --fsx-recall together")
	}
	if fsxID != "" && fsxRecall != "" {
		return nil, fmt.Errorf("cannot use --fsx-id and --fsx-recall together")
	}
	if fsxCreate && fsxS3Bucket == "" {
		return nil, fmt.Errorf("--fsx-create requires --fsx-s3-bucket")
	}

	// Lifecycle contract (#193): an auto-created FSx is expensive and holds the
	// only copy of results, so its lifetime must be stated explicitly — never
	// inferred or defaulted. Fail closed if --fsx-create lacks a lifecycle, and
	// require a TTL for durable so no death-clock-less filesystem can exist.
	if fsxCreate {
		switch fsxLifecycle {
		case "ephemeral":
			if fsxTTL != "" {
				return nil, fmt.Errorf("--fsx-ttl is only valid with --fsx-lifecycle=durable (ephemeral FSx is reaped when the instance terminates)")
			}
		case "durable":
			if fsxTTL == "" {
				return nil, fmt.Errorf("--fsx-lifecycle=durable requires --fsx-ttl (e.g. 7d) — a durable FSx must have a death clock so it can't bill indefinitely")
			}
			if _, err := parseDuration(fsxTTL); err != nil {
				return nil, fmt.Errorf("invalid --fsx-ttl %q: %w", fsxTTL, err)
			}
		case "":
			return nil, fmt.Errorf("--fsx-create requires --fsx-lifecycle: 'ephemeral' (reaped with this instance) or 'durable' (persists; needs --fsx-ttl). An FSx costs money and holds your results — choose its lifetime explicitly")
		default:
			return nil, fmt.Errorf("invalid --fsx-lifecycle %q: must be 'ephemeral' or 'durable'", fsxLifecycle)
		}
	}
	if !fsxCreate && (fsxLifecycle != "" || fsxTTL != "") {
		return nil, fmt.Errorf("--fsx-lifecycle/--fsx-ttl only apply with --fsx-create")
	}

	// Validate storage capacity (must be 1200, 2400, or multiples of 2400)
	if fsxCreate && fsxStorageCapacity > 0 {
		if fsxStorageCapacity < 1200 {
			return nil, fmt.Errorf("minimum FSx storage capacity is 1200 GB")
		}
		if fsxStorageCapacity != 1200 && fsxStorageCapacity != 2400 && (fsxStorageCapacity-2400)%2400 != 0 {
			return nil, fmt.Errorf("invalid FSx storage capacity: must be 1200, 2400, or increments of 2400")
		}
	}

	return config, nil
}

// resolveStrataEnvironment resolves a Strata formation or profile to a lockfile
// S3 URI, which is set as the strata:lockfile-s3-uri EC2 instance tag at launch.
// strata-agent on the instance reads this tag at boot and mounts the environment.
func resolveStrataEnvironment(ctx context.Context, formation, profilePath, registry string) (string, error) {
	var profile *spec.Profile
	if profilePath != "" {
		data, err := os.ReadFile(profilePath)
		if err != nil {
			return "", fmt.Errorf("read profile: %w", err)
		}
		if err := yaml.Unmarshal(data, &profile); err != nil {
			return "", fmt.Errorf("parse profile: %w", err)
		}
	} else {
		profile = &spec.Profile{
			Name:     formation,
			Base:     spec.BaseRef{OS: "al2023"},
			Software: []spec.SoftwareRef{{Formation: formation}},
		}
	}
	c, err := strata.NewClient(ctx, strata.Options{RegistryURL: registry})
	if err != nil {
		return "", fmt.Errorf("new client: %w", err)
	}
	lf, err := c.Resolve(ctx, profile, strata.ResolveOptions{})
	if err != nil {
		return "", fmt.Errorf("resolve: %w", err)
	}
	uri, err := c.UploadLockfile(ctx, lf)
	if err != nil {
		return "", fmt.Errorf("upload lockfile: %w", err)
	}
	return uri, nil
}

// resolveTargetOS decides the instance OS: an explicit --os flag wins (for
// custom AMIs whose platform metadata is unset), otherwise auto-detect from the
// AMI via IsWindowsAMI. Returns "windows" or "linux".
func resolveTargetOS(ctx context.Context, awsClient *aws.Client, region, amiID, osFlag string) string {
	switch strings.ToLower(strings.TrimSpace(osFlag)) {
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	}
	if awsClient.IsWindowsAMI(ctx, region, amiID) {
		return "windows"
	}
	return "linux"
}

// defaultWindowsInstanceType is the default for `--os windows` when the user
// doesn't pass --instance-type. Windows must not default to a burstable type:
// the Sysprep/OOBE first boot starves on CPU credits and takes ~20+ min (#95).
const defaultWindowsInstanceType = "m7i.xlarge"

// isBurstableInstanceType reports whether an instance type is a burstable
// (t-family) type, e.g. "t3.large", "t4g.micro". Burstable CPU credits make
// Windows first-boot painfully slow, so we reject them for Windows.
func isBurstableInstanceType(instanceType string) bool {
	return strings.HasPrefix(instanceType, "t") && strings.Contains(instanceType, ".") &&
		(strings.HasPrefix(instanceType, "t2.") || strings.HasPrefix(instanceType, "t3.") ||
			strings.HasPrefix(instanceType, "t3a.") || strings.HasPrefix(instanceType, "t4g."))
}

// guardWindowsInstanceType rejects burstable instance types for Windows, with a
// clear, actionable error. Returns nil for non-Windows or acceptable types.
func guardWindowsInstanceType(targetOS, instanceType string) error {
	if targetOS != "windows" {
		return nil
	}
	if isBurstableInstanceType(instanceType) {
		return fmt.Errorf("instance type %q is burstable (t-family); Windows first boot starves on burst CPU credits and takes ~20+ min — choose a non-burstable type (default for Windows is %s)", instanceType, defaultWindowsInstanceType)
	}
	return nil
}

// preflightInstanceConstraints validates that the requested instance type
// supports the requested features (MPI cluster placement group, EFA,
// hibernation) BEFORE any AWS resources are created, with actionable errors
// (#110). One DescribeInstanceTypes call backs all checks. HPC types are exempt
// from the MPI/placement-group requirement — they use AWS HPC networking and
// spawn skips the placement group for them (#104), so --mpi alone is fine.
func preflightInstanceConstraints(ctx context.Context, awsClient *aws.Client, config *aws.LaunchConfig, wantMPI, wantEFA, wantHibernate bool) error {
	if !wantMPI && !wantEFA && !wantHibernate {
		return nil // nothing feature-specific to check
	}
	// truffle is the instance-type capability authority — consume it rather than
	// re-querying EC2 from spawn. Build a truffle client from spawn's AWS config
	// so creds/region match.
	tc := truffleaws.NewClientFromConfig(awsClient.Config())
	caps, err := tc.GetCapabilities(ctx, config.InstanceType, config.Region)
	if err != nil {
		return fmt.Errorf("pre-flight instance-type check: %w", err)
	}
	if !caps.Found {
		return fmt.Errorf("instance type %q not found in region %s", config.InstanceType, config.Region)
	}

	// --efa: must support EFA.
	if wantEFA && !caps.EFA {
		return fmt.Errorf("instance type %q does not support EFA (required for --efa).\n       Find EFA-capable types: truffle find \"%s\" efa  (e.g. c5n.18xlarge, hpc6a.48xlarge)",
			config.InstanceType, instanceFamilyHint(config.InstanceType))
	}

	// --hibernate / --hibernate-on-idle: must support hibernation.
	if wantHibernate && !caps.Hibernation {
		return fmt.Errorf("instance type %q does not support hibernation (required for --hibernate/--hibernate-on-idle).\n       Choose a hibernation-capable type, or drop the hibernation flag.", config.InstanceType)
	}

	// --mpi: needs a cluster placement group UNLESS it's an HPC type (which spawn
	// skips the placement group for). So only block --mpi when neither holds.
	if wantMPI && !caps.ClusterPlacement && !isHPCInstanceType(config.InstanceType) {
		return fmt.Errorf("instance type %q does not support cluster placement groups (needed for --mpi).\n       Use an MPI-capable type (e.g. c5n.18xlarge, c6i.32xlarge) or an HPC type (hpc6a/hpc7a/hpc7g), or run: truffle find \"%s\" efa",
			config.InstanceType, instanceFamilyHint(config.InstanceType))
	}
	return nil
}

// instanceFamilyHint returns a glob hint for the instance's family for use in
// suggested truffle commands, e.g. "c5n.18xlarge" -> "c5n*".
func instanceFamilyHint(instanceType string) string {
	if i := strings.IndexByte(instanceType, '.'); i > 0 {
		return instanceType[:i] + "*"
	}
	return instanceType
}

// isHPCInstanceType reports whether the type is in the AWS HPC family, which
// gets low-latency networking from HPC infrastructure rather than placement
// groups (so --mpi is valid without a cluster placement group). Detected by the
// "hpc" family prefix rather than a hardcoded list, so new HPC families
// (hpc6a/hpc6id/hpc7a/hpc7g/hpc8a/… as of June 2026, and future ones) are
// covered automatically — the EC2 naming convention is the contract. A real
// family is "hpc" followed by a generation digit (hpc6a, hpc7g…), so we require
// the digit to avoid matching a stray "hpc.weird".
func isHPCInstanceType(instanceType string) bool {
	const p = "hpc"
	if !strings.HasPrefix(instanceType, p) || len(instanceType) <= len(p) {
		return false
	}
	c := instanceType[len(p)]
	return c >= '0' && c <= '9'
}

// windowsLifecycleGuard enforces cost safety for Windows launches. Windows has
// no in-instance spored yet (#77), so idle-timeout cannot work and the only
// thing that will stop the instance is its TTL plus the server-side reaper
// backstop (#70). We therefore REQUIRE --ttl for Windows and warn loudly that no
// agent runs. Linux is unaffected.
func windowsLifecycleGuard(config *aws.LaunchConfig) error {
	if config.TargetOS != "windows" {
		return nil
	}
	// spored now runs on Windows as a Service (#77), so idle-timeout, completion,
	// and pre-stop work in-instance — same as Linux. We still require a timeout
	// (TTL or idle) so a Windows box can't run unbounded if the agent fails to
	// install; the server-side reaper (#70) backstops the TTL deadline regardless.
	if config.TTL == "" && config.IdleTimeout == "" {
		return fmt.Errorf("Windows instances require a timeout: set --ttl (hard deadline) " +
			"and/or --idle-timeout. The in-instance agent enforces these and the server-side " +
			"reaper backstops the TTL deadline.\n  Re-run with e.g. --ttl 8h")
	}
	return nil
}

func setupSSHKey(ctx context.Context, awsClient *aws.Client, region, amiID string, plat *platform.Platform) (string, error) {
	// Choose the keypair algorithm from the target OS. Windows requires RSA —
	// the EC2 Administrator password (GetPasswordData) can only be decrypted with
	// an RSA private key; ED25519 cannot. Everything else defaults to ED25519.
	algo := sshkey.ED25519
	if awsClient.IsWindowsAMI(ctx, region, amiID) {
		algo = sshkey.RSA
	}

	// Find-or-create spawn's managed keypair under ~/.spawn/keys (separate from
	// the user's personal ~/.ssh). Generated in-process — no ssh-keygen shell-out.
	kp, err := sshkey.EnsureKey(plat.HomeDir, plat.GetUsername(), algo)
	if err != nil {
		return "", fmt.Errorf("failed to ensure spawn SSH key: %w", err)
	}

	// Reuse an already-imported EC2 key with the same fingerprint, so re-launches
	// don't re-import.
	fingerprint, err := sshkey.Fingerprint(kp.PublicKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to get key fingerprint: %w", err)
	}
	existingKeyName, err := awsClient.FindKeyPairByFingerprint(ctx, region, fingerprint)
	if err != nil {
		return "", fmt.Errorf("failed to search for existing key: %w", err)
	}
	if existingKeyName != "" {
		return existingKeyName, nil
	}

	// Import under the algorithm-qualified name (spawn-key-<user> /
	// spawn-key-<user>-rsa) so both can coexist in EC2.
	publicKey, err := os.ReadFile(kp.PublicKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to read public key: %w", err)
	}
	if err := awsClient.ImportKeyPair(ctx, region, kp.Name, publicKey); err != nil {
		return "", fmt.Errorf("failed to import key pair: %w", err)
	}
	return kp.Name, nil
}

// encodeUserData gzip-compresses the script and base64-encodes the result.
// cloud-init on Amazon Linux 2023 and Ubuntu supports gzip+base64 user-data,
// which keeps the payload well under EC2's 16 KB limit even when combining
// spored bootstrap + MPI + FSx mount scripts (fixes #304).
func encodeUserData(script string) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(script))
	_ = gz.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// encodeUserDataForOS picks the right encoding for the target OS. Windows
// EC2Launch does NOT gzip-decompress user-data (only cloud-init does), so a
// Windows <powershell> script must be plain base64, not gzip+base64.
func encodeUserDataForOS(script, targetOS string) string {
	if targetOS == "windows" {
		return base64.StdEncoding.EncodeToString([]byte(script))
	}
	return encodeUserData(script)
}

// spawnPublicKeyForUserData returns the authorized_keys-format public key to
// install on the instance, matching the keypair registered with EC2. It resolves
// the private key for keyName via the shared resolver and reads its ".pub"; if
// that's unavailable (e.g. a user-supplied/legacy key only in ~/.ssh), it falls
// back to the personal public key for back-compat.
func spawnPublicKeyForUserData(plat *platform.Platform, keyName string) ([]byte, error) {
	if keyName != "" {
		if priv, err := sshkey.Resolve(plat.HomeDir, keyName); err == nil {
			if pub, err := os.ReadFile(priv + ".pub"); err == nil {
				return pub, nil
			}
		}
	}
	pub, err := plat.ReadPublicKey()
	if err != nil {
		return nil, fmt.Errorf("failed to read SSH public key: %w", err)
	}
	return pub, nil
}

// buildWindowsUserData emits the EC2Launch <powershell> bootstrap for a Windows
// instance (#55/#77). It (1) enables OpenSSH and trusts the spawn public key so
// `spawn connect` can SSH-over-SSM in addition to the Administrator-password/RDP
// path, and (2) installs spored as a Windows Service from the regional S3 bucket
// — the same buckets/binary the Linux bootstrap uses — so the instance enforces
// TTL/idle/completion in-instance just like Linux.
func buildWindowsUserData(authorizedKey string) (string, error) {
	// authorizedKey is an authorized_keys line (e.g. "ssh-rsa AAAA... spawn").
	// Guard against breaking out of the PowerShell here-string.
	if strings.Contains(authorizedKey, "\"@") || strings.Contains(authorizedKey, "@\"") {
		return "", fmt.Errorf("invalid public key content for Windows user-data")
	}
	key := strings.TrimSpace(authorizedKey)

	// EC2Launch v2 runs the <powershell> block on first boot. We:
	//  1. install + enable the OpenSSH Server optional feature,
	//  2. write the spawn public key to administrators_authorized_keys (the file
	//     Windows OpenSSH uses for members of the Administrators group) with the
	//     ACL it requires (Administrators + SYSTEM only),
	//  3. set the default shell to PowerShell for nicer interactive sessions,
	//  4. download spored.exe from the regional spawn-binaries S3 bucket and
	//     install it as an auto-start Windows Service (spored's own subcommand
	//     sets recovery actions). The instance role carries S3 read access, the
	//     same as Linux. Mirrors install-spored.ps1.
	script := fmt.Sprintf(`<powershell>
$ErrorActionPreference = "Continue"
try {
  Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0 -ErrorAction SilentlyContinue
  Set-Service -Name sshd -StartupType Automatic
  Start-Service sshd

  # Open inbound TCP 22 for ALL firewall profiles. The OpenSSH feature only
  # creates a Private-profile allow rule, but an EC2 instance's network is
  # classified Public, so SSH-to-the-public-IP (spawn connect --ssh) is blocked
  # by Windows Firewall even though sshd listens and the EC2 security group
  # allows it. (Confirmed live: RDP worked, SSH didn't, until this rule.)
  if (-not (Get-NetFirewallRule -Name spawn-sshd-22 -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name spawn-sshd-22 -DisplayName "spawn OpenSSH 22 (all profiles)" `+"`"+`
      -Enabled True -Direction Inbound -Protocol TCP -LocalPort 22 -Action Allow -Profile Any `+"`"+`
      -ErrorAction SilentlyContinue | Out-Null
  }

  $admins = "C:\ProgramData\ssh\administrators_authorized_keys"
  Set-Content -Path $admins -Value "%s" -Encoding ascii
  icacls $admins /inheritance:r | Out-Null
  icacls $admins /grant "Administrators:F" /grant "SYSTEM:F" | Out-Null

  New-ItemProperty -Path "HKLM:\SOFTWARE\OpenSSH" -Name DefaultShell `+"`"+`
    -Value "C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe" `+"`"+`
    -PropertyType String -Force | Out-Null
} catch {
  Write-Output "spawn windows ssh bootstrap warning: $_"
}

# Ensure the SSM agent is installed, set to auto-start, and running (#95).
# Stock Windows *Server* AMIs ship it running, but an imported Windows 11
# *client* AMI (spawn image import) bakes the agent without it auto-starting
# after Sysprep, so SSM never registers and SSH-over-SSM / RDP-over-SSM fail.
# Don't assume the AMI did it — same posture as the AWS CLI install below.
try {
  $svc = Get-Service -Name AmazonSSMAgent -ErrorAction SilentlyContinue
  if (-not $svc) {
    $msi = "$env:TEMP\SSMAgent_latest.exe"
    Invoke-WebRequest -Uri 'https://s3.amazonaws.com/ec2-downloads-windows/SSMAgent/latest/windows_amd64/AmazonSSMAgentSetup.exe' -OutFile $msi -UseBasicParsing
    Start-Process $msi -ArgumentList '/S' -Wait
  }
  Set-Service -Name AmazonSSMAgent -StartupType Automatic -ErrorAction SilentlyContinue
  Restart-Service -Name AmazonSSMAgent -ErrorAction SilentlyContinue
} catch {
  Write-Output "spawn ssm-agent bootstrap warning: $_"
}

# Install spored as a Windows Service from the regional S3 bucket (#77).
try {
  $token = Invoke-RestMethod -Method Put -Uri 'http://169.254.169.254/latest/api/token' -Headers @{'X-aws-ec2-metadata-token-ttl-seconds'='21600'} -TimeoutSec 5
  $region = Invoke-RestMethod -Uri 'http://169.254.169.254/latest/meta-data/placement/region' -Headers @{'X-aws-ec2-metadata-token'=$token} -TimeoutSec 5
  if (-not $region) { $region = 'us-east-1' }

  # The stock Windows Server AMI has no AWS CLI (unlike AL2023), so install it
  # before pulling spored from S3. aws.exe lands in Program Files; call it by
  # full path since the current session PATH won't yet include it.
  $aws = "$env:ProgramFiles\Amazon\AWSCLIV2\aws.exe"
  if (-not (Test-Path $aws)) {
    $msi = "$env:TEMP\AWSCLIV2.msi"
    Invoke-WebRequest -Uri 'https://awscli.amazonaws.com/AWSCLIV2.msi' -OutFile $msi -UseBasicParsing
    Start-Process msiexec.exe -ArgumentList @('/i', $msi, '/qn') -Wait
  }

  $dir = Join-Path $env:ProgramFiles 'spored'
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $exe = Join-Path $dir 'spored.exe'
  $bucket = "spawn-binaries-$region"
  $ok = $false
  foreach ($uri in @("s3://$bucket/spawn/spored-windows-amd64.exe","s3://$bucket/spored-windows-amd64.exe","s3://spawn-binaries-us-east-1/spawn/spored-windows-amd64.exe")) {
    & $aws s3 cp $uri $exe --region $region 2>$null
    if ($LASTEXITCODE -eq 0 -and (Test-Path $exe)) { $ok = $true; break }
  }
  if ($ok) {
    & $exe service install $exe
    & $exe service start
  } else {
    Write-Output "spawn: could not download spored.exe from S3"
  }
} catch {
  Write-Output "spawn spored install warning: $_"
}
</powershell>
<persist>false</persist>`, key)

	return script, nil
}

func buildUserData(plat *platform.Platform, config *aws.LaunchConfig, storageScript string) (string, error) {
	// Inject the PUBLIC key of the same keypair we registered with EC2
	// (config.KeyName), so the instance trusts the key `spawn connect` will use.
	publicKey, err := spawnPublicKeyForUserData(plat, config.KeyName)
	if err != nil {
		return "", err
	}

	// Windows uses a completely different bootstrap: EC2Launch runs a
	// <powershell> block, not bash, and there is no spored yet (#77). Branch
	// early to a minimal PowerShell script.
	if config.TargetOS == "windows" {
		return buildWindowsUserData(string(publicKey))
	}

	username := plat.GetUsername()

	// Read custom user data if provided. This is the CLI-only part: resolving
	// --user-data-file and the @path form against the local filesystem. The
	// resolved text is handed to the shared launcher below.
	customUserData := ""

	if userDataFile != "" {
		// Validate path for security
		if err := security.ValidatePathForReading(userDataFile); err != nil {
			return "", fmt.Errorf("invalid user data file path: %w", err)
		}
		data, err := os.ReadFile(userDataFile)
		if err != nil {
			return "", err
		}
		customUserData = string(data)
	} else if userData != "" {
		if strings.HasPrefix(userData, "@") {
			path := userData[1:]
			// Validate path for security
			if err := security.ValidatePathForReading(path); err != nil {
				return "", fmt.Errorf("invalid user data file path: %w", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			customUserData = string(data)
		} else {
			customUserData = userData
		}
	}

	// Delegate to the shared, headless bootstrap builder (pkg/launcher) so the
	// CLI and SDK consumers (lagotto, cohort) emit identical spored user-data.
	return launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{
		Username:       username,
		PublicKey:      publicKey,
		Plugins:        collectPluginDeclarations(),
		StorageScript:  storageScript,
		CustomUserData: customUserData,
	})
}

// collectPluginDeclarations merges plugin refs from --plugin flags and --config file.
func collectPluginDeclarations() []plugin.Declaration {
	var decls []plugin.Declaration

	// From --config YAML file.
	if launchConfigFile != "" {
		if cfg, err := loadLaunchConfig(launchConfigFile); err == nil {
			decls = append(decls, cfg.Plugins...)
		}
	}

	// From --plugin flags (simple refs without per-plugin config).
	for _, ref := range launchPlugins {
		decls = append(decls, plugin.Declaration{Ref: ref})
	}

	return decls
}

// LaunchConfig is the YAML structure for --config files passed to spawn launch.
type LaunchConfig struct {
	Plugins []plugin.Declaration `yaml:"plugins"`
}

// loadLaunchConfig reads a launch config YAML file.
func loadLaunchConfig(path string) (*LaunchConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read launch config %s: %w", path, err)
	}
	var cfg LaunchConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse launch config %s: %w", path, err)
	}
	return &cfg, nil
}

func isTerminal(f *os.File) bool {
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

// launchInputMode selects how `spawn launch` obtains its configuration.
type launchInputMode int

const (
	modeFlags  launchInputMode = iota // explicit flags (--instance-type set)
	modeWizard                        // interactive TTY wizard
	modePipe                          // truffle JSON piped on stdin
)

// launchMode decides the input mode from the --interactive flag, whether
// --instance-type was given, and whether stdin is a TTY.
//
//   - explicit --interactive, or no instance type on a TTY → wizard.
//   - no instance type and stdin is a pipe → pipe (consume truffle JSON).
//   - otherwise (instance type given) → flags, and stdin is NOT read.
//
// The last rule is the #34 fix: a caller that passes --instance-type with a
// piped, non-TTY stdin (e.g. a Java/ProcessBuilder subprocess) must use flags
// mode, not try to parse an empty stdin as JSON.
func launchMode(interactive bool, instanceType string, stdinIsTTY bool) launchInputMode {
	if interactive || (instanceType == "" && stdinIsTTY) {
		return modeWizard
	}
	if instanceType == "" && !stdinIsTTY {
		return modePipe
	}
	return modeFlags
}

func registerDNS(plat *platform.Platform, keyName, instanceID, publicIP, recordName, domain, apiEndpoint string) (string, error) {
	// Build SSH command to register DNS from within the instance
	sshScript := fmt.Sprintf(`
# Get IMDSv2 token
TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" -s 2>/dev/null)

# Get instance identity
IDENTITY_DOC=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" -s http://169.254.169.254/latest/dynamic/instance-identity/document 2>/dev/null | base64 -w0)
IDENTITY_SIG=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" -s http://169.254.169.254/latest/dynamic/instance-identity/signature 2>/dev/null | tr -d '\n')
PUBLIC_IP=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" -s http://169.254.169.254/latest/meta-data/public-ipv4 2>/dev/null)

# Call DNS API
curl -s -X POST %s \
  -H "Content-Type: application/json" \
  -d "{
    \"instance_identity_document\": \"$IDENTITY_DOC\",
    \"instance_identity_signature\": \"$IDENTITY_SIG\",
    \"record_name\": \"%s\",
    \"ip_address\": \"$PUBLIC_IP\",
    \"action\": \"UPSERT\"
  }" 2>/dev/null || echo '{"success":false,"error":"DNS API call failed"}'
`, apiEndpoint, recordName)

	// Execute SSH command using the same keypair registered with EC2 (resolved
	// via the shared resolver: spawn-managed key first, then ~/.ssh fallback).
	sshKeyPath, err := sshkey.Resolve(plat.HomeDir, keyName)
	if err != nil {
		sshKeyPath = plat.SSHKeyPath // back-compat last resort
	}
	username := plat.GetUsername()

	// Build SSH command arguments. ControlMaster=no / ControlPath=none ensure
	// spawn's own SSH never piggybacks the user's ~/.ssh/config connection
	// multiplexing — otherwise many concurrent launches/connects serialize on
	// one shared control socket (#56).
	sshArgs := []string{
		"-i", sshKeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		fmt.Sprintf("%s@%s", username, publicIP),
		sshScript,
	}

	// Execute
	cmd := exec.Command("ssh", sshArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to execute SSH command: %w (output: %s)", err, string(output))
	}

	// Parse response
	var response struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Message string `json:"message"`
		Record  string `json:"record"`
	}

	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &response); err != nil {
		return "", fmt.Errorf("failed to parse DNS API response: %w (output: %s)", err, string(output))
	}

	if !response.Success {
		return "", fmt.Errorf("%s", response.Error)
	}

	return response.Record, nil
}

// detectBestRegion automatically selects the closest AWS region
// that has the requested instance type available and is allowed by SCPs.
// It prioritizes in-country/in-continent regions based on IP geolocation.
func detectBestRegion(ctx context.Context, instanceType string) (string, error) {
	// First, get allowed regions from AWS (respects SCPs)
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to create AWS client: %w", err)
	}

	allowedRegions, err := awsClient.GetEnabledRegions(ctx)
	if err != nil || len(allowedRegions) == 0 {
		// Fallback to common regions if we can't get the list
		allowedRegions = []string{
			"us-east-1", "us-west-2", "eu-west-1",
			"ap-southeast-1", "us-east-2", "eu-central-1",
		}
	}

	// Try to detect user's location via IP geolocation
	userContinent := detectUserContinent()

	// Measure latency to each allowed region's EC2 endpoint
	type regionScore struct {
		region         string
		latency        time.Duration
		continentMatch bool
	}

	results := make([]regionScore, 0, len(allowedRegions))

	for _, region := range allowedRegions {
		start := time.Now()

		// Quick connectivity test to EC2 endpoint
		endpoint := fmt.Sprintf("ec2.%s.amazonaws.com", region)
		conn, err := net.DialTimeout("tcp", endpoint+":443", 2*time.Second)
		if err != nil {
			// Skip regions we can't reach (may be blocked by SCP or network)
			continue
		}
		_ = conn.Close()

		latency := time.Since(start)
		continentMatch := matchesContinent(region, userContinent)

		results = append(results, regionScore{
			region:         region,
			latency:        latency,
			continentMatch: continentMatch,
		})
	}

	if len(results) == 0 {
		return "", fmt.Errorf("could not connect to any allowed AWS region")
	}

	// Sort by: continent match first, then latency
	sort.Slice(results, func(i, j int) bool {
		// Prioritize continent matches
		if results[i].continentMatch != results[j].continentMatch {
			return results[i].continentMatch
		}
		// Within same continent preference, choose lowest latency
		return results[i].latency < results[j].latency
	})

	// Return the best scored region
	return results[0].region, nil
}

// detectUserContinent attempts to determine the user's continent from their public IP
func detectUserContinent() string {
	// Try ipapi.co (free, no API key needed for moderate usage)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("https://ipapi.co/json/")
	if err != nil {
		return "" // Failed, will fall back to latency-only
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return ""
	}

	var result struct {
		CountryCode string `json:"country_code"`
		Continent   string `json:"continent_code"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}

	// Map continent codes: AF, AN, AS, EU, NA, OC, SA
	return result.Continent
}

// matchesContinent checks if an AWS region matches the user's continent
func matchesContinent(region, continentCode string) bool {
	if continentCode == "" {
		return false // Unknown continent, no preference
	}

	// Map AWS region prefixes to continent codes
	regionToContinentMap := map[string]string{
		"us-":      "NA", // North America
		"ca-":      "NA", // Canada
		"eu-":      "EU", // Europe
		"me-":      "AS", // Middle East (Asia)
		"af-":      "AF", // Africa
		"ap-":      "AS", // Asia Pacific
		"sa-":      "SA", // South America
		"il-":      "AS", // Israel (Middle East)
		"ap-south": "AS", // India
	}

	// Check region prefix
	for prefix, continent := range regionToContinentMap {
		if len(region) >= len(prefix) && region[:len(prefix)] == prefix {
			return continent == continentCode
		}
	}

	return false
}

// Job Array Helper Functions

// generateJobArrayID creates a unique ID for a job array
// Format: {name}-{timestamp}-{random}
// Example: compute-20260113-abc123
func generateJobArrayID(name string) string {
	timestamp := time.Now().Format("20060102")
	// Generate 6-character random suffix (base36: 0-9a-z)
	random := fmt.Sprintf("%06x", time.Now().UnixNano()%0xFFFFFF)
	return fmt.Sprintf("%s-%s-%s", name, timestamp, random)
}

// formatInstanceName applies template substitution for instance names
// Supported variables: {index}, {job-array-name}
// Default template: "{job-array-name}-{index}"
func formatInstanceName(template string, jobArrayName string, index int) string {
	if template == "" {
		template = "{job-array-name}-{index}"
	}

	name := template
	name = strings.ReplaceAll(name, "{index}", fmt.Sprintf("%d", index))
	name = strings.ReplaceAll(name, "{job-array-name}", jobArrayName)

	return name
}

// launchJobArray launches N instances in parallel as a job array
func launchJobArray(ctx context.Context, awsClient *aws.Client, baseConfig *aws.LaunchConfig, plat *platform.Platform, prog *progress.Progress, fsxInfo *aws.FSxInfo, auditLog *audit.AuditLogger) error {
	// Generate unique job array ID
	jobArrayID := generateJobArrayID(jobArrayName)
	createdAt := time.Now()

	fmt.Fprintf(os.Stderr, "\n🚀 Launching job array: %s (%d instances)\n", jobArrayName, count)
	fmt.Fprintf(os.Stderr, "   Job Array ID: %s\n\n", jobArrayID)

	// Log job array launch initiation
	auditLog.LogOperationWithData("launch_job_array", jobArrayID, "initiated",
		map[string]interface{}{
			"job_array_name": jobArrayName,
			"instance_count": count,
			"instance_type":  baseConfig.InstanceType,
			"region":         baseConfig.Region,
		}, nil)

	// Phase 1: Launch all instances in parallel
	prog.Start(fmt.Sprintf("Launching %d instances in parallel", count))

	type launchResult struct {
		index  int
		result *aws.LaunchResult
		err    error
	}

	results := make(chan launchResult, count)
	var wg sync.WaitGroup

	// Launch each instance in a goroutine
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			// Clone config for this instance
			instanceConfig := *baseConfig

			// Set job array fields
			instanceConfig.JobArrayID = jobArrayID
			instanceConfig.JobArrayName = jobArrayName
			instanceConfig.JobArraySize = count
			instanceConfig.JobArrayIndex = index
			instanceConfig.JobArrayCommand = command

			// Set instance name from template
			instanceConfig.Name = formatInstanceName(instanceNames, jobArrayName, index)

			// Set DNS name with index suffix if DNS is enabled
			if baseConfig.DNSName != "" {
				instanceConfig.DNSName = fmt.Sprintf("%s-%d", baseConfig.DNSName, index)
			}

			// Append MPI and/or storage user-data if enabled
			if mpiEnabled || efsID != "" || fsxInfo != nil {
				// Decode base user-data
				baseUserDataBytes, err := base64.StdEncoding.DecodeString(instanceConfig.UserData)
				if err != nil {
					results <- launchResult{
						index: index,
						err:   fmt.Errorf("failed to decode base user-data: %w", err),
					}
					return
				}

				combinedUserData := string(baseUserDataBytes)

				// Add MPI user-data if enabled
				if mpiEnabled {
					// Generate MPI user-data for this instance
					mpiConfig := userdata.MPIConfig{
						Region:              baseConfig.Region,
						JobArrayID:          jobArrayID,
						JobArrayIndex:       index,
						JobArraySize:        count,
						MPIProcessesPerNode: mpiProcessesPerNode,
						MPICommand:          mpiCommand,
						SkipInstall:         mpiSkipInstall,
						EFAEnabled:          efaEnabled,
					}

					mpiScript, err := userdata.GenerateMPIUserData(mpiConfig)
					if err != nil {
						results <- launchResult{
							index: index,
							err:   fmt.Errorf("failed to generate MPI user-data: %w", err),
						}
						return
					}

					combinedUserData += "\n" + mpiScript
				}

				// NOTE: storage mounts (EFS/FSx/attached EBS volumes) are NOT added
				// here. They're already baked into baseConfig.UserData by
				// buildUserData, injected BEFORE the user script so the workload
				// sees them live (#166). Re-appending here would double-mount AND
				// land the mount after the script again. MPI user-data above is the
				// only per-instance addition the array path needs.

				// Re-encode with gzip compression
				instanceConfig.UserData = encodeUserData(combinedUserData)
			}

			// Launch the instance
			result, err := awsClient.Launch(ctx, instanceConfig)
			results <- launchResult{
				index:  index,
				result: result,
				err:    err,
			}
		}(i)
	}

	// Wait for all launches to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	launchedInstances := make([]*aws.LaunchResult, 0, count)
	var launchErrors []string
	successCount := 0
	failureCount := 0

	for result := range results {
		if result.err != nil {
			launchErrors = append(launchErrors, fmt.Sprintf("Instance %d: %v", result.index, result.err))
			failureCount++
		} else {
			launchedInstances = append(launchedInstances, result.result)
			successCount++
		}
	}

	// Handle partial failures
	if failureCount > 0 {
		prog.Error(fmt.Sprintf("Launching %d instances", count), fmt.Errorf("%d/%d instances failed to launch", failureCount, count))

		auditLog.LogOperationWithData("launch_job_array", jobArrayID, "failed",
			map[string]interface{}{
				"success_count": successCount,
				"failure_count": failureCount,
			}, fmt.Errorf("%d/%d instances failed", failureCount, count))

		// Terminate successfully launched instances
		if successCount > 0 {
			fmt.Fprintf(os.Stderr, "\n⚠️  Cleaning up %d successfully launched instances...\n", successCount)
			for _, inst := range launchedInstances {
				_ = awsClient.Terminate(ctx, baseConfig.Region, inst.InstanceID)
			}
		}

		// Return detailed error
		return fmt.Errorf("job array launch failed: %d/%d instances failed:\n  %s",
			failureCount, count, strings.Join(launchErrors, "\n  "))
	}

	auditLog.LogOperationWithData("launch_job_array", jobArrayID, "success",
		map[string]interface{}{
			"instance_count": successCount,
		}, nil)

	prog.Complete(fmt.Sprintf("Launching %d instances", count))

	// Sort instances by index for consistent display
	sort.Slice(launchedInstances, func(i, j int) bool {
		// Extract index from Name (assumes format: name-{index})
		getName := func(r *aws.LaunchResult) int {
			parts := strings.Split(r.Name, "-")
			if len(parts) > 0 {
				if idx, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
					return idx
				}
			}
			return 0
		}
		return getName(launchedInstances[i]) < getName(launchedInstances[j])
	})

	// Phase 2: Wait for all instances to reach "running" state
	prog.Start("Waiting for all instances to reach running state")
	maxWaitTime := 2 * time.Minute
	checkInterval := 5 * time.Second
	startTime := time.Now()

	allRunning := false
	for time.Since(startTime) < maxWaitTime {
		allRunning = true
		for _, inst := range launchedInstances {
			state, err := awsClient.GetInstanceState(ctx, baseConfig.Region, inst.InstanceID)
			if err != nil || state != "running" {
				allRunning = false
				break
			}
		}

		if allRunning {
			break
		}

		time.Sleep(checkInterval)
	}

	if !allRunning {
		prog.Error("Waiting for instances", fmt.Errorf("timeout waiting for all instances to reach running state"))
		return fmt.Errorf("timeout: not all instances reached running state within %v", maxWaitTime)
	}

	prog.Complete("Waiting for all instances")

	// Phase 3: Get public IPs for all instances
	prog.Start("Getting public IPs")
	for _, inst := range launchedInstances {
		publicIP, err := awsClient.GetInstancePublicIP(ctx, baseConfig.Region, inst.InstanceID)
		if err != nil {
			prog.Error("Getting public IP", err)
			// Non-fatal: continue with other instances
			fmt.Fprintf(os.Stderr, "\n⚠️  Failed to get IP for %s: %v\n", inst.InstanceID, err)
		} else {
			inst.PublicIP = publicIP
		}
	}
	prog.Complete("Getting public IPs")

	// Note: Peer discovery is handled dynamically by spored agent
	// Each agent queries EC2 for all instances with the same spawn:job-array-id tag
	// This avoids AWS tag size limitations (256 char max) and scales to any array size

	// Write job array ID to file for workflow integration
	if err := writeOutputID(jobArrayID, outputIDFile); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write job array ID to file: %v\n", err)
	}

	// JSON output mode — always an array, consistent with single-instance path
	if getOutputFormat() == "json" {
		out := make([]map[string]interface{}, len(launchedInstances))
		for i, inst := range launchedInstances {
			out[i] = map[string]interface{}{
				"instance_id":     inst.InstanceID,
				"name":            inst.Name,
				"instance_type":   baseConfig.InstanceType,
				"region":          baseConfig.Region,
				"public_ip":       inst.PublicIP,
				"state":           "running",
				"job_array_name":  jobArrayName,
				"job_array_id":    jobArrayID,
				"job_array_index": i,
				"job_array_size":  count,
			}
		}
		jsonBytes, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	// Display success for job array
	fmt.Fprintf(os.Stderr, "\n✅ Job array launched successfully!\n\n")
	fmt.Fprintf(os.Stderr, "Job Array: %s\n", jobArrayName)
	fmt.Fprintf(os.Stderr, "Array ID:  %s\n", jobArrayID)
	fmt.Fprintf(os.Stderr, "Created:   %s\n", createdAt.Format(time.RFC3339))
	fmt.Fprintf(os.Stderr, "Count:     %d instances\n", count)
	fmt.Fprintf(os.Stderr, "Region:    %s\n\n", baseConfig.Region)

	// Display table of instances
	fmt.Fprintf(os.Stderr, "Instances:\n")
	fmt.Fprintf(os.Stderr, "%-5s %-20s %-19s %-15s\n", "Index", "Instance ID", "Name", "Public IP")
	fmt.Fprintf(os.Stderr, "%-5s %-20s %-19s %-15s\n", "-----", "--------------------", "-------------------", "---------------")

	for i, inst := range launchedInstances {
		ipDisplay := inst.PublicIP
		if ipDisplay == "" {
			ipDisplay = "(pending)"
		}
		fmt.Fprintf(os.Stderr, "%-5d %-20s %-19s %-15s\n", i, inst.InstanceID, inst.Name, ipDisplay)
	}

	fmt.Fprintf(os.Stderr, "\nManagement:\n")
	fmt.Fprintf(os.Stderr, "  • List:      spawn list --job-array-name %s\n", jobArrayName)
	fmt.Fprintf(os.Stderr, "  • Terminate: spawn terminate --job-array-name %s\n", jobArrayName)
	fmt.Fprintf(os.Stderr, "  • Extend:    spawn extend --job-array-name %s --ttl 4h\n", jobArrayName)

	if launchedInstances[0].PublicIP != "" {
		fmt.Fprintf(os.Stderr, "\nConnect to instances:\n")
		for i, inst := range launchedInstances {
			if inst.PublicIP != "" {
				sshCmd := plat.GetSSHCommand("ec2-user", inst.PublicIP)
				fmt.Fprintf(os.Stderr, "  [%d] %s\n", i, sshCmd)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "\n")

	return nil
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

// launchSweepDetached launches a parameter sweep in detached mode (Lambda orchestration)
func launchSweepDetached(ctx context.Context, paramFormat *ParamFileFormat, baseConfig *aws.LaunchConfig, sweepID, sweepName string, maxConcurrent int, launchDelay string) error {
	// Determine region (auto-detect if not specified)
	sweepRegion := baseConfig.Region
	if sweepRegion == "" {
		fmt.Fprintf(os.Stderr, "🌍 No region specified, auto-detecting closest region...\n")
		detectedRegion, err := detectBestRegion(ctx, baseConfig.InstanceType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not auto-detect region: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Using default: us-east-1\n")
			sweepRegion = "us-east-1"
		} else {
			fmt.Fprintf(os.Stderr, "✓ Selected region: %s\n", sweepRegion)
			sweepRegion = detectedRegion
		}
	}

	// Load dev account config to get account ID
	// IMPORTANT: Always use spore-host-dev profile for target account ID
	// regardless of what AWS_PROFILE is set in environment
	devCfg, err := spawnconfig.LoadComputeAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get AWS account ID from dev account
	stsClient := sts.NewFromConfig(devCfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get AWS account ID: %w", err)
	}
	accountID := *identity.Account

	// Use spore-host-infra config for Lambda/S3/DynamoDB operations
	infraCfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load infra AWS config: %w", err)
	}

	// Convert ParamFileFormat to sweep.ParamFileFormat
	sweepParamFormat := &sweep.ParamFileFormat{
		Defaults: paramFormat.Defaults,
		Params:   paramFormat.Params,
	}

	// Validate parameter sets before launching (best-effort, warn on failure)
	fmt.Fprintf(os.Stderr, "🔍 Validating parameter sets...\n")
	if err := sweep.ValidateParameterSets(ctx, infraCfg, sweepParamFormat, accountID); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Parameter validation skipped (requires cross-account access)\n")
		fmt.Fprintf(os.Stderr, "   Parameters will be validated by Lambda orchestrator\n\n")
	} else {
		fmt.Fprintf(os.Stderr, "✓ All parameter sets validated\n\n")
	}

	// Estimate cost
	fmt.Fprintf(os.Stderr, "💰 Estimating cost...\n")
	costEstimate, err := pricing.EstimateSweepCost(&pricing.ParamFileFormat{
		Defaults: paramFormat.Defaults,
		Params:   paramFormat.Params,
	})
	if err != nil {
		return fmt.Errorf("failed to estimate cost: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\n%s\n\n", costEstimate.Display())

	// Check budget
	if budget > 0 {
		if costEstimate.TotalCost > budget {
			fmt.Fprintf(os.Stderr, "⚠️  WARNING: Estimated cost ($%.2f) exceeds budget ($%.2f) by $%.2f\n\n",
				costEstimate.TotalCost, budget, costEstimate.TotalCost-budget)
		} else {
			fmt.Fprintf(os.Stderr, "✓ Within budget: $%.2f remaining of $%.2f\n\n",
				budget-costEstimate.TotalCost, budget)
		}
	}

	// If estimate-only, exit here
	if estimateOnly {
		fmt.Fprintf(os.Stderr, "✅ Cost estimate complete (--estimate-only specified)\n")
		return nil
	}

	// If not auto-approved, prompt for confirmation
	if !autoYes {
		fmt.Fprintf(os.Stderr, "Launch sweep? [Y/n]: ")
		var response string
		_, _ = fmt.Scanln(&response)
		response = strings.ToLower(strings.TrimSpace(response))
		if response != "" && response != "y" && response != "yes" {
			fmt.Fprintf(os.Stderr, "\n❌ Launch cancelled by user\n")
			return nil
		}
		fmt.Fprintf(os.Stderr, "\n")
	}

	// Upload parameters to S3
	fmt.Fprintf(os.Stderr, "📤 Uploading parameters to S3...\n")
	s3Key, err := sweep.UploadParamsToS3(ctx, infraCfg, sweepParamFormat, sweepID, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to upload params to S3: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Uploaded: %s\n\n", s3Key)

	// Create DynamoDB record
	fmt.Fprintf(os.Stderr, "💾 Creating sweep orchestration record...\n")
	record := &sweep.SweepRecord{
		SweepID:                sweepID,
		SweepName:              sweepName,
		S3ParamsKey:            s3Key,
		MaxConcurrent:          maxConcurrent,
		MaxConcurrentPerRegion: maxConcurrentPerRegion,
		LaunchDelay:            launchDelay,
		TotalParams:            len(paramFormat.Params),
		Region:                 sweepRegion,
		AWSAccountID:           accountID,
		EstimatedCost:          costEstimate.TotalCost,
		Budget:                 budget,
	}

	// Check if multi-region sweep
	regionGroups := sweep.GroupParamsByRegion(sweepParamFormat.Params, sweepParamFormat.Defaults)

	// Apply region constraints if specified
	if shouldApplyRegionConstraints() {
		constraint := &sweep.RegionConstraint{
			Include:       regionsInclude,
			Exclude:       regionsExclude,
			Geographic:    regionsGeographic,
			ProximityFrom: proximityFrom,
			CostTier:      costTier,
		}

		// Validate constraint
		if err := validateRegionConstraint(constraint); err != nil {
			return fmt.Errorf("invalid region constraint: %w", err)
		}

		// Get all regions from parameter file
		allRegions := make([]string, 0, len(regionGroups))
		for region := range regionGroups {
			allRegions = append(allRegions, region)
		}

		// Apply constraints
		filteredRegions, err := applyRegionConstraints(allRegions, constraint)
		if err != nil {
			return fmt.Errorf("region constraints failed: %w", err)
		}

		// Remove filtered-out regions
		for region := range regionGroups {
			if !containsString(filteredRegions, region) {
				delete(regionGroups, region)
			}
		}

		// Store constraint in record
		record.RegionConstraints = constraint
		record.FilteredRegions = filteredRegions

		fmt.Fprintf(os.Stderr, "🌍 Applied region constraints: %d regions allowed\n", len(filteredRegions))
		fmt.Fprintf(os.Stderr, "   Filtered regions: %v\n", filteredRegions)
		fmt.Fprintf(os.Stderr, "   Constraint: %s\n", formatConstraint(constraint))
	}

	if len(regionGroups) == 1 {
		// Single region - use that as the sweep region
		for region := range regionGroups {
			record.Region = region
			break
		}
	} else if len(regionGroups) > 1 {
		// Multi-region sweep
		record.MultiRegion = true
		record.RegionStatus = make(map[string]*sweep.RegionProgress)

		regions := make([]string, 0, len(regionGroups))
		for region, indices := range regionGroups {
			regions = append(regions, region)
			record.RegionStatus[region] = &sweep.RegionProgress{
				NextToLaunch: indices,
				Launched:     0,
				Failed:       0,
				ActiveCount:  0,
			}
		}

		fmt.Fprintf(os.Stderr, "🌍 Multi-region sweep detected: %v\n", regions)
	}

	// Set distribution mode (only applies to multi-region sweeps)
	if record.MultiRegion {
		record.DistributionMode = distributionMode
		if distributionMode == "opportunistic" {
			fmt.Fprintf(os.Stderr, "📊 Distribution mode: opportunistic (prioritize available regions)\n")
		}
	}

	err = sweep.CreateSweepRecord(ctx, infraCfg, record)
	if err != nil {
		return fmt.Errorf("failed to create sweep record: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Record created\n\n")

	// Invoke Lambda orchestrator
	fmt.Fprintf(os.Stderr, "🚀 Invoking Lambda orchestrator...\n")
	err = sweep.InvokeSweepOrchestrator(ctx, infraCfg, sweepID)
	if err != nil {
		return fmt.Errorf("failed to invoke Lambda: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Lambda invoked\n\n")

	// Display success
	fmt.Fprintf(os.Stderr, "✅ Parameter sweep queued successfully!\n\n")
	fmt.Fprintf(os.Stderr, "Sweep ID:          %s\n", sweepID)
	fmt.Fprintf(os.Stderr, "Sweep Name:        %s\n", sweepName)
	fmt.Fprintf(os.Stderr, "Total Parameters:  %d\n", len(paramFormat.Params))
	fmt.Fprintf(os.Stderr, "Max Concurrent:    %d\n", maxConcurrent)
	fmt.Fprintf(os.Stderr, "Region:            %s\n", sweepRegion)
	fmt.Fprintf(os.Stderr, "Orchestration:     Lambda (infra account)\n\n")

	fmt.Fprintf(os.Stderr, "The sweep is now running in Lambda. You can disconnect safely.\n\n")
	fmt.Fprintf(os.Stderr, "To check status:\n")
	fmt.Fprintf(os.Stderr, "  spawn status --sweep-id %s\n\n", sweepID)
	fmt.Fprintf(os.Stderr, "To resume if needed:\n")
	fmt.Fprintf(os.Stderr, "  spawn resume --sweep-id %s --detach\n", sweepID)

	// Wait for completion if requested
	if wait {
		timeout, _ := time.ParseDuration(waitTimeout)
		if err := waitForSweepCompletion(ctx, sweepID, timeout); err != nil {
			return fmt.Errorf("wait failed: %w", err)
		}
	}

	return nil
}

// launchWithBatchQueue launches a single instance with a batch job queue
func launchWithBatchQueue(ctx context.Context, plat *platform.Platform, auditLog *audit.AuditLogger) error {
	fmt.Fprintf(os.Stderr, "\n📦 Launching Batch Queue Instance\n\n")

	// Load and validate queue configuration
	var queueConfig *queue.QueueConfig
	var err error

	if queueTemplate != "" {
		// Generate from template
		fmt.Fprintf(os.Stderr, "📋 Loading template: %s\n", queueTemplate)
		tmpl, err := queue.LoadTemplate(queueTemplate)
		if err != nil {
			return fmt.Errorf("failed to load template: %w", err)
		}

		// Show required variables if none provided
		if len(templateVars) == 0 {
			var requiredVars []string
			for _, v := range tmpl.Variables {
				if v.Required {
					requiredVars = append(requiredVars, v.Name)
				}
			}
			if len(requiredVars) > 0 {
				return fmt.Errorf("template requires variables: %v\nUse --template-var KEY=VALUE", requiredVars)
			}
		}

		fmt.Fprintf(os.Stderr, "✓ Template loaded: %s (%d jobs)\n", tmpl.Description, len(tmpl.Config.Jobs))
		fmt.Fprintf(os.Stderr, "🔧 Substituting variables...\n")

		queueConfig, err = tmpl.Substitute(templateVars)
		if err != nil {
			return fmt.Errorf("failed to generate queue from template: %w", err)
		}

		fmt.Fprintf(os.Stderr, "✓ Queue generated: %d jobs\n", len(queueConfig.Jobs))
	} else if batchQueueFile != "" {
		// Load from file
		fmt.Fprintf(os.Stderr, "📋 Loading queue configuration...\n")
		queueConfig, err = queue.LoadConfig(batchQueueFile)
		if err != nil {
			return fmt.Errorf("failed to load queue configuration: %w", err)
		}
		fmt.Fprintf(os.Stderr, "✓ Queue loaded: %d jobs\n", len(queueConfig.Jobs))
	} else {
		return fmt.Errorf("either --batch-queue or --queue-template is required")
	}

	// Generate queue ID if not set
	if queueConfig.QueueID == "" {
		queueConfig.QueueID = queue.GenerateQueueID()
	}

	// Validate required flags
	if instanceType == "" {
		return fmt.Errorf("--instance-type is required for batch queue mode")
	}

	// Auto-detect region if not specified
	queueRegion := region
	if queueRegion == "" {
		fmt.Fprintf(os.Stderr, "🌍 No region specified, auto-detecting closest region...\n")
		detectedRegion, err := detectBestRegion(ctx, instanceType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not auto-detect region: %v\n", err)
			fmt.Fprintf(os.Stderr, "   Using default: us-east-1\n")
			queueRegion = "us-east-1"
		} else {
			fmt.Fprintf(os.Stderr, "✓ Selected region: %s\n", detectedRegion)
			queueRegion = detectedRegion
		}
	}

	// Load AWS config for spore-host-dev (where EC2 instances run)
	devCfg, err := spawnconfig.LoadComputeAWSConfig(ctx, queueRegion)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get AWS account ID
	stsClient := sts.NewFromConfig(devCfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("failed to get caller identity: %w", err)
	}
	accountID := *identity.Account

	// Upload queue configuration to S3
	fmt.Fprintf(os.Stderr, "\n📤 Uploading queue configuration to S3...\n")

	// Create queue JSON
	queueJSON, err := json.MarshalIndent(queueConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal queue config: %w", err)
	}

	// Write to temp file
	tmpFile, err := os.CreateTemp("", "spawn-queue-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if _, err := tmpFile.Write(queueJSON); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	_ = tmpFile.Close()

	// Upload to S3
	stagingClient := staging.NewClient(devCfg, accountID)
	scheduleBucket, s3Key, size, _, err := stagingClient.UploadScheduleParams(ctx, tmpFile.Name(), queueConfig.QueueID, queueRegion)
	if err != nil {
		return fmt.Errorf("failed to upload queue config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Uploaded: %s (%.2f KB)\n", s3Key, float64(size)/1024)

	// Build combined user-data: standard spored installer + queue runner.
	// The queue runner script waits for spored to be ready before executing jobs.
	s3URL := fmt.Sprintf("s3://%s/%s", scheduleBucket, s3Key)
	queueRunnerScript := userdata.GenerateQueueRunnerUserData(s3URL, queueConfig.QueueID)

	// Build the standard spored userdata (SSH key setup + spored installer)
	stdScript, buildErr := buildUserData(plat, &aws.LaunchConfig{
		InstanceType: instanceType,
		Region:       queueRegion,
	}, "")
	var combinedScript string
	if buildErr == nil && stdScript != "" {
		// Append queue runner after spored installer (strip duplicate #!/bin/bash header)
		queuePart := queueRunnerScript
		if len(queuePart) > 3 && queuePart[:2] == "#!" {
			// Find end of first line to strip shebang
			nl := 0
			for i, c := range queuePart {
				if c == '\n' {
					nl = i + 1
					break
				}
			}
			queuePart = queuePart[nl:]
		}
		combinedScript = stdScript + "\n\n# === Batch queue runner ===\n" + queuePart
	} else {
		combinedScript = queueRunnerScript
	}
	queueUserData := encodeUserData(combinedScript)

	// Auto-detect AMI if not specified
	resolvedAMI := ami
	if resolvedAMI == "" {
		awsClientForAMI, amiErr := aws.NewClient(ctx)
		if amiErr == nil {
			if detected, amiErr2 := awsClientForAMI.GetRecommendedAMI(ctx, queueRegion, instanceType); amiErr2 == nil {
				resolvedAMI = detected
			}
		}
	}

	// Build launch config
	instanceName := name
	if instanceName == "" {
		instanceName = fmt.Sprintf("%s-%s", queueConfig.QueueName, queueConfig.QueueID)
	}
	launchConfig := &aws.LaunchConfig{
		Name:         instanceName,
		InstanceType: instanceType,
		Region:       queueRegion,
		AMI:          resolvedAMI,
		KeyName:      keyPair,
		UserData:     queueUserData,
		Spot:         spot,
		SpotMaxPrice: spotMaxPrice,
		Hibernate:    hibernate,
		TTL:          queueConfig.GlobalTimeout, // Use global timeout as TTL
		DNSName:      instanceName,
	}

	// Add IAM role if specified
	if iamRole != "" {
		launchConfig.IamInstanceProfile = iamRole
	}

	// Add network config if specified (sgID may be empty — let spawn auto-create)
	if sgID != "" {
		launchConfig.SecurityGroupIDs = []string{sgID}
	}
	if subnetID != "" {
		launchConfig.SubnetID = subnetID
	}

	// CRITICAL SAFETY CHECK: Prevent zombie instances
	// If neither TTL nor idle timeout are set, default to 1h idle timeout
	if launchConfig.TTL == "" && launchConfig.IdleTimeout == "" && !noTimeout {
		launchConfig.IdleTimeout = "1h"
		fmt.Fprintf(os.Stderr, "\n⚠️  Auto-setting --idle-timeout=1h to prevent zombie instances\n")
		fmt.Fprintf(os.Stderr, "   Instance will terminate after 1 hour of inactivity.\n")
		fmt.Fprintf(os.Stderr, "   Override with --ttl, --idle-timeout, or --no-timeout\n\n")
	} else if noTimeout {
		fmt.Fprintf(os.Stderr, "\n⚠️  WARNING: --no-timeout specified\n")
		fmt.Fprintf(os.Stderr, "   Instance will run indefinitely until manually terminated.\n\n")
	}

	// Initialize AWS client
	awsClient, err := aws.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize AWS client: %w", err)
	}

	// Set up SSH key pair if not specified
	if launchConfig.KeyName == "" {
		keyName, err := setupSSHKey(ctx, awsClient, queueRegion, launchConfig.AMI, plat)
		if err != nil {
			return fmt.Errorf("failed to setup SSH key: %w", err)
		}
		launchConfig.KeyName = keyName
	}

	// Set up IAM instance profile if not specified
	if launchConfig.IamInstanceProfile == "" {
		instanceProfile, err := awsClient.SetupSporedIAMRole(ctx)
		if err != nil {
			return fmt.Errorf("failed to setup IAM role: %w", err)
		}
		launchConfig.IamInstanceProfile = instanceProfile
	}

	// Launch instance
	fmt.Fprintf(os.Stderr, "\n🚀 Launching instance...\n")
	instance, err := awsClient.Launch(ctx, *launchConfig)
	if err != nil {
		return fmt.Errorf("failed to launch instance: %w", err)
	}

	// Write instance ID to file for workflow integration
	if err := writeOutputID(instance.InstanceID, outputIDFile); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Failed to write instance ID to file: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "\n✅ Batch queue instance launched!\n\n")
	fmt.Fprintf(os.Stderr, "Queue ID:       %s\n", queueConfig.QueueID)
	fmt.Fprintf(os.Stderr, "Instance ID:    %s\n", instance.InstanceID)
	fmt.Fprintf(os.Stderr, "Instance Type:  %s\n", instanceType)
	fmt.Fprintf(os.Stderr, "Region:         %s\n", queueRegion)
	fmt.Fprintf(os.Stderr, "Total Jobs:     %d\n", len(queueConfig.Jobs))
	fmt.Fprintf(os.Stderr, "Global Timeout: %s\n", queueConfig.GlobalTimeout)
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "The instance will execute jobs sequentially according to dependencies.\n")
	fmt.Fprintf(os.Stderr, "Results will be uploaded to: %s/%s/\n", queueConfig.ResultS3Bucket, queueConfig.ResultS3Prefix)
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "To check queue status:\n")
	fmt.Fprintf(os.Stderr, "  spawn queue status %s\n\n", instance.InstanceID)
	fmt.Fprintf(os.Stderr, "To download results:\n")
	fmt.Fprintf(os.Stderr, "  spawn queue results %s --output ./results/\n", queueConfig.QueueID)
	fmt.Fprintf(os.Stderr, "\n")

	return nil
}

// shouldApplyRegionConstraints checks if any region constraint flags are set
func shouldApplyRegionConstraints() bool {
	return len(regionsInclude) > 0 ||
		len(regionsExclude) > 0 ||
		len(regionsGeographic) > 0 ||
		proximityFrom != "" ||
		costTier != ""
}

// validateRegionConstraint validates region constraint parameters
func validateRegionConstraint(constraint *sweep.RegionConstraint) error {
	// Validate cost tier
	if constraint.CostTier != "" {
		validTiers := map[string]bool{
			"low":      true,
			"standard": true,
			"premium":  true,
		}
		if !validTiers[constraint.CostTier] {
			return fmt.Errorf("invalid cost tier: %s (valid: low, standard, premium)", constraint.CostTier)
		}
	}

	// Validate proximity region
	if constraint.ProximityFrom != "" {
		if !regions.IsValidRegion(constraint.ProximityFrom) {
			return fmt.Errorf("invalid proximity region: %s", constraint.ProximityFrom)
		}
	}

	// Validate geographic groups
	for _, group := range constraint.Geographic {
		if _, ok := regions.GeographicGroups[group]; !ok {
			return fmt.Errorf("invalid geographic group: %s", group)
		}
	}

	return nil
}

// applyRegionConstraints filters regions based on constraints
func applyRegionConstraints(allRegions []string, constraint *sweep.RegionConstraint) ([]string, error) {
	candidates := make([]string, len(allRegions))
	copy(candidates, allRegions)

	// Apply include filter
	if len(constraint.Include) > 0 {
		candidates = filterIncludeRegions(candidates, constraint.Include)
	}

	// Apply exclude filter
	if len(constraint.Exclude) > 0 {
		candidates = filterExcludeRegions(candidates, constraint.Exclude)
	}

	// Apply geographic filter
	if len(constraint.Geographic) > 0 {
		candidates = filterGeographicRegions(candidates, constraint.Geographic)
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no regions match constraints: %s", formatConstraint(constraint))
	}

	return candidates, nil
}

// filterIncludeRegions keeps only regions matching include patterns
func filterIncludeRegions(allRegions []string, patterns []string) []string {
	result := make([]string, 0, len(allRegions))
	for _, region := range allRegions {
		if matchesAnyPattern(region, patterns) {
			result = append(result, region)
		}
	}
	return result
}

// filterExcludeRegions removes regions matching exclude patterns
func filterExcludeRegions(allRegions []string, patterns []string) []string {
	result := make([]string, 0, len(allRegions))
	for _, region := range allRegions {
		if !matchesAnyPattern(region, patterns) {
			result = append(result, region)
		}
	}
	return result
}

// filterGeographicRegions keeps only regions in specified geographic groups
func filterGeographicRegions(allRegions []string, groups []string) []string {
	allowed := make(map[string]bool)
	for _, group := range groups {
		if groupRegions, ok := regions.GeographicGroups[group]; ok {
			for _, r := range groupRegions {
				allowed[r] = true
			}
		}
	}

	result := make([]string, 0, len(allRegions))
	for _, region := range allRegions {
		if allowed[region] {
			result = append(result, region)
		}
	}
	return result
}

// matchesAnyPattern checks if region matches any of the patterns
func matchesAnyPattern(region string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchesWildcard(region, pattern) {
			return true
		}
	}
	return false
}

// matchesWildcard matches region against pattern with wildcard support
func matchesWildcard(s, pattern string) bool {
	// Exact match
	if s == pattern {
		return true
	}

	// Prefix wildcard (us-*)
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(s, prefix)
	}

	// Suffix wildcard (*-1)
	if strings.HasPrefix(pattern, "*") {
		suffix := strings.TrimPrefix(pattern, "*")
		return strings.HasSuffix(s, suffix)
	}

	return false
}

// containsString checks if slice contains string
func containsString(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// formatConstraint returns human-readable constraint description
func formatConstraint(c *sweep.RegionConstraint) string {
	parts := []string{}

	if len(c.Include) > 0 {
		parts = append(parts, fmt.Sprintf("include=%s", strings.Join(c.Include, ",")))
	}
	if len(c.Exclude) > 0 {
		parts = append(parts, fmt.Sprintf("exclude=%s", strings.Join(c.Exclude, ",")))
	}
	if len(c.Geographic) > 0 {
		parts = append(parts, fmt.Sprintf("geographic=%s", strings.Join(c.Geographic, ",")))
	}
	if c.ProximityFrom != "" {
		parts = append(parts, fmt.Sprintf("proximity_from=%s", c.ProximityFrom))
	}
	if c.CostTier != "" {
		parts = append(parts, fmt.Sprintf("cost_tier=%s", c.CostTier))
	}

	if len(parts) == 0 {
		return "no constraints"
	}

	return strings.Join(parts, ", ")
}

// writeOutputID writes sweep/instance ID to file for workflow integration
func writeOutputID(id, filepath string) error {
	if filepath == "" {
		return nil
	}
	return os.WriteFile(filepath, []byte(id+"\n"), 0644)
}

// waitForSSHReady polls TCP port 22 until it accepts a connection or the
// deadline passes. This replaces a fixed sleep with an actual readiness probe:
// it returns the instant SSH is reachable and is bounded so it can't hang.
// Best-effort — a timeout is not fatal (the user can still connect later).
func waitForSSHReady(ctx context.Context, host string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	addr := net.JoinHostPort(host, "22")
	for {
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// waitForSweepCompletion polls sweep status until completion or timeout
func waitForSweepCompletion(ctx context.Context, sweepID string, timeout time.Duration) error {
	cfg, err := spawnconfig.LoadInfraAWSConfig(ctx, "us-east-1")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	startTime := time.Now()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	fmt.Fprintf(os.Stderr, "\n⏳ Waiting for sweep to complete...\n")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			status, err := sweep.QuerySweepStatus(ctx, cfg, sweepID)
			if err != nil {
				return fmt.Errorf("failed to query status: %w", err)
			}

			fmt.Fprintf(os.Stderr, "   Progress: %d/%d launched, Status: %s\n",
				status.Launched, status.TotalParams, status.Status)

			switch status.Status {
			case "COMPLETED":
				fmt.Fprintf(os.Stderr, "✅ Sweep completed successfully\n")
				return nil
			case "FAILED":
				return fmt.Errorf("sweep failed: %s", status.ErrorMessage)
			case "CANCELLED":
				return fmt.Errorf("sweep was cancelled")
			}

			// Check timeout
			if timeout > 0 && time.Since(startTime) > timeout {
				return fmt.Errorf("timeout waiting for completion")
			}
		}
	}
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
	if !cmd.Flags().Changed("hibernate-on-idle") && d.HibernateOnIdle != nil {
		hibernateOnIdle = *d.HibernateOnIdle
	}
}
