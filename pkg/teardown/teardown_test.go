package teardown

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/lifecycle"
	"github.com/spore-host/libs/lifecycle/lifecycletest"
	libtags "github.com/spore-host/libs/tags"

	"github.com/spore-host/spawn/internal/sdkanswer"
	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/poolqueue"
)

const acct = "123456789012"

var (
	ctx  = context.Background()
	now  = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	long = now.Add(-30 * 24 * time.Hour)
)

func spawnTags(kv ...string) map[string]string {
	m := map[string]string{libtags.ManagedBy: "spawn", "spawn:managed": "true"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func iamT(m map[string]string) []iamtypes.Tag {
	var out []iamtypes.Tag
	for k, v := range m {
		out = append(out, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

func ec2T(m map[string]string) []ec2types.Tag {
	var out []ec2types.Tag
	for k, v := range m {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

// account is a small stateful account: a per-task role and its profile and
// spored policy, a security group, a volume, and whatever extra a test adds.
// It records every call, and deletes take effect.
type account struct {
	mu      sync.Mutex
	calls   []string
	gone    map[string]bool
	tags    map[string]map[string]string // live tags by name, when a test overrides them
	eniOn   map[string]bool              // security groups a network interface carries
	extraSG map[string]string            // id -> name
}

func newAccount() *account {
	return &account{gone: map[string]bool{}, tags: map[string]map[string]string{}, eniOn: map[string]bool{},
		extraSG: map[string]string{"sg-idle": "spawn-mpi-x"}}
}

func (a *account) liveTags(name string) map[string]string {
	if t, ok := a.tags[name]; ok {
		return t
	}
	return spawnTags()
}

func (a *account) answer(service, op string, in any) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, service+"."+op)
	noSuch := &iamtypes.NoSuchEntityException{Message: aws.String("gone (fake)")}
	switch service + "." + op {
	case "IAM.GetRole":
		n := aws.ToString(in.(*iam.GetRoleInput).RoleName)
		if a.gone["role/"+n] {
			return nil, noSuch
		}
		return &iam.GetRoleOutput{Role: &iamtypes.Role{RoleName: aws.String(n), CreateDate: aws.Time(long),
			RoleLastUsed: &iamtypes.RoleLastUsed{LastUsedDate: aws.Time(long)}, Tags: iamT(a.liveTags(n))}}, nil
	case "IAM.GetInstanceProfile":
		n := aws.ToString(in.(*iam.GetInstanceProfileInput).InstanceProfileName)
		if a.gone["profile/"+n] {
			return nil, noSuch
		}
		return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{InstanceProfileName: aws.String(n),
			CreateDate: aws.Time(long), Tags: iamT(a.liveTags(n)), Roles: []iamtypes.Role{{RoleName: aws.String(n)}}}}, nil
	case "IAM.ListInstanceProfilesForRole":
		n := aws.ToString(in.(*iam.ListInstanceProfilesForRoleInput).RoleName)
		if a.gone["profile/"+n] {
			return nil, nil
		}
		return &iam.ListInstanceProfilesForRoleOutput{InstanceProfiles: []iamtypes.InstanceProfile{{InstanceProfileName: aws.String(n)}}}, nil
	case "IAM.DeleteInstanceProfile":
		a.gone["profile/"+aws.ToString(in.(*iam.DeleteInstanceProfileInput).InstanceProfileName)] = true
	case "IAM.DeleteRole":
		a.gone["role/"+aws.ToString(in.(*iam.DeleteRoleInput).RoleName)] = true
	case "IAM.GetPolicy":
		arn := aws.ToString(in.(*iam.GetPolicyInput).PolicyArn)
		if a.gone[arn] {
			return nil, noSuch
		}
		name := arn[strings.LastIndex(arn, "/")+1:]
		return &iam.GetPolicyOutput{Policy: &iamtypes.Policy{Arn: aws.String(arn), PolicyName: aws.String(name),
			AttachmentCount: aws.Int32(0), Tags: iamT(a.liveTags(name))}}, nil
	case "IAM.DeletePolicy":
		a.gone[aws.ToString(in.(*iam.DeletePolicyInput).PolicyArn)] = true
	case "EC2.DescribeSecurityGroups":
		id := in.(*ec2.DescribeSecurityGroupsInput).Filters[0].Values[0]
		name, ok := a.extraSG[id]
		if !ok || a.gone[id] {
			return &ec2.DescribeSecurityGroupsOutput{}, nil
		}
		return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{{GroupId: aws.String(id),
			GroupName: aws.String(name), VpcId: aws.String("vpc-1"), Tags: ec2T(a.liveTags(name))}}}, nil
	case "EC2.DescribeNetworkInterfaces":
		id := in.(*ec2.DescribeNetworkInterfacesInput).Filters[0].Values[0]
		if a.eniOn[id] {
			return &ec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: []ec2types.NetworkInterface{{}}}, nil
		}
	case "EC2.DeleteSecurityGroup":
		a.gone[aws.ToString(in.(*ec2.DeleteSecurityGroupInput).GroupId)] = true
	case "EC2.DescribeVolumes":
		id := in.(*ec2.DescribeVolumesInput).Filters[0].Values[0]
		if a.gone[id] {
			return &ec2.DescribeVolumesOutput{}, nil
		}
		return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{{VolumeId: aws.String(id), State: ec2types.VolumeStateAvailable, Tags: ec2T(spawnTags())}}}, nil
	case "EC2.DeleteVolume":
		a.gone[aws.ToString(in.(*ec2.DeleteVolumeInput).VolumeId)] = true
	case "SQS.GetQueueUrl":
		n := aws.ToString(in.(*sqs.GetQueueUrlInput).QueueName)
		return &sqs.GetQueueUrlOutput{QueueUrl: aws.String("https://sqs.us-east-1.amazonaws.com/" + acct + "/" + n)}, nil
	case "SQS.ListQueueTags":
		url := aws.ToString(in.(*sqs.ListQueueTagsInput).QueueUrl)
		return &sqs.ListQueueTagsOutput{Tags: a.liveTags(url[strings.LastIndex(url, "/")+1:])}, nil
	case "SQS.GetQueueAttributes":
		return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{"ApproximateNumberOfMessages": "0",
			"ApproximateNumberOfMessagesNotVisible": "0", "ApproximateNumberOfMessagesDelayed": "0", "CreatedTimestamp": "1"}}, nil
	}
	return nil, nil
}

