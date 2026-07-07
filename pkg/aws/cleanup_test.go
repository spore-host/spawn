package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// notFoundErr is a smithy APIError with the given code, so isVolumeNotFound /
// isInstanceNotFound (which use errors.As) classify it exactly as the real SDK.
type notFoundErr struct{ code string }

func (e *notFoundErr) Error() string     { return e.code + ": not found" }
func (e *notFoundErr) ErrorCode() string { return e.code }
func (e *notFoundErr) ErrorMessage() string {
	return "not found"
}
func (e *notFoundErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

// fakeVolumeAPI reproduces real EC2's behavior that the substrate emulator does
// NOT: a batched DescribeVolumes fails the WHOLE call with InvalidVolume.NotFound
// if any requested id is absent; a per-id call for an absent id fails likewise.
type fakeVolumeAPI struct {
	states map[string]string // id -> state; absent id == deleted
}

func (f *fakeVolumeAPI) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	for _, id := range in.VolumeIds {
		if _, ok := f.states[id]; !ok {
			return nil, &notFoundErr{code: "InvalidVolume.NotFound"}
		}
	}
	var vols []ec2types.Volume
	for _, id := range in.VolumeIds {
		id := id
		vols = append(vols, ec2types.Volume{VolumeId: &id, State: ec2types.VolumeState(f.states[id])})
	}
	return &ec2.DescribeVolumesOutput{Volumes: vols}, nil
}

// TestEnrichVolumeStatePerID_DeletedDoesNotBlankSurvivors is the #262 regression:
// one already-deleted volume must not poison the batch and blank every other
// volume's state (which then got mis-reported as an orphan). The per-id fallback
// must give live volumes their real state and mark only the gone one "deleted".
func TestEnrichVolumeStatePerID_DeletedDoesNotBlankSurvivors(t *testing.T) {
	c := &Client{}
	api := &fakeVolumeAPI{states: map[string]string{
		"vol-live1": "in-use",
		"vol-live2": "available",
		// vol-gone is absent -> deleted
	}}
	resources := []ManagedResource{
		{ResourceType: "volume", ID: "vol-live1"},
		{ResourceType: "volume", ID: "vol-gone"},
		{ResourceType: "volume", ID: "vol-live2"},
	}
	idx := map[string]int{"vol-live1": 0, "vol-gone": 1, "vol-live2": 2}

	if err := c.enrichVolumeStatePerID(context.Background(), api, resources, idx); err != nil {
		t.Fatalf("enrichVolumeStatePerID: %v", err)
	}
	if resources[0].State != "in-use" {
		t.Errorf("vol-live1 state = %q, want in-use", resources[0].State)
	}
	if resources[1].State != "deleted" {
		t.Errorf("vol-gone state = %q, want deleted", resources[1].State)
	}
	if resources[2].State != "available" {
		t.Errorf("vol-live2 state = %q, want available", resources[2].State)
	}

	// End-to-end: the deleted volume must NOT be an orphan; the available one must.
	if IsLikelyOrphan(resources[1], false) {
		t.Error("deleted volume must not be reported as orphan (#262)")
	}
	if !IsLikelyOrphan(resources[2], false) {
		t.Error("genuinely available volume should still be an orphan")
	}
}

func TestParseARN(t *testing.T) {
	tests := []struct {
		arn      string
		service  string
		resource string
		id       string
	}{
		{"arn:aws:ec2:us-east-1:123456789012:instance/i-0abc", "ec2", "instance", "i-0abc"},
		{"arn:aws:ec2:us-east-1:123456789012:security-group/sg-0abc", "ec2", "security-group", "sg-0abc"},
		{"arn:aws:ec2:us-east-1:123456789012:key-pair/key-0abc", "ec2", "key-pair", "key-0abc"},
		{"arn:aws:ec2:us-east-1:123456789012:volume/vol-0abc", "ec2", "volume", "vol-0abc"},
		{"arn:aws:iam::123456789012:role/spored-role", "iam", "role", "spored-role"},
		{"arn:aws:iam::123456789012:instance-profile/spored-profile", "iam", "instance-profile", "spored-profile"},
		{"arn:aws:dynamodb:us-east-1:123456789012:table/spore-installs", "dynamodb", "table", "spore-installs"},
		{"arn:aws:logs:us-east-1:123456789012:log-group:spawn-foo:*", "logs", "log-group", "spawn-foo:*"},
		{"garbage", "", "", ""},
	}
	for _, tt := range tests {
		svc, rt, id := parseARN(tt.arn)
		if svc != tt.service || rt != tt.resource || id != tt.id {
			t.Errorf("parseARN(%q) = (%q,%q,%q), want (%q,%q,%q)", tt.arn, svc, rt, id, tt.service, tt.resource, tt.id)
		}
	}
}

func TestIsRunningInstance(t *testing.T) {
	cases := []struct {
		r    ManagedResource
		want bool
	}{
		{ManagedResource{ResourceType: "instance", State: "running"}, true},
		{ManagedResource{ResourceType: "instance", State: "pending"}, true},
		{ManagedResource{ResourceType: "instance", State: "stopped"}, false},
		{ManagedResource{ResourceType: "instance", State: "terminated"}, false},
		{ManagedResource{ResourceType: "security-group"}, false},
	}
	for _, c := range cases {
		if got := c.r.IsRunningInstance(); got != c.want {
			t.Errorf("IsRunningInstance(%+v) = %v, want %v", c.r, got, c.want)
		}
	}
}

