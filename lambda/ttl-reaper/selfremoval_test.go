package main

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cwlogs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/spore-host/libs/lifecycle"

	"github.com/spore-host/spawn/pkg/reaperdeploy"
)

// selfAWS is every AWS API the self-removal's Deployer holds, in one fake:
// reads answer from its fields, and every write (create, put, tag, attach,
// remove, delete) is recorded in writes. Report mode must leave writes empty.
type selfAWS struct {
	mu      sync.Mutex
	writes  []string
	reads   []string
	objects []string
	tagged  bool
	// otherRegionReaper: GetFunction finds a reaper in the other regions.
	otherRegionReaper bool
}

func (f *selfAWS) w(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, op)
}

func (f *selfAWS) r(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, op)
}

// IAM
func (f *selfAWS) CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.w("iam:CreateRole")
	return &iam.CreateRoleOutput{}, nil
}
func (f *selfAWS) GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.r("iam:GetRole")
	return &iam.GetRoleOutput{}, nil
}
func (f *selfAWS) PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.w("iam:PutRolePolicy")
	return &iam.PutRolePolicyOutput{}, nil
}
func (f *selfAWS) AttachRolePolicy(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	f.w("iam:AttachRolePolicy")
	return &iam.AttachRolePolicyOutput{}, nil
}
func (f *selfAWS) DeleteRolePolicy(context.Context, *iam.DeleteRolePolicyInput, ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	f.w("iam:DeleteRolePolicy")
	return &iam.DeleteRolePolicyOutput{}, nil
}
func (f *selfAWS) DetachRolePolicy(context.Context, *iam.DetachRolePolicyInput, ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	f.w("iam:DetachRolePolicy")
	return &iam.DetachRolePolicyOutput{}, nil
}
func (f *selfAWS) DeleteRole(context.Context, *iam.DeleteRoleInput, ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	f.w("iam:DeleteRole")
	return &iam.DeleteRoleOutput{}, nil
}

// Lambda
func (f *selfAWS) CreateFunction(context.Context, *lambda.CreateFunctionInput, ...func(*lambda.Options)) (*lambda.CreateFunctionOutput, error) {
	f.w("lambda:CreateFunction")
	return &lambda.CreateFunctionOutput{}, nil
}
func (f *selfAWS) GetFunction(_ context.Context, in *lambda.GetFunctionInput, _ ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
	f.r("lambda:GetFunction")
	return &lambda.GetFunctionOutput{Tags: map[string]string{reaperdeploy.ArtifactBucketTag: "spawn-reaper-artifacts-" + idleAcct + "-us-east-1", "spawn:version": "0.170.0"}}, nil
}
func (f *selfAWS) UpdateFunctionCode(context.Context, *lambda.UpdateFunctionCodeInput, ...func(*lambda.Options)) (*lambda.UpdateFunctionCodeOutput, error) {
	f.w("lambda:UpdateFunctionCode")
	return &lambda.UpdateFunctionCodeOutput{}, nil
}
func (f *selfAWS) UpdateFunctionConfiguration(context.Context, *lambda.UpdateFunctionConfigurationInput, ...func(*lambda.Options)) (*lambda.UpdateFunctionConfigurationOutput, error) {
	f.w("lambda:UpdateFunctionConfiguration")
	return &lambda.UpdateFunctionConfigurationOutput{}, nil
}
func (f *selfAWS) TagResource(context.Context, *lambda.TagResourceInput, ...func(*lambda.Options)) (*lambda.TagResourceOutput, error) {
	f.w("lambda:TagResource")
	return &lambda.TagResourceOutput{}, nil
}
func (f *selfAWS) AddPermission(context.Context, *lambda.AddPermissionInput, ...func(*lambda.Options)) (*lambda.AddPermissionOutput, error) {
	f.w("lambda:AddPermission")
	return &lambda.AddPermissionOutput{}, nil
}
func (f *selfAWS) RemovePermission(context.Context, *lambda.RemovePermissionInput, ...func(*lambda.Options)) (*lambda.RemovePermissionOutput, error) {
	f.w("lambda:RemovePermission")
	return &lambda.RemovePermissionOutput{}, nil
}
func (f *selfAWS) DeleteFunction(context.Context, *lambda.DeleteFunctionInput, ...func(*lambda.Options)) (*lambda.DeleteFunctionOutput, error) {
	f.w("lambda:DeleteFunction")
	return &lambda.DeleteFunctionOutput{}, nil
}

