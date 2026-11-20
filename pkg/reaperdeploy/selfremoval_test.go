package reaperdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	libtags "github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/iamtags"
	"github.com/spore-host/libs/tags/schedulertags"

	"github.com/spore-host/spawn/pkg/instancerole"
)

const srAcct = "123456789012"

var srNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// --- the one-shot builder ----------------------------------------------------

// The documented offsets and fixed names, in run order.
func TestOneShotsOffsetsAndFixedNames(t *testing.T) {
	jobs := OneShots("us-east-1", srAcct, srNow, true)
	want := []struct {
		name   string
		offset time.Duration
		target string
		res    string
	}{
		{JobFunctionDelete, 2 * time.Minute, "arn:aws:scheduler:::aws-sdk:lambda:deleteFunction", "arn:aws:lambda:us-east-1:" + srAcct + ":function:spawn-ttl-reaper-selfscan"},
		{JobRoleInline, 4 * time.Minute, "arn:aws:scheduler:::aws-sdk:iam:deleteRolePolicy", "arn:aws:iam::" + srAcct + ":role/spawn-ttl-reaper-selfscan-role"},
		{JobRoleManaged, 4 * time.Minute, "arn:aws:scheduler:::aws-sdk:iam:detachRolePolicy", "arn:aws:iam::" + srAcct + ":role/spawn-ttl-reaper-selfscan-role"},
		{JobRole, 5 * time.Minute, "arn:aws:scheduler:::aws-sdk:iam:deleteRole", "arn:aws:iam::" + srAcct + ":role/spawn-ttl-reaper-selfscan-role"},
		{JobLogGroup, 15 * time.Minute, "arn:aws:scheduler:::aws-sdk:cloudwatchlogs:deleteLogGroup", "arn:aws:logs:us-east-1:" + srAcct + ":log-group:/aws/lambda/spawn-ttl-reaper-selfscan"},
	}
	if len(jobs) != len(want) {
		t.Fatalf("got %d jobs, want %d: %+v", len(jobs), len(want), jobs)
	}
	for i, w := range want {
		j := jobs[i]
		if j.Name != w.name || j.Offset != w.offset || j.Target != w.target || j.Resource != w.res {
			t.Errorf("job %d = %s +%s %s %s, want %s +%s %s %s", i, j.Name, j.Offset, j.Target, j.Resource, w.name, w.offset, w.target, w.res)
		}
		if got := j.At; !got.Equal(srNow.Add(w.offset)) {
			t.Errorf("%s runs at %s, want %s", j.Name, got, srNow.Add(w.offset))
		}
		if want := "at(" + srNow.Add(w.offset).Format("2006-01-02T15:04:05") + ")"; j.Expression() != want {
			t.Errorf("%s expression %s, want %s", j.Name, j.Expression(), want)
		}
	}
	// The same jobs planned later carry the same names, so a resumed run
	// finds the ones it already made.
	later := OneShots("us-east-1", srAcct, srNow.Add(time.Hour), true)
	for i := range later {
		if later[i].Name != jobs[i].Name {
			t.Errorf("job %d is %s when planned an hour later, %s now: the names must be fixed", i, later[i].Name, jobs[i].Name)
		}
	}
	// The function goes before its role, the role is emptied before it is
	// deleted, and the log group goes last.
	for i := 1; i < len(jobs); i++ {
		if jobs[i].Offset < jobs[i-1].Offset {
			t.Errorf("%s (+%s) runs before %s (+%s)", jobs[i].Name, jobs[i].Offset, jobs[i-1].Name, jobs[i-1].Offset)
		}
	}
}

func TestOneShotsKeepASharedRole(t *testing.T) {
	jobs := OneShots("us-east-1", srAcct, srNow, false)
	for _, j := range jobs {
		if strings.Contains(j.Target, ":iam:") {
			t.Errorf("role kept, but the plan has %s", j.Name)
		}
	}
	if len(jobs) != 2 {
		t.Errorf("want the function and log group jobs only, got %+v", jobs)
	}
}

