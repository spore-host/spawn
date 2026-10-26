package main

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/spore-host/spawn/pkg/instancerole"
)

// profileEC2 answers the role sweep's state-only DescribeInstances with one
// instance per profile in profiles, and the instance scan (filtered on
// spawn:managed) with nothing, so only the role sweep sees these instances.
type profileEC2 struct {
	emptyEC2
	profiles []string
	err      error
	calls    int
}

func (f *profileEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	for _, flt := range in.Filters {
		if strings.HasPrefix(aws.ToString(flt.Name), "tag:") {
			return &ec2.DescribeInstancesOutput{}, nil
		}
	}
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var insts []ec2types.Instance
	for _, p := range f.profiles {
		insts = append(insts, ec2types.Instance{IamInstanceProfile: &ec2types.IamInstanceProfile{
			Arn: aws.String("arn:aws:iam::111111111111:instance-profile/" + p),
		}})
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: insts}}}, nil
}

// roleIAM is an account's IAM: roles by name (all tagged spawn:managed=true),
// every call recorded.
type roleIAM struct {
	roles   map[string]time.Time // name -> created
	listErr error
	calls   []string
}

func (f *roleIAM) mutating() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "Get") && !strings.HasPrefix(c, "List") {
			out = append(out, c)
		}
	}
	return out
}

func managedTags() []iamtypes.Tag {
	return []iamtypes.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}}
}

func (f *roleIAM) ListRoles(context.Context, *iam.ListRolesInput, ...func(*iam.Options)) (*iam.ListRolesOutput, error) {
	f.calls = append(f.calls, "ListRoles")
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := &iam.ListRolesOutput{}
	for n := range f.roles {
		out.Roles = append(out.Roles, iamtypes.Role{RoleName: aws.String(n)})
	}
	return out, nil
}

func (f *roleIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.calls = append(f.calls, "GetRole")
	c := f.roles[aws.ToString(in.RoleName)]
	role := &iamtypes.Role{RoleName: in.RoleName, CreateDate: aws.Time(c), Tags: managedTags()}
	// A role under a day old has not been used yet (a launch in progress), so
	// only its age protects it; older ones were last used when created.
	if time.Since(c) >= instancerole.Grace {
		role.RoleLastUsed = &iamtypes.RoleLastUsed{LastUsedDate: aws.Time(c)}
	}
	return &iam.GetRoleOutput{Role: role}, nil
}

func (f *roleIAM) GetInstanceProfile(_ context.Context, in *iam.GetInstanceProfileInput, _ ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	f.calls = append(f.calls, "GetInstanceProfile")
	return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
		InstanceProfileName: in.InstanceProfileName, Tags: managedTags(),
		CreateDate: aws.Time(f.roles[aws.ToString(in.InstanceProfileName)]),
	}}, nil
}

func (f *roleIAM) ListInstanceProfilesForRole(_ context.Context, in *iam.ListInstanceProfilesForRoleInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	f.calls = append(f.calls, "ListInstanceProfilesForRole")
	return &iam.ListInstanceProfilesForRoleOutput{InstanceProfiles: []iamtypes.InstanceProfile{{InstanceProfileName: in.RoleName}}}, nil
}

func (f *roleIAM) ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	f.calls = append(f.calls, "ListRolePolicies")
	return &iam.ListRolePoliciesOutput{PolicyNames: []string{"spawn-scoped-policy"}}, nil
}

func (f *roleIAM) DeleteRolePolicy(_ context.Context, in *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	f.calls = append(f.calls, "DeleteRolePolicy "+aws.ToString(in.RoleName))
	return &iam.DeleteRolePolicyOutput{}, nil
}

func (f *roleIAM) ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	f.calls = append(f.calls, "ListAttachedRolePolicies")
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}

func (f *roleIAM) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	f.calls = append(f.calls, "DetachRolePolicy "+aws.ToString(in.RoleName))
	return &iam.DetachRolePolicyOutput{}, nil
}

func (f *roleIAM) RemoveRoleFromInstanceProfile(_ context.Context, in *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	f.calls = append(f.calls, "RemoveRoleFromInstanceProfile "+aws.ToString(in.RoleName))
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

func (f *roleIAM) DeleteInstanceProfile(_ context.Context, in *iam.DeleteInstanceProfileInput, _ ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	f.calls = append(f.calls, "DeleteInstanceProfile "+aws.ToString(in.InstanceProfileName))
	return &iam.DeleteInstanceProfileOutput{}, nil
}

func (f *roleIAM) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	f.calls = append(f.calls, "DeleteRole "+aws.ToString(in.RoleName))
	return &iam.DeleteRoleOutput{}, nil
}

