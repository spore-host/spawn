package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/imagebuilder"
)

// AMIInfo represents information about an AMI
type AMIInfo struct {
	AMIID        string
	Name         string
	Description  string
	Architecture string
	CreationDate time.Time
	Size         int64    // Size in GB
	SnapshotIDs  []string // backing EBS snapshots
	Tags         map[string]string

	// spawn-specific fields
	Stack      string
	Version    string
	GPU        bool
	BaseOS     string
	Deprecated bool

	// Provenance is what the image records about the spawn that created it
	// and its source instance (#1364).
	Provenance ImageProvenance
}

// CreateAMIInput contains parameters for creating an AMI
type CreateAMIInput struct {
	InstanceID  string
	Name        string
	Description string
	Tags        map[string]string
	NoReboot    bool
	// SourceTags are the source instance's tags. The image records some of
	// them as spawn:source-* tags (ImageCreateTags, #1364). Nil records none.
	SourceTags map[string]string
}

type createImageAPI interface {
	CreateImage(ctx context.Context, in *ec2.CreateImageInput, optFns ...func(*ec2.Options)) (*ec2.CreateImageOutput, error)
}

// CreateAMI creates an AMI from a running instance. The image and its
// snapshots are tagged, atomically with the CreateImage call, with
// ImageCreateTags(input.Tags, input.SourceTags): the caller's tags plus the
// image's provenance (#1364), which input.Tags cannot override.
func (c *Client) CreateAMI(ctx context.Context, region string, input CreateAMIInput) (string, error) {
	return createAMI(ctx, c.regionalEC2(region), input)
}

func createAMI(ctx context.Context, api createImageAPI, input CreateAMIInput) (string, error) {
	tagMap := ImageCreateTags(input.Tags, input.SourceTags)
	if err := checkImageTagCount("create the image", tagMap); err != nil {
		return "", err
	}

	// Tag both the AMI and its snapshots
	var tagSpecs []types.TagSpecification
	if len(tagMap) > 0 {
		tagSpecs = []types.TagSpecification{
			{ResourceType: types.ResourceTypeImage, Tags: tagList(tagMap)},
			{ResourceType: types.ResourceTypeSnapshot, Tags: tagList(tagMap)},
		}
	}

	result, err := api.CreateImage(ctx, &ec2.CreateImageInput{
		InstanceId:        aws.String(input.InstanceID),
		Name:              aws.String(input.Name),
		Description:       aws.String(input.Description),
		NoReboot:          aws.Bool(input.NoReboot),
		TagSpecifications: tagSpecs,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create AMI: %w", err)
	}

	return aws.ToString(result.ImageId), nil
}

// WaitForAMI waits for an AMI to become available
func (c *Client) WaitForAMI(ctx context.Context, region string, amiID string, timeout time.Duration) error {
	// Create regional client
	ec2Client := c.regionalEC2(region)

	// Create waiter
	waiter := ec2.NewImageAvailableWaiter(ec2Client)

	// Wait for AMI to be available
	err := waiter.Wait(ctx, &ec2.DescribeImagesInput{
		ImageIds: []string{amiID},
	}, timeout)
	if err != nil {
		return fmt.Errorf("failed waiting for AMI: %w", err)
	}

	return nil
}

// ListAMIs lists AMIs with optional filters
// Filters are applied in-memory after retrieving all AMIs owned by the account
func (c *Client) ListAMIs(ctx context.Context, region string, filters map[string]string) ([]AMIInfo, error) {
	// Create regional client
	ec2Client := c.regionalEC2(region)

	// Get current account ID
	accountID, err := c.GetAccountID(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get account ID: %w", err)
	}

	// Build EC2 filters - only filter by owner
	ec2Filters := []types.Filter{
		{
			Name:   aws.String("owner-id"),
			Values: []string{accountID},
		},
	}

	// List AMIs (all AMIs owned by this account)
	result, err := ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Filters: ec2Filters,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list AMIs: %w", err)
	}

	// Convert to AMIInfo
	amis := make([]AMIInfo, 0, len(result.Images))
	for _, img := range result.Images {
		amis = append(amis, amiInfoFromImage(img))
	}

	return amis, nil
}