// The inputs are what each SDK operation takes.
func TestOneShotInputs(t *testing.T) {
	for _, j := range OneShots("eu-west-1", srAcct, srNow, true) {
		var in map[string]string
		if err := json.Unmarshal([]byte(j.Input), &in); err != nil {
			t.Fatalf("%s: input %q: %v", j.Name, j.Input, err)
		}
		want := map[string]map[string]string{
			JobFunctionDelete: {"FunctionName": FunctionName},
			JobRoleInline:     {"RoleName": RoleName, "PolicyName": PolicyName},
			JobRoleManaged:    {"RoleName": RoleName, "PolicyArn": BasicExecutionPolicyARN},
			JobRole:           {"RoleName": RoleName},
			JobLogGroup:       {"LogGroupName": LogGroupName()},
		}[j.Name]
		if fmt.Sprint(in) != fmt.Sprint(want) {
			t.Errorf("%s input %v, want %v", j.Name, in, want)
		}
	}
}

type fakeScheduler struct {
	groups    []*scheduler.CreateScheduleGroupInput
	schedules []*scheduler.CreateScheduleInput
	deleted   []string
	// conflict lists the names (group or job) that already exist.
	conflict map[string]bool
	fail     map[string]error
}

func (f *fakeScheduler) CreateScheduleGroup(_ context.Context, in *scheduler.CreateScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.CreateScheduleGroupOutput, error) {
	f.groups = append(f.groups, in)
	if f.conflict[awssdk.ToString(in.Name)] {
		return nil, &schedulertypes.ConflictException{Message: awssdk.String("exists")}
	}
	return &scheduler.CreateScheduleGroupOutput{}, nil
}

func (f *fakeScheduler) CreateSchedule(_ context.Context, in *scheduler.CreateScheduleInput, _ ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error) {
	f.schedules = append(f.schedules, in)
	name := awssdk.ToString(in.Name)
	if err := f.fail[name]; err != nil {
		return nil, err
	}
	if f.conflict[name] {
		return nil, &schedulertypes.ConflictException{Message: awssdk.String("exists")}
	}
	return &scheduler.CreateScheduleOutput{}, nil
}

func (f *fakeScheduler) DeleteScheduleGroup(_ context.Context, in *scheduler.DeleteScheduleGroupInput, _ ...func(*scheduler.Options)) (*scheduler.DeleteScheduleGroupOutput, error) {
	f.deleted = append(f.deleted, awssdk.ToString(in.Name))
	return &scheduler.DeleteScheduleGroupOutput{}, nil
}

func TestCreateOneShotsCreatesTheGroupTaggedAndEveryJob(t *testing.T) {
	f := &fakeScheduler{}
	jobs := OneShots("us-east-1", srAcct, srNow, true)
	created, err := CreateOneShots(context.Background(), f, "us-east-1", srAcct, "0.170.0", jobs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.groups) != 1 || awssdk.ToString(f.groups[0].Name) != TeardownScheduleGroup {
		t.Fatalf("groups %+v, want one %s", f.groups, TeardownScheduleGroup)
	}
	tg := schedulertags.ToMap(f.groups[0].Tags)
	if tg[libtags.ManagedBy] != "spawn" || tg[libtags.Role] != "" {
		t.Errorf("group tags %v: want spore-host:managed-by=spawn and no spore-host:role (the self-hosted reaper is not the service)", tg)
	}
	if len(created) != len(jobs) || len(f.schedules) != len(jobs) {
		t.Fatalf("created %v from %d jobs", created, len(jobs))
	}
	for i, in := range f.schedules {
		j := jobs[i]
		switch {
		case awssdk.ToString(in.Name) != j.Name, awssdk.ToString(in.GroupName) != TeardownScheduleGroup:
			t.Errorf("job %d is %s/%s", i, awssdk.ToString(in.GroupName), awssdk.ToString(in.Name))
		case in.ActionAfterCompletion != schedulertypes.ActionAfterCompletionDelete:
			t.Errorf("%s does not delete itself after it runs", j.Name)
		case awssdk.ToString(in.ScheduleExpression) != j.Expression():
			t.Errorf("%s expression %s", j.Name, awssdk.ToString(in.ScheduleExpression))
		case awssdk.ToString(in.Target.RoleArn) != TeardownInvokeRoleARN(srAcct):
			t.Errorf("%s runs as %s, want the teardown invoke role, never the reaper's own", j.Name, awssdk.ToString(in.Target.RoleArn))
		case awssdk.ToString(in.Target.Arn) != j.Target, awssdk.ToString(in.Target.Input) != j.Input:
			t.Errorf("%s target %s %s", j.Name, awssdk.ToString(in.Target.Arn), awssdk.ToString(in.Target.Input))
		}
		if awssdk.ToString(in.Target.RoleArn) == RoleARN(srAcct) {
			t.Errorf("%s runs as the reaper's role", j.Name)
		}
	}
}

