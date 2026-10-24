package aws

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeOrphanEC2 answers DescribeAddresses from addrs and DescribeInstances from
// states (an ID absent from states is one EC2 no longer knows), counting calls.
type fakeOrphanEC2 struct {
	addrs             []ec2types.Address
	states            map[string]ec2types.InstanceStateName
	addrErr, instErr  error
	addrCalls, dCalls int
	askedIDs          []string
}

func (f *fakeOrphanEC2) DescribeAddresses(_ context.Context, _ *ec2.DescribeAddressesInput, _ ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	f.addrCalls++
	if f.addrErr != nil {
		return nil, f.addrErr
	}
	return &ec2.DescribeAddressesOutput{Addresses: f.addrs}, nil
}

func (f *fakeOrphanEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.dCalls++
	if f.instErr != nil {
		return nil, f.instErr
	}
	var insts []ec2types.Instance
	for _, flt := range in.Filters {
		if aws.ToString(flt.Name) != "instance-id" {
			continue
		}
		for _, id := range flt.Values {
			f.askedIDs = append(f.askedIDs, id)
			if st, ok := f.states[id]; ok {
				insts = append(insts, ec2types.Instance{InstanceId: aws.String(id), State: &ec2types.InstanceState{Name: st}})
			}
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: insts}}}, nil
}

func launchEIP(alloc, instanceID string) ec2types.Address {
	return ec2types.Address{
		AllocationId: aws.String(alloc),
		PublicIp:     aws.String("198.51.100.1"),
		Tags: []ec2types.Tag{
			{Key: aws.String("spawn:managed"), Value: aws.String("true")},
			{Key: aws.String(ElasticIPInstanceTagKey), Value: aws.String(instanceID)},
			{Key: aws.String("spawn:purpose"), Value: aws.String("efa-public-ip")},
		},
	}
}

func allocIDs(rs []ManagedResource) []string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// #1232: an orphaned launch Elastic IP is spawn-tagged, attached to nothing,
// and its instance is terminated or gone. Every other address — attached, the
// user's, or one whose instance still exists in any non-terminated state — is
// not, because releasing it would take an address someone still holds.
func TestFindOrphanedSpawnAddresses(t *testing.T) {
	associated := launchEIP("eipalloc-associated", "i-gone2")
	associated.AssociationId = aws.String("eipassoc-1")
	associated.InstanceId = aws.String("i-gone2")
	eniOnly := launchEIP("eipalloc-eni", "i-gone3")
	eniOnly.NetworkInterfaceId = aws.String("eni-1")
	userOwned := ec2types.Address{
		AllocationId: aws.String("eipalloc-user"),
		Tags:         []ec2types.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}},
	}

	f := &fakeOrphanEC2{
		addrs: []ec2types.Address{
			launchEIP("eipalloc-gone", "i-gone"),
			launchEIP("eipalloc-terminated", "i-term"),
			launchEIP("eipalloc-running", "i-run"),
			launchEIP("eipalloc-stopped", "i-stop"),
			launchEIP("eipalloc-pending", "i-pend"),
			launchEIP("eipalloc-shutting", "i-shut"),
			associated, eniOnly, userOwned,
		},
		states: map[string]ec2types.InstanceStateName{
			"i-term": ec2types.InstanceStateNameTerminated,
			"i-run":  ec2types.InstanceStateNameRunning,
			"i-stop": ec2types.InstanceStateNameStopped,
			"i-pend": ec2types.InstanceStateNamePending,
			"i-shut": ec2types.InstanceStateNameShuttingDown,
		},
	}
	got, err := findOrphanedSpawnAddresses(context.Background(), f, "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"eipalloc-gone", "eipalloc-terminated"}; !equalStrings(allocIDs(got), want) {
		t.Errorf("orphans = %v, want %v", allocIDs(got), want)
	}
	for _, r := range got {
		if r.Region != "us-west-2" || !IsOrphanedSpawnAddress(r) {
			t.Errorf("%s: region %q, IsOrphanedSpawnAddress %v", r.ID, r.Region, IsOrphanedSpawnAddress(r))
		}
	}
	// No per-address calls: one DescribeAddresses, one DescribeInstances for
	// every candidate together.
	if f.addrCalls != 1 || f.dCalls != 1 {
		t.Errorf("DescribeAddresses x%d, DescribeInstances x%d; want 1 and 1", f.addrCalls, f.dCalls)
	}
	if len(f.askedIDs) != 6 {
		t.Errorf("asked about %v; want the six unassociated launch addresses' instances only", f.askedIDs)
	}
}

// With no unassociated launch address there is nothing to look up, so the
// warning costs list/status exactly one DescribeAddresses per region.
func TestFindOrphanedSpawnAddresses_NoCandidatesNoInstanceLookup(t *testing.T) {
	a := launchEIP("eipalloc-associated", "i-x")
	a.AssociationId = aws.String("eipassoc-1")
	a.InstanceId = aws.String("i-x")
	f := &fakeOrphanEC2{addrs: []ec2types.Address{a}}
	got, err := findOrphanedSpawnAddresses(context.Background(), f, "us-east-1")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want none", allocIDs(got), err)
	}
	if f.dCalls != 0 {
		t.Errorf("DescribeInstances called %d time(s) with no candidate", f.dCalls)
	}
}

// A failed instance lookup must not turn every candidate into an orphan: the
// definition fails closed, so cleanup can never release on a guess.
func TestFindOrphanedSpawnAddresses_InstanceLookupErrorFailsClosed(t *testing.T) {
	f := &fakeOrphanEC2{addrs: []ec2types.Address{launchEIP("eipalloc-a", "i-a")}, instErr: errors.New("throttled")}
	got, err := findOrphanedSpawnAddresses(context.Background(), f, "us-east-1")
	if err == nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want an error and no orphans", allocIDs(got), err)
	}
	unresolved := ManagedResource{ResourceType: "address", State: "unassociated",
		Tags: map[string]string{"spawn:managed": "true", ElasticIPInstanceTagKey: "i-a"}}
	if IsOrphanedSpawnAddress(unresolved) {
		t.Error("an address whose instance was never looked up counts as orphaned")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