// deletes is the delete calls made, in order.
func (a *account) deletes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, c := range a.calls {
		op := c[strings.Index(c, ".")+1:]
		for _, p := range []string{"Delete", "Terminate", "Release", "Remove", "Detach", "Revoke"} {
			if strings.HasPrefix(op, p) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// noSchedules is a reference source naming no role.
type noSchedules struct{}

func (noSchedules) Describe() string { return "no schedules" }
func (noSchedules) ProfileReferences(context.Context) (map[string][]string, error) {
	return map[string][]string{}, nil
}

func collected(t *testing.T) instancerole.References {
	t.Helper()
	refs, err := instancerole.CollectReferences(ctx, noSchedules{})
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func newDeleter(t *testing.T, a *account) footprint.ByType {
	t.Helper()
	return New(sdkanswer.Config("us-east-1", a.answer), Options{
		Account: acct, Now: func() time.Time { return now }, RoleReferences: collected(t),
		Workers: func(context.Context) (poolqueue.InUse, error) {
			return poolqueue.InUse{Profiles: map[string]bool{}, RunIDs: map[string]bool{}}, nil
		},
	})
}

// testLease is a lease nobody contends for.
func testLease(t *testing.T) *lifecycle.Lease {
	t.Helper()
	f := lifecycletest.New()
	c, err := lifecycle.New(lifecycle.Options{DynamoDB: func(string) (lifecycle.API, error) { return f, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Observe(ctx, fs.Tool, "test", false); err != nil {
		t.Fatal(err)
	}
	l, err := c.Claim(ctx, fs.Tool, "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

const (
	roleARN    = "arn:aws:iam::" + acct + ":role/spawn-instance-abc"
	profileARN = "arn:aws:iam::" + acct + ":instance-profile/spawn-instance-abc"
	policyARN  = "arn:aws:iam::" + acct + ":policy/spore-host/spored-policy-v3"
	sgARN      = "arn:aws:ec2:us-east-1:" + acct + ":security-group/sg-idle"
	volARN     = "arn:aws:ec2:us-east-1:" + acct + ":volume/vol-0123456789abcdef0"
)

// planned is a resource as `spawn footprint -o json` reports it, finished by
// the footprint's own rules.
func planned(arn, typ, region, name string, tags map[string]string, f fs.Facts) footprint.Resource {
	r := footprint.Resource{ARN: arn, Type: typ, Region: region, Name: name, Tags: tags}
	f.Account = acct
	fs.Finish(&r, f)
	return r
}

// The plan is listed policy first, so only DependsOn can put the role and the
// profile before it.
func iamPlan() []footprint.Resource {
	pol := planned(policyARN, fs.TypeManagedPolicy, fs.RegionGlobal, "spored-policy-v3", spawnTags(), fs.Facts{})
	role := planned(roleARN, fs.TypeRole, fs.RegionGlobal, "spawn-instance-abc", spawnTags(), fs.Facts{})
	role.DependsOn = []string{policyARN}
	prof := planned(profileARN, fs.TypeInstanceProfile, fs.RegionGlobal, "spawn-instance-abc", spawnTags(), fs.Facts{})
	prof.DependsOn = []string{roleARN}
	sg := planned(sgARN, fs.TypeSecurityGroup, "us-east-1", "spawn-mpi-x", spawnTags(), fs.Facts{})
	vol := planned(volARN, fs.TypeVolume, "us-east-1", "vol-0123456789abcdef0", spawnTags(), fs.Facts{})
	return []footprint.Resource{pol, role, prof, sg, vol}
}

func run(t *testing.T, a *account, mode footprint.Mode, rs []footprint.Resource) *footprint.Result {
	t.Helper()
	plan, err := footprint.NewPlan(fs.Tool, fs.Patterns, rs)
	if err != nil {
		t.Fatal(err)
	}
	var l *lifecycle.Lease
	if mode == footprint.ModeReap {
		l = testLease(t)
	}
	res, err := footprint.Execute(ctx, plan, mode, newDeleter(t, a), l)
	if err != nil && !errors.Is(err, ErrInUse) {
		t.Logf("execute: %v", err)
	}
	return res
}

func outcomes(res *footprint.Result) map[string]footprint.Action {
	m := map[string]footprint.Action{}
	for _, a := range res.Actions {
		m[a.ARN] = a
	}
	return m
}

// Report mode re-reads everything and makes no delete call.
func TestReportModeDeletesNothing(t *testing.T) {
	a := newAccount()
	res := run(t, a, footprint.ModeReport, iamPlan())
	if d := a.deletes(); len(d) != 0 {
		t.Fatalf("report mode made delete calls: %v", d)
	}
	for _, act := range res.Actions {
		if act.Outcome != footprint.OutcomeWouldDelete {
			t.Errorf("%s: %s (%s %v), want would-delete", act.Name, act.Outcome, act.Reason, act.Err)
		}
	}
}

// Reap mode deletes dependents first: the profile before its role, the role
// before its policy.
func TestReapDeletesInDependencyOrder(t *testing.T) {
	a := newAccount()
	res := run(t, a, footprint.ModeReap, iamPlan())
	for _, act := range res.Actions {
		if act.Outcome != footprint.OutcomeDeleted {
			t.Errorf("%s: %s (%s %v), want deleted", act.Name, act.Outcome, act.Reason, act.Err)
		}
	}
	pos := map[string]int{}
	for i, c := range a.deletes() {
		if _, ok := pos[c]; !ok {
			pos[c] = i
		}
	}
	for _, op := range []string{"IAM.DeleteInstanceProfile", "IAM.DeleteRole", "IAM.DeletePolicy", "EC2.DeleteSecurityGroup", "EC2.DeleteVolume"} {
		if _, ok := pos[op]; !ok {
			t.Fatalf("no %s; deletes %v", op, a.deletes())
		}
	}
	if !(pos["IAM.DeleteInstanceProfile"] < pos["IAM.DeleteRole"] && pos["IAM.DeleteRole"] < pos["IAM.DeletePolicy"]) {
		t.Errorf("delete order %v: want the profile, then the role, then the policy", a.deletes())
	}
}

// Something in use is kept, and so is everything it depends on.
func TestInUseIsKeptWithWhatItDependsOn(t *testing.T) {
	a := newAccount()
	a.eniOn["sg-idle"] = true
	vol := planned(volARN, fs.TypeVolume, "us-east-1", "vol-0123456789abcdef0", spawnTags(), fs.Facts{})
	vol.DependsOn = []string{sgARN} // contrived, to see the skip
	sg := planned(sgARN, fs.TypeSecurityGroup, "us-east-1", "spawn-mpi-x", spawnTags(), fs.Facts{})
	res := run(t, a, footprint.ModeReap, []footprint.Resource{sg, vol})
	o := outcomes(res)
	if o[sgARN].Outcome != footprint.OutcomeFailed || !errors.Is(o[sgARN].Err, ErrInUse) {
		t.Errorf("security group on an interface: %s %v, want failed with ErrInUse", o[sgARN].Outcome, o[sgARN].Err)
	}
	for _, d := range a.deletes() {
		if d == "EC2.DeleteSecurityGroup" {
			t.Error("deleted a security group a network interface carries")
		}
	}
}

// The guard refuses every never-deletable kind, including plans edited to
// claim otherwise, which only the live re-check can catch. No delete call
// reaches the account for any of them.
func TestTheGuardRefusesTheNeverDeletable(t *testing.T) {
	boundaryARN := "arn:aws:iam::" + acct + ":policy/spore-host/spawn-instance-boundary"
	resultsARN := "arn:aws:s3:::spawn-results-" + acct + "-us-east-1"
	lie := func(r footprint.Resource) footprint.Resource {
		// What a hand-edited plan would say to sneak r through.
		r.Ownership, r.Class, r.Deletable, r.Reason = footprint.OwnershipTagged, footprint.ClassLitter, true, ""
		r.Tags = spawnTags()
		return r
	}
	for _, c := range []struct {
		what string
		r    footprint.Resource
		live map[string]map[string]string // live tags, by name, when the account disagrees with the plan
		want footprint.Outcome
	}{
		{"user data: a results bucket holding results",
			planned(resultsARN, fs.TypeBucket, "us-east-1", "spawn-results-"+acct+"-us-east-1", spawnTags(), fs.Facts{UserData: "holds results"}),
			nil, footprint.OutcomeKept},
		{"the boundary, planned as litter",
			lie(footprint.Resource{ARN: boundaryARN, Type: fs.TypeManagedPolicy, Region: fs.RegionGlobal, Name: "spawn-instance-boundary"}),
			nil, footprint.OutcomeKept},
		{"role=service, as planned",
			planned(sgARN, fs.TypeSecurityGroup, "us-east-1", "spawn-mpi-x", spawnTags(libtags.Role, libtags.RoleService), fs.Facts{}),
			nil, footprint.OutcomeKept},
		{"role=service, planned without the tag",
			lie(footprint.Resource{ARN: sgARN, Type: fs.TypeSecurityGroup, Region: "us-east-1", Name: "spawn-mpi-x"}),
			map[string]map[string]string{"spawn-mpi-x": spawnTags(libtags.Role, libtags.RoleService)}, footprint.OutcomeRefused},
		{"calque's queue, as planned",
			planned("arn:aws:sqs:us-east-1:"+acct+":spawn-pool-c", fs.TypeQueue, "us-east-1", "spawn-pool-c", map[string]string{libtags.ManagedBy: "calque"}, fs.Facts{}),
			nil, footprint.OutcomeKept},
		{"calque's queue, planned as spawn's",
			lie(footprint.Resource{ARN: "arn:aws:sqs:us-east-1:" + acct + ":spawn-pool-c", Type: fs.TypeQueue, Region: "us-east-1", Name: "spawn-pool-c"}),
			map[string]map[string]string{"spawn-pool-c": {libtags.ManagedBy: "calque", "spawn:managed": "true", "spawn:pool-run-id": "c"}}, footprint.OutcomeRefused},
		{"lagotto's role, planned as spawn's",
			lie(footprint.Resource{ARN: roleARN, Type: fs.TypeRole, Region: fs.RegionGlobal, Name: "spawn-instance-abc"}),
			map[string]map[string]string{"spawn-instance-abc": {libtags.ManagedBy: "lagotto", "spawn:managed": "true"}}, footprint.OutcomeRefused},
		{"legacy-name: spawn:managed only, as planned",
			planned(volARN, fs.TypeVolume, "us-east-1", "vol-0123456789abcdef0", map[string]string{"spawn:managed": "true"}, fs.Facts{}),
			nil, footprint.OutcomeKept},
		{"legacy-name, planned as tagged, untagged in the account",
			lie(footprint.Resource{ARN: policyARN, Type: fs.TypeManagedPolicy, Region: fs.RegionGlobal, Name: "spored-policy-v3"}),
			map[string]map[string]string{"spored-policy-v3": {"spawn:managed": "true"}}, footprint.OutcomeRefused},
		{"another account's resource",
			lie(footprint.Resource{ARN: "arn:aws:ec2:us-east-1:999999999999:volume/vol-0123456789abcdef1", Type: fs.TypeVolume, Region: "us-east-1", Name: "vol-0123456789abcdef1"}),
			nil, footprint.OutcomeFailed},
	} {
		t.Run(c.what, func(t *testing.T) {
			a := newAccount()
			for k, v := range c.live {
				a.tags[k] = v
			}
			res := run(t, a, footprint.ModeReap, []footprint.Resource{c.r})
			if got := res.Actions[0]; got.Outcome != c.want {
				t.Errorf("outcome %s (%s %v), want %s", got.Outcome, got.Reason, got.Err, c.want)
			}
			if d := a.deletes(); len(d) != 0 {
				t.Errorf("delete calls reached the account: %v", d)
			}
		})
	}
}

// A per-task role is only deleted when the schedules that may name it were
// read: otherwise it is kept, as by `spawn cleanup --instance-roles`.
func TestPerTaskRoleNeedsTheScheduleReferences(t *testing.T) {
	a := newAccount()
	d := New(sdkanswer.Config("us-east-1", a.answer), Options{Account: acct, Now: func() time.Time { return now },
		Workers: func(context.Context) (poolqueue.InUse, error) {
			return poolqueue.InUse{Profiles: map[string]bool{}}, nil
		}})
	plan, err := footprint.NewPlan(fs.Tool, fs.Patterns, iamPlan()[1:3])
	if err != nil {
		t.Fatal(err)
	}
	res, _ := footprint.Execute(ctx, plan, footprint.ModeReap, d, testLease(t))
	for _, act := range res.Actions {
		if act.Outcome == footprint.OutcomeDeleted {
			t.Errorf("%s deleted without the schedule references", act.Name)
		}
	}
	if len(a.deletes()) != 0 {
		t.Errorf("delete calls: %v", a.deletes())
	}
}

// Every type spawn's patterns register has a deleter, and every deleter a
// pattern: a type with a pattern and no deleter would plan deletions that
// can only fail, and a deleter with no pattern is dead code.
func TestEveryPatternTypeHasADeleter(t *testing.T) {
	d := New(aws.Config{}, Options{Account: acct})
	var have []string
	for typ := range d {
		have = append(have, typ)
	}
	want := fs.DeletableTypes()
	sort.Strings(have)
	sort.Strings(want)
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Errorf("deleters %v, pattern types %v", have, want)
	}
}