// A run that crashed after creating the group and two jobs runs again: the
// ConflictExceptions count as success and the rest are created.
func TestCreateOneShotsResumesOnConflict(t *testing.T) {
	f := &fakeScheduler{conflict: map[string]bool{TeardownScheduleGroup: true, JobFunctionDelete: true, JobRoleInline: true}}
	jobs := OneShots("us-east-1", srAcct, srNow, true)
	created, err := CreateOneShots(context.Background(), f, "us-east-1", srAcct, "", jobs, nil)
	if err != nil {
		t.Fatalf("a resumed run failed on ConflictException: %v", err)
	}
	if len(created) != len(jobs) {
		t.Errorf("created %v, want all %d (the existing ones count)", created, len(jobs))
	}
}

func TestCreateOneShotsStopsOnAnyOtherError(t *testing.T) {
	boom := errors.New("AccessDenied")
	f := &fakeScheduler{fail: map[string]error{JobRoleInline: boom}}
	created, err := CreateOneShots(context.Background(), f, "us-east-1", srAcct, "", OneShots("us-east-1", srAcct, srNow, true), nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err %v, want %v", err, boom)
	}
	if strings.Join(created, ",") != JobFunctionDelete {
		t.Errorf("created %v, want only the job before the failure", created)
	}
	if len(f.schedules) != 2 {
		t.Errorf("tried %d jobs, want it to stop at the failed one", len(f.schedules))
	}
}

// The lease heartbeat runs before each job, and its error (a lost lease)
// stops the creation.
func TestCreateOneShotsHeartbeatsAndStopsOnALostLease(t *testing.T) {
	f := &fakeScheduler{}
	lost := errors.New("lease lost")
	var steps []string
	before := func(_ context.Context, step string) error {
		steps = append(steps, step)
		if len(steps) == 3 {
			return lost
		}
		return nil
	}
	created, err := CreateOneShots(context.Background(), f, "us-east-1", srAcct, "", OneShots("us-east-1", srAcct, srNow, true), before)
	if !errors.Is(err, lost) || len(created) != 2 || len(f.schedules) != 2 {
		t.Fatalf("err=%v created=%v schedules=%d, want a stop before the third job", err, created, len(f.schedules))
	}
	if steps[0] != "create one-shot "+JobFunctionDelete {
		t.Errorf("first step %q", steps[0])
	}
}

// --- the plan ----------------------------------------------------------------

func srDeployer(lamF *fakeLambda, s3F *fakeS3) *Deployer {
	d := newTestDeployer(&fakeIAM{roleExists: true}, lamF, &fakeEvents{}, s3F)
	d.Scheduler = &fakeScheduler{}
	return d
}

func srInput() SelfRemovalInput {
	return SelfRemovalInput{AccountID: srAcct, Region: "us-east-1", Now: srNow, Regions: []string{"us-east-1", "us-west-2"},
		LambdaIn: func(string) LambdaAPI { return &fakeLambda{} }}
}

func srTaggedBucket() *fakeS3 {
	return &fakeS3{bucketExists: true, tagged: true, objects: []string{"ttl-reaper/v0.169.0.zip", "ttl-reaper/v0.170.0.zip"}}
}

func stepsOf(p *SelfRemovalPlan) string {
	var out []string
	for _, s := range p.Steps {
		out = append(out, s.How+":"+s.Step+"+"+s.Offset.String())
	}
	return strings.Join(out, ",")
}

