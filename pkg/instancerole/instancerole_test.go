package instancerole

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// deletable is a role every condition allows deleting: tagged, its profile
// tagged, in no other profile, attached to nothing, two days old, last used
// two days ago.
func deletable() Facts {
	return Facts{
		Name:           "spawn-instance-af541989",
		RoleTags:       map[string]string{ManagedTagKey: ManagedTagValue},
		ProfileExists:  true,
		ProfileTags:    map[string]string{ManagedTagKey: ManagedTagValue},
		ProfileCreated: now.Add(-48 * time.Hour),
		Created:        now.Add(-48 * time.Hour),
		LastUsed:       now.Add(-48 * time.Hour),
	}
}

// Each condition in the issue is required on its own: break exactly one, keep
// the rest deletable, and the role must be kept for that reason.
func TestDecideEachConditionIsRequired(t *testing.T) {
	if ok, why := Decide(deletable(), now); !ok {
		t.Fatalf("the baseline must be deletable, kept: %s", why)
	}
	cases := []struct {
		name   string
		mutate func(*Facts)
		want   string
	}{
		{"in use by an instance", func(f *Facts) { f.InUse = true }, KeepInUse},
		{"younger than the grace", func(f *Facts) { f.Created = now.Add(-Grace + time.Minute) }, KeepYoung},
		{"used within the grace", func(f *Facts) { f.LastUsed = now.Add(-Grace + time.Minute) }, KeepRecentlyUsed},
		{"role untagged", func(f *Facts) { f.RoleTags = map[string]string{} }, KeepUntagged},
		{"role tagged false", func(f *Facts) { f.RoleTags[ManagedTagKey] = "false" }, KeepUntagged},
		{"profile untagged", func(f *Facts) { f.ProfileTags = nil }, KeepProfileUntagged},
		{"profile younger than the grace", func(f *Facts) { f.ProfileCreated = now.Add(-Grace + time.Minute) }, KeepProfileYoung},
		{"profile creation time unknown", func(f *Facts) { f.ProfileCreated = time.Time{} }, KeepProfileYoung},
		{"in another profile", func(f *Facts) { f.OtherProfiles = []string{"someone-elses"} }, KeepOtherProfiles},
		{"creation time unknown", func(f *Facts) { f.Created = time.Time{} }, KeepNoCreateDate},
		{"not the per-task prefix", func(f *Facts) { f.Name = "my-own-role" }, KeepNotPerTask},
		{"the shared spored role", func(f *Facts) { f.Name = SharedRoleName }, KeepShared},
		{"the shared spored profile", func(f *Facts) { f.Name = SharedProfileName }, KeepShared},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := deletable()
			c.mutate(&f)
			ok, why := Decide(f, now)
			if ok {
				t.Fatalf("deleted a role that is %s", c.name)
			}
			if why != c.want {
				t.Errorf("kept for %q, want %q", why, c.want)
			}
		})
	}
}

// A role IAM has never seen used is judged on its age alone, and a role with
// no profile (a prior run deleted the profile and failed on the role) can
// still be finished off.
func TestDecideNeverUsedAndProfileGone(t *testing.T) {
	f := deletable()
	f.LastUsed = time.Time{}
	if ok, why := Decide(f, now); !ok {
		t.Errorf("never-used old role kept: %s", why)
	}
	f = deletable()
	f.ProfileExists, f.ProfileTags, f.ProfileCreated = false, nil, time.Time{}
	if ok, why := Decide(f, now); !ok {
		t.Errorf("role whose profile is already gone kept: %s", why)
	}
}

// --- fakes ---

type fakeRole struct {
	created, lastUsed time.Time
	tags              map[string]string
	profile           bool
	profileTags       map[string]string
	otherProfiles     []string
	inline            []string
	managed           []string
}

type fakeIAM struct {
	roles map[string]*fakeRole
	calls []string
	fail  string // a call name that fails
}

func (f *fakeIAM) rec(call string) error {
	f.calls = append(f.calls, call)
	if f.fail != "" && strings.HasPrefix(call, f.fail) {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeIAM) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "Get") && !strings.HasPrefix(c, "List") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeIAM) ListRoles(_ context.Context, _ *iam.ListRolesInput, _ ...func(*iam.Options)) (*iam.ListRolesOutput, error) {
	_ = f.rec("ListRoles")
	var names []string
	for n := range f.roles {
		names = append(names, n)
	}
	sort.Strings(names)
	out := &iam.ListRolesOutput{}
	for _, n := range names {
		out.Roles = append(out.Roles, iamtypes.Role{RoleName: aws.String(n)})
	}
	return out, nil
}