// fakeRegions is DescribeRegions for an account with regions enabled.
type fakeRegions struct {
	regions []string
	err     error
}

func (f *fakeRegions) DescribeRegions(_ context.Context, in *ec2.DescribeRegionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	if aws.ToBool(in.AllRegions) {
		return nil, errors.New("AllRegions=true would list regions the account cannot use")
	}
	out := &ec2.DescribeRegionsOutput{}
	for _, r := range f.regions {
		out.Regions = append(out.Regions, ec2types.Region{RegionName: aws.String(r)})
	}
	return out, nil
}

// roleReaper is one account that the reaper scans in two regions but that has
// a third enabled, ap-south-1, outside REAPER_REGIONS. spawn-instance-old is
// unused and three days old; spawn-instance-busy is as old but its profile is
// attached to an instance in ap-south-1 only; spawn-instance-new is an hour old.
func roleReaper(mode string, dryRun bool) (*reaper, *roleIAM, map[string]*profileEC2) {
	now := time.Now()
	iamc := &roleIAM{roles: map[string]time.Time{
		"spawn-instance-old":  now.Add(-72 * time.Hour),
		"spawn-instance-busy": now.Add(-72 * time.Hour),
		"spawn-instance-new":  now.Add(-time.Hour),
	}}
	ec2s := map[string]*profileEC2{
		"us-east-1":  {},
		"us-west-2":  {},
		"ap-south-1": {profiles: []string{"spawn-instance-busy"}},
	}
	regs := &fakeRegions{regions: []string{"us-east-1", "us-west-2", "ap-south-1"}}
	r := &reaper{
		regions:       []string{"us-east-1", "us-west-2"},
		maxAge:        defaultMaxAge,
		dryRun:        dryRun,
		netResources:  netResourcesOff,
		spotRequests:  spotRequestsOff,
		elasticIPs:    elasticIPsOff,
		instanceRoles: mode,
		accounts: []account{{
			label: "acct", accountID: "111111111111",
			ec2For: func(region string) ec2API { return ec2s[region] },
			iamFor: func() iamAPI { return iamc },
		}},
	}
	r.accounts[0].regionsFor = func() regionsAPI { return regs }
	return r, iamc, ec2s
}

// reap deletes the unused old role through the reaper's own run, keeps the
// one in use in the other region and the young one; report changes nothing
// and logs what reap would delete; a dry run wins over reap; off never asks.
func TestInstanceRoleSweepModes(t *testing.T) {
	t.Run("reap", func(t *testing.T) {
		r, iamc, _ := roleReaper(instanceRolesReap, false)
		var sum Summary
		out := captureLogs(func() { sum, _ = r.run(context.Background()) })
		for _, m := range iamc.mutating() {
			if !strings.HasSuffix(m, " spawn-instance-old") {
				t.Errorf("reap changed a role it must keep: %s", m)
			}
		}
		if len(iamc.mutating()) == 0 {
			t.Error("reap deleted nothing")
		}
		if sum.InstanceRolesScanned != 3 || sum.InstanceRolesDeleted != 1 || sum.InstanceRolesWouldDelete != 0 || sum.InstanceRolesSkipped != 2 {
			t.Errorf("summary scanned/deleted/would/skipped = %d/%d/%d/%d, want 3/1/0/2",
				sum.InstanceRolesScanned, sum.InstanceRolesDeleted, sum.InstanceRolesWouldDelete, sum.InstanceRolesSkipped)
		}
		if !strings.Contains(out, "DELETED role spawn-instance-old") {
			t.Errorf("no DELETED line:\n%s", out)
		}
	})
	for _, c := range []struct {
		name   string
		mode   string
		dryRun bool
	}{{"report", instanceRolesReport, false}, {"dry run beats reap", instanceRolesReap, true}} {
		t.Run(c.name, func(t *testing.T) {
			r, iamc, _ := roleReaper(c.mode, c.dryRun)
			var sum Summary
			out := captureLogs(func() { sum, _ = r.run(context.Background()) })
			if m := iamc.mutating(); len(m) != 0 {
				t.Errorf("%s changed IAM: %q", c.name, m)
			}
			if sum.InstanceRolesWouldDelete != 1 || sum.InstanceRolesDeleted != 0 {
				t.Errorf("would/deleted = %d/%d, want 1/0", sum.InstanceRolesWouldDelete, sum.InstanceRolesDeleted)
			}
			if !strings.Contains(out, "WOULD delete role spawn-instance-old") || strings.Contains(out, "WOULD delete role spawn-instance-busy") {
				t.Errorf("report must name exactly the role reap would delete:\n%s", out)
			}
		})
	}
	t.Run("off", func(t *testing.T) {
		r, iamc, _ := roleReaper(instanceRolesOff, false)
		_, _ = r.run(context.Background())
		if len(iamc.calls) != 0 {
			t.Errorf("off still called IAM: %q", iamc.calls)
		}
	})
}