func TestPlanSelfRemovalOrderAndOffsets(t *testing.T) {
	lamF := &fakeLambda{exists: true, getTags: map[string]string{ArtifactBucketTag: "spawn-reaper-artifacts-" + srAcct + "-us-east-1", "spawn:version": "0.170.0"}}
	s3F := srTaggedBucket()
	d := srDeployer(lamF, s3F)
	p, err := d.PlanSelfRemoval(context.Background(), srInput())
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"direct:create-schedule-group+0s",
		"one-shot:one-shot+2m0s", "one-shot:one-shot+4m0s", "one-shot:one-shot+4m0s", "one-shot:one-shot+5m0s", "one-shot:one-shot+15m0s",
		"direct:delete-artifact-bucket+0s",
		"direct:remove-target+0s", "direct:delete-rule+0s", "direct:remove-permission+0s",
		"direct:remove-target+0s", "direct:delete-rule+0s", "direct:remove-permission+0s",
	}, ",")
	if got := stepsOf(p); got != want {
		t.Errorf("plan\n got %s\nwant %s", got, want)
	}
	if p.Version != "0.170.0" || p.RoleKept != "" || len(p.OneShots()) != 5 {
		t.Errorf("plan %+v", p)
	}
	var rules []string
	for _, s := range p.Steps {
		if s.Step == "delete-rule" {
			rules = append(rules, path.Base(s.Resource))
		}
	}
	if strings.Join(rules, ",") != RuleName+","+InstanceEventRuleName {
		t.Errorf("rules %v, want the schedule and the instance-terminated rule", rules)
	}
	// Planning is reading: nothing was written.
	for _, c := range s3F.calls {
		if strings.HasPrefix(c, "Delete") || strings.HasPrefix(c, "Put") || strings.HasPrefix(c, "Create") {
			t.Errorf("planning called S3 %s", c)
		}
	}
	for _, c := range lamF.calls {
		if c != "GetFunction" {
			t.Errorf("planning called Lambda %s", c)
		}
	}
	if sch := d.Scheduler.(*fakeScheduler); len(sch.groups)+len(sch.schedules) != 0 {
		t.Error("planning created Scheduler resources")
	}
}

// The bucket rule: the reaper deletes its artifact bucket only when it is
// tagged spawn:managed=true and holds only ttl-reaper/ objects, the rule
// `spawn reaper teardown` applies; holding anything else means no plan at all.
func TestPlanSelfRemovalBucketRule(t *testing.T) {
	cases := []struct {
		name     string
		s3       *fakeS3
		wantErr  error
		anyErr   bool
		wantHow  string
		inReason string
	}{
		{name: "tagged and only artifacts", s3: srTaggedBucket(), wantHow: HowDirect, inReason: "2 ttl-reaper/ object(s)"},
		{name: "untagged", s3: &fakeS3{bucketExists: true, objects: []string{"ttl-reaper/v1.zip"}}, wantHow: HowKept, inReason: "--force-artifacts"},
		{name: "repurposed", s3: &fakeS3{bucketExists: true, tagged: true, objects: []string{"ttl-reaper/v1.zip", "results/data.csv"}}, wantErr: ErrRepurposedBucket},
		{name: "already gone", s3: &fakeS3{headErr: &s3types.NotFound{}}, wantHow: HowKept, inReason: "already gone"},
		{name: "unreadable", s3: &fakeS3{headErr: errors.New("AccessDenied")}, anyErr: true},
		{name: "unlistable", s3: &fakeS3{bucketExists: true, tagged: true, listErr: errors.New("AccessDenied")}, anyErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := srDeployer(&fakeLambda{exists: true}, c.s3)
			p, err := d.PlanSelfRemoval(context.Background(), srInput())
			switch {
			case c.wantErr != nil || c.anyErr:
				if err == nil || (c.wantErr != nil && !errors.Is(err, c.wantErr)) {
					t.Fatalf("err %v, want %v and no plan", err, c.wantErr)
				}
				if p != nil {
					t.Fatal("a plan was made")
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			var b *SelfRemovalStep
			for i := range p.Steps {
				if p.Steps[i].Step == "delete-artifact-bucket" {
					b = &p.Steps[i]
				}
			}
			if b == nil || b.How != c.wantHow || !strings.Contains(b.Reason, c.inReason) {
				t.Errorf("bucket step %+v, want %s with %q", b, c.wantHow, c.inReason)
			}
			if b.Resource != "arn:aws:s3:::"+DefaultBucketName(srAcct, "us-east-1") {
				t.Errorf("bucket %s: with no recorded bucket it is the default name", b.Resource)
			}
		})
	}
}

func TestPlanSelfRemovalUsesTheRecordedBucket(t *testing.T) {
	s3F := srTaggedBucket()
	d := srDeployer(&fakeLambda{exists: true, getTags: map[string]string{ArtifactBucketTag: "my-own-bucket"}}, s3F)
	p, err := d.PlanSelfRemoval(context.Background(), srInput())
	if err != nil {
		t.Fatal(err)
	}
	if p.Bucket.Name != "my-own-bucket" {
		t.Errorf("bucket %s, want the one the function records", p.Bucket.Name)
	}
}

// The role is account-wide: another region's reaper runs as it, so it stays,
// and so it does when that cannot be checked.
func TestPlanSelfRemovalKeepsASharedRole(t *testing.T) {
	cases := map[string]func(*SelfRemovalInput){
		"another region has a reaper": func(in *SelfRemovalInput) {
			in.LambdaIn = func(string) LambdaAPI { return &fakeLambda{exists: true} }
		},
		"regions unlisted": func(in *SelfRemovalInput) { in.RegionsErr = errors.New("boom") },
		"no regions":       func(in *SelfRemovalInput) { in.Regions = nil },
		"no client":        func(in *SelfRemovalInput) { in.LambdaIn = nil },
		"unreadable": func(in *SelfRemovalInput) {
			in.LambdaIn = func(string) LambdaAPI { return &errLambda{} }
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			in := srInput()
			mod(&in)
			p, err := srDeployer(&fakeLambda{exists: true}, srTaggedBucket()).PlanSelfRemoval(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if p.RoleKept == "" {
				t.Fatal("the role is removed")
			}
			for _, j := range p.OneShots() {
				if strings.Contains(j.Target, ":iam:") {
					t.Errorf("role kept (%s), but %s is planned", p.RoleKept, j.Name)
				}
			}
			if p.Count(HowKept) == 0 {
				t.Error("the kept role is not in the plan")
			}
		})
	}
}

type errLambda struct{ fakeLambda }

func (*errLambda) GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
	return nil, errors.New("AccessDenied")
}