func tagsOf(m map[string]string) []iamtypes.Tag {
	var out []iamtypes.Tag
	for k, v := range m {
		out = append(out, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func (f *fakeIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	n := aws.ToString(in.RoleName)
	if err := f.rec("GetRole " + n); err != nil {
		return nil, err
	}
	r := f.roles[n]
	role := &iamtypes.Role{RoleName: in.RoleName, CreateDate: aws.Time(r.created), Tags: tagsOf(r.tags)}
	if !r.lastUsed.IsZero() {
		role.RoleLastUsed = &iamtypes.RoleLastUsed{LastUsedDate: aws.Time(r.lastUsed)}
	}
	return &iam.GetRoleOutput{Role: role}, nil
}

func (f *fakeIAM) GetInstanceProfile(_ context.Context, in *iam.GetInstanceProfileInput, _ ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	n := aws.ToString(in.InstanceProfileName)
	_ = f.rec("GetInstanceProfile " + n)
	r := f.roles[n]
	if r == nil || !r.profile {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no profile")}
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
		InstanceProfileName: in.InstanceProfileName, Tags: tagsOf(r.profileTags), CreateDate: aws.Time(r.created),
	}}, nil
}

func (f *fakeIAM) ListInstanceProfilesForRole(_ context.Context, in *iam.ListInstanceProfilesForRoleInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	n := aws.ToString(in.RoleName)
	_ = f.rec("ListInstanceProfilesForRole " + n)
	r := f.roles[n]
	out := &iam.ListInstanceProfilesForRoleOutput{}
	if r.profile {
		out.InstanceProfiles = append(out.InstanceProfiles, iamtypes.InstanceProfile{InstanceProfileName: aws.String(n)})
	}
	for _, o := range r.otherProfiles {
		out.InstanceProfiles = append(out.InstanceProfiles, iamtypes.InstanceProfile{InstanceProfileName: aws.String(o)})
	}
	return out, nil
}

func (f *fakeIAM) ListRolePolicies(_ context.Context, in *iam.ListRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	_ = f.rec("ListRolePolicies")
	return &iam.ListRolePoliciesOutput{PolicyNames: f.roles[aws.ToString(in.RoleName)].inline}, nil
}

func (f *fakeIAM) DeleteRolePolicy(_ context.Context, in *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	return &iam.DeleteRolePolicyOutput{}, f.rec("DeleteRolePolicy " + aws.ToString(in.RoleName) + " " + aws.ToString(in.PolicyName))
}

func (f *fakeIAM) ListAttachedRolePolicies(_ context.Context, in *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	_ = f.rec("ListAttachedRolePolicies")
	out := &iam.ListAttachedRolePoliciesOutput{}
	for _, a := range f.roles[aws.ToString(in.RoleName)].managed {
		out.AttachedPolicies = append(out.AttachedPolicies, iamtypes.AttachedPolicy{PolicyArn: aws.String(a)})
	}
	return out, nil
}

func (f *fakeIAM) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	return &iam.DetachRolePolicyOutput{}, f.rec("DetachRolePolicy " + aws.ToString(in.RoleName) + " " + aws.ToString(in.PolicyArn))
}

func (f *fakeIAM) RemoveRoleFromInstanceProfile(_ context.Context, in *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return &iam.RemoveRoleFromInstanceProfileOutput{}, f.rec("RemoveRoleFromInstanceProfile " + aws.ToString(in.InstanceProfileName))
}

func (f *fakeIAM) DeleteInstanceProfile(_ context.Context, in *iam.DeleteInstanceProfileInput, _ ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	return &iam.DeleteInstanceProfileOutput{}, f.rec("DeleteInstanceProfile " + aws.ToString(in.InstanceProfileName))
}

func (f *fakeIAM) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	return &iam.DeleteRoleOutput{}, f.rec("DeleteRole " + aws.ToString(in.RoleName))
}

func tagged() map[string]string { return map[string]string{ManagedTagKey: ManagedTagValue} }

func oldRole() *fakeRole {
	return &fakeRole{
		created: now.Add(-72 * time.Hour), lastUsed: now.Add(-48 * time.Hour),
		tags: tagged(), profile: true, profileTags: tagged(),
		inline:  []string{"spawn-scoped-policy", "spored-self-management"},
		managed: []string{"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"},
	}
}

