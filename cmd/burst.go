package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/registry"
	"github.com/spore-host/spawn/pkg/tagprefix"
	ttlpkg "github.com/spore-host/spawn/pkg/ttl"
)

var burstCmd = &cobra.Command{
	Use:   "burst",
	Short: "Launch cloud instances to join local job array",
	Long: `Launch EC2 instances that will register with the hybrid registry
and coordinate with local instances to process workloads.

This enables "cloud bursting" where local compute capacity can be
extended with on-demand cloud resources.

Every instance gets a TTL (--ttl, default 8h) and terminates when it expires.

Each instance installs and verifies spored at boot, the same way spawn launch
does, so any Amazon Linux 2023 image works. Without --ami, burst uses the newest
AMI this account owns whose name matches spawn-* and that fits the instance
type (architecture, Linux, GPU), else the standard Amazon Linux 2023 AMI for the
instance type, as spawn launch picks it.`,
	Example: `  # Launch 10 instances to help process a job array
  spawn burst --count 10 --job-array-id my-array --instance-type c5.4xlarge

  # Launch with specific AMI
  spawn burst --count 5 --job-array-id genomics --ami ami-abc123

  # Launch Spot instances for cost savings, terminated after 2 hours
  spawn burst --count 20 --job-array-id simulation --spot --ttl 2h`,
	RunE: runBurst,
}

// defaultBurstTTL bounds a burst member when --ttl is not given (#917). Burst
// previously set no TTL at all, so nothing on the instance ever ended it.
const defaultBurstTTL = "8h"

var (
	burstCount           int
	burstJobArrayID      string
	burstJobArrayName    string
	burstInstanceType    string
	burstAMI             string
	burstSpot            bool
	burstKeyName         string
	burstSubnetID        string
	burstSecurityGroups  []string
	burstInstanceProfile string
	burstTTL             string
	burstIdleTimeout     string
)

func init() {
	rootCmd.AddCommand(burstCmd)

	burstCmd.Flags().IntVar(&burstCount, "count", 1, "Number of instances to launch")
	burstCmd.Flags().StringVar(&burstJobArrayID, "job-array-id", "", "Job array ID to join (required)")
	burstCmd.Flags().StringVar(&burstJobArrayName, "job-array-name", "", "Job array name, used by spawn array status/cancel (defaults to the job array ID)")
	burstCmd.Flags().StringVar(&burstInstanceType, "instance-type", "t3.micro", "EC2 instance type")
	burstCmd.Flags().StringVar(&burstAMI, "ami", "", "Linux AMI ID (default: the newest self-owned spawn-* AMI that fits --instance-type, else Amazon Linux 2023)")
	burstCmd.Flags().BoolVar(&burstSpot, "spot", false, "Use Spot instances")
	burstCmd.Flags().StringVar(&burstKeyName, "key-name", "", "SSH key pair name")
	burstCmd.Flags().StringVar(&burstSubnetID, "subnet-id", "", "Subnet ID")
	burstCmd.Flags().StringSliceVar(&burstSecurityGroups, "security-group-ids", nil, "Security group IDs (comma-separated or repeated)")
	// Deprecated alias for --security-group-ids (bound to the same var).
	burstCmd.Flags().StringSliceVar(&burstSecurityGroups, "security-groups", nil, "Security group IDs")
	_ = burstCmd.Flags().MarkDeprecated("security-groups", "use --security-group-ids instead")
	burstCmd.Flags().StringVar(&burstInstanceProfile, "instance-profile", "", "Attach this EXISTING IAM instance profile by name instead of the default spored-instance-profile, which spawn creates or reuses as spawn launch does. Its role must let spored read and write its own tags, terminate itself, and use the spawn-hybrid-registry table.")
	burstCmd.Flags().StringVar(&burstTTL, "ttl", defaultBurstTTL, "Terminate each instance after this duration (e.g. 30m, 2h, 36h). Required to be positive: burst instances always have a TTL.")
	burstCmd.Flags().StringVar(&burstIdleTimeout, "idle-timeout", "", "Also terminate an instance after it has been idle this long (e.g. 1h). Empty disables idle termination.")

	_ = burstCmd.MarkFlagRequired("job-array-id")
}

