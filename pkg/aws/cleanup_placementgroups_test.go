package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakePGAPI scripts DescribePlacementGroups and DescribeInstances, and records
// the filters it was asked for — the filters are the half of this that a
// returned-value assertion cannot check.
type fakePGAPI struct {
	groups    []ec2types.PlacementGroup
	instances []ec2types.Instance
	pgFilters []ec2types.Filter
	inFilters []ec2types.Filter
	pgErr     error
	inErr     error
}

func (f *fakePGAPI) DescribePlacementGroups(_ context.Context, in *ec2.DescribePlacementGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribePlacementGroupsOutput, error) {
	f.pgFilters = in.Filters
	if f.pgErr != nil {
		return nil, f.pgErr
	}
	return &ec2.DescribePlacementGroupsOutput{PlacementGroups: f.groups}, nil
}

func (f *fakePGAPI) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.inFilters = in.Filters
	if f.inErr != nil {
		return nil, f.inErr
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{Instances: f.instances}},
	}, nil
}

func pg(name string) ec2types.PlacementGroup {
	return ec2types.PlacementGroup{
		GroupName: aws.String(name),
		State:     ec2types.PlacementGroupStateAvailable,
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String("spawn:purpose"), Value: aws.String("mpi")},
		},
	}
}

func instIn(group string) ec2types.Instance {
	return ec2types.Instance{Placement: &ec2types.Placement{GroupName: aws.String(group)}}
}

func filterValues(fs []ec2types.Filter, name string) ([]string, bool) {
	for _, f := range fs {
		if aws.ToString(f.Name) == name {
			return f.Values, true
		}
	}
	return nil, false
}

// TestScanPlacementGroupsSeparatesEmptyFromInUse is the core of spawn#685's
// reaping half.
//
// An MPI cohort creates one cluster placement group per AZ it tries, so a cohort
// that fell back from us-east-1a to us-east-1b leaves 1a's group genuinely empty
// while 1b's is live. Reporting the live one as an orphan would send the operator
// after something EC2 will refuse to delete; missing the empty one is the leak
// itself — nine were found in a single region of one account.
func TestScanPlacementGroupsSeparatesEmptyFromInUse(t *testing.T) {
	api := &fakePGAPI{
		groups:    []ec2types.PlacementGroup{pg("spawn-mpi-gchp-us-east-1a"), pg("spawn-mpi-gchp-us-east-1b")},
		instances: []ec2types.Instance{instIn("spawn-mpi-gchp-us-east-1b")},
	}

	got, err := scanPlacementGroupsWith(context.Background(), api, "us-east-1")
	if err != nil {
		t.Fatalf("scanPlacementGroupsWith: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d resources, want 2", len(got))
	}

	state := map[string]string{}
	for _, r := range got {
		state[r.ID] = r.State
		if r.ResourceType != "placement-group" {
			t.Errorf("%s: ResourceType = %q, want placement-group", r.ID, r.ResourceType)
		}
		if r.Region != "us-east-1" {
			t.Errorf("%s: Region = %q; a group deleted in the wrong region is the other "+
				"half of #685, so the region has to be carried through", r.ID, r.Region)
		}
		if r.Tags["spawn:purpose"] != "mpi" {
			t.Errorf("%s: tags not carried through: %v", r.ID, r.Tags)
		}
	}
	if state["spawn-mpi-gchp-us-east-1a"] != "empty" {
		t.Errorf("abandoned-AZ group state = %q, want empty — this is the leak", state["spawn-mpi-gchp-us-east-1a"])
	}
	if state["spawn-mpi-gchp-us-east-1b"] != "in-use" {
		t.Errorf("live group state = %q, want in-use", state["spawn-mpi-gchp-us-east-1b"])
	}

	// An empty group is an orphan; a live one is not. Keyed on the GROUP, not on
	// whether anything is running in the region — the fallback case above has
	// both at once, which is exactly where a region-wide test would be wrong.
	for _, r := range got {
		want := r.ID == "spawn-mpi-gchp-us-east-1a"
		if IsLikelyOrphan(r, true) != want {
			t.Errorf("IsLikelyOrphan(%s, hasRunning=true) = %v, want %v", r.ID, !want, want)
		}
		if IsLikelyOrphan(r, false) != want {
			t.Errorf("IsLikelyOrphan(%s, hasRunning=false) = %v, want %v", r.ID, !want, want)
		}
	}
}

