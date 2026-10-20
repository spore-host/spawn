package main

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// launchEIP is a spawn-allocated launch Elastic IP (#661) of the given age,
// tagged for instanceID, unassociated unless assoc is set.
func launchEIP(alloc, instanceID string, age time.Duration, now time.Time, assoc string) ec2types.Address {
	tags := append(createdTag(age, now),
		ec2types.Tag{Key: awssdk.String(tagprefix.Tag("managed")), Value: awssdk.String("true")},
		ec2types.Tag{Key: awssdk.String(tagprefix.Tag("instance-id")), Value: awssdk.String(instanceID)},
		ec2types.Tag{Key: awssdk.String(tagprefix.Tag("purpose")), Value: awssdk.String("efa-public-ip")},
	)
	a := ec2types.Address{AllocationId: awssdk.String(alloc), PublicIp: awssdk.String("198.51.100.1"), Tags: tags}
	if assoc != "" {
		a.AssociationId = awssdk.String(assoc)
		a.NetworkInterfaceId = awssdk.String("eni-1")
		a.InstanceId = awssdk.String(instanceID)
	}
	return a
}

// The eipInstanceTagKey this module computes must be the tag pkg/aws writes.
// The reaper does not import pkg/aws, so the two can drift silently; a drift
// would make every launch Elastic IP invisible to the sweep, and they bill.
func TestEIPInstanceTagMatchesWhatLaunchWrites(t *testing.T) {
	if eipInstanceTagKey != "spawn:instance-id" {
		t.Fatalf("eipInstanceTagKey = %q; pkg/aws.ElasticIPInstanceTagKey is spawn:instance-id", eipInstanceTagKey)
	}
}

// TestEIPSweepReleasesOnlyOrphans: an Elastic IP is released only when it is
// spawn's (managed + instance tag), unassociated, past the grace, and its
// instance is gone. Every other combination is spared.
func TestEIPSweepReleasesOnlyOrphans(t *testing.T) {
	now := time.Now()
	old := eipGrace + time.Hour
	userOwned := ec2types.Address{
		AllocationId: awssdk.String("eipalloc-user"),
		// Old and managed, so only the missing instance tag spares it.
		Tags: append(createdTag(old, now),
			ec2types.Tag{Key: awssdk.String(tagprefix.Tag("managed")), Value: awssdk.String("true")}),
	}
	// Managed and tagged for a gone instance, but not the launch purpose: some
	// other spawn address, which the default-on release must not reach.
	otherPurpose := launchEIP("eipalloc-otherpurpose", "i-gone", old, now, "")
	for i, tg := range otherPurpose.Tags {
		if awssdk.ToString(tg.Key) == tagprefix.Tag("purpose") {
			otherPurpose.Tags[i].Value = awssdk.String("something-else")
		}
	}
	f := &fakeNetAPI{
		instances: []ec2types.Instance{{InstanceId: awssdk.String("i-live")}},
		addrs: []ec2types.Address{
			otherPurpose,
			launchEIP("eipalloc-orphan", "i-gone", old, now, ""),
			launchEIP("eipalloc-live", "i-live", old, now, ""),               // instance still exists
			launchEIP("eipalloc-attached", "i-gone", old, now, "eipassoc-1"), // still associated
			launchEIP("eipalloc-young", "i-gone2", eipGrace-time.Minute, now, ""),
			userOwned, // no instance tag: the user's (#262), never released
		},
	}
	sum, _ := runNetSweep(t, f, false)

	sort.Strings(f.released)
	if strings.Join(f.released, ",") != "eipalloc-orphan" {
		t.Fatalf("released = %v, want only eipalloc-orphan", f.released)
	}
	if sum.NetReaped != 1 {
		t.Errorf("NetReaped = %d, want 1", sum.NetReaped)
	}
}

// #661: unlike security and placement groups, an orphaned launch Elastic IP is
// released in `report`, the default, as well as in `reap`: it is unambiguously
// spawn's, its instance is provably gone, and it bills until someone notices
// (two leaked in the first field use). A dry run still releases nothing, and
// `off` skips the sweep altogether.
func TestEIPSweepReleasesByDefault(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name        string
		dryRun      bool
		mode        string
		wantRelease bool
	}{
		{"report mode (the default)", false, netResourcesReport, true},
		{"unset mode resolves to report", false, parseNetResources(""), true},
		{"reap mode", false, netResourcesReap, true},
		{"dry run beats report", true, netResourcesReport, false},
		{"dry run beats reap", true, netResourcesReap, false},
		{"off skips the sweep", false, netResourcesOff, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeNetAPI{addrs: []ec2types.Address{launchEIP("eipalloc-orphan", "i-gone", eipGrace+time.Hour, now, "")}}
			sum, _ := runNetSweepMode(t, f, tc.dryRun, tc.mode)
			if got := len(f.released) == 1; got != tc.wantRelease {
				t.Fatalf("released %v in %s, want release=%v", f.released, tc.name, tc.wantRelease)
			}
			if tc.wantRelease && sum.NetReaped != 1 {
				t.Errorf("NetReaped = %d, want 1", sum.NetReaped)
			}
			if tc.dryRun && sum.NetSkipped != 1 {
				t.Errorf("NetSkipped = %d, want 1 (the would-release)", sum.NetSkipped)
			}
		})
	}
}

