// Package aws provides spawn-managed EC2 instance launch and lifecycle operations.
// It is the primary entry point for external consumers of the spawn Go library.
// Use [NewClient] to create a client from ambient AWS credentials, then call
// [Client.Launch], [Client.ListInstances], [Client.StopInstance], and related
// methods to manage the full instance lifecycle.
//
// All instances launched through this package are tagged with spawn: metadata
// (TTL, idle timeout, DNS name, etc.) so the spored daemon can manage their
// lifecycle independently of the user's machine.
//
// Credentials are loaded from the default AWS credential chain
// (environment variables, ~/.aws/credentials, EC2 IMDS, or ECS task role).
package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	awspricing "github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spore-host/libs/pricing"
	"github.com/spore-host/spawn/pkg/observability/tracing"
)

// Client wraps an AWS SDK configuration and provides EC2 lifecycle operations
// for spawn-managed instances. Create one with [NewClient] or [NewClientFromConfig].
type Client struct {
	cfg aws.Config
}

// NewClient creates a Client using the default AWS credential chain.
// Use [NewClientFromConfig] in tests to inject a pre-configured aws.Config.
func NewClient(ctx context.Context) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &Client{cfg: cfg}, nil
}

// NewClientFromConfig creates a Client from an existing AWS config.
// Used in tests to point all SDK calls at an emulator such as Substrate.
func NewClientFromConfig(cfg aws.Config) *Client {
	return &Client{cfg: cfg}
}

// EnableTracing instruments AWS SDK calls with OpenTelemetry tracing
func (c *Client) EnableTracing() {
	tracing.InstrumentAWSConfig(&c.cfg)
}

// Config returns the AWS config (for use with service clients)
func (c *Client) Config() aws.Config {
	return c.cfg
}

// GetEnabledRegions returns a list of AWS regions enabled for this account
// This respects Service Control Policies (SCPs) that may restrict regions
func (c *Client) GetEnabledRegions(ctx context.Context) ([]string, error) {
	// Use default region for the DescribeRegions call
	ec2Client := ec2.NewFromConfig(c.cfg)

	// DescribeRegions returns only regions that are enabled for the account
	// If SCPs block certain regions, they won't appear in this list
	result, err := ec2Client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{
		AllRegions: aws.Bool(false), // Only enabled regions
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe regions: %w", err)
	}

	regions := make([]string, 0, len(result.Regions))
	for _, region := range result.Regions {
		if region.RegionName != nil {
			regions = append(regions, *region.RegionName)
		}
	}

	return regions, nil
}

// LaunchConfig contains all settings for launching a spawn-managed EC2 instance.
// Most fields are optional — at minimum, provide InstanceType, Region, and AMI.
// The TTL field should always be set to prevent runaway instances.
type LaunchConfig struct {
	// Core AWS launch parameters
	InstanceType       string   // EC2 instance type, e.g. "m7i.large" or "p4d.24xlarge"
	Region             string   // AWS region, e.g. "us-east-1"
	AvailabilityZone   string   // Optional AZ; leave empty to let EC2 choose
	AMI                string   // AMI ID, e.g. "ami-0abc1234ef567890"
	KeyName            string   // EC2 key pair name for SSH access
	IamInstanceProfile string   // IAM instance profile name (not ARN); spored needs EC2/DynamoDB permissions
	SecurityGroupIDs   []string // Security group IDs; a default spawn SG is created if empty
	SubnetID           string   // VPC subnet ID; leave empty to use default subnet
	UserData           string   // User-data script (plain text or base64); spored is injected automatically
	Spot               bool     // If true, launch as a Spot instance
	SpotMaxPrice       string   // Optional Spot max price in $/hr, e.g. "0.50"; empty = on-demand cap
	ReservationID      string   // On-Demand Capacity Reservation ID to target
	Hibernate          bool     // If true, configure the instance for hibernation support
	PlacementGroup     string   // Cluster placement group name (MPI / EFA workloads)
	EFAEnabled         bool     // If true, attach an Elastic Fabric Adapter network interface

	// spawn-specific tags
	TTL             string
	IdleTimeout     string
	HibernateOnIdle bool
	CostLimit       float64
	DNSName         string

	// Pre-stop hook: runs before any lifecycle-triggered terminate/stop/hibernate
	PreStop        string // Shell command to run before stopping (e.g., "aws s3 sync /results s3://bucket/")
	PreStopTimeout string // Max time to wait for pre-stop command (default: 5m)

	// Completion signal settings
	OnComplete      string // Action: terminate, stop, hibernate
	CompletionFile  string // File path to watch (default: /tmp/SPAWN_COMPLETE)
	CompletionDelay string // Grace period before action (default: 30s)

	// Session management
	SessionTimeout string // Auto-logout idle shells (default: 30m, 0 to disable)

	// Job array settings
	JobArrayID      string // Unique job array ID (e.g., "compute-20260113-abc123")
	JobArrayName    string // User-friendly job array name (e.g., "compute")
	JobArraySize    int    // Total number of instances in the array
	JobArrayIndex   int    // This instance's index (0..N-1)
	JobArrayCommand string // Command to run on all instances (optional)

	// Parameter sweep settings
	SweepID    string            // Unique sweep ID (e.g., "hyperparam-20260115-abc123")
	SweepName  string            // User-friendly sweep name (e.g., "hyperparam")
	SweepIndex int               // This instance's index in the sweep (0..N-1)
	SweepSize  int               // Total number of parameter sets in the sweep
	Parameters map[string]string // Parameter key-value pairs for PARAM_* env vars and tags

	// Shared storage settings
	EFSID         string // EFS filesystem ID to mount (fs-xxx)
	EFSMountPoint string // EFS mount point (default: /efs)

	// FSx Lustre settings
	FSxLustreCreate    bool   // Create new FSx Lustre filesystem
	FSxLustreID        string // Existing FSx filesystem ID to mount (fs-xxx)
	FSxMountName       string // Per-filesystem Lustre mount name (e.g. "q5pdvb4v") — from FSx API
	FSxLustreRecall    string // Recall FSx by stack name
	FSxStorageCapacity int32  // Storage capacity in GB (1200, 2400, +2400)
	FSxS3Bucket        string // S3 bucket for import/export
	FSxImportPath      string // S3 import path (s3://bucket/prefix)
	FSxExportPath      string // S3 export path (s3://bucket/prefix)
	FSxMountPoint      string // FSx mount point (default: /fsx)

	// Compliance settings
	EBSEncrypted   bool   // Force EBS encryption (compliance requirement)
	EBSKMSKeyID    string // Customer-managed KMS key for EBS encryption
	IMDSv2Enforced bool   // Require IMDSv2 (no IMDSv1 fallback)
	IMDSv2HopLimit int    // IMDSv2 hop limit (default: 1)

	// Slack lifecycle notifications
	SlackWorkspaceID   string // Slack workspace ID — injected as spawn:slack-workspace-id tag
	NotifyURL          string // spore-bot Lambda Function URL — injected as spawn:notify-url tag
	NotifyCommand      string // Slash command for workspace routing — injected as spawn:notify-command tag
	ActivePortsRaw     string // comma-separated ports to monitor — injected as spawn:active-ports tag
	ActiveProcessesRaw string // comma-separated process names — injected as spawn:active-processes tag

	// DCV application streaming
	DCVSessionID      string // NICE DCV session ID — activates DCV idle detection in spored (e.g. "console")
	AppName           string // Catalog application name — informational tag (e.g. "paraview")
	RootVolumeSizeGiB int32  // Override root EBS volume size in GiB (0 = use default 20 GiB)

	// Pricing (populated at launch from AWS Pricing API)
	PricePerHour float64 // actual on-demand rate; 0 means look it up

	// Metadata
	Name string
	Tags map[string]string
}

