package aws

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeWindowsGroupEC2 is one instance with its groups, and the other live
// instances each group is attached to.
type fakeWindowsGroupEC2 struct {
	instanceID string
	state      types.InstanceStateName
	groups     []types.SecurityGroup
	others     map[string]string // group id -> another live instance using it
	// dependencyViolations is how many DeleteSecurityGroup calls fail with
	// DependencyViolation before one succeeds (the ENI detaching).
	dependencyViolations int
	deleted              []string
	deleteCalls          int
}

func (f *fakeWindowsGroupEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if len(in.InstanceIds) == 1 {
		var refs []types.GroupIdentifier
		for _, g := range f.groups {
			refs = append(refs, types.GroupIdentifier{GroupId: g.GroupId})
		}
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{{
			InstanceId: aws.String(f.instanceID), State: &types.InstanceState{Name: f.state}, SecurityGroups: refs,
		}}}}}, nil
	}
	for _, fl := range in.Filters {
		if aws.ToString(fl.Name) == "instance.group-id" {
			if other := f.others[fl.Values[0]]; other != "" {
				return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{{InstanceId: aws.String(other)}}}}}, nil
			}
		}
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (f *fakeWindowsGroupEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	var out []types.SecurityGroup
	for _, g := range f.groups {
		for _, id := range in.GroupIds {
			if id == aws.ToString(g.GroupId) {
				out = append(out, g)
			}
		}
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: out}, nil
}

func (f *fakeWindowsGroupEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	f.deleteCalls++
	if f.dependencyViolations > 0 {
		f.dependencyViolations--
		return nil, errors.New("api error DependencyViolation: resource sg has a dependent object")
	}
	f.deleted = append(f.deleted, aws.ToString(in.GroupId))
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func group(id, purpose string) types.SecurityGroup {
	tags := []types.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}}
	if purpose != "" {
		tags = append(tags, types.Tag{Key: aws.String("spawn:purpose"), Value: aws.String(purpose)})
	}
	return types.SecurityGroup{GroupId: aws.String(id), Tags: tags}
}

func noSleep(context.Context, time.Duration) error { return nil }

// Only the per-instance Windows group is the instance's to delete: not the
// MPI group, not a user's own group (#1623).
func TestWindowsGroupsOfSelectsOnlyTheWindowsGroup(t *testing.T) {
	f := &fakeWindowsGroupEC2{instanceID: "i-win", groups: []types.SecurityGroup{
		group("sg-win", WindowsSecurityGroupPurpose),
		group("sg-mpi", "mpi"),
		{GroupId: aws.String("sg-user")},
	}}
	got, err := windowsGroupsOf(context.Background(), f, "i-win")
	if err != nil || len(got) != 1 || got[0] != "sg-win" {
		t.Fatalf("windowsGroupsOf = %v, %v; want [sg-win]", got, err)
	}
}

// After the instance terminates, its group is deleted, retrying while its
// network interface detaches.
func TestDeleteGroupsAfterTerminate(t *testing.T) {
	f := &fakeWindowsGroupEC2{instanceID: "i-win", state: types.InstanceStateNameTerminated,
		groups: []types.SecurityGroup{group("sg-win", WindowsSecurityGroupPurpose)}, dependencyViolations: 2}
	deleted, err := deleteGroupsAfterTerminate(context.Background(), f, "i-win", []string{"sg-win"}, time.Minute, time.Millisecond, noSleep)
	if err != nil || len(deleted) != 1 || deleted[0] != "sg-win" {
		t.Fatalf("deleted %v, err %v; want [sg-win]", deleted, err)
	}
	if f.deleteCalls != 3 {
		t.Errorf("%d delete calls, want 3 (two DependencyViolation retries)", f.deleteCalls)
	}
}

// A group another live instance still uses (two launches with one --name
// share spawn-windows-<name>) is left for the reaper, not fought over.
func TestDeleteGroupsAfterTerminateLeavesASharedGroup(t *testing.T) {
	f := &fakeWindowsGroupEC2{instanceID: "i-win", state: types.InstanceStateNameTerminated,
		groups: []types.SecurityGroup{group("sg-win", WindowsSecurityGroupPurpose)}, others: map[string]string{"sg-win": "i-twin"}}
	deleted, err := deleteGroupsAfterTerminate(context.Background(), f, "i-win", []string{"sg-win"}, time.Minute, time.Millisecond, noSleep)
	if len(deleted) != 0 || f.deleteCalls != 0 {
		t.Fatalf("deleted %v with %d calls; a group in use by i-twin must be left", deleted, f.deleteCalls)
	}
	if err == nil || !strings.Contains(err.Error(), "i-twin") {
		t.Errorf("error %v does not name the instance still using the group", err)
	}
}

// An instance that never reaches terminated leaves its group, and says so.
func TestDeleteGroupsAfterTerminateWaitsForTermination(t *testing.T) {
	f := &fakeWindowsGroupEC2{instanceID: "i-win", state: types.InstanceStateNameShuttingDown,
		groups: []types.SecurityGroup{group("sg-win", WindowsSecurityGroupPurpose)}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	deleted, err := deleteGroupsAfterTerminate(ctx, f, "i-win", []string{"sg-win"}, 50*time.Millisecond, 10*time.Millisecond, noSleep)
	if err == nil || len(deleted) != 0 || f.deleteCalls != 0 {
		t.Fatalf("deleted %v (%d calls), err %v; nothing may be deleted before the instance is gone", deleted, f.deleteCalls, err)
	}
}

// The TTL reaper, which does not import this package, gives groups with this
// purpose the short per-instance grace; the two literals must agree.
func TestReaperKnowsTheWindowsGroupPurpose(t *testing.T) {
	b, err := os.ReadFile("../../lambda/ttl-reaper/netresources.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := `const windowsGroupPurpose = "` + WindowsSecurityGroupPurpose + `"`; !strings.Contains(string(b), want) {
		t.Errorf("lambda/ttl-reaper/netresources.go has no %s", want)
	}
}
