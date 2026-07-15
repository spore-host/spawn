package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/fsx"
	"github.com/aws/aws-sdk-go-v2/service/fsx/types"
)

// FSxInfo contains information about an FSx Lustre filesystem
type FSxInfo struct {
	FileSystemID    string
	DNSName         string
	MountName       string
	StorageCapacity int32
	S3Bucket        string
	S3ImportPath    string
	S3ExportPath    string
}

// FSxConfig contains configuration for creating FSx Lustre filesystem
type FSxConfig struct {
	StackName                string
	Region                   string
	StorageCapacity          int32
	S3Bucket                 string
	ImportPath               string
	ExportPath               string
	AutoCreateBucket         bool
	SubnetID                 string   // Optional: specify subnet, otherwise uses default VPC
	SecurityGroupIDs         []string // Security groups to associate with FSx; must allow port 988 (Lustre)
	PerUnitStorageThroughput int32    // MB/s/TiB — required for PERSISTENT_2; valid values: 125, 250, 500, 1000

	// Lifecycle (#193): tagged so the reaper (#192) can reclaim the filesystem.
	// Lifecycle is "ephemeral" or "durable". TTLDeadline, when non-zero, is
	// written as spawn:ttl-deadline (durable filesystems carry an explicit
	// death clock; ephemeral ones are reaped on refcount→0 instead).
	Lifecycle   string
	TTLDeadline time.Time
}

// startFSxCreate runs the shared creation steps (paths, bucket, subnet, the
// CreateFileSystem call) and returns the new filesystem id plus a regional FSx
// client and the resolved import/export paths. It does NOT wait for AVAILABLE or
// create the data-repository association — callers choose: CreateFSxLustreFilesystem
// blocks and sets up the DRA; CreateFSxLustreFilesystemAsync returns immediately
// and leaves AVAILABLE-wait + DRA to spored (#194).
func (c *Client) startFSxCreate(ctx context.Context, config FSxConfig) (filesystemID string, fsxClient *fsx.Client, importPath, exportPath string, err error) {
	// 1. Construct import/export paths early (needed for S3 bucket tags)
	importPath = config.ImportPath
	if importPath == "" && config.S3Bucket != "" {
		importPath = fmt.Sprintf("s3://%s/", config.S3Bucket)
	}
	exportPath = config.ExportPath
	if exportPath == "" && config.S3Bucket != "" {
		exportPath = fmt.Sprintf("s3://%s/", config.S3Bucket)
	}

	// 2. Ensure S3 bucket exists with FSx configuration tags (auto-create if specified)
	if config.AutoCreateBucket {
		if err = c.CreateS3BucketWithTags(ctx, S3BucketConfig{
			BucketName:      config.S3Bucket,
			Region:          config.Region,
			StackName:       config.StackName,
			StorageCapacity: config.StorageCapacity,
			ImportPath:      importPath,
			ExportPath:      exportPath,
		}); err != nil {
			return "", nil, "", "", fmt.Errorf("failed to create S3 bucket: %w", err)
		}
	}

	// 3. Get subnet ID (use default VPC if not specified). Co-location matters:
	// the FSx must be in the SAME AZ as the instance that mounts it (#194), so
	// callers that know the instance's subnet should pass it in config.SubnetID.
	subnetID := config.SubnetID
	if subnetID == "" {
		vpcID, verr := c.GetDefaultVPC(ctx, config.Region)
		if verr != nil {
			return "", nil, "", "", fmt.Errorf("failed to get default VPC: %w", verr)
		}
		subnets, serr := c.GetSubnets(ctx, config.Region, vpcID)
		if serr != nil {
			return "", nil, "", "", fmt.Errorf("failed to get subnets: %w", serr)
		}
		if len(subnets) == 0 {
			return "", nil, "", "", fmt.Errorf("no subnets found in default VPC")
		}
		subnetID = subnets[0]
	}

	fsxClient = fsx.NewFromConfig(c.regionalConfig(config.Region))

	input := buildFSxCreateInput(config, subnetID, importPath, exportPath)
	result, cerr := fsxClient.CreateFileSystem(ctx, input)
	if cerr != nil {
		return "", nil, "", "", fmt.Errorf("failed to create FSx filesystem: %w", cerr)
	}
	return *result.FileSystem.FileSystemId, fsxClient, importPath, exportPath, nil
}

// CreateFSxLustreFilesystemAsync creates the filesystem and returns its id
// immediately, without waiting for AVAILABLE or setting up the S3 DRA. Used by
// the ephemeral launch path (#194): the instance is tagged spawn:fsx-pending and
// spored does the wait → DRA → mount once it boots, so neither the CLI nor the
// lagotto Lambda blocks on the ~10-minute provisioning.
func (c *Client) CreateFSxLustreFilesystemAsync(ctx context.Context, config FSxConfig) (filesystemID string, err error) {
	id, _, _, _, err := c.startFSxCreate(ctx, config)
	return id, err
}