// LaunchResult contains information about the launched instance returned by [Client.Launch].
type LaunchResult struct {
	InstanceID       string // EC2 instance ID, e.g. "i-0abc123def456"
	Name             string // Value of the Name EC2 tag set at launch
	PublicIP         string // Public IPv4 address; empty if no public IP was assigned
	PrivateIP        string // Private IPv4 address within the VPC
	AvailabilityZone string // AZ where the instance was placed, e.g. "us-east-1a"
	State            string // Initial instance state — typically "pending"
	KeyName          string // EC2 key pair name used for SSH access
}

// Launch starts a new EC2 instance as described by launchConfig and returns its
// ID, IP addresses, and initial state. All spawn: lifecycle tags (TTL, idle
// timeout, DNS name, cost limit, etc.) are applied at launch time so spored
// can manage the instance autonomously after the caller disconnects.
//
// If LaunchConfig.PricePerHour is 0, Launch queries the AWS Pricing API to
// determine the on-demand rate and falls back to a static table if unavailable.
func (c *Client) Launch(ctx context.Context, launchConfig LaunchConfig) (*LaunchResult, error) {
	// Update config for region
	cfg := c.cfg.Copy()
	cfg.Region = launchConfig.Region
	ec2Client := ec2.NewFromConfig(cfg)

	// Get caller identity for per-user isolation tagging
	accountID, userARN, err := c.GetCallerIdentityInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get caller identity: %w", err)
	}

	// Look up the actual on-demand price from the AWS Pricing API.
	// This is stored in the spawn:price-per-hour tag so spored can compute effective cost
	// without needing to know the instance type at runtime.
	if launchConfig.PricePerHour == 0 {
		if price := LookupEC2OnDemandPrice(ctx, launchConfig.Region, launchConfig.InstanceType); price > 0 {
			launchConfig.PricePerHour = price
			log.Printf("pricing: %s in %s = $%.4f/hr (from AWS Pricing API)", launchConfig.InstanceType, launchConfig.Region, price)
		} else {
			// Fall back to static table only as a last resort
			launchConfig.PricePerHour = pricing.GetEC2HourlyRate(launchConfig.Region, launchConfig.InstanceType)
			log.Printf("pricing: %s in %s = $%.4f/hr (from static table — API unavailable)", launchConfig.InstanceType, launchConfig.Region, launchConfig.PricePerHour)
		}
	}

	// Build tags (including account and user tags for per-user isolation)
	tags := buildTags(launchConfig, accountID, userARN)

	// Build block device mappings. The AMI's root snapshot sets a hard minimum:
	// EC2 rejects a root volume smaller than the snapshot it was created from
	// (InvalidBlockDeviceMapping). Look that minimum up so a custom AMI with a
	// large baked root (common for data-science images) launches without the
	// caller having to pass --volume-size (#25).
	amiMinGiB := rootVolumeSizeFromAMI(ctx, ec2Client, launchConfig.AMI)
	blockDevices := buildBlockDevices(launchConfig, amiMinGiB)

	// Build run instances input
	input := &ec2.RunInstancesInput{
		InstanceType: types.InstanceType(launchConfig.InstanceType),
		ImageId:      aws.String(launchConfig.AMI),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		KeyName:      aws.String(launchConfig.KeyName),
		UserData:     aws.String(launchConfig.UserData),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         tags,
			},
			{
				ResourceType: types.ResourceTypeVolume,
				Tags:         tags,
			},
		},
		BlockDeviceMappings: blockDevices,
	}

	// Add IAM instance profile if specified
	if launchConfig.IamInstanceProfile != "" {
		input.IamInstanceProfile = &types.IamInstanceProfileSpecification{
			Name: aws.String(launchConfig.IamInstanceProfile),
		}
	}

	// Add network configuration
	if launchConfig.EFAEnabled {
		// EFA requires specific network interface configuration
		netInterface := types.InstanceNetworkInterfaceSpecification{
			DeviceIndex:              aws.Int32(0),
			AssociatePublicIpAddress: aws.Bool(true),
			DeleteOnTermination:      aws.Bool(true),
			InterfaceType:            aws.String("efa"), // EFA interface type
		}

		if launchConfig.SubnetID != "" {
			netInterface.SubnetId = aws.String(launchConfig.SubnetID)
		}

		if len(launchConfig.SecurityGroupIDs) > 0 {
			netInterface.Groups = launchConfig.SecurityGroupIDs
		}

		input.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{netInterface}
	} else {
		// Always request a public IP via a NetworkInterface spec — this works
		// even when the subnet has MapPublicIpOnLaunch=false (fixes #308).
		ni := types.InstanceNetworkInterfaceSpecification{
			AssociatePublicIpAddress: aws.Bool(true),
			DeviceIndex:              aws.Int32(0),
			DeleteOnTermination:      aws.Bool(true),
		}
		if launchConfig.SubnetID != "" {
			ni.SubnetId = aws.String(launchConfig.SubnetID)
		}
		if len(launchConfig.SecurityGroupIDs) > 0 {
			ni.Groups = launchConfig.SecurityGroupIDs
		}
		input.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{ni}
	}

	// Add placement (AZ, placement group, and reservation)
	placement := &types.Placement{}
	if launchConfig.AvailabilityZone != "" {
		placement.AvailabilityZone = aws.String(launchConfig.AvailabilityZone)
	}
	if launchConfig.PlacementGroup != "" {
		placement.GroupName = aws.String(launchConfig.PlacementGroup)
	}
	if placement.AvailabilityZone != nil || placement.GroupName != nil {
		input.Placement = placement
	}

	// Add hibernation if enabled
	if launchConfig.Hibernate {
		input.HibernationOptions = &types.HibernationOptionsRequest{
			Configured: aws.Bool(true),
		}
	}

	// Add Spot configuration if needed
	if launchConfig.Spot {
		input.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType: types.SpotInstanceTypeOneTime,
			},
		}

		if launchConfig.SpotMaxPrice != "" {
			input.InstanceMarketOptions.SpotOptions.MaxPrice = aws.String(launchConfig.SpotMaxPrice)
		}
	}

	// Add IMDSv2 configuration if enforced (compliance requirement)
	if launchConfig.IMDSv2Enforced {
		hopLimit := int32(1) // Default: only local access
		if launchConfig.IMDSv2HopLimit > 0 {
			hopLimit = int32(launchConfig.IMDSv2HopLimit)
		}

		input.MetadataOptions = &types.InstanceMetadataOptionsRequest{
			HttpTokens:              types.HttpTokensStateRequired, // Require IMDSv2
			HttpPutResponseHopLimit: aws.Int32(hopLimit),
			HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
		}
	}

	// Launch instance
	result, err := ec2Client.RunInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to launch instance: %w", err)
	}

	if len(result.Instances) == 0 {
		return nil, fmt.Errorf("no instances returned")
	}

	return newLaunchResult(result.Instances[0], launchConfig.Name, launchConfig.KeyName), nil
}

