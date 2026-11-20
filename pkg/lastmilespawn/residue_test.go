package lastmilespawn

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/tags"

	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/reaperdeploy"
	"github.com/spore-host/spawn/pkg/teardown"
)

var regions = []string{"us-east-1", "us-west-2"}

func managed() map[string]string { return map[string]string{tags.ManagedBy: string(tags.ToolSpawn)} }

func TestResidueIsTheReapersTeardownLeftovers(t *testing.T) {
	res := Residue(testAccount, regions)
	role := reaperdeploy.TeardownInvokeRoleARN(testAccount)
	want := map[string]string{role: fs.RegionGlobal}
	for _, r := range regions {
		want[reaperdeploy.TeardownScheduleGroupARN(r, testAccount)] = r
	}
	got := map[string]string{}
	for _, r := range res {
		got[r.ARN] = r.Region
		if ok, why := footprint.Deletable(r, tags.ToolSpawn, Patterns()); !ok {
			t.Errorf("%s would be kept by spawn's own guard: %s", r.ARN, why)
		}
		if tags.IsService(r.Tags) || r.Tags[tags.Role] != "" {
			t.Errorf("%s carries %s: the residue is never the service", r.ARN, tags.Role)
		}
		if r.Name == lifecycle.TableName {
			t.Errorf("%s: the suite's table is lastmile's to report, not spawn's residue", r.ARN)
		}
		switch r.Type {
		case TypeScheduleGroup:
			if !reflect.DeepEqual(r.DependsOn, []string{role}) {
				t.Errorf("%s DependsOn %v, want the invoke role, so a group still holding a job keeps it", r.ARN, r.DependsOn)
			}
		case fs.TypeRole:
			if r.Name != "spawn-reaper-teardown-invoke" {
				t.Errorf("role %q", r.Name)
			}
		default:
			t.Errorf("unexpected residue type %s", r.Type)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Residue = %v, want %v", got, want)
	}
	// Patterns are spawn's teardown patterns plus the residue's two names,
	// and nothing else spawn's teardown does not register.
	p := Patterns()
	if len(p) != len(fs.Patterns)+2 || !reflect.DeepEqual(p[:len(fs.Patterns)], fs.Patterns) {
		t.Errorf("Patterns = %d entries, want footprintspawn.Patterns and two", len(p))
	}
	other := footprint.Resource{Type: fs.TypeRole, Name: "spawn-reaper-teardown-invoke-2", Ownership: footprint.OwnershipTagged,
		Class: footprint.ClassResidue, Tags: managed()}
	if ok, _ := footprint.Deletable(other, tags.ToolSpawn, p); ok {
		t.Error("the residue pattern matches more than the invoke role's exact name")
	}
}

// ---- fakes -----------------------------------------------------------------

type iamFake struct {
	mu    sync.Mutex
	roles map[string]map[string]string // name -> tags
	err   map[string]error
	n     int
}

func (f *iamFake) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	name := aws.ToString(in.RoleName)
	if err := f.err[name]; err != nil {
		return nil, err
	}
	t, ok := f.roles[name]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no role " + name)}
	}
	var ts []iamtypes.Tag
	for k, v := range t {
		ts = append(ts, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: aws.String(name), Tags: ts}}, nil
}

type schedFake struct {
	mu      sync.Mutex
	exists  bool
	tags    map[string]string
	jobs    []string
	getErr  error
	tagErr  error
	listErr error
	n       int
}

func (f *schedFake) count() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
}

func (f *schedFake) GetScheduleGroup(_ context.Context, in *scheduler.GetScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.GetScheduleGroupOutput, error) {
	f.count()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if !f.exists || aws.ToString(in.Name) != reaperdeploy.TeardownScheduleGroup {
		return nil, &schedulertypes.ResourceNotFoundException{Message: aws.String("no group")}
	}
	return &scheduler.GetScheduleGroupOutput{Name: in.Name}, nil
}