// amiInfoFromImage is how ListAMIs (and so `spawn ami list`) reads one image.
func amiInfoFromImage(img types.Image) AMIInfo {
	// Parse tags
	tags := make(map[string]string)
	for _, tag := range img.Tags {
		if tag.Key != nil && tag.Value != nil {
			tags[*tag.Key] = *tag.Value
		}
	}

	// Parse creation date
	var creationDate time.Time
	if img.CreationDate != nil {
		creationDate, _ = time.Parse(time.RFC3339, *img.CreationDate)
	}

	// Calculate total size + collect backing snapshots from block device mappings
	var totalSize int64
	var snapshotIDs []string
	if img.BlockDeviceMappings != nil {
		for _, bdm := range img.BlockDeviceMappings {
			if bdm.Ebs != nil && bdm.Ebs.VolumeSize != nil {
				totalSize += int64(*bdm.Ebs.VolumeSize)
			}
			if bdm.Ebs != nil && bdm.Ebs.SnapshotId != nil {
				snapshotIDs = append(snapshotIDs, *bdm.Ebs.SnapshotId)
			}
		}
	}

	// Extract spawn-specific tags (check both namespaced and non-namespaced)
	stack := tags["spawn:stack"]
	if stack == "" {
		stack = tags["stack"]
	}
	version := tags["spawn:version"]
	if version == "" {
		version = tags["version"]
	}
	gpu := tags["spawn:gpu"] == "true"
	baseOS := tags["spawn:base"]
	if baseOS == "" {
		baseOS = tags["base"]
	}
	deprecated := tags["spawn:deprecated"] == "true"

	amiInfo := AMIInfo{
		AMIID:        aws.ToString(img.ImageId),
		Name:         aws.ToString(img.Name),
		Description:  aws.ToString(img.Description),
		Architecture: string(img.Architecture),
		CreationDate: creationDate,
		Size:         totalSize,
		SnapshotIDs:  snapshotIDs,
		Tags:         tags,
		Stack:        stack,
		Version:      version,
		GPU:          gpu,
		BaseOS:       baseOS,
		Deprecated:   deprecated,
		Provenance:   ImageProvenanceFromTags(tags),
	}
	return amiInfo
}

// IsSpawnManagedAMI is what `spawn ami list` counts as spawn's: an AMI tagged
// spawn:managed=true, or one carrying a stack tag (AMIs from before the
// managed tag).
func IsSpawnManagedAMI(ami AMIInfo) bool {
	return ami.Tags["spawn:managed"] == "true" || ami.Stack != ""
}

// DeleteAMIResult reports what DeleteAMI did.
type DeleteAMIResult struct {
	AMIID             string
	DeletedSnapshots  []string          // snapshots actually deleted
	RetainedSnapshots map[string]string // snapshot -> why it was kept (shared with other AMIs)
	ImageBuilderArn   string            // IB image resource deleted, if any
	SnapshotErrors    map[string]string // snapshot -> deletion error (aggregated, non-fatal)
	ImageBuilderError string            // IB-resource deletion error, if any (non-fatal)
}

// DeleteAMI deregisters an AMI and cleans up its resources intelligently:
//
//   - Backing EBS snapshots are deleted ONLY if no other AMI still references
//     them. A snapshot shared by another AMI is RETAINED (deleting it would break
//     the other image), recorded in RetainedSnapshots — not treated as an error.
//   - If the AMI was produced by EC2 Image Builder (Ec2ImageBuilderArn tag, e.g.
//     `spawn image import`), the corresponding Image Builder image resource is
//     also deleted so its name/version is freed. AMIs NOT from Image Builder skip
//     this entirely (no tag → no call), so it never fails on a missing resource.
//
// Deregister is the one irreversible, must-succeed step; snapshot and
// Image-Builder cleanup are best-effort and AGGREGATED into the result (errors
// are collected, not fatal) so one stuck snapshot doesn't abort the rest. The
// returned error is non-nil only if some cleanup was incomplete, but the result
// always details exactly what happened.
func (c *Client) DeleteAMI(ctx context.Context, region, amiID string) (*DeleteAMIResult, error) {
	cfg := c.regionalConfig(region)
	ec2Client := ec2.NewFromConfig(cfg)

	// Describe first to capture backing snapshots + any Image Builder tag.
	desc, err := ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		ImageIds: []string{amiID},
	})
	if err != nil {
		return nil, fmt.Errorf("describe AMI %s: %w", amiID, err)
	}
	if len(desc.Images) == 0 {
		return nil, fmt.Errorf("AMI %s not found in %s", amiID, region)
	}
	img := desc.Images[0]

	res := &DeleteAMIResult{
		AMIID:             amiID,
		RetainedSnapshots: map[string]string{},
		SnapshotErrors:    map[string]string{},
	}
	var snapshots []string
	for _, bdm := range img.BlockDeviceMappings {
		if bdm.Ebs != nil && bdm.Ebs.SnapshotId != nil {
			snapshots = append(snapshots, *bdm.Ebs.SnapshotId)
		}
	}
	for _, tag := range img.Tags {
		if tag.Key != nil && *tag.Key == "Ec2ImageBuilderArn" && tag.Value != nil {
			res.ImageBuilderArn = *tag.Value
		}
	}

	// Deregister the AMI (the critical, irreversible step).
	if _, err := ec2Client.DeregisterImage(ctx, &ec2.DeregisterImageInput{
		ImageId: aws.String(amiID),
	}); err != nil {
		return nil, fmt.Errorf("deregister AMI %s: %w", amiID, err)
	}

	// Delete backing snapshots — but only those no OTHER AMI still references.
	for _, snap := range snapshots {
		others, lookupErr := c.amisReferencingSnapshot(ctx, ec2Client, snap, amiID)
		if del, reason := snapshotDeletionDecision(others, lookupErr); !del {
			res.RetainedSnapshots[snap] = reason
			continue
		}
		if _, err := ec2Client.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
			SnapshotId: aws.String(snap),
		}); err != nil {
			res.SnapshotErrors[snap] = err.Error()
			continue
		}
		res.DeletedSnapshots = append(res.DeletedSnapshots, snap)
	}

	// If it came from Image Builder, delete that image resource too. Best-effort.
	if res.ImageBuilderArn != "" {
		ibClient := imagebuilder.NewFromConfig(cfg)
		if _, err := ibClient.DeleteImage(ctx, &imagebuilder.DeleteImageInput{
			ImageBuildVersionArn: aws.String(res.ImageBuilderArn),
		}); err != nil {
			res.ImageBuilderError = err.Error()
		}
	}

	if len(res.SnapshotErrors) > 0 || res.ImageBuilderError != "" {
		return res, fmt.Errorf("AMI %s deregistered, but some cleanup was incomplete (see result)", amiID)
	}
	return res, nil
}

