package tasklaunch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// vpcAWS is fakeAWS that also answers the default-VPC question, the way
// *aws.Client does, and counts how often it was asked.
type vpcAWS struct {
	fakeAWS
	defaultVPC string
	hints      []aws.SubnetHint
	err        error
	calls      int
}

func (f *vpcAWS) DefaultVPCOrSubnets(_ context.Context, _ string, max int) (string, []aws.SubnetHint, error) {
	f.calls++
	hints := f.hints
	if len(hints) > max {
		hints = hints[:max]
	}
	return f.defaultVPC, hints, f.err
}

// *aws.Client must satisfy the optional interface Plan asserts for. If the
// method is renamed, the assertion silently fails and no plan checks the
// default VPC any more, which this catches at compile time.
var _ defaultVPCLookup = (*aws.Client)(nil)

func networkSpec(subnet string) *taskproto.TaskSpec {
	return &taskproto.TaskSpec{
		TaskID:    "net-1267",
		Command:   []string{"true"},
		Resources: taskproto.ResourceRequest{InstanceType: "c7g.large"},
		Placement: taskproto.Placement{SubnetID: subnet},
		Lifecycle: taskproto.Lifecycle{TTL: "30m", OnComplete: "terminate"},
	}
}

func networkDeps(a AWS, prov Provisioner) Deps {
	return Deps{
		Finder:      fakeTaskFinder{cands: []taskproto.Candidate{{InstanceType: "c7g.large", Family: "c7g", VCPUs: 2, MemoryGiB: 4, OnDemandPrice: 0.0725}}},
		AWS:         a,
		Provisioner: prov,
	}
}

// The spawn#1267 gate: placement.subnet_id reaches the LaunchConfig that
// Provision receives, through Launch, the entry point both `spawn task run`
// and spore-host-mcp's spawn_task_run call. Before the fix the TaskSpec had no
// such field, so every task launched into the default VPC.
func TestLaunch_SubnetIDReachesLaunchConfig(t *testing.T) {
	prov := &capacityProvisioner{}
	fa := &vpcAWS{}
	if _, err := Launch(context.Background(), networkDeps(fa, prov), networkSpec("subnet-0aaa1111bbbb2222c"), "us-west-2"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(prov.configs) != 1 {
		t.Fatalf("Provision called %d times, want 1", len(prov.configs))
	}
	if got := prov.configs[0].SubnetID; got != "subnet-0aaa1111bbbb2222c" {
		t.Errorf("LaunchConfig.SubnetID = %q, want the spec's placement.subnet_id", got)
	}
	if fa.calls != 0 {
		t.Errorf("asked about the default VPC %d time(s) for a spec that names its subnet", fa.calls)
	}
}

// A spec without placement.subnet_id in a region with no default VPC fails in
// Plan, before anything is created or launched, naming the subnets it could
// use, public-IP ones as the fake orders them (spawn#1267). It used to fail at
// RunInstances.
func TestLaunch_NoDefaultVPCFailsBeforeLaunching(t *testing.T) {
	prov := &capacityProvisioner{}
	fa := &vpcAWS{hints: []aws.SubnetHint{
		{SubnetID: "subnet-0pub", VPCID: "vpc-0project", AvailabilityZone: "us-west-2a", MapPublicIPOnLaunch: true},
		{SubnetID: "subnet-0priv", VPCID: "vpc-0project", AvailabilityZone: "us-west-2b"},
	}}
	_, err := Launch(context.Background(), networkDeps(fa, prov), networkSpec(""), "us-west-2")
	if err == nil {
		t.Fatal("Launch succeeded in a region with no default VPC and no placement.subnet_id")
	}
	for _, want := range []string{"no default VPC in us-west-2", "placement.subnet_id", "subnet-0pub", "maps public IPs", "subnet-0priv"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
	if len(prov.configs) != 0 {
		t.Errorf("Provision was called %d time(s); the launch must stop before it", len(prov.configs))
	}
	if len(fa.profiles) != 0 || len(fa.deleted) != 0 {
		t.Errorf("Launch did account-side work before refusing: profiles=%v deleted=%v", fa.profiles, fa.deleted)
	}
}

// No default VPC and no subnet at all: the error says nothing can launch and
// how to fix that, rather than listing nothing.
func TestPlan_NoDefaultVPCNoSubnets(t *testing.T) {
	_, err := Plan(context.Background(), networkDeps(&vpcAWS{}, nil), networkSpec(""), "us-west-2")
	if err == nil || !strings.Contains(err.Error(), "no default VPC and no subnets in us-west-2") || !strings.Contains(err.Error(), "create-default-vpc") {
		t.Fatalf("err = %v", err)
	}
}

// The Network line says where the task goes in each case Plan can tell apart,
// and a failed EC2 lookup does not block the launch: it never did before.
func TestPlan_NetworkLine(t *testing.T) {
	cases := []struct {
		name   string
		aws    AWS
		subnet string
		want   string
	}{
		{"explicit subnet", &vpcAWS{}, "subnet-0abc", "subnet-0abc (placement.subnet_id)"},
		{"default VPC present", &vpcAWS{defaultVPC: "vpc-0default"}, "", "default VPC vpc-0default"},
		{"lookup failed", &vpcAWS{err: errors.New("throttled")}, "", "could not check it exists: throttled"},
		{"AWS without the lookup", &fakeAWS{}, "", "default VPC (placement.subnet_id not set)"},
		{"no AWS", nil, "", "default VPC (placement.subnet_id not set)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Plan(context.Background(), networkDeps(tc.aws, nil), networkSpec(tc.subnet), "us-west-2")
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !strings.Contains(p.Network, tc.want) {
				t.Errorf("Network = %q, want it to contain %q", p.Network, tc.want)
			}
			var out strings.Builder
			Render(&out, p)
			if !strings.Contains(out.String(), "Network:      "+p.Network) {
				t.Errorf("the dry run does not print the Network line:\n%s", out.String())
			}
		})
	}
}

// taskLaunchConfig is where the field is copied; checked directly as well so a
// failure points at the mapping rather than at Launch.
func TestTaskLaunchConfig_SubnetID(t *testing.T) {
	cfg := taskLaunchConfig(networkSpec("subnet-0abc"), &taskproto.SizeResult{InstanceType: "c7g.large"}, "us-west-2", "p", "w")
	if cfg.SubnetID != "subnet-0abc" {
		t.Errorf("SubnetID = %q", cfg.SubnetID)
	}
}

// Memory prints as it is, not rounded to a whole GiB (#1276): a t4g.nano's
// 0.5 GiB read "0 GiB".
func TestInstanceSizeSummary_FractionalMemory(t *testing.T) {
	for mem, want := range map[float64]string{0.5: "0.5 GiB", 1.5: "1.5 GiB", 64: "64 GiB"} {
		got := instanceSizeSummary(&taskproto.SizeResult{InstanceType: "x", VCPUs: 2, MemoryGiB: mem})
		if !strings.Contains(got, ", "+want) {
			t.Errorf("MemoryGiB %v: summary %q, want it to contain %q", mem, got, want)
		}
	}
}