// The security-group and placement-group sweep keeps its staging: report mode
// still deletes no group. The EIP default must not leak into them.
func TestEIPDefaultDoesNotArmTheGroupSweep(t *testing.T) {
	now := time.Now()
	f := &fakeNetAPI{
		sgs:   []ec2types.SecurityGroup{sg("sg-old", "spawn-mpi-x", netResourceGrace+time.Hour, now)},
		addrs: []ec2types.Address{launchEIP("eipalloc-orphan", "i-gone", eipGrace+time.Hour, now, "")},
	}
	runNetSweepMode(t, f, false, netResourcesReport)
	if len(f.deletedSG) != 0 {
		t.Errorf("report mode deleted security groups %v", f.deletedSG)
	}
	if len(f.released) != 1 {
		t.Errorf("report mode released %v, want the orphaned Elastic IP", f.released)
	}
}

// eipTerminateNet is the net-resources client for the reaper's terminate path,
// recording into the same call log as the EC2 client so order is observable.
type eipTerminateNet struct {
	*fakeNetAPI
	calls *[]string
	inUse bool
}

func (e *eipTerminateNet) DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, o ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	*e.calls = append(*e.calls, "describe-addresses")
	return e.fakeNetAPI.DescribeAddresses(ctx, in, o...)
}

func (e *eipTerminateNet) ReleaseAddress(ctx context.Context, in *ec2.ReleaseAddressInput, o ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	*e.calls = append(*e.calls, "release "+awssdk.ToString(in.AllocationId))
	if e.inUse {
		return nil, errors.New("api error InvalidIPAddress.InUse: Address is in use")
	}
	return e.fakeNetAPI.ReleaseAddress(ctx, in, o...)
}

// #661: an instance the reaper terminates is one whose spored failed to end it,
// so nothing on the instance released its launch Elastic IP. The reaper does,
// after the terminate, and only that instance's.
func TestReapTerminateReleasesTheInstancesLaunchElasticIP(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		mode      string
		inUse     bool
		wantCalls string
	}{
		{"default mode", "", false, "terminate,describe-addresses,release eipalloc-mine"},
		{"reap mode", netResourcesReap, false, "terminate,describe-addresses,release eipalloc-mine"},
		// Nondefault VPC: InUse until EC2 detaches it; the sweep finishes it.
		{"still associated", netResourcesReport, true, "terminate,describe-addresses,release eipalloc-mine"},
		{"off", netResourcesOff, false, "terminate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ec2c := &spotEC2{}
			net := &eipTerminateNet{
				fakeNetAPI: &fakeNetAPI{addrs: []ec2types.Address{
					launchEIP("eipalloc-other", "i-other", time.Hour, now, ""),
					launchEIP("eipalloc-mine", "i-spot", time.Hour, now, "eipassoc-1"),
				}},
				calls: &ec2c.calls,
				inUse: tc.inUse,
			}
			r := &reaper{netResources: parseNetResources(tc.mode)}
			acct := account{label: "acct", ec2For: func(string) ec2API { return ec2c }, netFor: func(string) netResourceAPI { return net }}
			if err := r.terminate(context.Background(), acct, candidate{id: "i-spot", region: "us-east-1"}); err != nil {
				t.Fatalf("terminate: %v (an Elastic IP problem must never fail the reap)", err)
			}
			if got := strings.Join(ec2c.calls, ","); got != tc.wantCalls {
				t.Errorf("calls = %s, want %s", got, tc.wantCalls)
			}
		})
	}
}

// A failed release is logged and counted, not fatal to the sweep.
func TestEIPSweepReleaseFailureIsNotFatal(t *testing.T) {
	now := time.Now()
	f := &fakeNetAPI{
		addrs:      []ec2types.Address{launchEIP("eipalloc-orphan", "i-gone", eipGrace+time.Hour, now, "")},
		releaseErr: errors.New("UnauthorizedOperation"),
	}
	sum, _ := runNetSweep(t, f, false)
	if sum.NetReaped != 0 || sum.NetSkipped != 1 {
		t.Errorf("NetReaped/NetSkipped = %d/%d, want 0/1", sum.NetReaped, sum.NetSkipped)
	}
}