// newLaunchResult maps a RunInstances instance into a LaunchResult. Placement
// and State are optional nested structs in the API response; guard against nil
// so a response that omits them (some AWS-compatible endpoints do) doesn't
// panic.
func newLaunchResult(instance types.Instance, name, keyName string) *LaunchResult {
	var az string
	if instance.Placement != nil {
		az = valueOrEmpty(instance.Placement.AvailabilityZone)
	}
	var state string
	if instance.State != nil {
		state = string(instance.State.Name)
	}
	return &LaunchResult{
		InstanceID:       valueOrEmpty(instance.InstanceId),
		Name:             name,
		PrivateIP:        valueOrEmpty(instance.PrivateIpAddress),
		PublicIP:         valueOrEmpty(instance.PublicIpAddress),
		AvailabilityZone: az,
		State:            state,
		KeyName:          keyName,
	}
}

func buildTags(config LaunchConfig, accountID string, userARN string) []types.Tag {
	// Convert account ID to base36 for DNS namespace
	accountBase36 := intToBase36(accountID)

	tags := []types.Tag{
		{Key: aws.String("spawn:managed"), Value: aws.String("true")},
		{Key: aws.String("spawn:root"), Value: aws.String("true")},
		{Key: aws.String("spawn:created-by"), Value: aws.String("spawn")},
		{Key: aws.String("spawn:version"), Value: aws.String("0.1.0")},
		{Key: aws.String("spawn:account-id"), Value: aws.String(accountID)},
		{Key: aws.String("spawn:account-base36"), Value: aws.String(accountBase36)},
		{Key: aws.String("spawn:iam-user"), Value: aws.String(userARN)}, // Per-user isolation
	}

	if config.Name != "" {
		tags = append(tags, types.Tag{Key: aws.String("Name"), Value: aws.String(config.Name)})
	}

	// Record the absolute launch time once — survives stop/wake cycles.
	launchTime := time.Now().UTC().Format(time.RFC3339)
	tags = append(tags, types.Tag{Key: aws.String("spawn:launch-time"), Value: aws.String(launchTime)})

	if config.TTL != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:ttl"), Value: aws.String(config.TTL)})
		// Compute the absolute deadline once at launch; spored uses this across stop/wake cycles
		// so that TTL is always relative to original launch time, never reset.
		if d, err := time.ParseDuration(config.TTL); err == nil {
			deadline := time.Now().Add(d).UTC().Format(time.RFC3339)
			tags = append(tags, types.Tag{Key: aws.String("spawn:ttl-deadline"), Value: aws.String(deadline)})
		}
	}

	if config.DNSName != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:dns-name"), Value: aws.String(config.DNSName)})
	}

	if config.SlackWorkspaceID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:slack-workspace-id"), Value: aws.String(config.SlackWorkspaceID)})
	}
	if config.NotifyURL != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:notify-url"), Value: aws.String(config.NotifyURL)})
	}
	if config.NotifyCommand != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:notify-command"), Value: aws.String(config.NotifyCommand)})
	}
	if config.ActivePortsRaw != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:active-ports"), Value: aws.String(config.ActivePortsRaw)})
	}
	if config.ActiveProcessesRaw != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:active-processes"), Value: aws.String(config.ActiveProcessesRaw)})
	}
	if config.JobArrayCommand != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:command"), Value: aws.String(config.JobArrayCommand)})
	}
	if config.DCVSessionID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:dcv-session-id"), Value: aws.String(config.DCVSessionID)})
	}
	if config.AppName != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:app-name"), Value: aws.String(config.AppName)})
	}

	// Storage filesystem tags — written so instance scripts can auto-mount
	// without needing the filesystem ID hardcoded (fixes #314).
	if config.FSxLustreID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-id"), Value: aws.String(config.FSxLustreID)})
		mp := config.FSxMountPoint
		if mp == "" {
			mp = "/fsx"
		}
		tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-mount-point"), Value: aws.String(mp)})
		if config.FSxMountName != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:fsx-mount-name"), Value: aws.String(config.FSxMountName)})
		}
	}
	if config.EFSID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:efs-id"), Value: aws.String(config.EFSID)})
		mp := config.EFSMountPoint
		if mp == "" {
			mp = "/efs"
		}
		tags = append(tags, types.Tag{Key: aws.String("spawn:efs-mount-point"), Value: aws.String(mp)})
	}

	if config.IdleTimeout != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:idle-timeout"), Value: aws.String(config.IdleTimeout)})
	}

	if config.HibernateOnIdle {
		tags = append(tags, types.Tag{Key: aws.String("spawn:hibernate-on-idle"), Value: aws.String("true")})
	}

	// Completion signal settings
	if config.OnComplete != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:on-complete"), Value: aws.String(config.OnComplete)})
	}

	if config.CompletionFile != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:completion-file"), Value: aws.String(config.CompletionFile)})
	}

	if config.CompletionDelay != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:completion-delay"), Value: aws.String(config.CompletionDelay)})
	}

	// Pre-stop hook
	if config.PreStop != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:pre-stop"), Value: aws.String(config.PreStop)})
		if config.PreStopTimeout != "" {
			tags = append(tags, types.Tag{Key: aws.String("spawn:pre-stop-timeout"), Value: aws.String(config.PreStopTimeout)})
		}
	}

	// Always tag the on-demand price — used by spored for effective cost calculation.
	if config.PricePerHour > 0 {
		tags = append(tags, types.Tag{Key: aws.String("spawn:price-per-hour"), Value: aws.String(fmt.Sprintf("%.6f", config.PricePerHour))})
	}
	if config.CostLimit > 0 {
		tags = append(tags, types.Tag{Key: aws.String("spawn:cost-limit"), Value: aws.String(fmt.Sprintf("%.4f", config.CostLimit))})
	}

	// Session management
	if config.SessionTimeout != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:session-timeout"), Value: aws.String(config.SessionTimeout)})
	}

	// Job array tags
	if config.JobArrayID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-id"), Value: aws.String(config.JobArrayID)})
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-name"), Value: aws.String(config.JobArrayName)})
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-size"), Value: aws.String(fmt.Sprintf("%d", config.JobArraySize))})
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-index"), Value: aws.String(fmt.Sprintf("%d", config.JobArrayIndex))})
		tags = append(tags, types.Tag{Key: aws.String("spawn:job-array-created"), Value: aws.String(time.Now().Format(time.RFC3339))})
	}

	// Parameter sweep tags
	if config.SweepID != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:sweep-id"), Value: aws.String(config.SweepID)})
		tags = append(tags, types.Tag{Key: aws.String("spawn:sweep-name"), Value: aws.String(config.SweepName)})
		tags = append(tags, types.Tag{Key: aws.String("spawn:sweep-size"), Value: aws.String(fmt.Sprintf("%d", config.SweepSize))})
		tags = append(tags, types.Tag{Key: aws.String("spawn:sweep-index"), Value: aws.String(fmt.Sprintf("%d", config.SweepIndex))})

		// Add parameter tags (up to 35 to stay under AWS 50-tag limit)
		paramCount := 0
		for k, v := range config.Parameters {
			if paramCount >= 35 {
				break
			}
			tags = append(tags, types.Tag{Key: aws.String("spawn:param:" + k), Value: aws.String(v)})
			paramCount++
		}
	}

	// Add custom tags
	for k, v := range config.Tags {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}

	return tags
}

