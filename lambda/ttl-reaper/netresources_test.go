package main

import (
	"context"
	"errors"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/spawn/pkg/tagprefix"
)

type fakeNetAPI struct {
	instances []ec2types.Instance
	sgs       []ec2types.SecurityGroup
	pgs       []ec2types.PlacementGroup
	deletedSG []string
	deletedPG []string
	sgDelErr  error
	pgDelErr  error
}

func (f *fakeNetAPI) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: f.instances}}}, nil
}
func (f *fakeNetAPI) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: f.sgs}, nil
}
func (f *fakeNetAPI) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	if f.sgDelErr != nil {
		return nil, f.sgDelErr
	}
	f.deletedSG = append(f.deletedSG, awssdk.ToString(in.GroupId))
	return &ec2.DeleteSecurityGroupOutput{}, nil
}
func (f *fakeNetAPI) DescribePlacementGroups(context.Context, *ec2.DescribePlacementGroupsInput, ...func(*ec2.Options)) (*ec2.DescribePlacementGroupsOutput, error) {
	return &ec2.DescribePlacementGroupsOutput{PlacementGroups: f.pgs}, nil
}
func (f *fakeNetAPI) DeletePlacementGroup(_ context.Context, in *ec2.DeletePlacementGroupInput, _ ...func(*ec2.Options)) (*ec2.DeletePlacementGroupOutput, error) {
	if f.pgDelErr != nil {
		return nil, f.pgDelErr
	}
	f.deletedPG = append(f.deletedPG, awssdk.ToString(in.GroupName))
	return &ec2.DeletePlacementGroupOutput{}, nil
}

func createdTag(age time.Duration, now time.Time) []ec2types.Tag {
	return []ec2types.Tag{{
		Key:   awssdk.String(tagprefix.Tag("created")),
		Value: awssdk.String(now.Add(-age).Format(time.RFC3339)),
	}}
}

func sg(id, name string, age time.Duration, now time.Time) ec2types.SecurityGroup {
	return ec2types.SecurityGroup{
		GroupId: awssdk.String(id), GroupName: awssdk.String(name), Tags: createdTag(age, now),
	}
}

func pgrp(name string, age time.Duration, now time.Time) ec2types.PlacementGroup {
	return ec2types.PlacementGroup{
		GroupName: awssdk.String(name), State: ec2types.PlacementGroupStateAvailable,
		Tags: createdTag(age, now),
	}
}

func runNetSweep(t *testing.T, f *fakeNetAPI, dryRun bool) (*Summary, *accountOutcome) {
	t.Helper()
	r := &reaper{dryRun: dryRun}
	acct := account{label: "self", netFor: func(string) netResourceAPI { return f }}
	sum := &Summary{}
	out := &accountOutcome{}
	r.reapNetResourcesRegion(context.Background(), acct, "us-east-1", time.Now(), sum, out)
	return sum, out
}

// TestNetSweepReapsOnlyAfterTheGrace is spawn#685's reaping half.
//
// Nothing reclaimed spawn-managed security groups or placement groups without
// someone running a command: 19 security groups and 9 placement groups were
// found in one region of one account, the oldest three months old.
//
// The grace exists because the MPI security group is reused BY NAME across runs
// (CreateOrGetMPISecurityGroup is a get-or-create, and #659 backfills rules onto
// an existing one), so reaping it eagerly would force a recreate on every repeat
// run of a named array. Seven days makes a weekly re-run free.
func TestNetSweepReapsOnlyAfterTheGrace(t *testing.T) {
	now := time.Now()
	f := &fakeNetAPI{
		sgs: []ec2types.SecurityGroup{
			sg("sg-old", "spawn-mpi-gchp", netResourceGrace+time.Hour, now),
			sg("sg-young", "spawn-mpi-fresh", netResourceGrace-time.Hour, now),
		},
		pgs: []ec2types.PlacementGroup{
			pgrp("spawn-mpi-gchp-us-east-1a", netResourceGrace+time.Hour, now),
			pgrp("spawn-mpi-fresh-us-east-1a", netResourceGrace-time.Hour, now),
		},
	}
	sum, _ := runNetSweep(t, f, false)

	if len(f.deletedSG) != 1 || f.deletedSG[0] != "sg-old" {
		t.Errorf("deleted security groups = %v, want only sg-old; a group inside the grace "+
			"is still reusable by a named array re-run", f.deletedSG)
	}
	if len(f.deletedPG) != 1 || f.deletedPG[0] != "spawn-mpi-gchp-us-east-1a" {
		t.Errorf("deleted placement groups = %v, want only the aged one", f.deletedPG)
	}
	if sum.NetReaped != 2 {
		t.Errorf("NetReaped = %d, want 2", sum.NetReaped)
	}
}

