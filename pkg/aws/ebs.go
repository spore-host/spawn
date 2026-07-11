// EBS block-device mapping and root-volume sizing.

package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

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

	mappings := []types.BlockDeviceMapping{
		{
			DeviceName: aws.String("/dev/xvda"),
			Ebs:        ebs,
		},
	}

	// Append data volumes created from snapshots (#144). The requested device
	// name (/dev/sdf, /dev/sdg, …) is only a hint on Nitro instances — they
	// surface as NVMe devices in a non-deterministic order — so the user-data
	// mount step resolves the real device by snapshot/volume, not by this name.
	for i, v := range config.AttachVolumes {
		dev := AttachDeviceName(i)
		vol := &types.EbsBlockDevice{
			SnapshotId:          aws.String(v.SnapshotID),
			VolumeType:          types.VolumeTypeGp3,
			DeleteOnTermination: aws.Bool(true),
			Encrypted:           aws.Bool(encrypted),
		}
		if v.SizeGiB > 0 {
			vol.VolumeSize = aws.Int32(v.SizeGiB)
		}
		if encrypted && config.EBSKMSKeyID != "" {
			vol.KmsKeyId = aws.String(config.EBSKMSKeyID)
		}
		mappings = append(mappings, types.BlockDeviceMapping{
			DeviceName: aws.String(dev),
			Ebs:        vol,
		})
	}

	return mappings
}

// AttachDeviceName returns the EC2 device name for the i-th attached data
// volume: /dev/sdf, /dev/sdg, … (a..e are conventionally reserved). On Nitro
// instances these are remapped to NVMe devices, so this is only the value EC2
// records in the block-device mapping; the mount step resolves the live device.
// The launch CLI uses the same scheme to tell the user-data which device each
// mount maps to, so the two sides stay in sync.
func AttachDeviceName(i int) string {
	return "/dev/sd" + string(rune('f'+i))
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