// buildBlockDevices constructs the root EBS mapping. amiMinGiB is the AMI root
// snapshot's minimum size (0 if unknown); the final volume is never smaller
// than that, so launches from custom AMIs with a large baked root don't fail
// with InvalidBlockDeviceMapping (#25).
func buildBlockDevices(config LaunchConfig, amiMinGiB int32) []types.BlockDeviceMapping {
	// Calculate volume size
	volumeSize := int32(20) // Default 20 GB

	if config.RootVolumeSizeGiB > 0 {
		volumeSize = config.RootVolumeSizeGiB
	} else if config.Hibernate {
		// For hibernation, need RAM + OS + buffer
		volumeSize = estimateVolumeSize(config.InstanceType)
	}

	// Never request less than the AMI's root snapshot requires. This also
	// rescues an explicit --volume-size that's smaller than the snapshot.
	if amiMinGiB > volumeSize {
		volumeSize = amiMinGiB
	}

	// Determine encryption settings
	encrypted := config.Hibernate || config.EBSEncrypted

	ebs := &types.EbsBlockDevice{
		VolumeSize:          aws.Int32(volumeSize),
		VolumeType:          types.VolumeTypeGp3,
		DeleteOnTermination: aws.Bool(true),
		Encrypted:           aws.Bool(encrypted),
	}

	// Add customer-managed KMS key if specified
	if encrypted && config.EBSKMSKeyID != "" {
		ebs.KmsKeyId = aws.String(config.EBSKMSKeyID)
	}

	return []types.BlockDeviceMapping{
		{
			DeviceName: aws.String("/dev/xvda"),
			Ebs:        ebs,
		},
	}
}

func estimateVolumeSize(instanceType string) int32 {
	// Rough estimation of RAM size by instance family
	// This should ideally query EC2 DescribeInstanceTypes
	ramEstimates := map[string]int32{
		"t3":  8,
		"t4g": 8,
		"m7i": 16,
		"m8g": 16,
		"c7i": 16,
		"r7i": 32,
		"p5":  768, // H100 instances have lots of RAM
		"g6":  32,
	}

	// Extract family
	for prefix, ram := range ramEstimates {
		if len(instanceType) >= len(prefix) && instanceType[:len(prefix)] == prefix {
			return ram + 10 // RAM + 10GB for OS
		}
	}

	return 20 // Default
}

// rootVolumeSizeFromAMI returns the AMI's root EBS volume size in GiB — the
// minimum a launch from this AMI may request. It is best-effort: any error
// (AMI not found, no permission, malformed mapping) returns 0, leaving the
// caller's chosen size unchanged. The root device is the mapping whose name
// matches the image's RootDeviceName; if that can't be matched, the largest
// EBS mapping is used as a safe floor.
func rootVolumeSizeFromAMI(ctx context.Context, ec2Client *ec2.Client, amiID string) int32 {
	if amiID == "" {
		return 0
	}
	out, err := ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		ImageIds: []string{amiID},
	})
	if err != nil || len(out.Images) == 0 {
		return 0
	}
	img := out.Images[0]
	return rootVolumeSizeFromMappings(aws.ToString(img.RootDeviceName), img.BlockDeviceMappings)
}

// rootVolumeSizeFromMappings picks the root volume size from an AMI's block
// device mappings: the EBS mapping matching rootName, or — if none matches —
// the largest EBS mapping as a safe floor. Returns 0 when there are no sized
// EBS mappings. Pure, so the selection logic is unit-testable without AWS.
func rootVolumeSizeFromMappings(rootName string, mappings []types.BlockDeviceMapping) int32 {
	var rootSize, maxSize int32
	for _, bdm := range mappings {
		if bdm.Ebs == nil || bdm.Ebs.VolumeSize == nil {
			continue
		}
		size := *bdm.Ebs.VolumeSize
		if size > maxSize {
			maxSize = size
		}
		if rootName != "" && aws.ToString(bdm.DeviceName) == rootName {
			rootSize = size
		}
	}
	if rootSize > 0 {
		return rootSize
	}
	return maxSize
}

func valueOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CheckKeyPairExists checks if a key pair exists in AWS EC2
func (c *Client) CheckKeyPairExists(ctx context.Context, region, keyName string) (bool, error) {
	cfg := c.cfg
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	input := &ec2.DescribeKeyPairsInput{
		KeyNames: []string{keyName},
	}

	_, err := ec2Client.DescribeKeyPairs(ctx, input)
	if err != nil {
		// Check if it's a "not found" error
		if contains(err.Error(), "InvalidKeyPair.NotFound") || contains(err.Error(), "does not exist") {
			return false, nil
		}
		return false, fmt.Errorf("failed to check key pair: %w", err)
	}

	return true, nil
}