// EventBridge
func (f *selfAWS) PutRule(context.Context, *eventbridge.PutRuleInput, ...func(*eventbridge.Options)) (*eventbridge.PutRuleOutput, error) {
	f.w("events:PutRule")
	return &eventbridge.PutRuleOutput{}, nil
}
func (f *selfAWS) PutTargets(context.Context, *eventbridge.PutTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.PutTargetsOutput, error) {
	f.w("events:PutTargets")
	return &eventbridge.PutTargetsOutput{}, nil
}
func (f *selfAWS) DescribeRule(context.Context, *eventbridge.DescribeRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DescribeRuleOutput, error) {
	f.r("events:DescribeRule")
	return &eventbridge.DescribeRuleOutput{}, nil
}
func (f *selfAWS) RemoveTargets(context.Context, *eventbridge.RemoveTargetsInput, ...func(*eventbridge.Options)) (*eventbridge.RemoveTargetsOutput, error) {
	f.w("events:RemoveTargets")
	return &eventbridge.RemoveTargetsOutput{}, nil
}
func (f *selfAWS) DeleteRule(context.Context, *eventbridge.DeleteRuleInput, ...func(*eventbridge.Options)) (*eventbridge.DeleteRuleOutput, error) {
	f.w("events:DeleteRule")
	return &eventbridge.DeleteRuleOutput{}, nil
}

// S3
func (f *selfAWS) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.r("s3:HeadBucket")
	return &s3.HeadBucketOutput{}, nil
}
func (f *selfAWS) CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.w("s3:CreateBucket")
	return &s3.CreateBucketOutput{}, nil
}
func (f *selfAWS) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.w("s3:PutObject")
	return &s3.PutObjectOutput{}, nil
}
func (f *selfAWS) PutBucketTagging(context.Context, *s3.PutBucketTaggingInput, ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error) {
	f.w("s3:PutBucketTagging")
	return &s3.PutBucketTaggingOutput{}, nil
}
func (f *selfAWS) GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	f.r("s3:GetBucketTagging")
	if !f.tagged {
		return nil, errors.New("NoSuchTagSet")
	}
	return &s3.GetBucketTaggingOutput{TagSet: []s3types.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}}}, nil
}
func (f *selfAWS) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.r("s3:ListObjectsV2")
	out := &s3.ListObjectsV2Output{}
	for _, k := range f.objects {
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
	}
	return out, nil
}
func (f *selfAWS) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.w("s3:DeleteObject")
	return &s3.DeleteObjectOutput{}, nil
}
func (f *selfAWS) DeleteBucket(context.Context, *s3.DeleteBucketInput, ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	f.w("s3:DeleteBucket")
	return &s3.DeleteBucketOutput{}, nil
}

// CloudWatch Logs
func (f *selfAWS) FilterLogEvents(context.Context, *cwlogs.FilterLogEventsInput, ...func(*cwlogs.Options)) (*cwlogs.FilterLogEventsOutput, error) {
	f.r("logs:FilterLogEvents")
	return &cwlogs.FilterLogEventsOutput{}, nil
}
func (f *selfAWS) CreateLogGroup(context.Context, *cwlogs.CreateLogGroupInput, ...func(*cwlogs.Options)) (*cwlogs.CreateLogGroupOutput, error) {
	f.w("logs:CreateLogGroup")
	return &cwlogs.CreateLogGroupOutput{}, nil
}
func (f *selfAWS) PutRetentionPolicy(context.Context, *cwlogs.PutRetentionPolicyInput, ...func(*cwlogs.Options)) (*cwlogs.PutRetentionPolicyOutput, error) {
	f.w("logs:PutRetentionPolicy")
	return &cwlogs.PutRetentionPolicyOutput{}, nil
}
func (f *selfAWS) DescribeLogGroups(context.Context, *cwlogs.DescribeLogGroupsInput, ...func(*cwlogs.Options)) (*cwlogs.DescribeLogGroupsOutput, error) {
	f.r("logs:DescribeLogGroups")
	return &cwlogs.DescribeLogGroupsOutput{}, nil
}
func (f *selfAWS) DeleteLogGroup(context.Context, *cwlogs.DeleteLogGroupInput, ...func(*cwlogs.Options)) (*cwlogs.DeleteLogGroupOutput, error) {
	f.w("logs:DeleteLogGroup")
	return &cwlogs.DeleteLogGroupOutput{}, nil
}