// The delete order is the one IAM accepts and the issue specifies: out of the
// profile, inline policies, managed policies, the profile, the role.
func TestDeleteOrder(t *testing.T) {
	f := &fakeIAM{roles: map[string]*fakeRole{"spawn-instance-1": oldRole()}}
	if err := Delete(context.Background(), f, "spawn-instance-1", true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"RemoveRoleFromInstanceProfile spawn-instance-1",
		"DeleteRolePolicy spawn-instance-1 spawn-scoped-policy",
		"DeleteRolePolicy spawn-instance-1 spored-self-management",
		"DetachRolePolicy spawn-instance-1 arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
		"DeleteInstanceProfile spawn-instance-1",
		"DeleteRole spawn-instance-1",
	}
	if got := f.mutations(); !reflect.DeepEqual(got, want) {
		t.Errorf("delete order:\n got %q\nwant %q", got, want)
	}
}

// A failure stops the delete: a role is never stripped while still in its
// profile, and the role is never deleted after its profile failed to go.
func TestDeleteStopsAtTheFirstFailure(t *testing.T) {
	for _, step := range []string{"RemoveRoleFromInstanceProfile", "DeleteRolePolicy", "DetachRolePolicy", "DeleteInstanceProfile"} {
		f := &fakeIAM{roles: map[string]*fakeRole{"spawn-instance-1": oldRole()}, fail: step}
		if err := Delete(context.Background(), f, "spawn-instance-1", true); err == nil {
			t.Errorf("%s failed but Delete returned nil", step)
		}
		m := f.mutations()
		if last := m[len(m)-1]; !strings.HasPrefix(last, step) {
			t.Errorf("after %s failed, Delete went on to %q", step, last)
		}
	}
}

// Report mode (reap=false) changes nothing and says what reap would delete;
// reap deletes exactly the qualifying role and touches no other.
func TestSweepReportVersusReap(t *testing.T) {
	roles := func() map[string]*fakeRole {
		young := oldRole()
		young.created = now.Add(-time.Hour)
		used := oldRole()
		used.lastUsed = now.Add(-time.Hour)
		untagged := oldRole()
		untagged.tags = map[string]string{}
		shared := oldRole()
		return map[string]*fakeRole{
			"spawn-instance-gone":     oldRole(),
			"spawn-instance-inuse":    oldRole(),
			"spawn-instance-young":    young,
			"spawn-instance-used":     used,
			"spawn-instance-untagged": untagged,
			SharedRoleName:            shared,
			"customer-role":           oldRole(),
		}
	}
	inUse := map[string]bool{"spawn-instance-inuse": true}

	rep := &fakeIAM{roles: roles()}
	res, err := Sweep(context.Background(), rep, inUse, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if m := rep.mutations(); len(m) != 0 {
		t.Errorf("report mode made changes: %q", m)
	}
	scanned, deleted, would, skipped := res.Count()
	if scanned != 5 || deleted != 0 || would != 1 || skipped != 4 {
		t.Errorf("report counts scanned=%d deleted=%d would=%d skipped=%d, want 5/0/1/4", scanned, deleted, would, skipped)
	}

	reap := &fakeIAM{roles: roles()}
	res, err = Sweep(context.Background(), reap, inUse, now, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range reap.mutations() {
		if !strings.HasSuffix(strings.Fields(m)[1], "-gone") {
			t.Errorf("reap touched a role it must keep: %s", m)
		}
	}
	for _, c := range reap.calls {
		if strings.Contains(c, SharedRoleName) || strings.Contains(c, "customer-role") {
			t.Errorf("the sweep even looked at %q, which is not a per-task role", c)
		}
	}
	if _, deleted, _, _ := res.Count(); deleted != 1 {
		t.Errorf("reap deleted %d roles, want 1", deleted)
	}
}

type fakeEC2 struct {
	in   *ec2.DescribeInstancesInput
	arns []string
	err  error
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	var insts []ec2types.Instance
	for _, a := range f.arns {
		insts = append(insts, ec2types.Instance{IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: aws.String(a)}})
	}
	insts = append(insts, ec2types.Instance{}) // no profile at all
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: insts}}}, nil
}