// ImportKeyPair imports a public key to AWS EC2
func (c *Client) ImportKeyPair(ctx context.Context, region, keyName string, publicKey []byte) error {
	cfg := c.cfg
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	input := &ec2.ImportKeyPairInput{
		KeyName:           aws.String(keyName),
		PublicKeyMaterial: publicKey,
	}

	_, err := ec2Client.ImportKeyPair(ctx, input)
	if err != nil {
		if strings.Contains(err.Error(), "InvalidKeyPair.Duplicate") || strings.Contains(err.Error(), "already exists") {
			return nil // key pair already exists — treat as success
		}
		return fmt.Errorf("failed to import key pair: %w", err)
	}

	return nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// GetInstancePublicIP queries an instance and returns its public IP
func (c *Client) GetInstancePublicIP(ctx context.Context, region, instanceID string) (string, error) {
	cfg := c.cfg
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	input := &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}

	result, err := ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to describe instance: %w", err)
	}

	if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance not found")
	}

	instance := result.Reservations[0].Instances[0]
	return valueOrEmpty(instance.PublicIpAddress), nil
}

// GetInstanceState returns the current state of an instance (e.g., "pending", "running", "stopping", "stopped", "terminated")
func (c *Client) GetInstanceState(ctx context.Context, region, instanceID string) (string, error) {
	cfg := c.cfg
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	input := &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}

	result, err := ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to describe instance: %w", err)
	}

	if len(result.Reservations) == 0 || len(result.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("instance not found")
	}

	instance := result.Reservations[0].Instances[0]
	if instance.State == nil || instance.State.Name == "" {
		return "", fmt.Errorf("instance state unavailable")
	}

	return string(instance.State.Name), nil
}

// WaitForRunning blocks until the instance reaches the "running" state or the
// timeout elapses, using the SDK's instance-running waiter (poll with backoff,
// returns as soon as it's running). Replaces fixed-duration sleeps.
func (c *Client) WaitForRunning(ctx context.Context, region, instanceID string, timeout time.Duration) error {
	cfg := c.cfg
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	waiter := ec2.NewInstanceRunningWaiter(ec2Client)
	if err := waiter.Wait(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}, timeout); err != nil {
		return fmt.Errorf("waiting for instance %s to run: %w", instanceID, err)
	}
	return nil
}

// Terminate terminates an EC2 instance
func (c *Client) Terminate(ctx context.Context, region, instanceID string) error {
	cfg := c.cfg
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}

	_, err := ec2Client.TerminateInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to terminate instance: %w", err)
	}

	return nil
}

// UpdateInstanceTags adds or updates tags on an EC2 instance
func (c *Client) UpdateInstanceTags(ctx context.Context, region, instanceID string, tags map[string]string) error {
	cfg := c.cfg
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	// Convert map to EC2 tag format
	ec2Tags := make([]types.Tag, 0, len(tags))
	for key, value := range tags {
		ec2Tags = append(ec2Tags, types.Tag{
			Key:   aws.String(key),
			Value: aws.String(value),
		})
	}

	input := &ec2.CreateTagsInput{
		Resources: []string{instanceID},
		Tags:      ec2Tags,
	}

	_, err := ec2Client.CreateTags(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to update tags: %w", err)
	}

	return nil
}

// FindKeyPairByFingerprint searches for a key pair matching the given fingerprint
// Returns the key name if found, empty string if not found
func (c *Client) FindKeyPairByFingerprint(ctx context.Context, region, fingerprint string) (string, error) {
	cfg := c.cfg
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	// List all key pairs
	input := &ec2.DescribeKeyPairsInput{}
	result, err := ec2Client.DescribeKeyPairs(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to list key pairs: %w", err)
	}

	// Search for matching fingerprint
	for _, kp := range result.KeyPairs {
		if kp.KeyFingerprint != nil && *kp.KeyFingerprint == fingerprint {
			if kp.KeyName != nil {
				return *kp.KeyName, nil
			}
		}
	}

	return "", nil // Not found
}

// InstanceInfo contains metadata about a spawn-managed instance
type InstanceInfo struct {
	InstanceID       string
	Name             string
	InstanceType     string
	State            string
	Region           string
	AvailabilityZone string
	PublicIP         string
	PrivateIP        string
	LaunchTime       time.Time
	TTL              string
	IdleTimeout      string
	KeyName          string
	SpotInstance     bool
	Tags             map[string]string
	IAMRole          string // IAM instance profile/role name

	// Job array fields
	JobArrayID    string
	JobArrayName  string
	JobArrayIndex string
	JobArraySize  string

	// Sweep fields
	SweepID    string
	SweepName  string
	SweepIndex string
	SweepSize  string
	Parameters map[string]string // Extracted from spawn:param:* tags
}

// ListInstances returns all spawn-managed instances, optionally filtered by region and state
func (c *Client) ListInstances(ctx context.Context, region string, stateFilter string) ([]InstanceInfo, error) {
	var allInstances []InstanceInfo

	// Determine which regions to search
	regions := []string{}
	if region != "" {
		regions = append(regions, region)
	} else {
		// Query all regions
		var err error
		regions, err = c.getAllRegions(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get regions: %w", err)
		}
	}

	// Search each region for spawn-managed instances
	for _, r := range regions {
		instances, err := c.listInstancesInRegion(ctx, r, stateFilter)
		if err != nil {
			// Log error but continue with other regions
			continue
		}
		allInstances = append(allInstances, instances...)
	}

	return allInstances, nil
}