func TestIsLikelyOrphan(t *testing.T) {
	cases := []struct {
		name       string
		r          ManagedResource
		hasRunning bool
		want       bool
	}{
		{"available volume", ManagedResource{ResourceType: "volume", State: "available"}, true, true},
		{"in-use volume", ManagedResource{ResourceType: "volume", State: "in-use"}, true, false},
		// #262: unknown/deleted volume state must NOT be treated as an orphan —
		// that false positive made 'orphans' report already-deleted volumes.
		{"blank-state volume not orphan", ManagedResource{ResourceType: "volume", State: ""}, true, false},
		{"deleted volume not orphan", ManagedResource{ResourceType: "volume", State: "deleted"}, true, false},
		{"SG with running instance", ManagedResource{ResourceType: "security-group"}, true, false},
		{"SG with no instances", ManagedResource{ResourceType: "security-group"}, false, true},
		{"key-pair no instances", ManagedResource{ResourceType: "key-pair"}, false, true},
		{"iam role no instances", ManagedResource{Service: "iam", ResourceType: "role"}, false, true},
		{"iam role with instances", ManagedResource{Service: "iam", ResourceType: "role"}, true, false},
		{"instance never orphan", ManagedResource{ResourceType: "instance", State: "stopped"}, false, false},
		{"table never orphan", ManagedResource{Service: "dynamodb", ResourceType: "table"}, false, false},
	}
	for _, c := range cases {
		if got := IsLikelyOrphan(c.r, c.hasRunning); got != c.want {
			t.Errorf("%s: IsLikelyOrphan = %v, want %v", c.name, got, c.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// TestClassifyAddresses covers the EIP leak classifier (#262): unassociated and
// stopped-instance EIPs are surfaced; a running-instance EIP and a foreign
// (non-spawn) instance's EIP are not.
func TestClassifyAddresses(t *testing.T) {
	instState := map[string]string{
		"i-stopped": "stopped",
		"i-running": "running",
	}
	addrs := []ec2types.Address{
		{AllocationId: ptr("eipalloc-free"), PublicIp: ptr("1.1.1.1")},                                                            // unassociated
		{AllocationId: ptr("eipalloc-stop"), PublicIp: ptr("2.2.2.2"), InstanceId: ptr("i-stopped"), AssociationId: ptr("a1")},    // stopped spawn instance
		{AllocationId: ptr("eipalloc-run"), PublicIp: ptr("3.3.3.3"), InstanceId: ptr("i-running"), AssociationId: ptr("a2")},     // running spawn instance
		{AllocationId: ptr("eipalloc-foreign"), PublicIp: ptr("4.4.4.4"), InstanceId: ptr("i-notmine"), AssociationId: ptr("a3")}, // not spawn-managed
	}
	got := classifyAddresses(addrs, instState, "us-east-1")

	byID := map[string]ManagedResource{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if _, ok := byID["eipalloc-free"]; !ok {
		t.Error("unassociated EIP should be surfaced")
	}
	if r, ok := byID["eipalloc-stop"]; !ok {
		t.Error("EIP on stopped instance should be surfaced")
	} else if r.State != "assoc:stopped" {
		t.Errorf("stopped EIP state = %q, want assoc:stopped", r.State)
	}
	if _, ok := byID["eipalloc-run"]; ok {
		t.Error("EIP on running instance must NOT be surfaced (legit)")
	}
	if _, ok := byID["eipalloc-foreign"]; ok {
		t.Error("EIP on a non-spawn instance must NOT be surfaced")
	}
	// Every surfaced address is an orphan worth reporting.
	for _, r := range got {
		if !IsLikelyOrphan(r, true) {
			t.Errorf("address %s should be IsLikelyOrphan", r.ID)
		}
	}
}

// TestRemoveResource_RefusesAddress is the core invariant: spawn allocates no
// EIP, so cleanup must never release one — RemoveResource returns an error for an
// address (defense in depth behind the cmd-layer filter).
func TestRemoveResource_RefusesAddress(t *testing.T) {
	c := &Client{}
	err := c.RemoveResource(context.Background(), ManagedResource{
		ResourceType: "address", ID: "eipalloc-abc", PublicIP: "5.5.5.5", Region: "us-east-1",
	})
	if err == nil {
		t.Fatal("RemoveResource on an address must return an error (spawn never releases EIPs)")
	}
	if !strings.Contains(err.Error(), "release-address") {
		t.Errorf("error should point the user at release-address, got: %v", err)
	}
}

func TestDeletionOrder(t *testing.T) {
	in := []ManagedResource{
		{Service: "dynamodb", ResourceType: "table"},
		{Service: "iam", ResourceType: "role"},
		{ResourceType: "instance"},
		{ResourceType: "security-group"},
		{Service: "iam", ResourceType: "instance-profile"},
		{ResourceType: "key-pair"},
	}
	got := DeletionOrder(in)
	// Instance must come before SG before key-pair before instance-profile
	// before role before table.
	wantOrder := []string{"instance", "security-group", "key-pair", "instance-profile", "role", "table"}
	for i, w := range wantOrder {
		if got[i].ResourceType != w {
			t.Errorf("position %d = %q, want %q (order: %+v)", i, got[i].ResourceType, w, resourceTypes(got))
		}
	}
}

func resourceTypes(rs []ManagedResource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ResourceType
	}
	return out
}