// Every not-yet-terminated state counts as "in use" (a stopped instance needs
// its role when it starts again), profiles are read from the response rather
// than an ARN filter, and one unreadable region fails the whole answer.
func TestProfilesInUse(t *testing.T) {
	east := &fakeEC2{arns: []string{"arn:aws:iam::111122223333:instance-profile/spawn-instance-aa"}}
	west := &fakeEC2{arns: []string{"arn:aws:iam::111122223333:instance-profile/some/path/spawn-instance-bb", "arn:aws:iam::111122223333:instance-profile/spored-instance-profile"}}
	got, err := ProfilesInUse(context.Background(), map[string]EC2Describer{"us-east-1": east, "us-west-2": west})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]bool{"spawn-instance-aa": true, "spawn-instance-bb": true}) {
		t.Errorf("in use = %v", got)
	}
	for _, flt := range east.in.Filters {
		if aws.ToString(flt.Name) != "instance-state-name" {
			t.Errorf("filters on %s; only the state may be filtered", aws.ToString(flt.Name))
			continue
		}
		states := append([]string(nil), flt.Values...)
		sort.Strings(states)
		if want := []string{"pending", "running", "shutting-down", "stopped", "stopping"}; !reflect.DeepEqual(states, want) {
			t.Errorf("states %v, want %v", states, want)
		}
	}

	broken := &fakeEC2{err: errors.New("throttled")}
	if _, err := ProfilesInUse(context.Background(), map[string]EC2Describer{"us-east-1": east, "eu-west-1": broken}); err == nil {
		t.Error("a region that could not be read must fail the answer, or a role used there looks unused")
	}
	if _, err := ProfilesInUse(context.Background(), nil); err == nil {
		t.Error("no regions must be an error, not an empty in-use set")
	}
}

type fakeRegionLister struct {
	in  *ec2.DescribeRegionsInput
	out []string
}

func (f *fakeRegionLister) DescribeRegions(_ context.Context, in *ec2.DescribeRegionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	f.in = in
	out := &ec2.DescribeRegionsOutput{}
	for _, r := range f.out {
		out.Regions = append(out.Regions, ec2types.Region{RegionName: aws.String(r)})
	}
	return out, nil
}

// The in-use check spans the account's enabled regions (AllRegions=false:
// default plus opted-in), with the caller's extra regions merged in; an empty
// answer is an error, never "no regions, so nothing is in use".
func TestEnabledRegions(t *testing.T) {
	l := &fakeRegionLister{out: []string{"us-east-1", "ap-south-1"}}
	got, err := EnabledRegions(context.Background(), l, "eu-west-1", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ap-south-1", "eu-west-1", "us-east-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("regions %v, want %v", got, want)
	}
	if l.in.AllRegions == nil || *l.in.AllRegions {
		t.Error("DescribeRegions must be called with AllRegions=false")
	}
	if _, err := EnabledRegions(context.Background(), &fakeRegionLister{}, "us-east-1"); err == nil {
		t.Error("an empty region list must be an error even with extra regions")
	}
}

// poolRole is a pool worker role every condition allows deleting, named name
// and tagged with runID.
func poolRole(runID string) *fakeRole {
	r := oldRole()
	r.tags[PoolRunIDTagKey] = runID
	r.profileTags[PoolRunIDTagKey] = runID
	return r
}

// A pool's role (#1319) is a per-task role: once no instance holds it and it
// has been unused for the grace, the default rule (the reaper's) deletes it,
// whether or not anyone ran `spawn pool drain`. Its run-id tag must hash to
// its name, so a hand-made role sharing the prefix is kept.
func TestDecidePoolWorkerRoles(t *testing.T) {
	name := PoolWorkerRoleName("nf-run-7")
	if !strings.HasPrefix(name, NamePrefix) || !IsCandidate(name) {
		t.Fatalf("pool role %q is outside %s*, which is all the reaper may touch", name, NamePrefix)
	}
	f := deletable()
	f.Name = name
	f.RoleTags[PoolRunIDTagKey] = "nf-run-7"
	if ok, why := Decide(f, now); !ok {
		t.Fatalf("abandoned pool role kept: %s", why)
	}
	for _, c := range []struct {
		name   string
		mutate func(*Facts)
		want   string
	}{
		{"its workers are live", func(f *Facts) { f.InUse = true }, KeepInUse},
		{"used within the grace", func(f *Facts) { f.LastUsed = now.Add(-time.Hour) }, KeepRecentlyUsed},
		{"no run-id tag", func(f *Facts) { delete(f.RoleTags, PoolRunIDTagKey) }, KeepPoolUnmatched},
		{"another pool's run id", func(f *Facts) { f.RoleTags[PoolRunIDTagKey] = "nf-run-8" }, KeepPoolUnmatched},
		{"not tagged spawn:managed", func(f *Facts) { delete(f.RoleTags, ManagedTagKey) }, KeepUntagged},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := f
			g.RoleTags = map[string]string{}
			for k, v := range f.RoleTags {
				g.RoleTags[k] = v
			}
			c.mutate(&g)
			if ok, why := Decide(g, now); ok || why != c.want {
				t.Errorf("Decide = %v, %q; want kept for %q", ok, why, c.want)
			}
		})
	}
}