func (c *Client) listInstancesInRegion(ctx context.Context, region string, stateFilter string) ([]InstanceInfo, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	// Build filters
	filters := []types.Filter{
		{
			Name:   aws.String("tag:spawn:managed"),
			Values: []string{"true"},
		},
	}

	// Add state filter if specified
	if stateFilter != "" {
		filters = append(filters, types.Filter{
			Name:   aws.String("instance-state-name"),
			Values: []string{stateFilter},
		})
	} else {
		// Default: show running and stopped instances (not terminated)
		filters = append(filters, types.Filter{
			Name:   aws.String("instance-state-name"),
			Values: []string{"pending", "running", "stopping", "stopped"},
		})
	}

	input := &ec2.DescribeInstancesInput{
		Filters: filters,
	}

	var instances []InstanceInfo

	// Paginate through results
	paginator := ec2.NewDescribeInstancesPaginator(ec2Client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to describe instances in %s: %w", region, err)
		}

		for _, reservation := range page.Reservations {
			for _, instance := range reservation.Instances {
				// State and Placement are pointers AWS may leave nil; guard both.
				var state string
				if instance.State != nil {
					state = string(instance.State.Name)
				}
				var az string
				if instance.Placement != nil {
					az = valueOrEmpty(instance.Placement.AvailabilityZone)
				}
				info := InstanceInfo{
					InstanceID:       valueOrEmpty(instance.InstanceId),
					InstanceType:     string(instance.InstanceType),
					State:            state,
					Region:           region,
					AvailabilityZone: az,
					PublicIP:         valueOrEmpty(instance.PublicIpAddress),
					PrivateIP:        valueOrEmpty(instance.PrivateIpAddress),
					KeyName:          valueOrEmpty(instance.KeyName),
					SpotInstance:     instance.InstanceLifecycle == types.InstanceLifecycleTypeSpot,
					Tags:             make(map[string]string),
					Parameters:       make(map[string]string),
				}

				if instance.LaunchTime != nil {
					info.LaunchTime = *instance.LaunchTime
				}

				// Extract IAM instance profile
				if instance.IamInstanceProfile != nil && instance.IamInstanceProfile.Arn != nil {
					// Extract role name from ARN (format: arn:aws:iam::account:instance-profile/RoleName)
					arn := *instance.IamInstanceProfile.Arn
					parts := strings.Split(arn, "/")
					if len(parts) > 0 {
						info.IAMRole = parts[len(parts)-1]
					}
				}

				// Extract tags
				for _, tag := range instance.Tags {
					if tag.Key != nil && tag.Value != nil {
						key := *tag.Key
						value := *tag.Value

						switch key {
						case "Name":
							info.Name = value
						case "spawn:ttl":
							info.TTL = value
						case "spawn:idle-timeout":
							info.IdleTimeout = value
						case "spawn:job-array-id":
							info.JobArrayID = value
						case "spawn:job-array-name":
							info.JobArrayName = value
						case "spawn:job-array-index":
							info.JobArrayIndex = value
						case "spawn:job-array-size":
							info.JobArraySize = value
						case "spawn:sweep-id":
							info.SweepID = value
						case "spawn:sweep-name":
							info.SweepName = value
						case "spawn:sweep-index":
							info.SweepIndex = value
						case "spawn:sweep-size":
							info.SweepSize = value
						default:
							// Check for parameter tags
							if strings.HasPrefix(key, "spawn:param:") {
								paramName := strings.TrimPrefix(key, "spawn:param:")
								info.Parameters[paramName] = value
							} else {
								info.Tags[key] = value
							}
						}
					}
				}

				instances = append(instances, info)
			}
		}
	}

	return instances, nil
}

func (c *Client) getAllRegions(ctx context.Context) ([]string, error) {
	// Use us-east-1 as the base region for the DescribeRegions call
	cfg := c.cfg.Copy()
	cfg.Region = "us-east-1"
	ec2Client := ec2.NewFromConfig(cfg)

	result, err := ec2Client.DescribeRegions(ctx, &ec2.DescribeRegionsInput{
		AllRegions: aws.Bool(false), // Only enabled regions
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe regions: %w", err)
	}

	var regions []string
	for _, region := range result.Regions {
		if region.RegionName != nil {
			regions = append(regions, *region.RegionName)
		}
	}

	return regions, nil
}

// StopInstance stops an EC2 instance
func (c *Client) StopInstance(ctx context.Context, region, instanceID string, hibernate bool) error {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	input := &ec2.StopInstancesInput{
		InstanceIds: []string{instanceID},
		Hibernate:   aws.Bool(hibernate),
	}

	_, err := ec2Client.StopInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to stop instance: %w", err)
	}

	return nil
}

// StartInstance starts a stopped EC2 instance
func (c *Client) StartInstance(ctx context.Context, region, instanceID string) error {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	input := &ec2.StartInstancesInput{
		InstanceIds: []string{instanceID},
	}

	_, err := ec2Client.StartInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to start instance: %w", err)
	}

	return nil
}

// SetupSporedIAMRole creates or retrieves the IAM role and instance profile for spored
// Returns the instance profile name
func (c *Client) SetupSporedIAMRole(ctx context.Context) (string, error) {
	iamClient := iam.NewFromConfig(c.cfg)

	roleName := "spored-instance-role"
	instanceProfileName := "spored-instance-profile"
	policyName := "spored-policy"

	roleCreated := false
	profileCreated := false

	// 1. Check if role exists, create if not
	_, err := iamClient.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	})

	if err != nil {
		roleCreated = true
		// Role doesn't exist, create it
		trustPolicy := `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Service": "ec2.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}`

		_, err = iamClient.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(roleName),
			AssumeRolePolicyDocument: aws.String(trustPolicy),
			Description:              aws.String("IAM role for spored daemon on EC2 instances"),
			Tags: []iamtypes.Tag{
				{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			},
		})
		if err != nil && !contains(err.Error(), "EntityAlreadyExists") {
			return "", fmt.Errorf("failed to create IAM role: %w", err)
		}
	}

	// 2. Attach inline policy to role
	policy := `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeTags",
        "ec2:DescribeInstances",
        "ec2:DescribeVolumes",
        "ec2:CreateTags"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:TerminateInstances",
        "ec2:StopInstances"
      ],
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/spawn:managed": "true"
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::spawn-certs-*/*"
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::dcv-license.*/*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:GetObjectVersion",
        "s3:ListBucket",
        "s3:GetBucketLocation"
      ],
      "Resource": [
        "arn:aws:s3:::spawn-schedules-*",
        "arn:aws:s3:::spawn-schedules-*/*"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:GetObjectVersion",
        "s3:ListBucket",
        "s3:GetBucketLocation"
      ],
      "Resource": [
        "arn:aws:s3:::spawn-binaries-*",
        "arn:aws:s3:::spawn-binaries-*/*"
      ]
    }
  ]
}`

	_, err = iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(policy),
	})
	if err != nil {
		return "", fmt.Errorf("failed to attach policy to role: %w", err)
	}

	// 3. Check if instance profile exists, create if not
	_, err = iamClient.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(instanceProfileName),
	})

	if err != nil {
		profileCreated = true
		// Instance profile doesn't exist, create it
		_, err = iamClient.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(instanceProfileName),
			Tags: []iamtypes.Tag{
				{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			},
		})
		if err != nil && !contains(err.Error(), "EntityAlreadyExists") {
			return "", fmt.Errorf("failed to create instance profile: %w", err)
		}

		// Add role to instance profile
		_, err = iamClient.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
			InstanceProfileName: aws.String(instanceProfileName),
			RoleName:            aws.String(roleName),
		})
		if err != nil && !contains(err.Error(), "LimitExceeded") {
			return "", fmt.Errorf("failed to add role to instance profile: %w", err)
		}
	}

	// If we created new resources, wait for IAM to propagate (eventual
	// consistency). Poll GetInstanceProfile until it's retrievable rather than
	// sleeping a fixed 10s — returns immediately once consistent (instant
	// against emulators), bounded so it can't hang.
	if roleCreated || profileCreated {
		waitForInstanceProfile(ctx, iamClient, instanceProfileName)
	}

	return instanceProfileName, nil
}