// CreateFSxLustreFilesystem creates an FSx for Lustre filesystem with S3 backing
// and BLOCKS until it is AVAILABLE, then sets up the S3 data-repository
// association. Used for synchronous/up-front provisioning (durable FSx, #195).
func (c *Client) CreateFSxLustreFilesystem(ctx context.Context, config FSxConfig) (*FSxInfo, error) {
	filesystemID, fsxClient, importPath, exportPath, err := c.startFSxCreate(ctx, config)
	if err != nil {
		return nil, err
	}
	return c.waitAndAssociateFSx(ctx, fsxClient, filesystemID, config, importPath, exportPath)
}

// buildFSxCreateInput assembles the CreateFileSystem input (PERSISTENT_2 +
// spawn tags). Used by startFSxCreate for both the blocking and async paths.
func buildFSxCreateInput(config FSxConfig, subnetID, importPath, exportPath string) *fsx.CreateFileSystemInput {
	// PERSISTENT_2 — Lustre server 2.15, matches the AL2023 lustre-client
	// (SCRATCH_2's 2.10 is rejected, #310). PERSISTENT_2 has no inline
	// Import/ExportPath; S3 linking is a separate CreateDataRepositoryAssociation
	// once AVAILABLE (the blocking path does it; async leaves it to spored).
	input := &fsx.CreateFileSystemInput{
		FileSystemType:   types.FileSystemTypeLustre,
		StorageCapacity:  aws.Int32(config.StorageCapacity),
		SubnetIds:        []string{subnetID}, // FSx Lustre requires a single subnet
		SecurityGroupIds: config.SecurityGroupIDs,
		LustreConfiguration: &types.CreateFileSystemLustreConfiguration{
			DeploymentType:           types.LustreDeploymentTypePersistent2,
			DataCompressionType:      types.DataCompressionTypeLz4,
			PerUnitStorageThroughput: aws.Int32(config.PerUnitStorageThroughput),
		},
		Tags: []types.Tag{
			{Key: aws.String("Name"), Value: aws.String(config.StackName)},
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:fsx-s3-backed"), Value: aws.String("true")},
			{Key: aws.String("spawn:fsx-s3-bucket"), Value: aws.String(config.S3Bucket)},
			{Key: aws.String("spawn:fsx-stack-name"), Value: aws.String(config.StackName)},
			{Key: aws.String("spawn:fsx-storage-capacity"), Value: aws.String(fmt.Sprintf("%d", config.StorageCapacity))},
			{Key: aws.String("spawn:fsx-created"), Value: aws.String(time.Now().UTC().Format(time.RFC3339))},
		},
	}
	if importPath != "" {
		input.Tags = append(input.Tags, types.Tag{Key: aws.String("spawn:fsx-s3-import-path"), Value: aws.String(importPath)})
	}
	if exportPath != "" {
		input.Tags = append(input.Tags, types.Tag{Key: aws.String("spawn:fsx-s3-export-path"), Value: aws.String(exportPath)})
	}
	// Lifecycle tags drive the reaper (#192/#193): durable filesystems carry an
	// explicit spawn:ttl-deadline; ephemeral ones rely on refcount→0 instead.
	if config.Lifecycle != "" {
		input.Tags = append(input.Tags, types.Tag{Key: aws.String("spawn:fsx-lifecycle"), Value: aws.String(config.Lifecycle)})
	}
	if !config.TTLDeadline.IsZero() {
		input.Tags = append(input.Tags, types.Tag{Key: aws.String("spawn:ttl-deadline"), Value: aws.String(config.TTLDeadline.UTC().Format(time.RFC3339))})
	}
	return input
}