// Pre-#1319 spawn-pool-worker-<hash> roles are outside the reaper's grants:
// the default rule never considers them, and the CLI's opt-in considers only
// the exact old shape whose run-id tag hashes to the name.
func TestDecideLegacyPoolWorkerRoles(t *testing.T) {
	name := LegacyPoolWorkerRoleName("nf-run-7")
	if IsCandidate(name) {
		t.Fatalf("legacy name %q must not be a default candidate (the reaper cannot read it)", name)
	}
	f := deletable()
	f.Name = name
	f.RoleTags[PoolRunIDTagKey] = "nf-run-7"
	if ok, why := Decide(f, now); ok || why != KeepNotPerTask {
		t.Errorf("default Decide on a legacy pool role = %v, %q; want kept, %q", ok, why, KeepNotPerTask)
	}
	legacy := Options{LegacyPoolWorkers: true}
	if ok, why := DecideWith(f, now, legacy); !ok {
		t.Errorf("legacy pool role kept under LegacyPoolWorkers: %s", why)
	}
	g := f
	g.RoleTags = map[string]string{ManagedTagKey: ManagedTagValue, PoolRunIDTagKey: "other"}
	if ok, why := DecideWith(g, now, legacy); ok || why != KeepPoolUnmatched {
		t.Errorf("legacy role with a mismatched run id = %v, %q; want kept, %q", ok, why, KeepPoolUnmatched)
	}
	g = f
	g.InUse = true
	if ok, _ := DecideWith(g, now, legacy); ok {
		t.Error("deleted a legacy pool role an instance still holds")
	}
	// Exact shape only: nothing else with the old prefix is ever considered.
	for _, n := range []string{
		LegacyPoolWorkerPrefix + "custom",
		name + "0",
		name[:len(name)-1],
		strings.ToUpper(name[:len(LegacyPoolWorkerPrefix)]) + name[len(LegacyPoolWorkerPrefix):],
		LegacyPoolWorkerPrefix + "0123456789ABCDEF",
		"x" + name,
	} {
		if IsLegacyPoolWorker(n) {
			t.Errorf("IsLegacyPoolWorker(%q) = true", n)
		}
		h := f
		h.Name = n
		if ok, _ := DecideWith(h, now, legacy); ok {
			t.Errorf("DecideWith deleted %q", n)
		}
	}
}

// The sweep a reaper runs (Sweep) deletes an abandoned pool's role and never
// even reads a legacy one; the CLI's SweepWith(LegacyPoolWorkers) deletes both
// and still keeps a legacy role that a live instance holds.
func TestSweepPoolWorkerRoles(t *testing.T) {
	roles := func() map[string]*fakeRole {
		return map[string]*fakeRole{
			PoolWorkerRoleName("a"):       poolRole("a"),
			LegacyPoolWorkerRoleName("b"): poolRole("b"),
			LegacyPoolWorkerRoleName("c"): poolRole("c"),
		}
	}
	east := &fakeEC2{arns: []string{"arn:aws:iam::111122223333:instance-profile/" + LegacyPoolWorkerRoleName("c")}}
	inUse, err := ProfilesInUse(context.Background(), map[string]EC2Describer{"us-east-1": east})
	if err != nil {
		t.Fatal(err)
	}

	def := &fakeIAM{roles: roles()}
	if _, err := Sweep(context.Background(), def, inUse, now, true); err != nil {
		t.Fatal(err)
	}
	for _, c := range def.calls {
		if strings.Contains(c, LegacyPoolWorkerPrefix) {
			t.Errorf("the default sweep touched a legacy pool role (outside the reaper's grants): %s", c)
		}
	}
	if !slices.Contains(def.mutations(), "DeleteRole "+PoolWorkerRoleName("a")) {
		t.Errorf("the default sweep did not delete the abandoned pool's role: %q", def.mutations())
	}

	cli := &fakeIAM{roles: roles()}
	if _, err := SweepWith(context.Background(), cli, inUse, now, true, Options{LegacyPoolWorkers: true}); err != nil {
		t.Fatal(err)
	}
	m := cli.mutations()
	for _, want := range []string{"DeleteRole " + PoolWorkerRoleName("a"), "DeleteRole " + LegacyPoolWorkerRoleName("b")} {
		if !slices.Contains(m, want) {
			t.Errorf("legacy-aware sweep missed %q: %q", want, m)
		}
	}
	for _, c := range m {
		if strings.Contains(c, LegacyPoolWorkerRoleName("c")) {
			t.Errorf("deleted a legacy pool role a live instance holds: %s", c)
		}
	}
}