// TestScanPlacementGroupsFiltersToSpawnManaged: the sweep must never offer a
// placement group spawn did not create. Cleanup deletes what this reports.
func TestScanPlacementGroupsFiltersToSpawnManaged(t *testing.T) {
	api := &fakePGAPI{groups: []ec2types.PlacementGroup{pg("spawn-mpi-x-us-east-1a")}}
	if _, err := scanPlacementGroupsWith(context.Background(), api, "us-east-1"); err != nil {
		t.Fatalf("scan: %v", err)
	}

	vals, ok := filterValues(api.pgFilters, "tag:spawn:managed")
	if !ok {
		t.Fatal("DescribePlacementGroups was not filtered by tag:spawn:managed — the sweep " +
			"would offer up groups spawn never created, and cleanup DELETES what it reports")
	}
	if len(vals) != 1 || vals[0] != "true" {
		t.Errorf("tag:spawn:managed values = %v, want [true]", vals)
	}
}

// TestPlacementGroupMembershipIgnoresTerminatedInstances: a terminated instance
// holds nothing, so counting it would mark every group a failed cohort left
// behind as in-use — which is precisely the population that needs reaping.
func TestPlacementGroupMembershipIgnoresTerminatedInstances(t *testing.T) {
	api := &fakePGAPI{groups: []ec2types.PlacementGroup{pg("spawn-mpi-x-us-east-1a")}}
	if _, err := scanPlacementGroupsWith(context.Background(), api, "us-east-1"); err != nil {
		t.Fatalf("scan: %v", err)
	}

	vals, ok := filterValues(api.inFilters, "instance-state-name")
	if !ok {
		t.Fatal("membership query has no instance-state-name filter, so a terminated " +
			"instance counts as a member and every abandoned group reads as in-use")
	}
	for _, v := range vals {
		if v == "terminated" {
			t.Error("instance-state-name includes 'terminated'; a terminated instance holds " +
				"nothing and must not keep a group out of the report")
		}
	}
	// A STOPPED instance does still occupy a group, and cleanup can terminate it
	// — so it must be counted, or cleanup would try the delete and fail.
	found := false
	for _, v := range vals {
		if v == "stopped" {
			found = true
		}
	}
	if !found {
		t.Error("instance-state-name omits 'stopped'; a stopped instance still occupies the " +
			"group, so the delete would be attempted and fail")
	}

	// And the membership query must be scoped to the groups in hand, not the
	// whole account.
	if names, ok := filterValues(api.inFilters, "placement-group-name"); !ok || len(names) != 1 {
		t.Errorf("placement-group-name filter = %v (present=%v), want the one scanned group", names, ok)
	}
}

// TestScanPlacementGroupsSkipsMembershipQueryWhenEmpty: no groups means no
// second API call. DescribeInstances with an empty filter value list is an API
// error, so this is correctness, not just thrift.
func TestScanPlacementGroupsSkipsMembershipQueryWhenEmpty(t *testing.T) {
	api := &fakePGAPI{inErr: errors.New("DescribeInstances should not have been called")}
	got, err := scanPlacementGroupsWith(context.Background(), api, "us-east-1")
	if err != nil {
		t.Fatalf("scan with no groups: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d resources with no groups present", len(got))
	}
}

// TestPlacementGroupDeletionOrderIsAfterInstances: EC2 refuses to delete a group
// with members, so the instances have to go first.
func TestPlacementGroupDeletionOrderIsAfterInstances(t *testing.T) {
	ordered := DeletionOrder([]ManagedResource{
		{ResourceType: "placement-group", ID: "spawn-mpi-x-us-east-1a"},
		{ResourceType: "instance", ID: "i-1", State: "stopped"},
	})
	if ordered[0].ResourceType != "instance" {
		t.Errorf("order = %s, %s; the instance must be terminated before its placement "+
			"group can be deleted", ordered[0].ResourceType, ordered[1].ResourceType)
	}
}
