package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
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

Every instance gets a TTL (--ttl, default 8h) and terminates when it expires.`,
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
	burstCmd.Flags().StringVar(&burstAMI, "ami", "", "AMI ID (auto-detect if not specified)")
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

	// Ensure DynamoDB table exists
	fmt.Fprintln(os.Stderr, "Ensuring DynamoDB registry table exists...")
	if err := registry.EnsureTable(ctx); err != nil {
		return fmt.Errorf("failed to ensure registry table: %w", err)
	}

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	launchParams.Region = cfg.Region

	awsClient := spawnaws.NewClientFromConfig(cfg)

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

	// Auto-detect AMI if not specified
	if launchParams.AMI == "" {
		amiID, err := findLatestSpawnAMI(ctx, ec2.NewFromConfig(cfg))
		if err != nil {
			return fmt.Errorf("failed to find AMI: %w", err)
		}
		launchParams.AMI = amiID
		fmt.Fprintf(os.Stderr, "Using AMI: %s\n", launchParams.AMI)
	}

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

	// RunInstances rejects user-data that is not base64 ("Invalid BASE64
	// encoding of user data", #127). Burst passed the raw script, so every
	// burst failed at RunInstances. Same encoding as the launcher (gzip +
	// base64; cloud-init gunzips).
	userData := launcher.EncodeLinuxUserData(generateBurstUserData(params))

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
			Spot:               params.Spot,
			TTL:                params.TTL,
			IdleTimeout:        params.IdleTimeout,
			OnComplete:         "terminate",
			JobArrayID:         params.JobArrayID,
			JobArrayName:       params.JobArrayName,
			JobArraySize:       params.Count,
			JobArrayIndex:      i,
			Tags:               map[string]string{tagprefix.Tag("burst"): "true"},
		})
	}
	return members, nil
}

func generateBurstUserData(params *LaunchParams) string {
	// User data script that registers with DynamoDB on startup.
	//
	// The spored wait is BOUNDED (#752). It was:
	//
	//	while [ ! -f /usr/local/bin/spored ]; do sleep 5; done
	//
	// an unbounded loop, so a failed spored install — a bad download, a checksum
	// mismatch, the wrong architecture, no network — hung the boot forever with no
	// output and no failure record. That is the one shape every other generated
	// script here deliberately avoids: pkg/userdata/queue.go caps the identical
	// wait at 300s and exits with a message naming cloud-init, and
	// pkg/launcher/bootstrap.go's readiness barrier caps at 600s and writes a
	// named failure. This now matches queue.go, which is the closest sibling.
	//
	// Failing loudly matters more here than elsewhere: an instance with no spored
	// has NO TTL, idle or cost enforcement in-instance (#50), so a silently hung
	// boot is also a silently unbounded bill. Exiting non-zero makes cloud-init
	// record the failure, which is what the reaper's max-age ceiling and
	// `spawn status` can then be reconciled against.
	return `#!/bin/bash
# Spawn burst instance setup

# Wait for spored to be installed, bounded. An unbounded wait here hid a failed
# install as a hung boot (#752).
MAX_WAIT=300  # 5 minutes
WAITED=0
while [ ! -f /usr/local/bin/spored ] && [ $WAITED -lt $MAX_WAIT ]; do
  sleep 5
  WAITED=$((WAITED + 5))
done

if [ ! -f /usr/local/bin/spored ]; then
  echo "ERROR: spored not installed after ${MAX_WAIT}s. Check cloud-init logs." >&2
  echo "spawn: this instance has NO TTL/idle/cost enforcement; terminate it by hand" >&2
  exit 1
fi

# Start spored with hybrid registry support
systemctl enable spored
systemctl start spored

# Register with DynamoDB registry happens automatically via spored
`
}

func findLatestSpawnAMI(ctx context.Context, client *ec2.Client) (string, error) {
	// Find the latest spawn AMI
	result, err := client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"self"},
		Filters: []types.Filter{
			{
				Name:   aws.String("name"),
				Values: []string{"spawn-*"},
			},
			{
				Name:   aws.String("state"),
				Values: []string{"available"},
			},
		},
	})

	if err != nil {
		return "", fmt.Errorf("failed to describe images: %w", err)
	}

	if len(result.Images) == 0 {
		return "", fmt.Errorf("no spawn AMIs found (use --ami to specify)")
	}

	// Return the most recent AMI
	latest := result.Images[0]
	for _, img := range result.Images {
		if *img.CreationDate > *latest.CreationDate {
			latest = img
		}
	}

	return *latest.ImageId, nil
}
