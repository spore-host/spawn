package cmd

import (
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestSplitCleanupResources_AlreadyGoneIsNotRemovable is the spawn#516
// regression test for cleanup's second compounding defect: a resource whose
// State the discovery/enrichment pipeline has already resolved to "deleted"
// (Resource Groups Tagging API tag-mapping residue for something that no
// longer exists) must land in its own bucket, not in removable. Before the
// fix, removable's switch had no case for State at all — only
// IsRunningInstance() and ResourceType == "address" were consulted, so a
// "deleted" instance or volume fell through to the default case and was
// counted as removable, contradicting its own displayed State.
func TestSplitCleanupResources_AlreadyGoneIsNotRemovable(t *testing.T) {
	found := []aws.ManagedResource{
		{ResourceType: "instance", ID: "i-live", State: "stopped"},
		{ResourceType: "instance", ID: "i-gone", State: "deleted"},
		{ResourceType: "volume", ID: "vol-gone", State: "deleted"},
		{ResourceType: "instance", ID: "i-running", State: "running"},
		{ResourceType: "address", ID: "eipalloc-x", State: "unassociated"},
	}

	running, addresses, alreadyGone, _, removable := splitCleanupResources(found)

	if len(running) != 1 || running[0].ID != "i-running" {
		t.Errorf("running = %v, want [i-running]", idsOf(running))
	}
	if len(addresses) != 1 || addresses[0].ID != "eipalloc-x" {
		t.Errorf("addresses = %v, want [eipalloc-x]", idsOf(addresses))
	}
	if len(alreadyGone) != 2 {
		t.Errorf("alreadyGone = %v, want [i-gone, vol-gone]", idsOf(alreadyGone))
	}
	for _, r := range alreadyGone {
		if r.ID != "i-gone" && r.ID != "vol-gone" {
			t.Errorf("unexpected resource in alreadyGone: %s", r.ID)
		}
	}
	if len(removable) != 1 || removable[0].ID != "i-live" {
		t.Errorf("removable = %v, want [i-live] (deleted resources must NOT be removable)", idsOf(removable))
	}
}

// TestSplitCleanupResources_NoStateIsStillRemovable confirms the fix doesn't
// over-correct: a resource with no resolved State yet (State == "", meaning
// unknown/unresolved rather than confirmed-gone) still goes to removable —
// only an explicit "deleted" is excluded.
func TestSplitCleanupResources_NoStateIsStillRemovable(t *testing.T) {
	found := []aws.ManagedResource{
		{ResourceType: "security-group", ID: "sg-x", State: ""},
	}
	_, _, alreadyGone, _, removable := splitCleanupResources(found)
	if len(alreadyGone) != 0 {
		t.Errorf("alreadyGone = %v, want empty for a resource with no State opinion", idsOf(alreadyGone))
	}
	if len(removable) != 1 {
		t.Errorf("removable = %v, want [sg-x]", idsOf(removable))
	}
}

func idsOf(rs []aws.ManagedResource) []string {
	ids := make([]string, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	return ids
}

// TestSplitCleanupResources_InUsePlacementGroupIsSkippedNotBlocking is the
// spawn#685 cleanup gate, and it guards against two opposite mistakes.
//
// EC2 refuses to delete a placement group that still has members, so offering
// one up is a wait that ends in a failure — it must not be `removable`. But it
// must also not land in `running`, because that bucket ABORTS the whole cleanup:
// a group whose only members are STOPPED instances would then block cleanup from
// terminating those very instances, and the group would never become deletable.
// Deadlock in one direction, pointless failure in the other.
func TestSplitCleanupResources_InUsePlacementGroupIsSkippedNotBlocking(t *testing.T) {
	found := []aws.ManagedResource{
		{ResourceType: "placement-group", ID: "spawn-mpi-x-us-east-1a", State: "empty"},
		{ResourceType: "placement-group", ID: "spawn-mpi-x-us-east-1b", State: "in-use"},
		{ResourceType: "instance", ID: "i-stopped", State: "stopped"},
	}

	running, _, _, notYet, removable := splitCleanupResources(found)

	if len(running) != 0 {
		t.Errorf("running = %v, want empty — an in-use placement group must not abort "+
			"cleanup, or a group holding only stopped instances deadlocks against the "+
			"terminate that would free it", idsOf(running))
	}
	if len(notYet) != 1 || notYet[0].ID != "spawn-mpi-x-us-east-1b" {
		t.Errorf("notYet = %v, want [spawn-mpi-x-us-east-1b]", idsOf(notYet))
	}
	// The empty group AND the stopped instance are both removable.
	if len(removable) != 2 {
		t.Errorf("removable = %v, want the empty group and the stopped instance", idsOf(removable))
	}
	for _, r := range removable {
		if r.ResourceType == "placement-group" && r.State == "in-use" {
			t.Error("an in-use placement group reached removable; the delete would wait out " +
				"its budget and then fail")
		}
	}
}
