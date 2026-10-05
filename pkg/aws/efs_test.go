package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
)

type fakeEFS struct {
	targets []efstypes.MountTargetDescription
	err     error
}

func (f *fakeEFS) DescribeMountTargets(_ context.Context, _ *efs.DescribeMountTargetsInput, _ ...func(*efs.Options)) (*efs.DescribeMountTargetsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &efs.DescribeMountTargetsOutput{MountTargets: f.targets}, nil
}

func mt(id, subnet, az, ip, state string) efstypes.MountTargetDescription {
	return efstypes.MountTargetDescription{
		MountTargetId:        aws.String(id),
		SubnetId:             aws.String(subnet),
		AvailabilityZoneName: aws.String(az),
		IpAddress:            aws.String(ip),
		LifeCycleState:       efstypes.LifeCycleState(state),
	}
}

// TestEFSMountTargetIPPrefersTheInstanceSubnet is spawn#704's fallback.
//
// Needed because the DNS name is not reliable at first boot: a real run failed
// all six retry attempts over ~60s with "Failed to resolve server", with the
// mount target available in the instance's OWN subnet and general DNS working,
// while mounting by this IP succeeded first try.
//
// Subnet exactness matters beyond correctness: NFS to a mount target in another
// subnet crosses AZs, which bills per GiB and adds latency even when it works.
func TestEFSMountTargetIPPrefersTheInstanceSubnet(t *testing.T) {
	api := &fakeEFS{targets: []efstypes.MountTargetDescription{
		mt("fsmt-b", "subnet-b", "us-east-1b", "172.31.2.50", "available"),
		mt("fsmt-a", "subnet-a", "us-east-1a", "172.31.1.24", "available"),
	}}

	ip, err := efsMountTargetIP(context.Background(), api, "fs-1", "subnet-a", "us-east-1a")
	if err != nil {
		t.Fatalf("efsMountTargetIP: %v", err)
	}
	if ip != "172.31.1.24" {
		t.Errorf("got %q, want the target in the instance's own subnet (172.31.1.24) — "+
			"another subnet means cross-AZ NFS traffic, which bills per GiB", ip)
	}
}

// TestEFSMountTargetIPFallsBackByAZThenAny: subnet, then AZ, then anything
// available. Any available target beats a name that does not resolve — cross-AZ
// NFS costs money, but a workload that cannot see its data costs the whole run.
func TestEFSMountTargetIPFallsBackByAZThenAny(t *testing.T) {
	targets := []efstypes.MountTargetDescription{
		mt("fsmt-b", "subnet-b", "us-east-1b", "172.31.2.50", "available"),
		mt("fsmt-c", "subnet-c", "us-east-1c", "172.31.3.70", "available"),
	}

	// No matching subnet, but the AZ matches subnet-b's.
	ip, err := efsMountTargetIP(context.Background(), &fakeEFS{targets: targets}, "fs-1", "subnet-zzz", "us-east-1b")
	if err != nil {
		t.Fatalf("efsMountTargetIP: %v", err)
	}
	if ip != "172.31.2.50" {
		t.Errorf("got %q, want the same-AZ target 172.31.2.50", ip)
	}

	// Neither subnet nor AZ matches: take any available one.
	ip, err = efsMountTargetIP(context.Background(), &fakeEFS{targets: targets}, "fs-1", "subnet-zzz", "us-west-2a")
	if err != nil {
		t.Fatalf("efsMountTargetIP: %v", err)
	}
	if ip == "" {
		t.Error("returned no IP though two available targets existed; a cross-AZ mount still " +
			"beats a DNS name that does not resolve")
	}
}

// TestEFSMountTargetIPSkipsUnavailableTargets: a creating or deleting mount
// target has no usable IP, and handing one to mount would produce a failure that
// looks like the DNS problem this is meant to work around.
func TestEFSMountTargetIPSkipsUnavailableTargets(t *testing.T) {
	api := &fakeEFS{targets: []efstypes.MountTargetDescription{
		mt("fsmt-a", "subnet-a", "us-east-1a", "172.31.1.24", "creating"),
		mt("fsmt-b", "subnet-b", "us-east-1b", "172.31.2.50", "deleting"),
	}}

	ip, err := efsMountTargetIP(context.Background(), api, "fs-1", "subnet-a", "us-east-1a")
	if err != nil {
		t.Fatalf("efsMountTargetIP: %v", err)
	}
	if ip != "" {
		t.Errorf("returned %q from a non-available mount target", ip)
	}
}

// TestEFSMountTargetIPReturnsEmptyNotAnError: the DNS name works in the steady
// state, so a filesystem with no usable mount target must not fail the launch —
// it just has no fallback.
func TestEFSMountTargetIPReturnsEmptyNotAnError(t *testing.T) {
	ip, err := efsMountTargetIP(context.Background(), &fakeEFS{}, "fs-1", "subnet-a", "us-east-1a")
	if err != nil {
		t.Errorf("no mount targets should not be an error, got %v", err)
	}
	if ip != "" {
		t.Errorf("got %q with no mount targets", ip)
	}

	// An API error IS reported, so a missing IAM grant is visible rather than
	// silently degrading to DNS-only.
	if _, err := efsMountTargetIP(context.Background(),
		&fakeEFS{err: errors.New("AccessDenied: elasticfilesystem:DescribeMountTargets")},
		"fs-1", "subnet-a", "us-east-1a"); err == nil {
		t.Error("an AccessDenied from DescribeMountTargets must be surfaced, not swallowed — " +
			"otherwise a missing IAM grant looks like a filesystem with no mount targets")
	}
}

func TestGetEFSDNSName(t *testing.T) {
	if got := GetEFSDNSName("fs-0cc326d51039a4df6", "us-east-1"); got != "fs-0cc326d51039a4df6.efs.us-east-1.amazonaws.com" {
		t.Errorf("GetEFSDNSName = %q", got)
	}
}