func runBurst(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// Prepare launch parameters, and validate them before creating anything
	// (the registry table, the IAM role) or launching anything.
	launchParams := &LaunchParams{
		Count:          burstCount,
		InstanceType:   burstInstanceType,
		AMI:            burstAMI,
		Spot:           burstSpot,
		KeyName:        burstKeyName,
		SubnetID:       burstSubnetID,
		SecurityGroups: burstSecurityGroups,
		JobArrayID:     burstJobArrayID,
		JobArrayName:   burstJobArrayName,
		TTL:            burstTTL,
		IdleTimeout:    burstIdleTimeout,
	}
	// `spawn array status|cancel` address an array by NAME (cmd/arraygroup.go),
	// and the hint printed below names it. With no --job-array-name the members
	// had an empty name, so the hint found nothing (#917).
	if launchParams.JobArrayName == "" {
		launchParams.JobArrayName = launchParams.JobArrayID
	}
	if err := validateBurstParams(launchParams); err != nil {
		return err
	}

	// Load the AWS config the way every other command does (#939): the root
	// --region and --profile flags, then SPORE_REGION/SPORE_PROFILE and the
	// shared spore config, then the ambient chain. A bare SDK load read none of
	// them, so burst launched in the ambient region whatever --region said. The
	// AMI lookup, the registry table and every member use this one config, so
	// they agree on the region.
	cfg, err := loadAWSConfig(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	if cfg.Region == "" {
		return fmt.Errorf("no AWS region: pass --region, or set SPORE_REGION or AWS_REGION")
	}
	launchParams.Region = cfg.Region
	fmt.Fprintf(os.Stderr, "Region: %s\n", launchParams.Region)

	awsClient := spawnaws.NewClientFromConfig(cfg)

	// Choose (or check) the AMI before creating anything: a Windows --ami is
	// refused here, not after the table and the IAM role exist (#924).
	amiID, err := resolveBurstAMI(ctx, awsClient, launchParams.Region, launchParams.AMI, launchParams.InstanceType)
	if err != nil {
		return err
	}
	launchParams.AMI = amiID
	fmt.Fprintf(os.Stderr, "Using AMI: %s\n", launchParams.AMI)

	// Ensure DynamoDB table exists
	fmt.Fprintln(os.Stderr, "Ensuring DynamoDB registry table exists...")
	if err := registry.EnsureTable(ctx, cfg); err != nil {
		return fmt.Errorf("failed to ensure registry table: %w", err)
	}

	// Resolve the instance profile BEFORE launching anything (#905). Burst
	// members run spored, and spored without credentials can read none of its
	// tags, register with nothing and terminate nothing — so it cannot enforce
	// the lifecycle that is the instance's only in-instance cost backstop.
	// Same resolution as `spawn launch` with no --iam-* flags: the shared
	// spored-instance-profile, created or reused by SetupSporedIAMRole.
	instanceProfile, err := resolveBurstInstanceProfile(ctx, burstInstanceProfile, awsClient.SetupSporedIAMRole)
	if err != nil {
		return err
	}
	launchParams.InstanceProfile = instanceProfile

	// Launch instances
	fmt.Fprintf(os.Stderr, "Launching %d instances to join job array %s (TTL %s)...\n", launchParams.Count, launchParams.JobArrayID, launchParams.TTL)
	instanceIDs, err := launchBurstInstances(ctx, awsClient, launchParams)
	if err != nil {
		if len(instanceIDs) > 0 {
			fmt.Printf("Launched %d of %d instances before the failure (each has TTL %s):\n", len(instanceIDs), launchParams.Count, launchParams.TTL)
			for i, id := range instanceIDs {
				fmt.Printf("  [%d] %s\n", i, id)
			}
			fmt.Printf("Terminate them with: spawn array cancel %s\n", launchParams.JobArrayName)
		}
		return fmt.Errorf("failed to launch instances: %w", err)
	}

	// Print results
	fmt.Printf("✓ Successfully launched %d instances:\n", len(instanceIDs))
	for i, id := range instanceIDs {
		fmt.Printf("  [%d] %s\n", i, id)
	}
	fmt.Printf("\nInstances will register with job array: %s\n", launchParams.JobArrayID)
	fmt.Printf("Each terminates after %s.\n", launchParams.TTL)
	fmt.Printf("Monitor with: spawn array status %s\n", launchParams.JobArrayName)

	return nil
}

type LaunchParams struct {
	Count          int
	InstanceType   string
	AMI            string
	Spot           bool
	KeyName        string
	SubnetID       string
	SecurityGroups []string
	JobArrayID     string
	JobArrayName   string
	// Region is where the members launch: the same region as the registry
	// table EnsureTable created, which is where spored will look for it.
	Region string
	// InstanceProfile is the IAM instance profile NAME every burst member is
	// launched with (#905). Required: burstMemberConfigs refuses to build a
	// member without one.
	InstanceProfile string
	// TTL bounds each member's lifetime (#917). Required and positive.
	TTL string
	// IdleTimeout, if set, also ends a member once idle.
	IdleTimeout string
}

// validateBurstParams rejects a burst that would launch members spored cannot
// bound. The TTL must parse with pkg/ttl's shared parser because that is how the
// shared tag builder (pkg/aws buildTags) computes spawn:ttl-deadline — the tag
// the TTL reaper enforces — and buildTags refuses a TTL it cannot turn into a
// deadline (#990). Checking here refuses it before anything launches.
func validateBurstParams(params *LaunchParams) error {
	if params.Count < 1 {
		return fmt.Errorf("--count must be at least 1, got %d", params.Count)
	}
	if params.TTL == "" {
		return fmt.Errorf("--ttl is required: burst instances must have a TTL")
	}
	if _, err := ttlpkg.ParseTTL(params.TTL); err != nil {
		return fmt.Errorf("--ttl %q: use a duration such as 30m, 2h, 36h or 1d: %w", params.TTL, err)
	}
	if params.IdleTimeout != "" {
		if _, err := ttlpkg.ParseTTL(params.IdleTimeout); err != nil {
			return fmt.Errorf("--idle-timeout %q: use a duration such as 30m or 1h: %w", params.IdleTimeout, err)
		}
	}
	return nil
}

// resolveBurstInstanceProfile picks the instance profile for burst members: the
// caller's explicit --instance-profile if given (no IAM calls at all, matching
// `spawn launch --instance-profile`, #550), otherwise the shared spored profile
// via setup (SetupSporedIAMRole in production).
func resolveBurstInstanceProfile(ctx context.Context, explicit string, setup func(context.Context) (string, error)) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	profile, err := setup(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to set up spored IAM instance profile (pass --instance-profile to use an existing one): %w", err)
	}
	if profile == "" {
		return "", fmt.Errorf("spored IAM setup returned no instance profile")
	}
	return profile, nil
}