// waitAndAssociateFSx blocks until the filesystem is AVAILABLE (FSx Lustre takes
// 5–25 min; 30 min timeout), creates the S3 data-repository association if
// import/export paths are set, and returns the filesystem info. This is the
// synchronous tail shared by the blocking create path.
func (c *Client) waitAndAssociateFSx(ctx context.Context, fsxClient *fsx.Client, filesystemID string, config FSxConfig, importPath, exportPath string) (*FSxInfo, error) {
	maxWaitTime := 30 * time.Minute
	startTime := time.Now()
	for {
		describeResult, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{
			FileSystemIds: []string{filesystemID},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to describe FSx filesystem: %w", err)
		}
		if len(describeResult.FileSystems) > 0 {
			fs := describeResult.FileSystems[0]
			if fs.Lifecycle == types.FileSystemLifecycleAvailable {
				break
			}
			if fs.Lifecycle == types.FileSystemLifecycleFailed {
				return nil, fmt.Errorf("FSx filesystem %s creation failed", filesystemID)
			}
		}
		if time.Since(startTime) > maxWaitTime {
			return nil, fmt.Errorf(
				"FSx filesystem creation timeout after %v (filesystem %s is still creating)\n"+
					"Retry with: spawn launch ... --fsx-id %s",
				maxWaitTime, filesystemID, filesystemID,
			)
		}
		time.Sleep(30 * time.Second)
	}

	if importPath != "" || exportPath != "" {
		if err := c.associateFSxS3(ctx, fsxClient, filesystemID, importPath, exportPath); err != nil {
			return nil, err
		}
	}

	describeResult, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{
		FileSystemIds: []string{filesystemID},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe FSx filesystem: %w", err)
	}
	if len(describeResult.FileSystems) == 0 {
		return nil, fmt.Errorf("FSx filesystem not found after creation")
	}
	fs := describeResult.FileSystems[0]
	return &FSxInfo{
		FileSystemID:    *fs.FileSystemId,
		DNSName:         *fs.DNSName,
		MountName:       *fs.LustreConfiguration.MountName,
		StorageCapacity: *fs.StorageCapacity,
		S3Bucket:        config.S3Bucket,
		S3ImportPath:    importPath,
		S3ExportPath:    exportPath,
	}, nil
}

// associateFSxS3 creates the continuous-export S3 data-repository association on
// an AVAILABLE PERSISTENT_2 filesystem (NEW/CHANGED/DELETED auto-import+export,
// so results mirror to S3 continuously — the #184 durability lesson). Shared by
// the blocking create path and spored's async mount path (#194).
func (c *Client) associateFSxS3(ctx context.Context, fsxClient *fsx.Client, filesystemID, importPath, exportPath string) error {
	dra := &fsx.CreateDataRepositoryAssociationInput{
		FileSystemId:                aws.String(filesystemID),
		FileSystemPath:              aws.String("/"),
		DataRepositoryPath:          aws.String(importPath),
		BatchImportMetaDataOnCreate: aws.Bool(true),
		S3: &types.S3DataRepositoryConfiguration{
			AutoImportPolicy: &types.AutoImportPolicy{
				Events: []types.EventType{types.EventTypeNew, types.EventTypeChanged, types.EventTypeDeleted},
			},
			AutoExportPolicy: &types.AutoExportPolicy{
				Events: []types.EventType{types.EventTypeNew, types.EventTypeChanged, types.EventTypeDeleted},
			},
		},
	}
	if exportPath != "" {
		dra.DataRepositoryPath = aws.String(exportPath)
	}
	if _, err := fsxClient.CreateDataRepositoryAssociation(ctx, dra); err != nil {
		return fmt.Errorf("create data repository association for %s: %w", filesystemID, err)
	}
	return nil
}

// GetFSxFilesystem retrieves info for existing FSx filesystem
func (c *Client) GetFSxFilesystem(ctx context.Context, filesystemID, region string) (*FSxInfo, error) {
	fsxClient := fsx.NewFromConfig(c.regionalConfig(region))

	result, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{
		FileSystemIds: []string{filesystemID},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe FSx filesystem: %w", err)
	}

	if len(result.FileSystems) == 0 {
		return nil, fmt.Errorf("FSx filesystem not found: %s", filesystemID)
	}

	fs := result.FileSystems[0]

	// Extract S3 info from tags
	s3Bucket := ""
	s3ImportPath := ""
	s3ExportPath := ""
	for _, tag := range fs.Tags {
		switch *tag.Key {
		case "spawn:fsx-s3-bucket":
			s3Bucket = *tag.Value
		case "spawn:fsx-s3-import-path":
			s3ImportPath = *tag.Value
		case "spawn:fsx-s3-export-path":
			s3ExportPath = *tag.Value
		}
	}

	return &FSxInfo{
		FileSystemID:    *fs.FileSystemId,
		DNSName:         *fs.DNSName,
		MountName:       *fs.LustreConfiguration.MountName,
		StorageCapacity: *fs.StorageCapacity,
		S3Bucket:        s3Bucket,
		S3ImportPath:    s3ImportPath,
		S3ExportPath:    s3ExportPath,
	}, nil
}

// DeleteFSxFilesystem deletes an FSx filesystem by id. It does NOT set
// SkipFinalExport, so a filesystem with an attached export DRA flushes remaining
// changes to S3 on delete rather than silently dropping un-exported data (#184).
// Already-deleting / not-found is treated as success (idempotent).
func (c *Client) DeleteFSxFilesystem(ctx context.Context, filesystemID, region string) error {
	fsxClient := fsx.NewFromConfig(c.regionalConfig(region))

	_, err := fsxClient.DeleteFileSystem(ctx, &fsx.DeleteFileSystemInput{
		FileSystemId: aws.String(filesystemID),
	})
	if err != nil {
		if strings.Contains(err.Error(), "FileSystemNotFound") {
			return nil
		}
		return fmt.Errorf("delete FSx filesystem %s: %w", filesystemID, err)
	}
	return nil
}