// --- the teardown invoke role --------------------------------------------------

func TestDeployCreatesTheTeardownInvokeRole(t *testing.T) {
	iamF := &fakeIAM{}
	d := newTestDeployer(iamF, &fakeLambda{}, &fakeEvents{}, &fakeS3{})
	res, err := d.Deploy(context.Background(), Options{AccountID: srAcct, Region: "us-east-1", Version: "0.170.0", Artifact: "/tmp/x.zip"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(iamF.createdRoles, ","), TeardownInvokeRoleName) {
		t.Fatalf("deploy created %v, not %s", iamF.createdRoles, TeardownInvokeRoleName)
	}
	if !strings.Contains(strings.Join(res.Actions, "\n"), "created role "+TeardownInvokeRoleName) {
		t.Errorf("deploy does not say it created the role: %v", res.Actions)
	}
	tg := iamtags.ToMap(iamF.roleTags[TeardownInvokeRoleName])
	if tg[libtags.ManagedBy] != "spawn" {
		t.Errorf("invoke role tags %v lack spore-host:managed-by=spawn", tg)
	}
	if _, ok := tg[libtags.Role]; ok {
		t.Errorf("invoke role carries %s=%s: spore-host:role=service is the live service's alone", libtags.Role, tg[libtags.Role])
	}
	var trust struct {
		Statement []struct {
			Principal map[string]string
			Condition map[string]map[string]string
		}
	}
	if err := json.Unmarshal([]byte(iamF.trust[TeardownInvokeRoleName]), &trust); err != nil {
		t.Fatal(err)
	}
	if len(trust.Statement) != 1 || trust.Statement[0].Principal["Service"] != "scheduler.amazonaws.com" ||
		trust.Statement[0].Condition["StringEquals"]["aws:SourceAccount"] != srAcct {
		t.Errorf("trust %s: want EventBridge Scheduler for this account only", iamF.trust[TeardownInvokeRoleName])
	}
	doc := iamF.policies[TeardownInvokeRoleName][TeardownInvokePolicyName]
	if want, _ := TeardownInvokePolicyDocument(srAcct); doc != want {
		t.Errorf("invoke policy written %s, want %s", doc, want)
	}
	if !strings.Contains(iamF.policyDoc, "scheduler:CreateSchedule") {
		t.Error("the reaper role's policy lacks the self-removal statements")
	}
}

func TestTeardownRemovesTheSelfRemovalResidue(t *testing.T) {
	iamF := &fakeIAM{roleExists: true}
	d := newTestDeployer(iamF, &fakeLambda{exists: true}, &fakeEvents{}, &fakeS3{})
	sch := &fakeScheduler{}
	d.Scheduler = sch
	removed, err := d.Teardown(context.Background(), TeardownOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sch.deleted, ",") != TeardownScheduleGroup {
		t.Errorf("deleted groups %v", sch.deleted)
	}
	if !strings.Contains(strings.Join(iamF.deletedRoles, ","), TeardownInvokeRoleName) {
		t.Errorf("deleted roles %v, not %s", iamF.deletedRoles, TeardownInvokeRoleName)
	}
	all := strings.Join(removed, "; ")
	for _, want := range []string{"schedule group " + TeardownScheduleGroup, "role " + TeardownInvokeRoleName} {
		if !strings.Contains(all, want) {
			t.Errorf("teardown does not report %q: %s", want, all)
		}
	}
}

// The Deny on the boundary names spawn's real boundary.
func TestTheBoundaryDenyNamesTheBoundary(t *testing.T) {
	want := fmt.Sprintf("arn:aws:iam::%s:policy%s%s", srAcct, instancerole.SporedPolicyPath, instancerole.BoundaryPolicyName)
	if got := boundaryPolicyARN(srAcct); got != want {
		t.Errorf("boundary %s, want %s", got, want)
	}
}

// --- a policy evaluator, for the two policies' reach ----------------------------

type evalStatement struct {
	Effect    string
	Action    []string
	Resource  []string
	Condition map[string]map[string]string
}

func parseStatements(t *testing.T, doc string) []evalStatement {
	t.Helper()
	var raw struct {
		Statement []struct {
			Effect    string
			Action    json.RawMessage
			Resource  json.RawMessage
			Condition map[string]map[string]string
		}
	}
	if err := json.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatal(err)
	}
	list := func(m json.RawMessage) []string {
		var one string
		if json.Unmarshal(m, &one) == nil {
			return []string{one}
		}
		var many []string
		if err := json.Unmarshal(m, &many); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		return many
	}
	var out []evalStatement
	for _, s := range raw.Statement {
		out = append(out, evalStatement{Effect: s.Effect, Action: list(s.Action), Resource: list(s.Resource), Condition: s.Condition})
	}
	return out
}