// burstLauncher is the launch call burst makes: pkg/aws Client.Launch, the same
// one `spawn launch` uses, so a burst member gets the same RunInstances request
// and, through buildTags, the same tags. An interface so tests can inspect what
// burst asks for without AWS.
type burstLauncher interface {
	Launch(ctx context.Context, cfg spawnaws.LaunchConfig) (*spawnaws.LaunchResult, error)
}

// launchBurstInstances launches one instance per member, so each member carries
// its own spawn:job-array-index (#917). It returns the IDs launched so far even
// on error, so the caller can report instances that are already running.
func launchBurstInstances(ctx context.Context, client burstLauncher, params *LaunchParams) ([]string, error) {
	members, err := burstMemberConfigs(params)
	if err != nil {
		return nil, err
	}

	var instanceIDs []string
	for _, member := range members {
		result, err := client.Launch(ctx, member)
		if err != nil {
			return instanceIDs, fmt.Errorf("member %d of %d: %w", member.JobArrayIndex, len(members), err)
		}
		instanceIDs = append(instanceIDs, result.InstanceID)
	}
	return instanceIDs, nil
}

// burstMemberConfigs builds the launch config for each burst member.
//
// The tags come from buildTags in pkg/aws, the path every `spawn launch`
// job-array member takes, so a burst member is a spawn-managed job-array member
// like any other (#917): spawn:managed=true (the TTL reaper and `spawn list` only
// see instances carrying it, and the spored role may only terminate those),
// spawn:ttl and spawn:ttl-deadline, spawn:launch-time, and a distinct
// spawn:job-array-index per member. Burst previously wrote five tags by hand
// and had none of these.
//
// spawn:burst=true is added here, as a burst-only tag, and never by buildTags:
// it marks the members spored registers with the hybrid registry (#904), and
// `spawn launch` members must not carry it.
func burstMemberConfigs(params *LaunchParams) ([]spawnaws.LaunchConfig, error) {
	if params.InstanceProfile == "" {
		return nil, fmt.Errorf("burst: no IAM instance profile resolved; refusing to launch instances spored cannot manage")
	}
	if err := validateBurstParams(params); err != nil {
		return nil, err
	}

	bootstrap, err := buildBurstUserData()
	if err != nil {
		return nil, err
	}
	// RunInstances rejects user-data that is not base64 ("Invalid BASE64
	// encoding of user data", #127). Burst passed the raw script, so every
	// burst failed at RunInstances. Same encoding as the launcher (gzip +
	// base64; cloud-init gunzips).
	userData := launcher.EncodeLinuxUserData(bootstrap)

	members := make([]spawnaws.LaunchConfig, 0, params.Count)
	for i := 0; i < params.Count; i++ {
		members = append(members, spawnaws.LaunchConfig{
			Name:               fmt.Sprintf("spawn-burst-%s-%d", params.JobArrayID, i),
			Region:             params.Region,
			InstanceType:       params.InstanceType,
			AMI:                params.AMI,
			KeyName:            params.KeyName,
			SubnetID:           params.SubnetID,
			SecurityGroupIDs:   params.SecurityGroups,
			IamInstanceProfile: params.InstanceProfile,
			UserData:           userData,
			// The user the bootstrap sets up, written as spawn:local-username
			// so spawn connect logs in as it (as launcher.Provision does).
			Username:      burstUsername,
			Spot:          params.Spot,
			TTL:           params.TTL,
			IdleTimeout:   params.IdleTimeout,
			OnComplete:    "terminate",
			JobArrayID:    params.JobArrayID,
			JobArrayName:  params.JobArrayName,
			JobArraySize:  params.Count,
			JobArrayIndex: i,
			Tags:          map[string]string{tagprefix.Tag("burst"): "true"},
		})
	}
	return members, nil
}

