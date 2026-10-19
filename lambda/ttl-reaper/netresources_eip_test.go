package main

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

// launchEIP is a spawn-allocated launch Elastic IP (#661) of the given age,
// tagged for instanceID, unassociated unless assoc is set.
func launchEIP(alloc, instanceID string, age time.Duration, now time.Time, assoc string) ec2types.Address {
	tags := append(createdTag(age, now),
		ec2types.Tag{Key: awssdk.String(tagprefix.Tag("managed")), Value: awssdk.String("true")},
		ec2types.Tag{Key: awssdk.String(tagprefix.Tag("instance-id")), Value: awssdk.String(instanceID)},
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
	f := &fakeNetAPI{
		instances: []ec2types.Instance{{InstanceId: awssdk.String("i-live")}},
		addrs: []ec2types.Address{
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

// Report mode (the default) and a dry run release nothing, as for the other
// net resources: the sweep is staged the same way (#685).
func TestEIPSweepReportOnlyReleasesNothing(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		dryRun bool
		mode   string
	}{
		{"report mode", false, netResourcesReport},
		{"dry run beats reap", true, netResourcesReap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeNetAPI{addrs: []ec2types.Address{launchEIP("eipalloc-orphan", "i-gone", eipGrace+time.Hour, now, "")}}
			sum, _ := runNetSweepMode(t, f, tc.dryRun, tc.mode)
			if len(f.released) != 0 {
				t.Fatalf("released %v in %s", f.released, tc.name)
			}
			if sum.NetSkipped != 1 {
				t.Errorf("NetSkipped = %d, want 1 (the would-release)", sum.NetSkipped)
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