// glob is IAM's wildcard match: * any run, ? one character.
func glob(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if glob(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && glob(pattern[1:], s[1:])
	}
	return s != "" && pattern[0] == s[0] && glob(pattern[1:], s[1:])
}

// conditionHolds evaluates the operators these policies use. A key absent
// from the request is false, as IAM evaluates StringEquals and ArnEquals.
func conditionHolds(c map[string]map[string]string, req map[string]string) bool {
	for op, kv := range c {
		for k, v := range kv {
			got, ok := req[k]
			if !ok {
				return false
			}
			switch op {
			case "StringEquals", "ArnEquals":
				if got != v {
					return false
				}
			default:
				panic("unmodelled condition operator " + op)
			}
		}
	}
	return true
}

// allows is IAM's evaluation of one identity policy: an explicit Deny wins,
// then an Allow, else the implicit deny.
func allows(st []evalStatement, action, resource string, req map[string]string) bool {
	matches := func(s evalStatement) bool {
		hitA, hitR := false, false
		for _, a := range s.Action {
			if glob(strings.ToLower(a), strings.ToLower(action)) {
				hitA = true
			}
		}
		for _, r := range s.Resource {
			if glob(r, resource) {
				hitR = true
			}
		}
		return hitA && hitR && conditionHolds(s.Condition, req)
	}
	allowed := false
	for _, s := range st {
		if !matches(s) {
			continue
		}
		if s.Effect == "Deny" {
			return false
		}
		allowed = true
	}
	return allowed
}

// iamAction maps a universal target to its IAM action:
// arn:aws:scheduler:::aws-sdk:lambda:deleteFunction is lambda:DeleteFunction,
// and CloudWatch Logs' SDK name cloudwatchlogs is IAM's logs.
func iamAction(target string) string {
	parts := strings.Split(strings.TrimPrefix(target, "arn:aws:scheduler:::aws-sdk:"), ":")
	svc := parts[0]
	if svc == "cloudwatchlogs" {
		svc = "logs"
	}
	return svc + ":" + strings.ToUpper(parts[1][:1]) + parts[1][1:]
}

