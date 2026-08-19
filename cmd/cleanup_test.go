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

	running, addresses, alreadyGone, removable := splitCleanupResources(found)

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
	_, _, alreadyGone, removable := splitCleanupResources(found)
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