// waitForInstanceProfile polls until the instance profile is retrievable (IAM
// is eventually consistent after creation), or a bounded deadline passes. It
// returns as soon as the profile is readable — instantly against a strongly
// consistent endpoint — instead of a blind fixed sleep. Best-effort: a timeout
// is not fatal (the subsequent RunInstances will surface any real problem).
func waitForInstanceProfile(ctx context.Context, iamClient *iam.Client, name string) {
	const (
		deadline = 30 * time.Second
		interval = 500 * time.Millisecond
	)
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := iamClient.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String(name),
		}); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// GetAccountID returns the AWS account ID of the current credentials, falling
// back to EC2 instance metadata (IMDS) if STS is unreachable.
func (c *Client) GetAccountID(ctx context.Context) (string, error) {
	stsClient := sts.NewFromConfig(c.cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err == nil && identity.Account != nil {
		return *identity.Account, nil
	}
	// STS failed or returned no account — try IMDS (e.g. STS VPC endpoint with
	// private DNS that this subnet can't route to, see #33).
	if doc, derr := imdsIdentity(ctx); derr == nil && doc.AccountID != "" {
		return doc.AccountID, nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get caller identity: %w", err)
	}
	return "", fmt.Errorf("account ID not returned by STS")
}

// GetCallerIdentityInfo returns account ID and user ARN for per-user isolation.
// If STS is unreachable (e.g. an STS VPC endpoint with private DNS that this
// subnet can't route to, #33), it falls back to EC2 instance metadata: IMDS
// yields the account ID and a synthesized assumed-role ARN derived from the
// instance profile — enough for the isolation tagging that consumes this.
func (c *Client) GetCallerIdentityInfo(ctx context.Context) (accountID string, userARN string, err error) {
	stsClient := sts.NewFromConfig(c.cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err == nil && identity.Account != nil && identity.Arn != nil {
		return *identity.Account, *identity.Arn, nil
	}
	stsErr := err

	// Fall back to IMDS.
	doc, derr := imdsIdentity(ctx)
	if derr != nil || doc.AccountID == "" {
		if stsErr != nil {
			return "", "", fmt.Errorf("failed to get caller identity (STS and IMDS both unavailable): %w", stsErr)
		}
		return "", "", fmt.Errorf("caller identity incomplete from STS and IMDS")
	}
	// IMDS gives the account ID but not a user ARN; synthesize a best-effort
	// identity ARN so downstream tagging has a stable value.
	arn := fmt.Sprintf("arn:aws:sts::%s:assumed-role/ec2-instance/%s", doc.AccountID, doc.InstanceID)
	return doc.AccountID, arn, nil
}

// imdsIdentity fetches the EC2 instance identity document via IMDS (IMDSv2),
// which provides the account ID and region without needing STS reachability.
// A short client timeout keeps the fallback fast when not on EC2.
func imdsIdentity(ctx context.Context) (imds.InstanceIdentityDocument, error) {
	client := imds.New(imds.Options{})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := client.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return imds.InstanceIdentityDocument{}, err
	}
	return out.InstanceIdentityDocument, nil
}

// intToBase36 converts a numeric string (AWS account ID) to base36
// Example: "942542972736" -> "c0zxr0ao"
func intToBase36(accountID string) string {
	// Parse account ID as integer
	num, err := strconv.ParseUint(accountID, 10, 64)
	if err != nil {
		// Fallback: return account ID as-is if parsing fails
		return accountID
	}

	// Convert to base36 (lowercase)
	return strconv.FormatUint(num, 36)
}

// GetConfig returns the AWS config
func (c *Client) GetConfig(ctx context.Context) (aws.Config, error) {
	return c.cfg, nil
}

// getRegionalConfig returns an AWS config for a specific region
func (c *Client) getRegionalConfig(ctx context.Context, region string) (aws.Config, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	return cfg, nil
}

// CreateOrGetMPISecurityGroup creates or gets a security group configured for MPI clusters
// The security group allows all TCP traffic from instances in the same security group
func (c *Client) CreateOrGetMPISecurityGroup(ctx context.Context, region, vpcID, groupName string) (string, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	// Try to find existing security group
	describeResult, err := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("group-name"),
				Values: []string{groupName},
			},
			{
				Name:   aws.String("vpc-id"),
				Values: []string{vpcID},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe security groups: %w", err)
	}

	// If security group exists, return it
	if len(describeResult.SecurityGroups) > 0 {
		return *describeResult.SecurityGroups[0].GroupId, nil
	}

	// Create new security group
	createResult, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(groupName),
		Description: aws.String("Security group for MPI cluster inter-node communication"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags: []types.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(groupName),
					},
					{
						Key:   aws.String("spawn:managed"),
						Value: aws.String("true"),
					},
					{
						Key:   aws.String("spawn:purpose"),
						Value: aws.String("mpi-cluster"),
					},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create security group: %w", err)
	}

	sgID := *createResult.GroupId

	// Add ingress rule: allow all TCP from same security group
	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(0),
				ToPort:     aws.Int32(65535),
				UserIdGroupPairs: []types.UserIdGroupPair{
					{
						GroupId:     aws.String(sgID),
						Description: aws.String("Allow all TCP from MPI cluster nodes"),
					},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to authorize security group ingress: %w", err)
	}

	// Add ingress rule: allow SSH from anywhere (for user access)
	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges: []types.IpRange{
					{
						CidrIp:      aws.String("0.0.0.0/0"),
						Description: aws.String("SSH access"),
					},
				},
			},
		},
	})
	if err != nil {
		// Non-fatal if SSH rule fails (might already exist from default)
		fmt.Printf("Warning: failed to add SSH rule: %v\n", err)
	}

	return sgID, nil
}