// The teardown invoke role can do what the one-shot jobs do, in any region,
// and nothing else: no other function, role, log group or policy, nothing of
// the service, and not the boundary.
func TestTheTeardownInvokeRoleCanDeleteOnlyTheReaper(t *testing.T) {
	doc, err := TeardownInvokePolicyDocument(srAcct)
	if err != nil {
		t.Fatal(err)
	}
	st := parseStatements(t, doc)
	for _, region := range []string{"us-east-1", "eu-west-1"} {
		for _, j := range OneShots(region, srAcct, srNow, true) {
			req := map[string]string{}
			var in map[string]string
			_ = json.Unmarshal([]byte(j.Input), &in)
			if arn := in["PolicyArn"]; arn != "" {
				req["iam:PolicyARN"] = arn
			}
			if !allows(st, iamAction(j.Target), j.Resource, req) {
				t.Errorf("%s in %s: %s on %s is denied", j.Name, region, iamAction(j.Target), j.Resource)
			}
		}
	}
	denied := []struct {
		action, resource string
		req              map[string]string
	}{
		{"lambda:DeleteFunction", "arn:aws:lambda:us-east-1:" + srAcct + ":function:spawn-ttl-reaper", nil},
		{"lambda:DeleteFunction", "arn:aws:lambda:us-east-1:" + srAcct + ":function:dashboard-api", nil},
		{"lambda:DeleteFunction", "arn:aws:lambda:us-east-1:999999999999:function:" + FunctionName, nil},
		{"lambda:UpdateFunctionCode", FunctionARN("us-east-1", srAcct), nil},
		{"iam:DeleteRole", TeardownInvokeRoleARN(srAcct), nil},
		{"iam:DeleteRolePolicy", TeardownInvokeRoleARN(srAcct), nil},
		{"iam:DeleteRole", "arn:aws:iam::" + srAcct + ":role/spored-instance-role", nil},
		{"iam:DeleteRole", "arn:aws:iam::" + srAcct + ":role/spawn-instance-abc", nil},
		{"iam:DeleteRole", "arn:aws:iam::999999999999:role/" + RoleName, nil},
		{"iam:PutRolePolicy", RoleARN(srAcct), nil},
		{"iam:AttachRolePolicy", RoleARN(srAcct), map[string]string{"iam:PolicyARN": "arn:aws:iam::aws:policy/AdministratorAccess"}},
		{"iam:DetachRolePolicy", RoleARN(srAcct), map[string]string{"iam:PolicyARN": "arn:aws:iam::aws:policy/AdministratorAccess"}},
		{"iam:DetachRolePolicy", RoleARN(srAcct), nil},
		{"iam:UpdateAssumeRolePolicy", RoleARN(srAcct), nil},
		{"iam:DeletePolicy", boundaryPolicyARN(srAcct), nil},
		{"iam:CreatePolicyVersion", boundaryPolicyARN(srAcct), nil},
		{"logs:DeleteLogGroup", "arn:aws:logs:us-east-1:" + srAcct + ":log-group:/aws/lambda/spawn-dashboard-api", nil},
		{"logs:PutRetentionPolicy", LogGroupARN("us-east-1", srAcct), nil},
		{"s3:DeleteBucket", "arn:aws:s3:::" + DefaultBucketName(srAcct, "us-east-1"), nil},
		{"scheduler:DeleteScheduleGroup", TeardownScheduleGroupARN("us-east-1", srAcct), nil},
		{"sts:AssumeRole", "arn:aws:iam::942542972736:role/anything", nil},
		// Even the reaper's own function, were it ever tagged as the service.
		{"lambda:DeleteFunction", FunctionARN("us-east-1", srAcct), map[string]string{"aws:ResourceTag/spore-host:role": "service"}},
		{"iam:DeleteRole", RoleARN(srAcct), map[string]string{"aws:ResourceTag/spore-host:role": "service"}},
	}
	for _, c := range denied {
		if allows(st, c.action, c.resource, c.req) {
			t.Errorf("the invoke role may %s on %s (%v)", c.action, c.resource, c.req)
		}
	}
	// The evaluator itself: it allows what a plain Allow names.
	if !allows(parseStatements(t, `{"Statement":[{"Effect":"Allow","Action":"s3:Get*","Resource":"arn:aws:s3:::b/*"}]}`), "s3:GetObject", "arn:aws:s3:::b/k", nil) {
		t.Fatal("the evaluator denies a plain Allow: every denial above proves nothing")
	}
}