// burstUsername is the user burst's bootstrap configures: Amazon Linux's
// default user, which a --key-name key pair's key is installed for. The
// bootstrap leaves that key in place because burst passes no public key of its
// own.
const burstUsername = launcher.DefaultUsername

// buildBurstUserData is a burst member's boot script: the shared spored
// bootstrap from pkg/launcher, the one `spawn launch` uses (#924).
//
// Burst used to ship its own script, which never installed spored. It waited up
// to 300s for /usr/local/bin/spored to appear and exited if it did not, so
// spored ran only on an AMI that already contained it. The shared bootstrap
// downloads spored from spawn-binaries-<region> for the instance's
// architecture, verifies its sha256 and signature (pkg/sporedinstall), aborts
// the boot rather than run an unverified agent, and starts spored under
// systemd. Nothing burst-specific is added: spored itself registers the member
// with the hybrid registry, from the spawn:burst and spawn:job-array-* tags.
func buildBurstUserData() (string, error) {
	script, err := launcher.BuildLinuxBootstrap(launcher.BootstrapConfig{Username: burstUsername})
	if err != nil {
		return "", fmt.Errorf("burst: build spored bootstrap: %w", err)
	}
	return script, nil
}

// burstAMIResolver is the AMI lookup burst does, on *spawnaws.Client.
type burstAMIResolver interface {
	GetSelfOwnedSpawnAMI(ctx context.Context, region, instanceType string) (string, error)
	GetRecommendedAMI(ctx context.Context, region, instanceType string) (string, error)
	IsWindowsAMI(ctx context.Context, region, amiID string) bool
}

// resolveBurstAMI picks the AMI burst members launch from (#924).
//
// With --ami, that image is used, unless it is Windows: burst's bootstrap is
// the Linux one, so a Windows member would never run spored.
//
// Without --ami ("" or "auto", as for spawn launch), burst prefers the newest
// self-owned spawn-* image that fits the instance type: its architecture,
// Linux, and GPU-ness (pkg/aws GetSelfOwnedSpawnAMI). It used to take the
// newest spawn-* image of any architecture or OS. When none fits, or the
// lookup fails, it falls back to GetRecommendedAMI, the AL2023 resolution
// `spawn launch` uses. Whether spored is baked into the image no longer
// matters, because the bootstrap installs it.
func resolveBurstAMI(ctx context.Context, r burstAMIResolver, region, ami, instanceType string) (string, error) {
	if !isAutoAMI(ami) {
		if r.IsWindowsAMI(ctx, region, ami) {
			return "", fmt.Errorf("--ami %s is a Windows AMI: spawn burst installs spored with the Linux bootstrap, so it needs a Linux AMI", ami)
		}
		return ami, nil
	}

	own, err := r.GetSelfOwnedSpawnAMI(ctx, region, instanceType)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "Warning: could not look up self-owned spawn-* AMIs (%v); using the standard Amazon Linux 2023 AMI\n", err)
	case own != "":
		fmt.Fprintf(os.Stderr, "Using self-owned spawn AMI %s, which fits %s\n", own, instanceType)
		return own, nil
	}

	rec, err := r.GetRecommendedAMI(ctx, region, instanceType)
	if err != nil {
		return "", fmt.Errorf("failed to find an AMI for %s in %s (pass --ami): %w", instanceType, region, err)
	}
	return rec, nil
}