// The in-use check covers every region enabled in the account, not only
// REAPER_REGIONS: spawn-instance-busy's only holder is in ap-south-1, which
// the reaper does not scan, and it must be kept. And if the region list itself
// cannot be read, nothing is touched.
func TestInstanceRoleSweepChecksEveryEnabledRegion(t *testing.T) {
	r, iamc, ec2s := roleReaper(instanceRolesReap, false)
	captureLogs(func() { _, _ = r.run(context.Background()) })
	if ec2s["ap-south-1"].calls == 0 {
		t.Error("never looked at ap-south-1, an enabled region outside REAPER_REGIONS")
	}
	for _, m := range iamc.mutating() {
		if strings.HasSuffix(m, "spawn-instance-busy") {
			t.Errorf("deleted a role held by an instance in an unscanned region: %s", m)
		}
	}

	r, iamc, _ = roleReaper(instanceRolesReap, false)
	r.accounts[0].regionsFor = func() regionsAPI { return &fakeRegions{err: errors.New("Throttling")} }
	var sum Summary
	captureLogs(func() { sum, _ = r.run(context.Background()) })
	if len(iamc.calls) != 0 || sum.Errors == 0 {
		t.Errorf("unreadable region list: IAM calls %q, errors %d; want none and an error", iamc.calls, sum.Errors)
	}
}

// One region that cannot be listed means no role in the account is touched,
// not even read: a role used there would look unused.
func TestInstanceRoleSweepFailsClosedOnAnUnreadableRegion(t *testing.T) {
	r, iamc, ec2s := roleReaper(instanceRolesReap, false)
	ec2s["ap-south-1"].err = errors.New("RequestLimitExceeded: slow down")
	var sum Summary
	captureLogs(func() { sum, _ = r.run(context.Background()) })
	if len(iamc.calls) != 0 {
		t.Errorf("swept roles without knowing which are in use: %q", iamc.calls)
	}
	if sum.Errors == 0 {
		t.Error("an unreadable region must count as an error")
	}
}

// A member role predating the #1300 grant is a standing configuration fact,
// counted once per account, not an operational error.
func TestInstanceRoleSweepDeniedIsCountedApart(t *testing.T) {
	r, iamc, _ := roleReaper(instanceRolesReap, false)
	iamc.listErr = apiErr("AccessDenied")
	var sum Summary
	captureLogs(func() { sum, _ = r.run(context.Background()) })
	if sum.InstanceRolesAccountsDenied != 1 || sum.Errors != 0 {
		t.Errorf("denied/errors = %d/%d, want 1/0", sum.InstanceRolesAccountsDenied, sum.Errors)
	}
}

// Unset is report, never reap: this is new deletion behaviour (#1300). A typo
// is report too. And the template's default, which is what an upgraded stack
// gets, says the same.
func TestInstanceRolesDefaultIsReport(t *testing.T) {
	for v, want := range map[string]string{
		"": instanceRolesReport, "reep": instanceRolesReport, "REAP": instanceRolesReap,
		"off": instanceRolesOff, " report ": instanceRolesReport,
	} {
		if got := parseInstanceRoles(v); got != want {
			t.Errorf("parseInstanceRoles(%q) = %q, want %q", v, got, want)
		}
	}
	b, err := os.ReadFile("template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^  InstanceRoles:\n    Type: String\n    Default: '([a-z]*)'`).FindSubmatch(b)
	if m == nil {
		t.Fatal("template.yaml has no InstanceRoles parameter with a quoted default")
	}
	if string(m[1]) != instanceRolesReport {
		t.Errorf("template default InstanceRoles=%q, want report", m[1])
	}
	if !regexp.MustCompile(`(?m)^\s+REAPER_INSTANCE_ROLES: !Ref InstanceRoles\s*$`).Match(b) {
		t.Error("template.yaml does not pass InstanceRoles to the function as REAPER_INSTANCE_ROLES")
	}
}

// The reaper's iamAPI is what pkg/reaperiam's drift gate reads; it must cover
// everything instancerole.Sweep calls, which this assignment proves at compile
// time.
var _ instancerole.API = iamAPI(nil)