func (f *schedFake) ListTagsForResource(context.Context, *scheduler.ListTagsForResourceInput, ...func(*scheduler.Options)) (*scheduler.ListTagsForResourceOutput, error) {
	f.count()
	if f.tagErr != nil {
		return nil, f.tagErr
	}
	var ts []schedulertypes.Tag
	for k, v := range f.tags {
		ts = append(ts, schedulertypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return &scheduler.ListTagsForResourceOutput{Tags: ts}, nil
}

func (f *schedFake) ListSchedules(_ context.Context, in *scheduler.ListSchedulesInput, _ ...func(*scheduler.Options)) (*scheduler.ListSchedulesOutput, error) {
	f.count()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if aws.ToString(in.GroupName) != reaperdeploy.TeardownScheduleGroup {
		return nil, errors.New("listed another group")
	}
	out := &scheduler.ListSchedulesOutput{}
	for _, j := range f.jobs {
		out.Schedules = append(out.Schedules, schedulertypes.ScheduleSummary{Name: aws.String(j)})
	}
	return out, nil
}

func checker(i *iamFake, s map[string]*schedFake) *ResidueChecker {
	return &ResidueChecker{Account: testAccount, IAM: i, Scheduler: func(region string) SchedulerAPI {
		if f, ok := s[region]; ok {
			return f
		}
		return &schedFake{}
	}}
}

func roleRes() footprint.Resource { return Residue(testAccount, regions)[0] }
func groupRes(region string) footprint.Resource {
	for _, r := range Residue(testAccount, regions) {
		if r.Region == region && r.Type == TypeScheduleGroup {
			return r
		}
	}
	panic("no group in " + region)
}

func TestResidueCheckerRole(t *testing.T) {
	ctx := context.Background()

	// Gone.
	c := checker(&iamFake{}, nil)
	if _, err := c.Describe(ctx, roleRes()); !errors.Is(err, footprint.ErrNotFound) {
		t.Errorf("a missing role = %v, want ErrNotFound", err)
	}

	// There, tagged, and the reaper's own role gone: residue.
	i := &iamFake{roles: map[string]map[string]string{reaperdeploy.TeardownInvokeRoleName: managed()}}
	live, err := checker(i, nil).Describe(ctx, roleRes())
	if err != nil {
		t.Fatal(err)
	}
	if live.Class != footprint.ClassResidue || live.Ownership != footprint.OwnershipTagged || !tags.IsManagedBy(live.Tags, tags.ToolSpawn) {
		t.Errorf("live = %+v, want tagged residue", live)
	}

	// Untagged now: ownership follows the live tags.
	i = &iamFake{roles: map[string]map[string]string{reaperdeploy.TeardownInvokeRoleName: nil}}
	if live, err := checker(i, nil).Describe(ctx, roleRes()); err != nil || live.Ownership == footprint.OwnershipTagged {
		t.Errorf("an untagged role = %+v, %v; want not tagged", live, err)
	}

	// The reaper's own role still there: in use.
	i = &iamFake{roles: map[string]map[string]string{reaperdeploy.TeardownInvokeRoleName: managed(), reaperdeploy.RoleName: managed()}}
	live, err = checker(i, nil).Describe(ctx, roleRes())
	if !errors.Is(err, teardown.ErrInUse) || live.Class != footprint.ClassActive || !strings.Contains(err.Error(), reaperdeploy.RoleName) {
		t.Errorf("with the reaper's role there: %+v, %v; want in use, naming it", live, err)
	}

	// Errors are returned, and a refusal stops the asking.
	i = &iamFake{err: map[string]error{reaperdeploy.TeardownInvokeRoleName: accessDenied}}
	c = NewResidueChecker(aws.Config{Region: "us-east-1"}, testAccount)
	c.IAM = i
	for k := 0; k < 3; k++ {
		if _, err := c.Describe(ctx, roleRes()); err == nil || !NotPermitted(err.Error()) {
			t.Errorf("denied role read = %v", err)
		}
	}
	if i.n != 1 {
		t.Errorf("%d IAM request(s) after a refusal, want 1", i.n)
	}
	i = &iamFake{roles: map[string]map[string]string{reaperdeploy.TeardownInvokeRoleName: managed()},
		err: map[string]error{reaperdeploy.RoleName: errors.New("throttled")}}
	if _, err := checker(i, nil).Describe(ctx, roleRes()); err == nil || errors.Is(err, teardown.ErrInUse) {
		t.Errorf("an unreadable reaper role = %v, want an error that is not in-use", err)
	}
	if _, err := (&ResidueChecker{Account: testAccount}).Describe(ctx, roleRes()); err == nil {
		t.Error("no IAM client succeeded")
	}
}

func TestResidueCheckerGroup(t *testing.T) {
	ctx := context.Background()
	west := groupRes("us-west-2")

	if _, err := checker(nil, nil).Describe(ctx, west); !errors.Is(err, footprint.ErrNotFound) {
		t.Errorf("a missing group = %v, want ErrNotFound", err)
	}
	s := &schedFake{exists: true, tags: managed()}
	live, err := checker(nil, map[string]*schedFake{"us-west-2": s}).Describe(ctx, west)
	if err != nil || live.Class != footprint.ClassResidue || live.Ownership != footprint.OwnershipTagged {
		t.Errorf("an empty tagged group = %+v, %v; want tagged residue", live, err)
	}
	s = &schedFake{exists: true, tags: managed(), jobs: []string{reaperdeploy.JobLogGroup}}
	live, err = checker(nil, map[string]*schedFake{"us-west-2": s}).Describe(ctx, west)
	if !errors.Is(err, teardown.ErrInUse) || live.Class != footprint.ClassActive || !strings.Contains(err.Error(), reaperdeploy.JobLogGroup) {
		t.Errorf("a group holding a job = %+v, %v; want in use, naming the job", live, err)
	}
	for name, f := range map[string]*schedFake{
		"get":  {getErr: errors.New("boom")},
		"tags": {exists: true, tagErr: errors.New("boom")},
		"list": {exists: true, listErr: errors.New("boom")},
	} {
		if _, err := checker(nil, map[string]*schedFake{"us-west-2": f}).Describe(ctx, west); err == nil || errors.Is(err, footprint.ErrNotFound) {
			t.Errorf("%s fails: %v, want an error", name, err)
		}
	}

	// One refusal, then no more requests in any region.
	denied := &schedFake{getErr: accessDenied}
	c := checker(nil, map[string]*schedFake{"us-west-2": denied, "us-east-1": denied})
	for _, r := range []string{"us-west-2", "us-east-1", "us-west-2"} {
		if _, err := c.Describe(ctx, groupRes(r)); err == nil || !NotPermitted(err.Error()) {
			t.Errorf("%s: %v", r, err)
		}
	}
	if denied.n != 1 {
		t.Errorf("%d Scheduler request(s) after a refusal, want 1", denied.n)
	}
	if _, err := (&ResidueChecker{Account: testAccount}).Describe(ctx, west); err == nil {
		t.Error("no Scheduler client succeeded")
	}
}

func TestResidueCheckerOwnsOnlyTheResidue(t *testing.T) {
	c := checker(nil, nil)
	for _, r := range Residue(testAccount, regions) {
		if !c.Owns(r) {
			t.Errorf("does not own %s", r.ARN)
		}
	}
	for _, r := range []footprint.Resource{
		{ARN: reaperdeploy.TeardownInvokeRoleARN("210987654321"), Type: fs.TypeRole},
		{ARN: reaperdeploy.TeardownScheduleGroupARN("us-west-2", testAccount), Type: TypeScheduleGroup},
		{ARN: "arn:aws:iam::" + testAccount + ":role/spored-instance-role", Type: fs.TypeRole},
		{ARN: reaperdeploy.TeardownInvokeRoleARN(testAccount), Type: fs.TypeInstanceProfile},
	} {
		if c.Owns(r) {
			t.Errorf("owns %s (%s, region %q)", r.ARN, r.Type, r.Region)
		}
	}
	if _, err := c.Describe(context.Background(), footprint.Resource{ARN: "arn:x", Type: fs.TypeRole}); err == nil {
		t.Error("described a resource that is not the residue")
	}
}

type planFake struct{ asked []string }

func (p *planFake) Describe(_ context.Context, r footprint.Resource) (footprint.Resource, error) {
	p.asked = append(p.asked, r.ARN)
	return r, nil
}

func (p *planFake) Delete(context.Context, footprint.Resource) error {
	return errors.New("the plan deleter's Delete was called")
}

func TestReportOnlyDeleter(t *testing.T) {
	ctx := context.Background()
	plan := &planFake{}
	d := ReportOnlyDeleter{Residue: checker(&iamFake{}, nil), Plan: plan}
	if _, err := d.Describe(ctx, roleRes()); !errors.Is(err, footprint.ErrNotFound) {
		t.Errorf("the residue went to the plan deleter: %v", err)
	}
	other := footprint.Resource{ARN: "arn:aws:ec2:us-west-2:" + testAccount + ":instance/i-0123456789abcdef0", Type: fs.TypeInstance}
	if _, err := d.Describe(ctx, other); err != nil || len(plan.asked) != 1 || plan.asked[0] != other.ARN {
		t.Errorf("an instance: %v, plan asked %v", err, plan.asked)
	}
	for _, r := range []footprint.Resource{roleRes(), other} {
		if err := d.Delete(ctx, r); !errors.Is(err, ErrReportOnly) {
			t.Errorf("Delete(%s) = %v, want ErrReportOnly", r.ARN, err)
		}
	}
	if _, err := (ReportOnlyDeleter{}).Describe(ctx, other); err == nil {
		t.Error("described with no deleter")
	}

	// The production one: the residue through IAM/Scheduler clients, the
	// rest through pkg/teardown's report-only deleter, deleting nothing.
	prod := NewReportOnlyDeleter(aws.Config{Region: "us-east-1"}, testAccount, time.Now)
	if prod.Residue == nil || prod.Residue.IAM == nil || prod.Residue.Scheduler == nil || prod.Plan == nil {
		t.Fatalf("NewReportOnlyDeleter = %+v", prod)
	}
	if c, ok := prod.Residue.Scheduler("eu-west-1").(*scheduler.Client); !ok || c.Options().Region != "eu-west-1" {
		t.Errorf("the Scheduler client is not regional: %T", prod.Residue.Scheduler("eu-west-1"))
	}
	if _, ok := prod.Plan.(footprint.ByType); !ok {
		t.Errorf("Plan is %T, want pkg/teardown's footprint.ByType", prod.Plan)
	}
	if err := prod.Delete(ctx, other); !errors.Is(err, ErrReportOnly) {
		t.Errorf("Delete = %v", err)
	}
}