// EventBridge Scheduler
func (f *selfAWS) CreateScheduleGroup(context.Context, *scheduler.CreateScheduleGroupInput, ...func(*scheduler.Options)) (*scheduler.CreateScheduleGroupOutput, error) {
	f.w("scheduler:CreateScheduleGroup")
	return &scheduler.CreateScheduleGroupOutput{}, nil
}
func (f *selfAWS) CreateSchedule(context.Context, *scheduler.CreateScheduleInput, ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error) {
	f.w("scheduler:CreateSchedule")
	return &scheduler.CreateScheduleOutput{}, nil
}
func (f *selfAWS) DeleteScheduleGroup(context.Context, *scheduler.DeleteScheduleGroupInput, ...func(*scheduler.Options)) (*scheduler.DeleteScheduleGroupOutput, error) {
	f.w("scheduler:DeleteScheduleGroup")
	return &scheduler.DeleteScheduleGroupOutput{}, nil
}

// otherRegion is the Lambda in the account's other regions.
type otherRegion struct {
	selfAWS
	parent *selfAWS
}

func (o *otherRegion) GetFunction(context.Context, *lambda.GetFunctionInput, ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
	o.parent.r("lambda:GetFunction(other region)")
	if o.parent.otherRegionReaper {
		return &lambda.GetFunctionOutput{}, nil
	}
	return nil, &lambdatypes.ResourceNotFoundException{}
}

// selfWorld is newIdleWorld for the self-hosted reaper's own account.
func selfWorld(t *testing.T) (*idleWorld, *selfAWS) {
	t.Helper()
	w := newIdleWorld(t)
	w.acct.label = "self"
	f := &selfAWS{tagged: true, objects: []string{"ttl-reaper/v0.170.0.zip"}}
	d := &reaperdeploy.Deployer{IAM: f, Lambda: f, Events: f, S3: f, Logs: f, Scheduler: f}
	w.r.selfRemoval = selfRemovalReport
	w.r.self = &selfHosted{accountID: idleAcct, region: "us-east-1", deployer: d,
		lambdaIn: func(string) reaperdeploy.LambdaAPI { return &otherRegion{parent: f} }}
	w.ec2.regions = []string{"us-east-1", "us-west-2"}
	return w, f
}

var reportStep = regexp.MustCompile(`reaper-selfremoval REPORT step=(\d+)/(\d+) action=(\S+) how=(\S+) offset=\+(\S+) target=(\S+) resource=(\S+)`)