// EnsureLustrePorts adds self-referencing inbound rules for the Lustre protocol
// to the specified security group if they are not already present.
// Lustre requires port 988 (MGS/MDS/OSS) and 1018–1023 (dynamic OST traffic)
// to be open between all nodes that share a filesystem (fixes #316).
func (c *Client) EnsureLustrePorts(ctx context.Context, region, sgID string) error {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	selfRef := []types.UserIdGroupPair{{GroupId: aws.String(sgID), Description: aws.String("Lustre inter-node (self)")}}

	perms := []types.IpPermission{
		{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(988), ToPort: aws.Int32(988), UserIdGroupPairs: selfRef},
		{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(1018), ToPort: aws.Int32(1023), UserIdGroupPairs: selfRef},
	}

	_, err := ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: perms,
	})
	if err != nil {
		// Duplicate rule error is fine — rules already exist
		errStr := err.Error()
		if strings.Contains(errStr, "InvalidPermission.Duplicate") || strings.Contains(errStr, "already exists") {
			return nil
		}
		return fmt.Errorf("ensure lustre ports on %s: %w", sgID, err)
	}
	return nil
}

// GetDefaultVPC returns the default VPC ID for the region
func (c *Client) GetDefaultVPC(ctx context.Context, region string) (string, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	result, err := ec2Client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("is-default"),
				Values: []string{"true"},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe VPCs: %w", err)
	}

	if len(result.Vpcs) == 0 {
		return "", fmt.Errorf("no default VPC found in region %s", region)
	}

	return *result.Vpcs[0].VpcId, nil
}

// CreateOrGetDCVSecurityGroup creates or retrieves a security group named "spawn-dcv"
// that allows inbound TCP 8443 (NICE DCV) from anywhere. Returns the security group ID.
func (c *Client) CreateOrGetDCVSecurityGroup(ctx context.Context, region, vpcID string) (string, error) {
	cfg := c.cfg.Copy()
	cfg.Region = region
	ec2Client := ec2.NewFromConfig(cfg)

	const sgName = "spawn-dcv"

	// Check if it already exists
	describeResult, err := ec2Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []types.Filter{
			{Name: aws.String("group-name"), Values: []string{sgName}},
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("describe security groups: %w", err)
	}
	if len(describeResult.SecurityGroups) > 0 {
		return *describeResult.SecurityGroups[0].GroupId, nil
	}

	// Create it
	createResult, err := ec2Client.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(sgName),
		Description: aws.String("spawn-managed: NICE DCV application streaming (TCP 8443)"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSecurityGroup,
				Tags: []types.Tag{
					{Key: aws.String("spawn:managed"), Value: aws.String("true")},
					{Key: aws.String("Name"), Value: aws.String(sgName)},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create security group: %w", err)
	}
	sgID := *createResult.GroupId

	// Authorize DCV port
	_, err = ec2Client.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []types.IpPermission{
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(8443),
				ToPort:     aws.Int32(8443),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("NICE DCV (IPv4)")},
				},
				Ipv6Ranges: []types.Ipv6Range{
					{CidrIpv6: aws.String("::/0"), Description: aws.String("NICE DCV (IPv6)")},
				},
			},
			// Also allow SSH so users can debug if needed
			{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int32(22),
				ToPort:     aws.Int32(22),
				IpRanges: []types.IpRange{
					{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("SSH")},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("authorize DCV ingress: %w", err)
	}

	return sgID, nil
}

// GetEFSDNSName constructs the EFS DNS name from filesystem ID and region
func GetEFSDNSName(filesystemID, region string) string {
	return fmt.Sprintf("%s.efs.%s.amazonaws.com", filesystemID, region)
}

// regionToLocationName maps AWS region codes to the location name used by the Pricing API.
var regionToLocationName = map[string]string{
	"us-east-1":      "US East (N. Virginia)",
	"us-east-2":      "US East (Ohio)",
	"us-west-1":      "US West (N. California)",
	"us-west-2":      "US West (Oregon)",
	"eu-west-1":      "Europe (Ireland)",
	"eu-west-2":      "Europe (London)",
	"eu-west-3":      "Europe (Paris)",
	"eu-central-1":   "Europe (Frankfurt)",
	"eu-north-1":     "Europe (Stockholm)",
	"eu-south-1":     "Europe (Milan)",
	"ap-northeast-1": "Asia Pacific (Tokyo)",
	"ap-northeast-2": "Asia Pacific (Seoul)",
	"ap-northeast-3": "Asia Pacific (Osaka)",
	"ap-southeast-1": "Asia Pacific (Singapore)",
	"ap-southeast-2": "Asia Pacific (Sydney)",
	"ap-south-1":     "Asia Pacific (Mumbai)",
	"ap-east-1":      "Asia Pacific (Hong Kong)",
	"ca-central-1":   "Canada (Central)",
	"sa-east-1":      "South America (Sao Paulo)",
	"me-south-1":     "Middle East (Bahrain)",
	"af-south-1":     "Africa (Cape Town)",
}

// LookupEC2OnDemandPrice queries the AWS Pricing API for the current on-demand price
// of an instance type in a region. Returns 0 and logs if the lookup fails.
// The Pricing API is only available in us-east-1 and ap-south-1.
func LookupEC2OnDemandPrice(ctx context.Context, region, instanceType string) float64 {
	location, ok := regionToLocationName[region]
	if !ok {
		log.Printf("pricing: unknown region %q, cannot look up price", region)
		return 0
	}

	// Pricing API is only in us-east-1 and ap-south-1 regardless of where the instance is
	pricingCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		log.Printf("pricing: failed to load config: %v", err)
		return 0
	}

	pricingClient := awspricing.NewFromConfig(pricingCfg)
	out, err := pricingClient.GetProducts(ctx, &awspricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []pricingtypes.Filter{
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("instanceType"), Value: aws.String(instanceType)},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("location"), Value: aws.String(location)},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("operatingSystem"), Value: aws.String("Linux")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("tenancy"), Value: aws.String("Shared")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("preInstalledSw"), Value: aws.String("NA")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("capacitystatus"), Value: aws.String("Used")},
		},
		MaxResults: aws.Int32(1),
	})
	if err != nil {
		log.Printf("pricing: GetProducts failed for %s in %s: %v", instanceType, region, err)
		return 0
	}
	if len(out.PriceList) == 0 {
		log.Printf("pricing: no price found for %s in %s", instanceType, region)
		return 0
	}

	// Parse the nested pricing JSON: terms → OnDemand → priceDimensions → pricePerUnit USD
	var priceDoc struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(out.PriceList[0]), &priceDoc); err != nil {
		log.Printf("pricing: parse error: %v", err)
		return 0
	}
	for _, term := range priceDoc.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			if usd, ok := dim.PricePerUnit["USD"]; ok {
				if price, err := strconv.ParseFloat(usd, 64); err == nil && price > 0 {
					return price
				}
			}
		}
	}
	return 0
}