// The reaper's own role may hand the jobs to Scheduler and nothing more:
// it can never delete its own function, log group or role from inside itself.
func TestTheReaperCannotDeleteItself(t *testing.T) {
	doc, err := PolicyDocument(srAcct, "")
	if err != nil {
		t.Fatal(err)
	}
	st := parseStatements(t, doc)
	for _, region := range []string{"us-east-1", "eu-west-1"} {
		for _, c := range []struct{ action, resource string }{
			{"lambda:DeleteFunction", FunctionARN(region, srAcct)},
			{"lambda:UpdateFunctionConfiguration", FunctionARN(region, srAcct)},
			{"logs:DeleteLogGroup", LogGroupARN(region, srAcct)},
			{"logs:DeleteLogGroup", LogGroupARN(region, srAcct) + ":*"},
			{"iam:DeleteRole", RoleARN(srAcct)},
			{"iam:DeleteRolePolicy", RoleARN(srAcct)},
			{"iam:DetachRolePolicy", RoleARN(srAcct)},
			{"iam:PutRolePolicy", RoleARN(srAcct)},
			{"iam:DeleteRole", TeardownInvokeRoleARN(srAcct)},
			{"iam:PutRolePolicy", TeardownInvokeRoleARN(srAcct)},
			{"iam:CreateRole", TeardownInvokeRoleARN(srAcct)},
			{"scheduler:DeleteSchedule", fmt.Sprintf("arn:aws:scheduler:%s:%s:schedule/%s/%s", region, srAcct, TeardownScheduleGroup, JobFunctionDelete)},
		} {
			if allows(st, c.action, c.resource, map[string]string{"iam:PolicyARN": BasicExecutionPolicyARN}) {
				t.Errorf("the reaper's role may %s on %s", c.action, c.resource)
			}
		}
	}
}

// What the reaper's role gains for its removal, and how narrowly.
func TestSelfRemovalStatementsReach(t *testing.T) {
	st := parseStatements(t, mustPolicy(t)(PolicyDocument(srAcct, "my-own-bucket")))
	sched := func(group, name string) string {
		return fmt.Sprintf("arn:aws:scheduler:us-east-1:%s:schedule/%s/%s", srAcct, group, name)
	}
	pass := map[string]string{"iam:PassedToService": "scheduler.amazonaws.com"}
	for _, c := range []struct {
		action, resource string
		req              map[string]string
		want             bool
	}{
		{"scheduler:CreateSchedule", sched(TeardownScheduleGroup, JobFunctionDelete), nil, true},
		{"scheduler:CreateSchedule", sched("default", JobFunctionDelete), nil, false},
		{"scheduler:CreateSchedule", sched("lagotto-teardown", "x"), nil, false},
		{"scheduler:CreateScheduleGroup", TeardownScheduleGroupARN("us-east-1", srAcct), nil, true},
		{"scheduler:TagResource", TeardownScheduleGroupARN("us-east-1", srAcct), nil, true},
		{"scheduler:CreateScheduleGroup", fmt.Sprintf("arn:aws:scheduler:us-east-1:%s:schedule-group/other", srAcct), nil, false},
		{"iam:PassRole", TeardownInvokeRoleARN(srAcct), pass, true},
		{"iam:PassRole", TeardownInvokeRoleARN(srAcct), map[string]string{"iam:PassedToService": "lambda.amazonaws.com"}, false},
		{"iam:PassRole", TeardownInvokeRoleARN(srAcct), nil, false},
		{"iam:PassRole", RoleARN(srAcct), pass, false},
		{"lambda:GetFunction", FunctionARN("eu-west-1", srAcct), nil, true},
		{"lambda:GetFunction", "arn:aws:lambda:eu-west-1:" + srAcct + ":function:other", nil, false},
		{"s3:GetBucketTagging", "arn:aws:s3:::" + DefaultBucketName(srAcct, "eu-west-1"), nil, true},
		{"s3:ListBucket", "arn:aws:s3:::my-own-bucket", nil, true},
		{"s3:ListBucket", "arn:aws:s3:::someone-elses-bucket", nil, false},
		{"s3:DeleteBucket", "arn:aws:s3:::" + DefaultBucketName(srAcct, "us-east-1"), nil, false},
	} {
		if got := allows(st, c.action, c.resource, c.req); got != c.want {
			t.Errorf("%s on %s (%v): allowed=%t, want %t", c.action, c.resource, c.req, got, c.want)
		}
	}
}

func mustPolicy(t *testing.T) func(string, error) string {
	return func(doc string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
}

// One inline policy holds all of it, and IAM caps a role's inline policies
// at 10,240 characters; with a custom bucket it is longest.
func TestThePolicyFitsAnInlinePolicy(t *testing.T) {
	doc := mustPolicy(t)(PolicyDocument(srAcct, strings.Repeat("b", 63)))
	if n := len(doc); n > 10240 {
		t.Errorf("the reaper's policy is %d characters; an inline role policy is limited to 10,240", n)
	} else {
		t.Logf("the reaper's policy is %d characters of 10,240", n)
	}
	inv := mustPolicy(t)(TeardownInvokePolicyDocument(srAcct))
	t.Logf("the teardown invoke policy is %d characters", len(inv))
}