// The acceptance path: an idle account waits out the grace, every scope is
// reported, and the reaper logs its own removal, every step with its offset,
// and creates and deletes nothing.
func TestSelfRemovalReportsThePlanAndChangesNothing(t *testing.T) {
	w, f := selfWorld(t)
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(25 * time.Hour)
	sum := w.run()
	if sum.AccountIdleReported != 2 || sum.SelfRemovalPlanned != 1 || sum.SelfRemovalOneShots != 5 {
		t.Fatalf("%+v\n%s", sum, w.logs.String())
	}
	if len(f.writes) != 0 {
		t.Fatalf("report mode wrote to AWS: %v", f.writes)
	}
	if len(w.deleter.deleted) != 0 {
		t.Fatalf("report mode deleted %v", w.deleter.deleted)
	}
	logs := w.logs.String()
	steps := reportStep.FindAllStringSubmatch(logs, -1)
	if len(steps) != 13 {
		t.Fatalf("logged %d plan steps, want 13:\n%s", len(steps), logs)
	}
	var offsets []string
	for _, m := range steps {
		if m[4] == reaperdeploy.HowOneShot {
			offsets = append(offsets, m[5])
		}
	}
	if got := strings.Join(offsets, ","); got != "2m0s,4m0s,4m0s,5m0s,15m0s" {
		t.Errorf("one-shot offsets %s", got)
	}
	for _, want := range []string{
		"action=one-shot how=one-shot offset=+2m0s target=arn:aws:scheduler:::aws-sdk:lambda:deleteFunction resource=arn:aws:lambda:us-east-1:" + idleAcct + ":function:spawn-ttl-reaper-selfscan name=spawn-reaper-teardown-function group=spawn-reaper-teardown",
		"offset=+5m0s target=arn:aws:scheduler:::aws-sdk:iam:deleteRole resource=arn:aws:iam::" + idleAcct + ":role/spawn-ttl-reaper-selfscan-role",
		"offset=+15m0s target=arn:aws:scheduler:::aws-sdk:cloudwatchlogs:deleteLogGroup",
		"action=delete-artifact-bucket how=direct offset=+0s target=s3:DeleteBucket resource=arn:aws:s3:::spawn-reaper-artifacts-" + idleAcct + "-us-east-1",
		"action=delete-rule how=direct offset=+0s target=events:DeleteRule resource=arn:aws:events:us-east-1:" + idleAcct + ":rule/spawn-ttl-reaper-selfscan-schedule",
		"resource=arn:aws:events:us-east-1:" + idleAcct + ":rule/spawn-ttl-reaper-selfscan-instance-terminated",
		"reaper-selfremoval REPORT account=" + idleAcct + " region=us-east-1 steps=13 one_shots=5 direct=8 kept=0 group=spawn-reaper-teardown invoke_role=arn:aws:iam::" + idleAcct + ":role/spawn-reaper-teardown-invoke",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}

// Rule 1 for the invariant above: the same plan through CreateOneShots, as
// reap will run it, writes to Scheduler through the same fake, so the empty
// writes above are report mode's doing, not a fake that records nothing.
func TestSelfRemovalReportIsWhatKeepsSchedulerUntouched(t *testing.T) {
	w, f := selfWorld(t)
	p, err := w.r.self.deployer.PlanSelfRemoval(context.Background(), reaperdeploy.SelfRemovalInput{
		AccountID: idleAcct, Region: "us-east-1", Now: w.clock.Now(), Regions: []string{"us-east-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reaperdeploy.CreateOneShots(context.Background(), f, "us-east-1", idleAcct, "", p.OneShots(), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 6 {
		t.Fatalf("CreateOneShots through the fake wrote %v, want the group and five jobs", f.writes)
	}
}

// reap is accepted and reports, until spawn#1657.
func TestSelfRemovalReapStillReports(t *testing.T) {
	if got := effectiveSelfRemovalMode(selfRemovalReap); got != selfRemovalReport {
		t.Fatalf("reap acts as %q; it must report until spawn#1657", got)
	}
	w, f := selfWorld(t)
	w.r.selfRemoval = selfRemovalReap
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(25 * time.Hour)
	if sum := w.run(); sum.SelfRemovalPlanned != 1 {
		t.Fatalf("%+v\n%s", sum, w.logs.String())
	}
	if len(f.writes) != 0 {
		t.Fatalf("REAPER_SELF_REMOVAL=reap wrote to AWS before spawn#1657: %v", f.writes)
	}
	if !strings.Contains(w.logs.String(), "reaper-selfremoval REPORT step=1/13") {
		t.Errorf("reap did not log a report:\n%s", w.logs.String())
	}
}

func TestSelfRemovalModeFromEnv(t *testing.T) {
	for v, want := range map[string]string{"": selfRemovalReport, "off": selfRemovalOff, "report": selfRemovalReport, "reap": selfRemovalReap, "bogus": selfRemovalReport} {
		t.Setenv("REAPER_SELF_REMOVAL", v)
		if got := selfRemovalModeFromEnv(); got != want {
			t.Errorf("REAPER_SELF_REMOVAL=%q is %q, want %q", v, got, want)
		}
	}
}

// Any active scope, and the reaper stays.
func TestSelfRemovalNotPlannedWhileAnythingIsActive(t *testing.T) {
	cases := map[string]func(w *idleWorld){
		"an instance":          func(w *idleWorld) { w.ec2.managed = 1 },
		"instances unreadable": func(w *idleWorld) { w.ec2.managedErr = errors.New("boom") },
		"a running sweep":      func(w *idleWorld) { w.infra.items[sweepsTable] = sweepItem("RUNNING") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w, f := selfWorld(t)
			w.markActive("us-east-1", lifecycle.ScopeGlobal)
			setup(w)
			w.run()
			w.clock.add(25 * time.Hour)
			sum := w.run()
			if sum.SelfRemovalPlanned != 0 || strings.Contains(w.logs.String(), "reaper-selfremoval REPORT") {
				t.Fatalf("planned while active: %+v\n%s", sum, w.logs.String())
			}
			if len(f.reads)+len(f.writes) != 0 {
				t.Errorf("touched its own resources while active: %v %v", f.reads, f.writes)
			}
		})
	}
}

// Activity found at the post-claim re-check of a region releases that scope,
// and the plan waits for a run in which every scope is reported.
func TestSelfRemovalNeedsEveryScopeReported(t *testing.T) {
	w, f := selfWorld(t)
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(25 * time.Hour)
	// This run reads the infra tables three times (three tables each): the
	// verdict, the region's re-check, global's re-check. A sweep starts for
	// the region's re-check alone, so the region is released and global
	// reported.
	w.infra.itemsAt = func(n int, table string) []map[string]ddbtypes.AttributeValue {
		if table == sweepsTable && n >= 4 && n <= 6 {
			return sweepItem("RUNNING")
		}
		return nil
	}
	w.infra.scans = nil
	sum := w.run()
	if sum.AccountIdleReleased != 1 || sum.AccountIdleReported != 1 {
		t.Fatalf("want the region released and global reported: %+v\n%s", sum, w.logs.String())
	}
	if sum.SelfRemovalPlanned != 0 || sum.SelfRemovalBlocked != 1 || len(f.writes) != 0 {
		t.Fatalf("planned with a scope released: %+v\n%s", sum, w.logs.String())
	}
	if !strings.Contains(w.logs.String(), "reaper-selfremoval: account="+idleAcct+": not planned: us-east-1 was not reported this run") {
		t.Errorf("the block is not explained:\n%s", w.logs.String())
	}
}

// A scope a creator marked later than the others comes due later. The due
// scopes are held, not reported, until it does, so that every scope is
// reported in one run; without the hold each report restarts its own grace
// and the scopes never line up.
func TestSelfRemovalHoldsDueScopesUntilEveryScopeIsDue(t *testing.T) {
	w, f := selfWorld(t)
	w.r.regions = []string{"us-east-1", "us-west-2"}
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(12 * time.Hour)
	w.markActive("us-west-2") // a launch there, 12 h later
	w.run()
	w.clock.add(13 * time.Hour) // us-east-1 and global due; us-west-2 12 h short
	sum := w.run()
	if sum.AccountIdleHeld != 2 || sum.AccountIdleClaimed != 0 || sum.SelfRemovalPlanned != 0 {
		t.Fatalf("want us-east-1 and global held: %+v\n%s", sum, w.logs.String())
	}
	if !strings.Contains(w.logs.String(), "held until us-west-2 are too") {
		t.Errorf("the hold is not explained:\n%s", w.logs.String())
	}
	w.clock.add(12 * time.Hour)
	sum = w.run()
	if sum.AccountIdleReported != 3 || sum.SelfRemovalPlanned != 1 {
		t.Fatalf("once every scope is due: %+v\n%s", sum, w.logs.String())
	}
	if len(f.writes) != 0 {
		t.Fatalf("wrote %v", f.writes)
	}
}

// The hold is only for the self-hosted reaper, and never while anything is
// active: then the per-scope reports go on as before (#1654).
func TestSelfRemovalHoldOnlyWhenNothingIsActive(t *testing.T) {
	js := []scopeJudgment{{scope: "a", state: scopeDue}, {scope: "b", state: scopeWaiting}, {scope: "global", state: scopeDue}}
	if held, waiting := holdForSelfRemoval(js); !held || strings.Join(waiting, ",") != "b" {
		t.Errorf("held=%t waiting=%v", held, waiting)
	}
	for _, st := range []int{scopeActive, scopeUnavailable} {
		js := append([]scopeJudgment{{scope: "c", state: st}}, js...)
		if held, _ := holdForSelfRemoval(js); held {
			t.Errorf("held with a scope in state %d", st)
		}
	}
	if held, _ := holdForSelfRemoval([]scopeJudgment{{scope: "a", state: scopeDue}, {scope: "global", state: scopeDue}}); held {
		t.Error("held with every scope due")
	}

	// Not self-hosted: no hold, the due scope is reported.
	w := newIdleWorld(t)
	w.r.regions = []string{"us-east-1", "us-west-2"}
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(12 * time.Hour)
	w.markActive("us-west-2")
	w.run()
	w.clock.add(13 * time.Hour)
	if sum := w.run(); sum.AccountIdleHeld != 0 || sum.AccountIdleReported != 2 || sum.SelfRemovalPlanned != 0 {
		t.Fatalf("an operated reaper held or planned: %+v", sum)
	}
}

// A scope no creator has marked is never torn down; it does not block the
// removal while nothing in it is active.
func TestSelfRemovalWithAnUnmarkedIdleRegion(t *testing.T) {
	w, _ := selfWorld(t)
	w.r.regions = []string{"us-east-1", "us-west-2"}
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(25 * time.Hour)
	if sum := w.run(); sum.SelfRemovalPlanned != 1 {
		t.Fatalf("%+v\n%s", sum, w.logs.String())
	}
}

func TestSelfRemovalOffAndNotSelfHosted(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		w, f := selfWorld(t)
		w.r.selfRemoval = selfRemovalOff
		w.markActive("us-east-1", lifecycle.ScopeGlobal)
		w.run()
		w.clock.add(25 * time.Hour)
		if sum := w.run(); sum.SelfRemovalPlanned != 0 || sum.AccountIdleReported != 2 || len(f.reads) != 0 {
			t.Fatalf("%+v reads=%v", sum, f.reads)
		}
	})
	t.Run("another account", func(t *testing.T) {
		w, f := selfWorld(t)
		w.r.self.accountID = "444455556666"
		w.markActive("us-east-1", lifecycle.ScopeGlobal)
		w.run()
		w.clock.add(25 * time.Hour)
		if sum := w.run(); sum.SelfRemovalPlanned != 0 || len(f.reads) != 0 {
			t.Fatalf("planned for an account that is not its own: %+v", sum)
		}
	})
}

