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
	"github.com/spore-host/spawn/pkg/locality"
	"github.com/spore-host/spawn/pkg/platform"
	"github.com/spore-host/spawn/pkg/plugin"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/queue"
	"github.com/spore-host/spawn/pkg/regions"
	"github.com/spore-host/spawn/pkg/security"
	"github.com/spore-host/spawn/pkg/staging"
	"github.com/spore-host/spawn/pkg/storage"
	"github.com/spore-host/spawn/pkg/sweep"
	"github.com/spore-host/spawn/pkg/userdata"
	"github.com/spore-host/spawn/pkg/wizard"
	"gopkg.in/yaml.v3"
)

var (
	// Instance config
	instanceType string
	region       string
	az           string
	ami          string

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

	// FSx Lustre
	fsxCreate          bool
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
	launchCmd.Flags().Int32Var(&launchVolumeSize, "volume-size", 0, "Root EBS volume size in GiB (0 = use AMI default)")

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
	launchCmd.Flags().StringVar(&reservationID, "reservation-id", "", "Capacity reservation ID")

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
	launchCmd.Flags().BoolVar(&fsxCreate, "fsx-create", false, "Create new FSx Lustre filesystem with S3 backing")
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

	// CRITICAL SAFETY CHECK: Prevent zombie instances
	// If neither --ttl nor --idle-timeout are set, default to 1h idle timeout
	// This prevents instances from running indefinitely if CLI disconnects
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

	// --estimate-only: print a cost summary and exit without launching (fixes #305).
	if estimateOnly {
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

	// Step 2: Setup SSH key
	prog.Start("Setting up SSH key")
	if firstConfig.KeyName == "" {
		keyName, err := setupSSHKey(ctx, awsClient, firstConfig.Region, plat)
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

	// Build user-data for each config
	for _, cfg := range launchConfigs {
		userDataScript, err := buildUserData(plat, cfg)
		if err != nil {
			return fmt.Errorf("failed to build user data: %w", err)
		}
		cfg.UserData = encodeUserData(userDataScript)
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

	// Step 2: Setup SSH key
	prog.Start("Setting up SSH key")
	if config.KeyName == "" {
		keyName, err := setupSSHKey(ctx, awsClient, config.Region, plat)
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
	} else {
		prog.Skip("Creating security group")
	}

	// Step 4.5: Create or get FSx Lustre filesystem
	var fsxInfo *aws.FSxInfo
	var err error

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
		}

		fsxInfo, err = awsClient.CreateFSxLustreFilesystem(ctx, fsxConfig)
		if err != nil {
			prog.Error("Creating FSx Lustre filesystem", err)
			return fmt.Errorf("failed to create FSx filesystem: %w", err)
		}

		// Ensure Lustre ports (988, 1018–1023) are open self-referencing on each
		// SG used by both FSx and instances. Without this, the MGS connection on
		// 988 succeeds but the follow-on dynamic-port traffic is blocked, causing
		// "client profile could not be read from MGS, rc=-22 EINVAL" (fixes #316).
		for _, sgID := range config.SecurityGroupIDs {
			if err := awsClient.EnsureLustrePorts(ctx, config.Region, sgID); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: could not ensure Lustre ports on %s: %v\n", sgID, err)
			}
		}

		prog.Complete("Creating FSx Lustre filesystem")

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

	// Step 5: Build user data
	userDataScript, err := buildUserData(plat, config)
	if err != nil {
		return fmt.Errorf("failed to build user data: %w", err)
	}

	// Add storage mounting if EFS or FSx enabled (single instance)
	if efsID != "" || fsxInfo != nil {
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

		storageScript, err := userdata.GenerateStorageUserData(storageConfig)
		if err != nil {
			return fmt.Errorf("failed to generate storage user-data: %w", err)
		}

		userDataScript += "\n" + storageScript
	}

	config.UserData = encodeUserData(userDataScript)

	// Validate MPI requirements
	if mpiEnabled {
		if count <= 1 {
			return fmt.Errorf("--mpi requires --count > 1 (need multiple nodes)")
		}
		if jobArrayName == "" {
			return fmt.Errorf("--mpi requires --job-array-name")
		}

		// Validate instance type supports placement groups if enabled
		if mpiAutoPlacementGroup || mpiPlacementGroup != "" {
			if err := awsClient.ValidateInstanceTypeForPlacementGroup(ctx, config.InstanceType); err != nil {
				return fmt.Errorf("placement group validation: %w", err)
			}
		}

		// Create auto placement group if needed
		if mpiAutoPlacementGroup && mpiPlacementGroup == "" {
			mpiPlacementGroup = fmt.Sprintf("spawn-mpi-%s", jobArrayName)
			fmt.Fprintf(os.Stderr, "Creating placement group: %s\n", mpiPlacementGroup)
			if err := awsClient.CreatePlacementGroup(ctx, mpiPlacementGroup, config.Region); err != nil {
				return fmt.Errorf("create placement group: %w", err)
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
		return err
	}
	auditLog.LogOperationWithData("launch_instance", result.InstanceID, "success",
		map[string]interface{}{
			"instance_type": config.InstanceType,
			"region":        config.Region,
		}, nil)
	prog.Complete("Launching instance")

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

	// Step 10: Wait for SSH to become reachable. Probe the port rather than
	// sleeping a fixed interval; gated by --wait-for-ssh.
	prog.Start("Waiting for SSH")
	if waitForSSH && result.PublicIP != "" {
		waitForSSHReady(ctx, result.PublicIP, 2*time.Minute)
	}
	prog.Complete("Waiting for SSH")

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
			fqdn, err := registerDNS(plat, result.InstanceID, result.PublicIP, dnsName, dnsConfig.Domain, dnsConfig.APIEndpoint)
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

	// Display success (TUI mode)
	sshCmd := plat.GetSSHCommand("ec2-user", result.PublicIP)
	prog.DisplaySuccess(result.InstanceID, result.PublicIP, sshCmd, config)

	// Show DNS info if registered
	if dnsRecord != "" {
		_, _ = fmt.Fprintf(os.Stdout, "\n🌐 DNS: %s\n", dnsRecord)
		_, _ = fmt.Fprintf(os.Stdout, "   Connect: ssh %s@%s\n", plat.GetUsername(), dnsRecord)
	}

	return nil
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
	if keyPair != "" {
		config.KeyName = keyPair
	}
	if spot {
		config.Spot = true
	}
	if hibernate {
		config.Hibernate = true
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

func setupSSHKey(ctx context.Context, awsClient *aws.Client, region string, plat *platform.Platform) (string, error) {
	// Check for local SSH key
	if !plat.HasSSHKey() {
		// Auto-create SSH key if running in a terminal
		if isTerminal(os.Stdin) {
			fmt.Fprintf(os.Stderr, "\n⚠️  No SSH key found at %s\n", plat.SSHKeyPath)
			fmt.Fprintf(os.Stderr, "   Creating SSH key automatically...\n")

			if err := plat.CreateSSHKey(); err != nil {
				return "", fmt.Errorf("failed to create SSH key: %w", err)
			}

			fmt.Fprintf(os.Stderr, "✅ SSH key created: %s\n\n", plat.SSHKeyPath)
		} else {
			// Non-interactive stdin (piped input) - provide helpful error
			return "", fmt.Errorf("no SSH key found at %s\n\nTo create one:\n  ssh-keygen -t rsa -b 4096 -f %s -N ''\n\nOr run spawn directly (not piped):\n  spawn launch --instance-type m7i.large --region us-east-1",
				plat.SSHKeyPath, plat.SSHKeyPath)
		}
	}

	// Get fingerprint of local key
	fingerprint, err := plat.GetPublicKeyFingerprint()
	if err != nil {
		return "", fmt.Errorf("failed to get key fingerprint: %w", err)
	}

	// Check if this key already exists in AWS (by fingerprint)
	existingKeyName, err := awsClient.FindKeyPairByFingerprint(ctx, region, fingerprint)
	if err != nil {
		return "", fmt.Errorf("failed to search for existing key: %w", err)
	}

	// If found, use the existing key
	if existingKeyName != "" {
		return existingKeyName, nil
	}

	// Key not found in AWS, upload it with generated name
	keyName := fmt.Sprintf("spawn-key-%s", plat.GetUsername())

	publicKey, err := plat.ReadPublicKey()
	if err != nil {
		return "", fmt.Errorf("failed to read public key: %w", err)
	}

	err = awsClient.ImportKeyPair(ctx, region, keyName, publicKey)
	if err != nil {
		return "", fmt.Errorf("failed to import key pair: %w", err)
	}

	return keyName, nil
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

func buildUserData(plat *platform.Platform, config *aws.LaunchConfig) (string, error) {
	// Get local username and SSH public key
	username := plat.GetUsername()
	publicKey, err := plat.ReadPublicKey()
	if err != nil {
		return "", fmt.Errorf("failed to read SSH public key: %w", err)
	}
	publicKeyBase64 := base64.StdEncoding.EncodeToString(publicKey)

	// Validate username and public key before using in script
	if err := security.ValidateUsername(username); err != nil {
		return "", fmt.Errorf("invalid username: %w", err)
	}
	if err := security.ValidateBase64(publicKeyBase64); err != nil {
		return "", fmt.Errorf("invalid public key encoding: %w", err)
	}

	// Read custom user data if provided
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

	// Build user-data with spored installer (S3-based with SHA256 verification)
	script := fmt.Sprintf(`#!/bin/bash
set -e

# User configuration
LOCAL_USERNAME=%s
LOCAL_SSH_KEY_BASE64=%s
`, security.ShellEscape(username), security.ShellEscape(publicKeyBase64)) + `

# Detect architecture
ARCH=$(uname -m)
echo "Installing spored for architecture: $ARCH"

# Detect region
TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" 2>/dev/null || true)
if [ -n "$TOKEN" ]; then
    REGION=$(curl -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region 2>/dev/null)
else
    REGION=$(curl -s http://169.254.169.254/latest/meta-data/placement/region 2>/dev/null || echo "us-east-1")
fi

echo "Region: $REGION"

# Determine binary name
case "$ARCH" in
    x86_64)
        BINARY="spored-linux-amd64"
        ;;
    aarch64)
        BINARY="spored-linux-arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

# Download from S3 (public buckets, regional for low latency)
S3_BASE_URL="https://spawn-binaries-${REGION}.s3.amazonaws.com"
FALLBACK_URL="https://spawn-binaries-us-east-1.s3.amazonaws.com"
PROJECT="spawn"

echo "Downloading spored binary..."

# Download to a temp path, NOT directly over /usr/local/bin/spored. Writing in
# place fails with "Text file busy" (ETXTBSY) if a spored is already running
# (baked into the AMI or from a previous boot); we install via atomic rename
# below, which the kernel allows even while the old binary executes (#27).
SPORED_TMP="$(mktemp /tmp/spored.XXXXXX)"

# Try regional bucket with project prefix first, then without prefix (legacy), then fallback to us-east-1
if curl -f -o "$SPORED_TMP" "${S3_BASE_URL}/${PROJECT}/${BINARY}" 2>/dev/null; then
    CHECKSUM_URL="${S3_BASE_URL}/${PROJECT}/${BINARY}.sha256"
    echo "Downloaded from ${REGION}"
elif curl -f -o "$SPORED_TMP" "${S3_BASE_URL}/${BINARY}" 2>/dev/null; then
    CHECKSUM_URL="${S3_BASE_URL}/${BINARY}.sha256"
    echo "Downloaded from ${REGION} (legacy path)"
else
    echo "Regional bucket unavailable, trying us-east-1..."
    if curl -f -o "$SPORED_TMP" "${FALLBACK_URL}/${PROJECT}/${BINARY}" 2>/dev/null; then
        CHECKSUM_URL="${FALLBACK_URL}/${PROJECT}/${BINARY}.sha256"
    else
        curl -f -o "$SPORED_TMP" "${FALLBACK_URL}/${BINARY}" || {
            echo "Failed to download spored binary"
            rm -f "$SPORED_TMP"
            exit 1
        }
        CHECKSUM_URL="${FALLBACK_URL}/${BINARY}.sha256"
    fi
    echo "Downloaded from us-east-1"
fi

# Download and verify SHA256 checksum (against the temp file).
echo "Verifying checksum..."
curl -f -o /tmp/spored.sha256 "${CHECKSUM_URL}" || {
    echo "Failed to download checksum"
    rm -f "$SPORED_TMP"
    exit 1
}

EXPECTED_CHECKSUM=$(cat /tmp/spored.sha256)
ACTUAL_CHECKSUM=$(sha256sum "$SPORED_TMP" | awk '{print $1}')

if [ "$EXPECTED_CHECKSUM" != "$ACTUAL_CHECKSUM" ]; then
    echo "❌ Checksum verification failed!"
    echo "   Expected: $EXPECTED_CHECKSUM"
    echo "   Actual:   $ACTUAL_CHECKSUM"
    rm -f "$SPORED_TMP"
    exit 1
fi

echo "✅ Checksum verified: $EXPECTED_CHECKSUM"
chmod +x "$SPORED_TMP"

# Stop any running spored so the rename doesn't leave a stale daemon, then
# atomically move the verified binary into place. rename(2) over a busy binary
# is permitted — the running process keeps the old (now-unlinked) inode — so
# this never hits ETXTBSY.
systemctl stop spored 2>/dev/null || true
mv -f "$SPORED_TMP" /usr/local/bin/spored

# Setup local user account
echo "Setting up user: $LOCAL_USERNAME"

# Create user if doesn't exist
if ! id "$LOCAL_USERNAME" &>/dev/null; then
    useradd -m -s /bin/bash "$LOCAL_USERNAME"
    echo "Created user: $LOCAL_USERNAME"
fi

# Add to sudo/wheel group (passwordless sudo like ec2-user)
usermod -aG wheel "$LOCAL_USERNAME" 2>/dev/null || usermod -aG sudo "$LOCAL_USERNAME" 2>/dev/null
echo "$LOCAL_USERNAME ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/$LOCAL_USERNAME
chmod 0440 /etc/sudoers.d/$LOCAL_USERNAME

# Setup SSH for local user
mkdir -p /home/$LOCAL_USERNAME/.ssh
chmod 700 /home/$LOCAL_USERNAME/.ssh

# Decode and write SSH public key
echo "$LOCAL_SSH_KEY_BASE64" | base64 -d > /home/$LOCAL_USERNAME/.ssh/authorized_keys
chmod 600 /home/$LOCAL_USERNAME/.ssh/authorized_keys
chown -R $LOCAL_USERNAME:$LOCAL_USERNAME /home/$LOCAL_USERNAME/.ssh

echo "✅ User $LOCAL_USERNAME configured with SSH access and sudo privileges"

# Configure automatic logout for idle sessions
# This prevents indefinite logins when users leave sessions idle
echo "Configuring session timeouts..."

# Get session timeout from EC2 tags
INSTANCE_ID=$(ec2-metadata --instance-id | cut -d " " -f 2)
SESSION_TIMEOUT=$(aws ec2 describe-tags --region $REGION \
    --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:session-timeout" \
    --query 'Tags[0].Value' --output text 2>/dev/null || echo "30m")

# Convert duration to seconds
parse_duration() {
    local duration="$1"
    local total=0

    # Handle 0 or disabled
    if [ "$duration" = "0" ] || [ "$duration" = "disabled" ]; then
        echo "0"
        return
    fi

    # Parse duration (e.g., 30m, 1h30m, 2h)
    while [[ $duration =~ ([0-9]+)([smhd]) ]]; do
        local value="${BASH_REMATCH[1]}"
        local unit="${BASH_REMATCH[2]}"

        case "$unit" in
            s) total=$((total + value)) ;;
            m) total=$((total + value * 60)) ;;
            h) total=$((total + value * 3600)) ;;
            d) total=$((total + value * 86400)) ;;
        esac

        duration="${duration#*${BASH_REMATCH[0]}}"
    done

    echo "$total"
}

TIMEOUT_SECONDS=$(parse_duration "$SESSION_TIMEOUT")

if [ "$TIMEOUT_SECONDS" -gt 0 ]; then
    # SSH server timeout (disconnect idle SSH connections)
    # Use 1/2 of session timeout for SSH keepalive
    SSH_INTERVAL=$((TIMEOUT_SECONDS / 6))
    if [ $SSH_INTERVAL -lt 60 ]; then
        SSH_INTERVAL=60
    fi

    if ! grep -q "^ClientAliveInterval" /etc/ssh/sshd_config; then
        echo "ClientAliveInterval $SSH_INTERVAL" >> /etc/ssh/sshd_config
        echo "ClientAliveCountMax 3" >> /etc/ssh/sshd_config
        systemctl reload sshd 2>/dev/null || service sshd reload 2>/dev/null || true
    fi

    # Shell timeout (auto-logout idle shells)
    # Set TMOUT for all users via /etc/profile.d/
    cat > /etc/profile.d/session-timeout.sh <<EOFTIMEOUT
# Automatic logout for idle shells ($SESSION_TIMEOUT)
# Prevents indefinite logins when users leave sessions idle
# Note: readonly prevents users from unsetting it
export TMOUT=$TIMEOUT_SECONDS
readonly TMOUT
EOFTIMEOUT

    chmod 644 /etc/profile.d/session-timeout.sh

    echo "✅ Session timeouts configured (Timeout: $SESSION_TIMEOUT)"
else
    echo "✅ Session timeouts disabled (set spawn:session-timeout tag to enable)"
fi

# Setup job array environment variables (if part of a job array)
JOB_ARRAY_ID=$(aws ec2 describe-tags --region $REGION \
    --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:job-array-id" \
    --query 'Tags[0].Value' --output text 2>/dev/null || echo "None")

if [ "$JOB_ARRAY_ID" != "None" ]; then
    echo "Setting up job array environment..."

    JOB_ARRAY_NAME=$(aws ec2 describe-tags --region $REGION \
        --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:job-array-name" \
        --query 'Tags[0].Value' --output text 2>/dev/null || echo "")

    JOB_ARRAY_SIZE=$(aws ec2 describe-tags --region $REGION \
        --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:job-array-size" \
        --query 'Tags[0].Value' --output text 2>/dev/null || echo "0")

    JOB_ARRAY_INDEX=$(aws ec2 describe-tags --region $REGION \
        --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:job-array-index" \
        --query 'Tags[0].Value' --output text 2>/dev/null || echo "0")

    # Create job array environment file
    cat > /etc/profile.d/job-array.sh <<EOFJOBARRAY
# Job Array Environment Variables
# Available to all shells for coordinated distributed computing
export JOB_ARRAY_ID="$JOB_ARRAY_ID"
export JOB_ARRAY_NAME="$JOB_ARRAY_NAME"
export JOB_ARRAY_SIZE="$JOB_ARRAY_SIZE"
export JOB_ARRAY_INDEX="$JOB_ARRAY_INDEX"
EOFJOBARRAY

    chmod 644 /etc/profile.d/job-array.sh

    echo "✅ Job array environment configured (Index: $JOB_ARRAY_INDEX/$JOB_ARRAY_SIZE)"
fi

# Setup parameter sweep environment variables (if part of a sweep)
SWEEP_ID=$(aws ec2 describe-tags --region $REGION \
    --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:sweep-id" \
    --query 'Tags[0].Value' --output text 2>/dev/null || echo "None")

if [ "$SWEEP_ID" != "None" ]; then
    echo "Setting up parameter sweep environment..."

    SWEEP_NAME=$(aws ec2 describe-tags --region $REGION \
        --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:sweep-name" \
        --query 'Tags[0].Value' --output text 2>/dev/null || echo "")

    SWEEP_SIZE=$(aws ec2 describe-tags --region $REGION \
        --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:sweep-size" \
        --query 'Tags[0].Value' --output text 2>/dev/null || echo "0")

    SWEEP_INDEX=$(aws ec2 describe-tags --region $REGION \
        --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:sweep-index" \
        --query 'Tags[0].Value' --output text 2>/dev/null || echo "0")

    # Query all spawn:param:* tags
    PARAM_TAGS=$(aws ec2 describe-tags --region $REGION \
        --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:param:*" \
        --query 'Tags[*].[Key,Value]' --output text 2>/dev/null || echo "")

    # Create parameter sweep environment file
    cat > /etc/profile.d/spawn-params.sh <<EOFPARAMS
# Parameter Sweep Environment Variables
# Available to all shells for parameter sweep workloads
export SWEEP_ID="$SWEEP_ID"
export SWEEP_NAME="$SWEEP_NAME"
export SWEEP_SIZE="$SWEEP_SIZE"
export SWEEP_INDEX="$SWEEP_INDEX"

# Parse and export PARAM_* environment variables from tags
EOFPARAMS

    # Parse param tags and add exports
    echo "$PARAM_TAGS" | while IFS=$'\t' read -r key value; do
        if [[ $key == spawn:param:* ]]; then
            param_name=${key#spawn:param:}
            echo "export PARAM_${param_name}=\"${value}\"" >> /etc/profile.d/spawn-params.sh
        fi
    done

    chmod 644 /etc/profile.d/spawn-params.sh

    echo "✅ Parameter sweep environment configured (Index: $SWEEP_INDEX/$SWEEP_SIZE)"
fi

# Execute the --command workload if specified. This runs for ANY instance
# carrying a spawn:command tag, not just parameter-sweep instances. Previously
# this block was nested inside the SWEEP_ID gate above, so a plain single
# launch with --command silently never ran the command (#61).
WORKFLOW_COMMAND=$(aws ec2 describe-tags --region $REGION \
    --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:command" \
    --query 'Tags[0].Value' --output text 2>/dev/null || echo "None")

if [ "$WORKFLOW_COMMAND" != "None" ] && [ -n "$WORKFLOW_COMMAND" ]; then
    STEP_NAME=$(aws ec2 describe-tags --region $REGION \
        --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:step" \
        --query 'Tags[0].Value' --output text 2>/dev/null || echo "")

    if [ -n "$STEP_NAME" ]; then
        echo "▶️  Executing workflow step: $STEP_NAME"
    else
        echo "▶️  Executing --command"
    fi

    # Create command execution script. Source the parameter-sweep env first if
    # present (sweep instances), but don't require it (single instances).
    cat > /tmp/spawn-command.sh <<'EOFCMD'
#!/bin/bash
set -e
[ -f /etc/profile.d/spawn-params.sh ] && source /etc/profile.d/spawn-params.sh
EOFCMD
    echo "$WORKFLOW_COMMAND" >> /tmp/spawn-command.sh
    chmod +x /tmp/spawn-command.sh

    # Execute command as the instance's primary user in the background.
    # ($LOCAL_USERNAME is the user the bootstrap creates above; the previous
    # hardcoded "local" user never exists, so the command silently failed — #61.)
    # Log output to both file and console.
    su - "$LOCAL_USERNAME" -c '/tmp/spawn-command.sh' 2>&1 | tee /var/log/spawn-command.log &

    echo "✅ Command execution started (logs: /var/log/spawn-command.log)"
fi

# Create login banner (MOTD) with spore configuration
echo "Creating login banner..."

# Get all spore tags for the banner
TTL_TAG=$(aws ec2 describe-tags --region $REGION \
    --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:ttl" \
    --query 'Tags[0].Value' --output text 2>/dev/null || echo "disabled")

IDLE_TIMEOUT_TAG=$(aws ec2 describe-tags --region $REGION \
    --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:idle-timeout" \
    --query 'Tags[0].Value' --output text 2>/dev/null || echo "disabled")

ON_COMPLETE_TAG=$(aws ec2 describe-tags --region $REGION \
    --filters "Name=resource-id,Values=$INSTANCE_ID" "Name=key,Values=spawn:on-complete" \
    --query 'Tags[0].Value' --output text 2>/dev/null || echo "disabled")

# Create the banner with optional job array info
if [ "$JOB_ARRAY_ID" != "None" ]; then
cat > /etc/motd <<EOFMOTD
╔═══════════════════════════════════════════════════════════════╗
║                  🌱 Welcome to Spore                         ║
╚═══════════════════════════════════════════════════════════════╝

Instance: $INSTANCE_ID
Region:   $REGION

Job Array:
  • Array Name:       $JOB_ARRAY_NAME
  • Array ID:         $JOB_ARRAY_ID
  • Instance Index:   $JOB_ARRAY_INDEX / $JOB_ARRAY_SIZE

Spore Agent Configuration:
  • TTL:              $TTL_TAG
  • Idle Timeout:     $IDLE_TIMEOUT_TAG
  • On Complete:      $ON_COMPLETE_TAG
  • Session Timeout:  $SESSION_TIMEOUT

⚠️  IMPORTANT: This shell will auto-logout after $SESSION_TIMEOUT of inactivity
⚠️  SSH connections will disconnect if idle for extended periods

To view current status:
  sudo spored status

To extend TTL:
  spawn extend <instance-id> <new-ttl>

Documentation: https://github.com/spore-host/spore-host

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
EOFMOTD
else
cat > /etc/motd <<EOFMOTD
╔═══════════════════════════════════════════════════════════════╗
║                  🌱 Welcome to Spore                         ║
╚═══════════════════════════════════════════════════════════════╝

Instance: $INSTANCE_ID
Region:   $REGION

Spore Agent Configuration:
  • TTL:              $TTL_TAG
  • Idle Timeout:     $IDLE_TIMEOUT_TAG
  • On Complete:      $ON_COMPLETE_TAG
  • Session Timeout:  $SESSION_TIMEOUT

⚠️  IMPORTANT: This shell will auto-logout after $SESSION_TIMEOUT of inactivity
⚠️  SSH connections will disconnect if idle for extended periods

To view current status:
  sudo spored status

To extend TTL:
  spawn extend <instance-id> <new-ttl>

Documentation: https://github.com/spore-host/spore-host

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
EOFMOTD
fi

chmod 644 /etc/motd
echo "✅ Login banner created"

# Install acpid so AWS stop/terminate signals trigger a graceful shutdown
# rather than a hard kill after the ACPI timeout.
dnf install -y acpid 2>/dev/null || yum install -y acpid 2>/dev/null || true
systemctl enable --now acpid

# Create systemd service
cat > /etc/systemd/system/spored.service <<'EOFSERVICE'
[Unit]
Description=Spawn Agent - Instance self-monitoring
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/spored
# on-failure (not always) prevents restart attempts during graceful shutdown
Restart=on-failure
RestartSec=10
# Give spored time to deregister DNS and clean up before SIGKILL
TimeoutStopSec=30
StandardOutput=journal
StandardError=journal
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOFSERVICE

# Enable and start
systemctl daemon-reload
systemctl enable spored
systemctl start spored

echo "spored installation complete"
`

	// Inject plugin declarations from --plugin flags or --config file.
	decls := collectPluginDeclarations()
	if len(decls) > 0 {
		declJSON, err := json.Marshal(decls)
		if err != nil {
			return "", fmt.Errorf("marshal plugin declarations: %w", err)
		}
		script += fmt.Sprintf(`
# Write plugin declarations for spored to load at startup
mkdir -p /etc/spawn
cat > /etc/spawn/plugins.json <<'EOFPLUGINS'
%s
EOFPLUGINS
chmod 644 /etc/spawn/plugins.json
echo "Plugin declarations written: %d plugin(s)"
`, string(declJSON), len(decls))
	}

	if customUserData != "" {
		// Run the user's script in its own section. The bootstrap above uses
		// `set -e`, so without resetting it a non-fatal earlier failure would
		// skip the user's script entirely and cloud-init would mark the whole
		// user-data failed (#27). Disable -e here so the user's script runs and
		// reports its own exit status; if the user wants strict mode they can
		// re-enable `set -e` at the top of their script.
		script += "\n# Custom user data (runs regardless of bootstrap warnings)\nset +e\n"
		script += customUserData
	}

	return script, nil
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

func registerDNS(plat *platform.Platform, instanceID, publicIP, recordName, domain, apiEndpoint string) (string, error) {
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

	// Execute SSH command
	sshKeyPath := plat.SSHKeyPath
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

				// Add storage user-data if EFS or FSx enabled
				if efsID != "" || fsxInfo != nil {
					storageConfig := userdata.StorageConfig{}

					// EFS configuration
					if efsID != "" {
						mountOptions, err := getEFSMountOptions()
						if err != nil {
							results <- launchResult{
								index: index,
								err:   fmt.Errorf("failed to get EFS mount options: %w", err),
							}
							return
						}

						storageConfig.EFSEnabled = true
						storageConfig.EFSFilesystemDNS = aws.GetEFSDNSName(efsID, baseConfig.Region)
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

					storageScript, err := userdata.GenerateStorageUserData(storageConfig)
					if err != nil {
						results <- launchResult{
							index: index,
							err:   fmt.Errorf("failed to generate storage user-data: %w", err),
						}
						return
					}

					combinedUserData += "\n" + storageScript
				}

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
	})
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
		keyName, err := setupSSHKey(ctx, awsClient, queueRegion, plat)
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
