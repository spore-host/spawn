package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs"
)

// GetEFSDNSName constructs the EFS DNS name from filesystem ID and region.
func GetEFSDNSName(filesystemID, region string) string {
	return fmt.Sprintf("%s.efs.%s.amazonaws.com", filesystemID, region)
}

// describeMountTargetsAPI is the slice of EFS this needs — an interface so the
// AZ-matching logic is tested without AWS.
type describeMountTargetsAPI interface {
	DescribeMountTargets(ctx context.Context, in *efs.DescribeMountTargetsInput, optFns ...func(*efs.Options)) (*efs.DescribeMountTargetsOutput, error)
}

// EFSMountTargetIP returns the IP of the filesystem's mount target in the given
// subnet, falling back to a mount target in the same AZ, then to any available
// one. Returns "" (no error) when none can be used — the DNS name still works in
// the normal case, so a failed lookup must not fail a launch.
//
// This exists because the DNS name is NOT reliable at boot (spawn#704). A real
// run: mount target `available` in the instance's own subnet, general DNS
// working, `enableDnsSupport` and `enableDnsHostnames` both true — and
//
//	mount.nfs4: Failed to resolve server fs-….efs.us-east-1.amazonaws.com
//
// on all six retry attempts across ~60 seconds, while mounting by the mount
// target's IP succeeded first try with identical options. EFS DNS propagation
// for a freshly created mount target takes longer than a boot-time retry budget
// worth spending, so the IP is the only thing that makes a first-boot mount of a
// new filesystem work.
//
// The IP is a FALLBACK and never goes in /etc/fstab: a mount-target replacement
// changes it, and the DNS name is the durable reference.
func (c *Client) EFSMountTargetIP(ctx context.Context, filesystemID, region, subnetID, az string) (string, error) {
	return efsMountTargetIP(ctx, efs.NewFromConfig(c.regionalConfig(region)), filesystemID, subnetID, az)
}

func efsMountTargetIP(ctx context.Context, api describeMountTargetsAPI, filesystemID, subnetID, az string) (string, error) {
	out, err := api.DescribeMountTargets(ctx, &efs.DescribeMountTargetsInput{
		FileSystemId: aws.String(filesystemID),
	})
	if err != nil {
		return "", fmt.Errorf("describe mount targets for %s: %w", filesystemID, err)
	}

	var sameAZ, any string
	for _, mt := range out.MountTargets {
		// Only an available mount target has a usable IP.
		if mt.LifeCycleState != "available" || aws.ToString(mt.IpAddress) == "" {
			continue
		}
		ip := aws.ToString(mt.IpAddress)

		// Exact subnet is best: NFS to a mount target in another subnet crosses
		// AZs, which costs money and latency even when it works.
		if subnetID != "" && aws.ToString(mt.SubnetId) == subnetID {
			return ip, nil
		}
		if az != "" && aws.ToString(mt.AvailabilityZoneName) == az && sameAZ == "" {
			sameAZ = ip
		}
		if any == "" {
			any = ip
		}
	}
	if sameAZ != "" {
		return sameAZ, nil
	}
	// Any available target still beats a name that does not resolve. Cross-AZ NFS
	// bills per GiB, but a workload that cannot see its data costs the whole run.
	return any, nil
}
