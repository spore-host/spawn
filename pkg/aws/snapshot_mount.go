package aws

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// MountSnapshotResult reports the volume created + attached for a snapshot mount.
type MountSnapshotResult struct {
	VolumeID   string
	DeviceName string // the EC2 device name requested (e.g. /dev/sdf)
	InstanceID string
}

// AttachSnapshotToSelf creates an EBS volume from a snapshot and attaches it to
// the EC2 instance this process is running on, returning the volume id + device
// name (#161 follow-up — the head-node convenience for the reference-data-volume
// recipe). It does NOT mount: mounting is a privileged local step the caller runs
// (the NVMe device-name remap means the live device must be resolved on-box, the
// same as the launch path). Only works ON an EC2 instance — IMDS identifies it.
func (c *Client) AttachSnapshotToSelf(ctx context.Context, snapshotID string) (*MountSnapshotResult, error) {
	// Who am I? IMDS gives the instance id + AZ; the volume must be in the same AZ.
	imdsClient := imds.NewFromConfig(c.cfg)
	idDoc, err := imdsClient.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return nil, fmt.Errorf("not running on an EC2 instance (IMDS unavailable): %w", err)
	}
	instanceID := idDoc.InstanceID
	az := idDoc.AvailabilityZone

	cfg := c.cfg.Copy()
	cfg.Region = idDoc.Region
	ec2Client := ec2.NewFromConfig(cfg)

	// Create the volume from the snapshot, in this instance's AZ.
	cv, err := ec2Client.CreateVolume(ctx, &ec2.CreateVolumeInput{
		SnapshotId:       aws.String(snapshotID),
		AvailabilityZone: aws.String(az),
		VolumeType:       ec2types.VolumeTypeGp3,
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVolume,
			Tags: []ec2types.Tag{
				{Key: aws.String("spawn:managed"), Value: aws.String("true")},
				{Key: aws.String("spawn:from-snapshot"), Value: aws.String(snapshotID)},
			},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("create volume from %s: %w", snapshotID, err)
	}
	volumeID := aws.ToString(cv.VolumeId)

	// Wait until the volume is available before attaching.
	availWaiter := ec2.NewVolumeAvailableWaiter(ec2Client)
	if err := availWaiter.Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volumeID}}, 5*time.Minute); err != nil {
		return nil, fmt.Errorf("volume %s never became available: %w", volumeID, err)
	}

	// Pick a free device name not already in use on this instance.
	device, err := freeDeviceName(ctx, ec2Client, instanceID)
	if err != nil {
		return nil, err
	}

	if _, err := ec2Client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String(volumeID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String(device),
	}); err != nil {
		return nil, fmt.Errorf("attach volume %s to %s: %w", volumeID, instanceID, err)
	}

	// Wait until attached (in-use).
	inUseWaiter := ec2.NewVolumeInUseWaiter(ec2Client)
	if err := inUseWaiter.Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volumeID}}, 5*time.Minute); err != nil {
		return nil, fmt.Errorf("volume %s never attached: %w", volumeID, err)
	}

	return &MountSnapshotResult{VolumeID: volumeID, DeviceName: device, InstanceID: instanceID}, nil
}

// freeDeviceName returns an EC2 device name (/dev/sdf … /dev/sdp) not already
// mapped on the instance, so a new attach doesn't collide with the root or an
// existing data volume.
func freeDeviceName(ctx context.Context, ec2Client *ec2.Client, instanceID string) (string, error) {
	desc, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return "", fmt.Errorf("describe self %s: %w", instanceID, err)
	}
	used := map[string]bool{}
	for _, r := range desc.Reservations {
		for _, inst := range r.Instances {
			for _, bdm := range inst.BlockDeviceMappings {
				used[aws.ToString(bdm.DeviceName)] = true
			}
		}
	}
	dev := pickFreeDevice(used)
	if dev == "" {
		return "", fmt.Errorf("no free device name available on %s (/dev/sdf../dev/sdp all in use)", instanceID)
	}
	return dev, nil
}

// pickFreeDevice returns the first /dev/sd[f-p] device not present in `used`
// (checking the /dev/xvd alias too), or "" if all are taken. Pure, for testing.
func pickFreeDevice(used map[string]bool) string {
	for c := byte('f'); c <= 'p'; c++ {
		dev := "/dev/sd" + string(rune(c))
		if !used[dev] && !used["/dev/xvd"+string(rune(c))] {
			return dev
		}
	}
	return ""
}
