package cmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/spore-host/spawn/pkg/audit"
	spawnaws "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/progress"
	"github.com/spore-host/spawn/pkg/testutil"
)

// #1168: --efa without --mpi gets a shared self-referencing group via
// --efa-group NAME, so several launches of different instance types can form
// one EFA fabric.

func TestValidateEFAGroup(t *testing.T) {
	for _, tc := range []struct {
		name string
		efa  bool
		ok   bool
	}{
		{"", false, true},
		{"", true, true},
		{"fleet-1", true, true},
		{"fleet-1", false, false}, // without --efa it would do nothing useful
		{"-x", true, false},
		{"a b", true, false},
		{"a/b", true, false},
		{strings.Repeat("a", 64), true, false},
	} {
		if err := validateEFAGroup(tc.name, tc.efa); (err == nil) != tc.ok {
			t.Errorf("validateEFAGroup(%q, efa=%v) = %v, want ok=%v", tc.name, tc.efa, err, tc.ok)
		}
	}
}

// Rejected in buildLaunchConfig, i.e. before any AWS resource is created
// (#685's ordering rule).
func TestBuildLaunchConfig_EFAGroupNeedsEFA(t *testing.T) {
	prevEFA, prevGroup, prevType := efaEnabled, efaGroup, instanceType
	t.Cleanup(func() { efaEnabled, efaGroup, instanceType = prevEFA, prevGroup, prevType })
	instanceType = "c8gn.48xlarge"
	efaEnabled, efaGroup = false, "fleet"
	if _, err := buildLaunchConfig(nil); err == nil || !strings.Contains(err.Error(), "--efa") {
		t.Errorf("--efa-group without --efa: err = %v, want a refusal naming --efa", err)
	}
}

type fakeEFAGroupAPI struct {
	created []string
	vpcs    int
}

func (f *fakeEFAGroupAPI) ResolveVPC(context.Context, string, string) (string, error) {
	f.vpcs++
	return "vpc-1", nil
}

func (f *fakeEFAGroupAPI) CreateOrGetEFASecurityGroup(_ context.Context, _, _, name string) (string, error) {
	f.created = append(f.created, name)
	return "sg-efa", nil
}

func quietSteps() (*progress.Progress, *audit.AuditLogger) {
	return progress.NewQuietProgress(), audit.NewLogger(io.Discard, "", "")
}

// The EFA group is ADDED to the groups the launch already has, never put in
// their place: an --mpi launch keeps its spawn-mpi-* group and a user's
// --security-group-ids still apply.
func TestEnsureEFAGroupAddsToExistingGroups(t *testing.T) {
	prog, al := quietSteps()
	f := &fakeEFAGroupAPI{}
	cfg := &spawnaws.LaunchConfig{Region: "us-east-1", SecurityGroupIDs: []string{"sg-user", "sg-mpi"}}
	if err := ensureEFAGroup(context.Background(), f, cfg, "fleet", prog, al); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || f.created[0] != "spawn-efa-fleet" {
		t.Errorf("created %v, want [spawn-efa-fleet]", f.created)
	}
	if got := strings.Join(cfg.SecurityGroupIDs, ","); got != "sg-user,sg-mpi,sg-efa" {
		t.Errorf("SecurityGroupIDs = %s, want sg-user,sg-mpi,sg-efa", got)
	}

	// No name, no call: a plain --efa launch is unchanged.
	f2 := &fakeEFAGroupAPI{}
	cfg2 := &spawnaws.LaunchConfig{Region: "us-east-1"}
	if err := ensureEFAGroup(context.Background(), f2, cfg2, "", prog, al); err != nil {
		t.Fatal(err)
	}
	if f2.vpcs+len(f2.created) != 0 || len(cfg2.SecurityGroupIDs) != 0 {
		t.Errorf("without --efa-group: %d VPC lookups, created %v, groups %v; want none", f2.vpcs, f2.created, cfg2.SecurityGroupIDs)
	}
}

// The launch path itself: ensureSecurityGroup, the step launchWithProgress
// runs, puts spawn-efa-NAME on a non-MPI launch, and two launches (different
// instance types) get the same group.
func TestEnsureSecurityGroupJoinsTheEFAGroupWithoutMPI(t *testing.T) {
	prevMPI, prevGroup := mpiEnabled, efaGroup
	t.Cleanup(func() { mpiEnabled, efaGroup = prevMPI, prevGroup })
	mpiEnabled, efaGroup = false, "fleet-1168"

	env := testutil.SubstrateServer(t)
	c := spawnaws.NewClientFromConfig(env.AWSConfig)
	ec2c := ec2.NewFromConfig(env.AWSConfig)
	vpc, err := ec2c.CreateVpc(context.Background(), &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")})
	if err != nil {
		t.Fatalf("create VPC: %v", err)
	}
	prog, al := quietSteps()

	var ids []string
	for _, it := range []string{"g7e.8xlarge", "c8gn.16xlarge"} {
		cfg := &spawnaws.LaunchConfig{Region: "us-east-1", VPCID: *vpc.Vpc.VpcId, InstanceType: it, EFAEnabled: true}
		if err := ensureSecurityGroup(context.Background(), c, cfg, prog, al); err != nil {
			t.Fatalf("%s: ensureSecurityGroup: %v", it, err)
		}
		if len(cfg.SecurityGroupIDs) != 1 {
			t.Fatalf("%s: groups %v, want exactly the EFA group", it, cfg.SecurityGroupIDs)
		}
		ids = append(ids, cfg.SecurityGroupIDs[0])
	}
	if ids[0] != ids[1] {
		t.Errorf("the two launches got groups %v; one fleet must share one group", ids)
	}
	out, err := ec2c.DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{GroupIds: ids[:1]})
	if err != nil || len(out.SecurityGroups) != 1 {
		t.Fatalf("describe %s: %v", ids[0], err)
	}
	if name := aws.ToString(out.SecurityGroups[0].GroupName); name != "spawn-efa-fleet-1168" {
		t.Errorf("group name %q, want spawn-efa-fleet-1168", name)
	}
}

func TestPreviewSecurityGroupPlanNamesTheEFAGroup(t *testing.T) {
	prevMPI, prevGroup := mpiEnabled, efaGroup
	t.Cleanup(func() { mpiEnabled, efaGroup = prevMPI, prevGroup })
	mpiEnabled, efaGroup = false, "fleet"
	got := previewSecurityGroupPlan(&spawnaws.LaunchConfig{})
	if !strings.Contains(got, "spawn-efa-fleet") || strings.Contains(got, "default security group") {
		t.Errorf("plan %q: want spawn-efa-fleet and no default group (naming any group replaces it)", got)
	}
	got = previewSecurityGroupPlan(&spawnaws.LaunchConfig{SecurityGroupIDs: []string{"sg-1"}})
	if !strings.Contains(got, "explicitly given") || !strings.Contains(got, "spawn-efa-fleet") {
		t.Errorf("plan %q: want both the explicit groups and spawn-efa-fleet", got)
	}
}