// TestNetSweepSparesReferencedResources: EC2 refuses to delete either resource
// while an instance references it, so attempting one is a failure that alarms for
// nothing. Membership is asked of EC2 rather than inferred, because ANY member
// blocks the delete — including one spawn did not create.
func TestNetSweepSparesReferencedResources(t *testing.T) {
	now := time.Now()
	old := netResourceGrace + 24*time.Hour
	f := &fakeNetAPI{
		instances: []ec2types.Instance{{
			SecurityGroups: []ec2types.GroupIdentifier{{GroupId: awssdk.String("sg-busy")}},
			Placement:      &ec2types.Placement{GroupName: awssdk.String("pg-busy")},
		}},
		sgs: []ec2types.SecurityGroup{sg("sg-busy", "spawn-mpi-live", old, now)},
		pgs: []ec2types.PlacementGroup{pgrp("pg-busy", old, now)},
	}
	runNetSweep(t, f, false)

	if len(f.deletedSG) != 0 {
		t.Errorf("deleted a security group still attached to an instance: %v", f.deletedSG)
	}
	if len(f.deletedPG) != 0 {
		t.Errorf("deleted a placement group that still has members: %v", f.deletedPG)
	}
}

// TestNetSweepSkipsUntimedResources: neither DescribeSecurityGroups nor
// DescribePlacementGroups returns a creation time, so spawn's own tag is the only
// source of age. Without it the sweep must SKIP, not assume old — an untimed
// group could be seconds from being used by a launch in flight.
func TestNetSweepSkipsUntimedResources(t *testing.T) {
	f := &fakeNetAPI{
		sgs: []ec2types.SecurityGroup{{GroupId: awssdk.String("sg-untimed"), GroupName: awssdk.String("spawn-mpi-x")}},
		pgs: []ec2types.PlacementGroup{{GroupName: awssdk.String("pg-untimed"), State: ec2types.PlacementGroupStateAvailable}},
	}
	sum, _ := runNetSweep(t, f, false)

	if len(f.deletedSG) != 0 || len(f.deletedPG) != 0 {
		t.Errorf("deleted a resource with no creation stamp (sg=%v pg=%v) — it could be "+
			"seconds old and about to be used by a launch in flight",
			f.deletedSG, f.deletedPG)
	}
	if sum.NetSkipped != 2 {
		t.Errorf("NetSkipped = %d, want 2 so the skip is visible in the summary", sum.NetSkipped)
	}
}

// TestNetSweepDryRunDeletesNothing: the reaper deploys unarmed and a dry run must
// touch nothing. #650 disarmed the live production reaper by accident; the
// mirror-image mistake deletes real infrastructure.
func TestNetSweepDryRunDeletesNothing(t *testing.T) {
	now := time.Now()
	old := netResourceGrace + time.Hour
	f := &fakeNetAPI{
		sgs: []ec2types.SecurityGroup{sg("sg-old", "spawn-mpi-x", old, now)},
		pgs: []ec2types.PlacementGroup{pgrp("pg-old", old, now)},
	}
	sum, _ := runNetSweep(t, f, true)

	if len(f.deletedSG) != 0 || len(f.deletedPG) != 0 {
		t.Errorf("DRY RUN deleted resources: sg=%v pg=%v", f.deletedSG, f.deletedPG)
	}
	if sum.NetReaped != 0 {
		t.Errorf("NetReaped = %d in a dry run, want 0", sum.NetReaped)
	}
	if sum.NetSkipped != 2 {
		t.Errorf("NetSkipped = %d, want 2 — a dry run's would-reaps must still be counted", sum.NetSkipped)
	}
}

// TestNetSweepNeverTouchesTheDefaultSecurityGroup: the VPC default group is never
// spawn's to delete, and EC2 refuses anyway — but a tagging accident is cheap to
// guard against and expensive to debug.
func TestNetSweepNeverTouchesTheDefaultSecurityGroup(t *testing.T) {
	now := time.Now()
	f := &fakeNetAPI{sgs: []ec2types.SecurityGroup{sg("sg-def", "default", netResourceGrace*10, now)}}
	runNetSweep(t, f, false)
	if len(f.deletedSG) != 0 {
		t.Errorf("attempted to delete the VPC default security group: %v", f.deletedSG)
	}
}

// TestNetSweepToleratesDependencyViolation: a security group can be referenced by
// another group's rule or an ENI, which DescribeInstances does not show. That is
// not an error worth alarming on — the next cycle retries.
func TestNetSweepToleratesDependencyViolation(t *testing.T) {
	now := time.Now()
	f := &fakeNetAPI{
		sgs:      []ec2types.SecurityGroup{sg("sg-dep", "spawn-mpi-x", netResourceGrace+time.Hour, now)},
		sgDelErr: errors.New("DependencyViolation: resource sg-dep has a dependent object"),
	}
	sum, out := runNetSweep(t, f, false)

	if sum.NetReaped != 0 {
		t.Errorf("counted a failed delete as reaped")
	}
	if sum.NetSkipped != 1 {
		t.Errorf("NetSkipped = %d, want 1", sum.NetSkipped)
	}
	if out.errorsToCount() != 0 {
		t.Errorf("a DependencyViolation was recorded as an error (%d); it is an expected "+
			"transient and would alarm on every cycle", out.errorsToCount())
	}
}