// RecallFSxFilesystem finds and recreates FSx filesystem by stack name
func (c *Client) RecallFSxFilesystem(ctx context.Context, stackName, region string) (*FSxInfo, error) {
	fsxClient := fsx.NewFromConfig(c.regionalConfig(region))

	// 1. Search for filesystems with this stack name tag
	result, err := fsxClient.DescribeFileSystems(ctx, &fsx.DescribeFileSystemsInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to list FSx filesystems: %w", err)
	}

	// 2. Find filesystem with matching stack name (may be deleted, so check all)
	var foundConfig *FSxConfig
	for _, fs := range result.FileSystems {
		for _, tag := range fs.Tags {
			if *tag.Key == "spawn:fsx-stack-name" && *tag.Value == stackName {
				// Extract configuration from tags
				config := &FSxConfig{
					StackName:       stackName,
					Region:          region,
					StorageCapacity: *fs.StorageCapacity,
				}

				for _, t := range fs.Tags {
					switch *t.Key {
					case "spawn:fsx-s3-bucket":
						config.S3Bucket = *t.Value
					case "spawn:fsx-s3-import-path":
						config.ImportPath = *t.Value
					case "spawn:fsx-s3-export-path":
						config.ExportPath = *t.Value
					}
				}

				// If filesystem is already available, return it
				if fs.Lifecycle == types.FileSystemLifecycleAvailable {
					return &FSxInfo{
						FileSystemID:    *fs.FileSystemId,
						DNSName:         *fs.DNSName,
						MountName:       *fs.LustreConfiguration.MountName,
						StorageCapacity: *fs.StorageCapacity,
						S3Bucket:        config.S3Bucket,
						S3ImportPath:    config.ImportPath,
						S3ExportPath:    config.ExportPath,
					}, nil
				}

				foundConfig = config
				break
			}
		}
		if foundConfig != nil {
			break
		}
	}

	// 3. If no active filesystem found, check S3 buckets for configuration
	if foundConfig == nil {
		var err error
		foundConfig, err = c.GetFSxConfigFromS3Bucket(ctx, stackName, region)
		if err != nil {
			return nil, fmt.Errorf("no FSx filesystem or S3 bucket found with stack name %s: %w", stackName, err)
		}
	}

	// 4. Create new filesystem with same configuration
	foundConfig.AutoCreateBucket = false // Bucket should already exist
	return c.CreateFSxLustreFilesystem(ctx, *foundConfig)
}

// GetSubnets returns subnet IDs for a VPC
func (c *Client) GetSubnets(ctx context.Context, region, vpcID string) ([]string, error) {
	ec2Client := c.regionalEC2(region)

	result, err := ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []string{vpcID},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe subnets: %w", err)
	}

	subnetIDs := make([]string, 0, len(result.Subnets))
	for _, subnet := range result.Subnets {
		subnetIDs = append(subnetIDs, *subnet.SubnetId)
	}

	return subnetIDs, nil
}

// NeedsAZSubnetResolution reports whether an FSx-create needs to resolve an
// availability zone to a subnet before creating the filesystem (#208). It is the
// pure decision shared by the CLI and headless launch paths: resolve only when no
// subnet is explicitly pinned AND an AZ is pinned. With an explicit subnet, that
// wins; with neither, startFSxCreate's default-VPC fallback matches the
// instance's own unpinned placement. Pulling this out keeps the (untestable
// without AWS) DescribeSubnets call thin and the branching logic unit-tested.
func NeedsAZSubnetResolution(pinnedSubnetID, pinnedAZ string) bool {
	return pinnedSubnetID == "" && pinnedAZ != ""
}

// GetSubnetForAZ returns the default-VPC subnet in the given availability zone
// (e.g. "us-east-1a"). FSx for Lustre is single-AZ and a mounting instance must
// be in the SAME AZ as the filesystem, so when a launch pins an AZ the FSx must
// be created in a subnet of THAT AZ — not an arbitrary subnets[0] (#208). Returns
// an error if the region has no default VPC or no subnet in that AZ.
func (c *Client) GetSubnetForAZ(ctx context.Context, region, az string) (string, error) {
	vpcID, err := c.GetDefaultVPC(ctx, region)
	if err != nil {
		return "", fmt.Errorf("failed to get default VPC: %w", err)
	}
	ec2Client := c.regionalEC2(region)

	result, err := ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{vpcID}},
			{Name: aws.String("availability-zone"), Values: []string{az}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe subnets in %s: %w", az, err)
	}
	if len(result.Subnets) == 0 {
		return "", fmt.Errorf("no subnet in availability zone %s (default VPC %s)", az, vpcID)
	}
	return *result.Subnets[0].SubnetId, nil
}
