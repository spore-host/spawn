package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// everyStateFleet is one spawn-managed instance in every EC2 instance state.
var everyStateFleet = []aws.InstanceInfo{
	{InstanceID: "i-pending", State: "pending"},
	{InstanceID: "i-running", State: "running"},
	{InstanceID: "i-stopping", State: "stopping"},
	{InstanceID: "i-stopped", State: "stopped"},
	{InstanceID: "i-shutting", State: "shutting-down"},
	{InstanceID: "i-terminated", State: "terminated"},
}

// fakeEC2StateFilter answers a ListInstances state filter the way EC2 would
// for everyStateFleet: "all"/"any" is every state, "" is the shared default
// (pending, running, stopping, stopped: it leaves out shutting-down and
// terminated, as #1167 describes), and anything else is that one state.
func fakeEC2StateFilter(stateFilter string) []aws.InstanceInfo {
	var out []aws.InstanceInfo
	for _, inst := range everyStateFleet {
		switch stateFilter {
		case "all", "any":
			out = append(out, inst)
		case "":
			if inst.State != "shutting-down" && inst.State != "terminated" {
				out = append(out, inst)
			}
		default:
			if inst.State == stateFilter {
				out = append(out, inst)
			}
		}
	}
	return out
}

func stubListForList(t *testing.T) *[]string {
	t.Helper()
	var asked []string
	real := listInstancesForList
	listInstancesForList = func(_ context.Context, _ *aws.Client, _, stateFilter string) ([]aws.InstanceInfo, []string, error) {
		asked = append(asked, stateFilter)
		return fakeEC2StateFilter(stateFilter), nil, nil
	}
	t.Cleanup(func() { listInstancesForList = real })
	return &asked
}

func listedStates(instances []aws.InstanceInfo) map[string]bool {
	got := map[string]bool{}
	for _, inst := range instances {
		got[inst.State] = true
	}
	return got
}

// #1167: `spawn list` with no --state must show a shutting-down instance (it
// still bills and holds quota, sometimes for half an hour), and every other
// state that bills, but not a terminated one.
func TestListDefaultShowsShuttingDown(t *testing.T) {
	for _, flag := range []string{"", "live"} {
		t.Run("state="+flag, func(t *testing.T) {
			asked := stubListForList(t)
			got, _, err := fetchListInstances(context.Background(), nil, "", flag)
			if err != nil {
				t.Fatal(err)
			}
			if len(*asked) != 1 {
				t.Fatalf("expected one listing call through the seam, saw %d", len(*asked))
			}
			states := listedStates(got)
			for _, want := range []string{"pending", "running", "stopping", "stopped", "shutting-down"} {
				if !states[want] {
					t.Errorf("--state %q listing is missing a %s instance (asked EC2 for %q); got %v", flag, want, (*asked)[0], states)
				}
			}
			if states["terminated"] {
				t.Errorf("--state %q listing includes a terminated instance; got %v", flag, states)
			}
		})
	}
}

// "active" is the listing before #1167: the shared default, no shutting-down.
func TestListStateActiveIsTheOldDefault(t *testing.T) {
	asked := stubListForList(t)
	got, _, err := fetchListInstances(context.Background(), nil, "", "active")
	if err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 || (*asked)[0] != "" {
		t.Fatalf("--state active asked EC2 for %q, want the shared default \"\"", *asked)
	}
	if states := listedStates(got); states["shutting-down"] || states["terminated"] {
		t.Errorf("--state active listed %v; want no shutting-down or terminated", states)
	}
}

// The shared ListInstances default is unchanged by #1167: resolveInstance, a
// representative of the callers that act on an instance (terminate, extend,
// connect...), still asks for "", which excludes shutting-down
// (TestInstanceStateFilterValues_DefaultExcludesShuttingDown in pkg/aws).
func TestInstanceLookupKeepsTheSharedStateDefault(t *testing.T) {
	setGlobalRegion(t, "")
	var asked []string
	real := listInstancesForLookup
	listInstancesForLookup = func(_ context.Context, _ *aws.Client, _, stateFilter string) ([]aws.InstanceInfo, error) {
		asked = append(asked, stateFilter)
		return []aws.InstanceInfo{{InstanceID: "i-0abc", State: "running"}}, nil
	}
	t.Cleanup(func() { listInstancesForLookup = real })

	if _, err := resolveInstance(context.Background(), nil, "i-0abc"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "" {
		t.Fatalf("resolveInstance asked for state filters %q, want exactly [\"\"] (the shared default)", asked)
	}
}

func TestShuttingDownNote(t *testing.T) {
	if n := shuttingDownNote(everyStateFleet[:4]); n != "" {
		t.Errorf("note with no shutting-down instance = %q, want empty", n)
	}
	n := shuttingDownNote(everyStateFleet)
	if !strings.Contains(n, "1 instance(s) shutting-down") || !strings.Contains(n, "billed") {
		t.Errorf("note = %q, want a count and that they are still billed", n)
	}
}
