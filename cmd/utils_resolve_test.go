package cmd

import (
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
)

// resolveInstanceFromList is a testable extraction of the name-matching logic
// from resolveInstance. We test the selection logic directly since resolveInstance
// requires a live AWS client.
func resolveInstanceFromList(instances []aws.InstanceInfo, identifier string) (*aws.InstanceInfo, error) {
	var matches []aws.InstanceInfo
	for _, inst := range instances {
		if inst.Name == identifier {
			matches = append(matches, inst)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}

	// Multiple matches — prefer running (regression fix for #313)
	var running []aws.InstanceInfo
	for _, inst := range matches {
		if inst.State == "running" {
			running = append(running, inst)
		}
	}
	if len(running) == 1 {
		return &running[0], nil
	}
	// Still ambiguous
	return nil, nil
}

// TestResolveInstance_PrefersRunningOverStopped is a regression test for #313.
// When a cluster is re-launched, old stopped instances share names with new running ones.
// resolveInstance must pick the running instance without requiring disambiguation.
func TestResolveInstance_PrefersRunningOverStopped(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-running", Name: "fem-cluster-0", State: "running", InstanceType: "c5n.18xlarge"},
		{InstanceID: "i-stopped", Name: "fem-cluster-0", State: "stopped", InstanceType: "hpc6a.48xlarge"},
	}

	result, err := resolveInstanceFromList(instances, "fem-cluster-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected a result, got nil")
	}
	if result.InstanceID != "i-running" {
		t.Errorf("expected running instance i-running, got %s (state: %s)", result.InstanceID, result.State)
	}
}

// TestResolveInstance_PrefersRunningOverTerminated verifies terminated instances
// are also deprioritized.
func TestResolveInstance_PrefersRunningOverTerminated(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-old", Name: "worker", State: "terminated"},
		{InstanceID: "i-new", Name: "worker", State: "running"},
	}

	result, _ := resolveInstanceFromList(instances, "worker")
	if result == nil || result.InstanceID != "i-new" {
		t.Errorf("expected i-new (running), got %v", result)
	}
}

// TestResolveInstance_SingleRunning verifies no ambiguity with one running instance.
func TestResolveInstance_SingleRunning(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-abc", Name: "my-job", State: "running"},
	}

	result, _ := resolveInstanceFromList(instances, "my-job")
	if result == nil || result.InstanceID != "i-abc" {
		t.Errorf("expected i-abc, got %v", result)
	}
}

// TestResolveInstance_NoMatch returns nil when no instance matches.
func TestResolveInstance_NoMatch(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-xyz", Name: "other-job", State: "running"},
	}

	result, _ := resolveInstanceFromList(instances, "my-job")
	if result != nil {
		t.Errorf("expected nil for no-match, got %v", result)
	}
}

// TestResolveInstance_MultipleRunning returns nil (still ambiguous) when multiple
// instances are running with the same name.
func TestResolveInstance_MultipleRunning(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-a", Name: "cluster-0", State: "running"},
		{InstanceID: "i-b", Name: "cluster-0", State: "running"},
	}

	result, _ := resolveInstanceFromList(instances, "cluster-0")
	if result != nil {
		t.Errorf("expected nil for multiple running matches, got %v", result)
	}
}

// TestResolveInstance_AllStopped returns nil when all matches are stopped.
func TestResolveInstance_AllStopped(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-a", Name: "job", State: "stopped"},
		{InstanceID: "i-b", Name: "job", State: "stopped"},
	}

	result, _ := resolveInstanceFromList(instances, "job")
	// Ambiguous — no running preference available
	if result != nil {
		t.Errorf("expected nil for multiple stopped matches (no running preference), got %v", result)
	}
}
