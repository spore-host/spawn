package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spf13/cobra"
	"github.com/spore-host/spawn/pkg/instancerole"
)

type cleanupRoleEC2 struct{ profiles []string }

func (f cleanupRoleEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	var insts []ec2types.Instance
	for _, p := range f.profiles {
		insts = append(insts, ec2types.Instance{IamInstanceProfile: &ec2types.IamInstanceProfile{
			Arn: awssdk.String("arn:aws:iam::111122223333:instance-profile/" + p)}})
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: insts}}}, nil
}

// cleanupRoleIAM: every listed role is tagged and three days old; calls are
// recorded so a test can see what changed.
type cleanupRoleIAM struct {
	names []string
	calls []string
}

func (f *cleanupRoleIAM) mutating() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "Get") && !strings.HasPrefix(c, "List") {
			out = append(out, c)
		}
	}
	return out
}

func tagsManaged() []iamtypes.Tag {
	return []iamtypes.Tag{{Key: awssdk.String("spawn:managed"), Value: awssdk.String("true")}}
}

var cleanupNow = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)

func (f *cleanupRoleIAM) ListRoles(context.Context, *iam.ListRolesInput, ...func(*iam.Options)) (*iam.ListRolesOutput, error) {
	out := &iam.ListRolesOutput{}
	for _, n := range f.names {
		out.Roles = append(out.Roles, iamtypes.Role{RoleName: awssdk.String(n)})
	}
	return out, nil
}
func (f *cleanupRoleIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.calls = append(f.calls, "GetRole "+awssdk.ToString(in.RoleName))
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: in.RoleName, Tags: tagsManaged(),
		CreateDate: awssdk.Time(cleanupNow.Add(-72 * time.Hour))}}, nil
}
func (f *cleanupRoleIAM) GetInstanceProfile(_ context.Context, in *iam.GetInstanceProfileInput, _ ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{InstanceProfileName: in.InstanceProfileName, Tags: tagsManaged(),
		CreateDate: awssdk.Time(cleanupNow.Add(-72 * time.Hour))}}, nil
}
func (f *cleanupRoleIAM) ListInstanceProfilesForRole(context.Context, *iam.ListInstanceProfilesForRoleInput, ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return &iam.ListInstanceProfilesForRoleOutput{}, nil
}
func (f *cleanupRoleIAM) ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	return &iam.ListRolePoliciesOutput{}, nil
}
func (f *cleanupRoleIAM) DeleteRolePolicy(context.Context, *iam.DeleteRolePolicyInput, ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	return &iam.DeleteRolePolicyOutput{}, nil
}
func (f *cleanupRoleIAM) ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}
func (f *cleanupRoleIAM) DetachRolePolicy(context.Context, *iam.DetachRolePolicyInput, ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	return &iam.DetachRolePolicyOutput{}, nil
}
func (f *cleanupRoleIAM) RemoveRoleFromInstanceProfile(_ context.Context, in *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	f.calls = append(f.calls, "RemoveRoleFromInstanceProfile "+awssdk.ToString(in.InstanceProfileName))
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}
func (f *cleanupRoleIAM) DeleteInstanceProfile(_ context.Context, in *iam.DeleteInstanceProfileInput, _ ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	f.calls = append(f.calls, "DeleteInstanceProfile "+awssdk.ToString(in.InstanceProfileName))
	return &iam.DeleteInstanceProfileOutput{}, nil
}
func (f *cleanupRoleIAM) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	f.calls = append(f.calls, "DeleteRole "+awssdk.ToString(in.RoleName))
	return &iam.DeleteRoleOutput{}, nil
}

func runRolesCleanup(t *testing.T, iamc *cleanupRoleIAM, dryRun bool) (string, error) {
	t.Helper()
	// The cleanup log goes under $HOME/.spawn; keep it out of the real one.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	savedDry, savedYes := cleanupDryRun, cleanupYes
	t.Cleanup(func() { cleanupDryRun, cleanupYes = savedDry, savedYes })
	cleanupDryRun, cleanupYes = dryRun, true
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetErr(&out)
	ec2s := map[string]instancerole.EC2Describer{
		"us-east-1":    cleanupRoleEC2{},
		"eu-central-1": cleanupRoleEC2{profiles: []string{"spawn-instance-busy"}},
	}
	err := runCleanupInstanceRoles(context.Background(), c, iamc, ec2s, cleanupNow)
	return out.String(), err
}

// `spawn cleanup --instance-roles` applies the reaper's rule: an unused old
// role goes, one attached to an instance in another region stays, and the
// shared spored-instance-role is never even read. --dry-run changes nothing.
func TestCleanupInstanceRoles(t *testing.T) {
	roles := []string{"spawn-instance-unused", "spawn-instance-busy", instancerole.SharedRoleName, "my-own-role"}

	dry := &cleanupRoleIAM{names: roles}
	out, err := runRolesCleanup(t, dry, true)
	if err != nil {
		t.Fatal(err)
	}
	if m := dry.mutating(); len(m) != 0 {
		t.Errorf("--dry-run changed IAM: %q", m)
	}
	if !strings.Contains(out, "Dry run: 1 role(s)") {
		t.Errorf("dry run did not say one role would go:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "spawn-instance-busy") && !strings.Contains(line, "keep") {
			t.Errorf("the role in use in another region is not kept: %q", line)
		}
	}

	real := &cleanupRoleIAM{names: roles}
	if _, err := runRolesCleanup(t, real, false); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"RemoveRoleFromInstanceProfile spawn-instance-unused",
		"DeleteInstanceProfile spawn-instance-unused",
		"DeleteRole spawn-instance-unused",
	}
	if got := real.mutating(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("cleanup changed %q, want %q", got, want)
	}
	for _, c := range append(dry.calls, real.calls...) {
		if strings.Contains(c, instancerole.SharedRoleName) || strings.Contains(c, "my-own-role") {
			t.Errorf("cleanup touched %q", c)
		}
	}
}