// A repurposed bucket means no plan at all, and the reaper stays.
func TestSelfRemovalNotPlannedWithARepurposedBucket(t *testing.T) {
	w, f := selfWorld(t)
	f.objects = append(f.objects, "someones/data.csv")
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(25 * time.Hour)
	sum := w.run()
	if sum.SelfRemovalPlanned != 0 || sum.SelfRemovalBlocked != 1 {
		t.Fatalf("%+v", sum)
	}
	if !strings.Contains(w.logs.String(), "not planned") || strings.Contains(w.logs.String(), "reaper-selfremoval REPORT") {
		t.Errorf("logs:\n%s", w.logs.String())
	}
}

// Another region's reaper shares the role: the jobs leave it.
func TestSelfRemovalKeepsARoleAnotherRegionUses(t *testing.T) {
	w, f := selfWorld(t)
	f.otherRegionReaper = true
	w.markActive("us-east-1", lifecycle.ScopeGlobal)
	w.run()
	w.clock.add(25 * time.Hour)
	if sum := w.run(); sum.SelfRemovalOneShots != 2 {
		t.Fatalf("%+v\n%s", sum, w.logs.String())
	}
	if !strings.Contains(w.logs.String(), `role_kept="the reaper in us-west-2 runs as it too"`) {
		t.Errorf("logs:\n%s", w.logs.String())
	}
}