// snapshotDeletionDecision decides whether a backing snapshot is safe to delete,
// given the result of looking up which OTHER AMIs reference it. It is the pure,
// deterministic core of the retain-shared-snapshots logic (separated out so it's
// unit-testable without a live EC2):
//   - lookup error  → retain (can't prove it's unshared; don't risk another AMI)
//   - others present → retain (still backs those AMIs)
//   - none           → delete (exclusive to the AMI being removed)
func snapshotDeletionDecision(otherAMIs []string, lookupErr error) (del bool, reason string) {
	if lookupErr != nil {
		return false, fmt.Sprintf("could not verify references: %v", lookupErr)
	}
	if len(otherAMIs) > 0 {
		return false, fmt.Sprintf("still used by %s", strings.Join(otherAMIs, ", "))
	}
	return true, ""
}

// amisReferencingSnapshot returns the IDs of AMIs OTHER than excludeAMI whose
// block device mappings reference the given snapshot. Used to avoid deleting a
// snapshot that still backs another image.
func (c *Client) amisReferencingSnapshot(ctx context.Context, ec2Client *ec2.Client, snapshotID, excludeAMI string) ([]string, error) {
	out, err := ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Filters: []types.Filter{
			{Name: aws.String("block-device-mapping.snapshot-id"), Values: []string{snapshotID}},
		},
	})
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, img := range out.Images {
		id := aws.ToString(img.ImageId)
		if id != "" && id != excludeAMI {
			refs = append(refs, id)
		}
	}
	return refs, nil
}

// SnapshotDetail describes one EBS snapshot backing an AMI.
type SnapshotDetail struct {
	SnapshotID  string
	VolumeSize  int32  // GiB
	State       string // completed/pending/error
	StartTime   time.Time
	Encrypted   bool
	Description string
	SharedWith  []string // OTHER AMIs that also reference this snapshot (empty = exclusive)
}

// GetAMISnapshots resolves an AMI's backing EBS snapshots with detail, including
// which OTHER AMIs share each snapshot (so callers know what can be safely
// deleted). Backs `spawn ami snapshots <ami-id>`.
func (c *Client) GetAMISnapshots(ctx context.Context, region, amiID string) ([]SnapshotDetail, error) {
	ec2Client := c.regionalEC2(region)

	desc, err := ec2Client.DescribeImages(ctx, &ec2.DescribeImagesInput{
		ImageIds: []string{amiID},
	})
	if err != nil {
		return nil, fmt.Errorf("describe AMI %s: %w", amiID, err)
	}
	if len(desc.Images) == 0 {
		return nil, fmt.Errorf("AMI %s not found in %s", amiID, region)
	}

	var snapIDs []string
	for _, bdm := range desc.Images[0].BlockDeviceMappings {
		if bdm.Ebs != nil && bdm.Ebs.SnapshotId != nil {
			snapIDs = append(snapIDs, *bdm.Ebs.SnapshotId)
		}
	}
	if len(snapIDs) == 0 {
		return nil, nil
	}

	snaps, err := ec2Client.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		SnapshotIds: snapIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("describe snapshots for %s: %w", amiID, err)
	}

	details := make([]SnapshotDetail, 0, len(snaps.Snapshots))
	for _, s := range snaps.Snapshots {
		d := SnapshotDetail{
			SnapshotID:  aws.ToString(s.SnapshotId),
			VolumeSize:  aws.ToInt32(s.VolumeSize),
			State:       string(s.State),
			Encrypted:   aws.ToBool(s.Encrypted),
			Description: aws.ToString(s.Description),
		}
		if s.StartTime != nil {
			d.StartTime = *s.StartTime
		}
		if shared, err := c.amisReferencingSnapshot(ctx, ec2Client, d.SnapshotID, amiID); err == nil {
			d.SharedWith = shared
		}
		details = append(details, d)
	}
	return details, nil
}