// Only `spawn reaper deploy`'s reaper, scanning only its own account, is
// self-hosted.
func TestSelfHostedFrom(t *testing.T) {
	self := []account{{label: "self", accountID: idleAcct}}
	cfg := aws.Config{Region: "us-east-1"}
	set := func(fn, scanSelf, roles string) {
		t.Setenv("AWS_LAMBDA_FUNCTION_NAME", fn)
		t.Setenv("REAPER_SCAN_SELF", scanSelf)
		t.Setenv("REAPER_ROLE_ARNS", roles)
		t.Setenv("EC2_ROLE_ARN", "")
	}
	set(reaperdeploy.FunctionName, "true", "")
	if s := selfHostedFrom(cfg, self); s == nil || s.accountID != idleAcct || s.region != "us-east-1" || s.deployer == nil {
		t.Fatalf("spawn reaper deploy's reaper is not self-hosted: %+v", s)
	}
	for name, c := range map[string]struct {
		fn, scanSelf, roles string
		accts               []account
		cfg                 aws.Config
	}{
		"the operated reaper":  {fn: "spawn-ttl-reaper", scanSelf: "true", accts: self, cfg: cfg},
		"cross-account roles":  {fn: reaperdeploy.FunctionName, scanSelf: "true", roles: "arn:aws:iam::1:role/x", accts: self, cfg: cfg},
		"not scanning itself":  {fn: reaperdeploy.FunctionName, scanSelf: "", accts: self, cfg: cfg},
		"two accounts":         {fn: reaperdeploy.FunctionName, scanSelf: "true", accts: append(self, account{label: "x", accountID: "2"}), cfg: cfg},
		"account unknown":      {fn: reaperdeploy.FunctionName, scanSelf: "true", accts: []account{{label: "self"}}, cfg: cfg},
		"no region":            {fn: reaperdeploy.FunctionName, scanSelf: "true", accts: self},
		"a SAM-deployed stack": {fn: "spawn-ttl-reaper-production", scanSelf: "true", accts: self, cfg: cfg},
	} {
		set(c.fn, c.scanSelf, c.roles)
		if s := selfHostedFrom(c.cfg, c.accts); s != nil {
			t.Errorf("%s: self-hosted", name)
		}
	}
}
