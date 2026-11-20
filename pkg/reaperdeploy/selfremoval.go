package reaperdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/libs/tags/schedulertags"

	"github.com/spore-host/spawn/pkg/reaperiam"
)

// The self-hosted reaper's removal of itself once the account it scans is
// idle (spawn#1655, part of the leave-no-trace epic spore-host/spore-host#752).
//
// The reaper never deletes its own function, log group or role from inside
// itself. Deleting the function mid-run is the obvious failure; the log group
// is worse, because Lambda's log delivery recreates a deleted group, untagged
// and with no retention, while the function is still flushing (seen live by
// lagotto#326 on 2026-11-16). Instead the plan hands those deletes to one-shot
// EventBridge Scheduler jobs in TeardownScheduleGroup, which run as
// TeardownInvokeRoleName, a role that is not the reaper's own, and delete
// themselves after they run (ActionAfterCompletion=DELETE). That role is
// created by `spawn reaper deploy`, never at idle time.
//
// The reaper's other pieces it removes directly: its two EventBridge rules,
// their targets and its two invoke permissions, and its artifact bucket, which
// it empties and deletes only when it is tagged spawn:managed=true and holds
// nothing but ttl-reaper/ objects (the rules removeArtifactBucket applies for
// `spawn reaper teardown`).
//
// This release only REPORTS the plan (REAPER_SELF_REMOVAL=report, the
// default; reap reports too until spawn#1657). The invoke role and the
// schedule group are what is left once the jobs have run: residue the lazy
// last mile (libs#78) reports.
//
// The design mirrors lagotto's pkg/idle (lagotto#335): fixed job names, so a
// run that crashed half-way creates the same jobs again, and a
// ConflictException (the job is already there) counts as success.
const (
	// TeardownScheduleGroup holds the one-shot jobs. Apart from anything
	// else in the account, so the invoke role's trust and the reaper's
	// scheduler grants name exactly it.
	TeardownScheduleGroup = "spawn-reaper-teardown"

	// TeardownInvokeRoleName is the role the one-shot jobs run as. It trusts
	// only EventBridge Scheduler, in this account, and may delete exactly
	// the reaper's function, log group and role (TeardownInvokePolicyDocument).
	TeardownInvokeRoleName = "spawn-reaper-teardown-invoke"

	// TeardownInvokePolicyName is its one inline policy.
	TeardownInvokePolicyName = "spawn-reaper-teardown-invoke-policy"

	// BasicExecutionPolicyARN is the managed policy Deploy attaches to the
	// reaper's role for its logs; DeleteRole refuses a role it is still
	// attached to.
	BasicExecutionPolicyARN = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
)

// The offsets of the one-shot jobs from the moment the plan is made.
const (
	// FunctionOffset: the function goes once the run that planned it has
	// long finished.
	FunctionOffset = 2 * time.Minute
	// RolePolicyOffset: the role is emptied after the function that runs as
	// it is gone (DeleteRole refuses a role that still has policies)...
	RolePolicyOffset = 4 * time.Minute
	// RoleOffset: ...and deleted a minute later.
	RoleOffset = 5 * time.Minute
	// LogGroupOffset: the log group goes well after the function's last log
	// flush, so nothing recreates it.
	LogGroupOffset = 15 * time.Minute
)

// The fixed one-shot job names, in the order they run.
const (
	JobFunctionDelete = "spawn-reaper-teardown-function"
	JobRoleInline     = "spawn-reaper-teardown-role-inline"
	JobRoleManaged    = "spawn-reaper-teardown-role-managed"
	JobRole           = "spawn-reaper-teardown-role"
	JobLogGroup       = "spawn-reaper-teardown-log-group"
)

// FunctionARN is the reaper function's ARN in region.
func FunctionARN(region, accountID string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", region, accountID, FunctionName)
}

// RoleARN is the reaper's execution role's ARN.
func RoleARN(accountID string) string {
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, RoleName)
}

// LogGroupARN is the reaper's log group's ARN in region.
func LogGroupARN(region, accountID string) string {
	return fmt.Sprintf("arn:aws:logs:%s:%s:log-group:%s", region, accountID, LogGroupName())
}

// TeardownInvokeRoleARN is the one-shot jobs' role.
func TeardownInvokeRoleARN(accountID string) string {
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, TeardownInvokeRoleName)
}

// TeardownScheduleGroupARN is the one-shot jobs' group in region.
func TeardownScheduleGroupARN(region, accountID string) string {
	return fmt.Sprintf("arn:aws:scheduler:%s:%s:schedule-group/%s", region, accountID, TeardownScheduleGroup)
}

func ruleARN(region, accountID, name string) string {
	return fmt.Sprintf("arn:aws:events:%s:%s:rule/%s", region, accountID, name)
}

// OneShot is one teardown job: at At (Offset after planning), EventBridge
// Scheduler calls the AWS API Target with Input, as TeardownInvokeRoleName.
type OneShot struct {
	// Name is the schedule's name in TeardownScheduleGroup, fixed per job.
	Name   string
	Offset time.Duration
	At     time.Time
	// Target is the universal target, arn:aws:scheduler:::aws-sdk:<service>:<action>.
	Target string
	Input  string
	// Resource is the ARN the job deletes; Why why it is a job.
	Resource string
	Why      string
}

// Expression is the job's schedule expression, at() in UTC.
func (o OneShot) Expression() string { return "at(" + o.At.UTC().Format("2006-01-02T15:04:05") + ")" }

func sdkTarget(service, action string) string {
	return "arn:aws:scheduler:::aws-sdk:" + service + ":" + action
}

func jobInput(v map[string]string) string {
	b, _ := json.Marshal(v) // a map of strings always marshals
	return string(b)
}

// OneShots are the self-removal jobs, in the order they run, planned at now.
// role says whether the reaper's role is removed: false when another
// region's reaper runs as it too (the role is account-wide).
func OneShots(region, accountID string, now time.Time, role bool) []OneShot {
	at := func(o OneShot) OneShot { o.At = now.UTC().Add(o.Offset).Truncate(time.Second); return o }
	out := []OneShot{at(OneShot{
		Name: JobFunctionDelete, Offset: FunctionOffset,
		Target:   sdkTarget("lambda", "deleteFunction"),
		Input:    jobInput(map[string]string{"FunctionName": FunctionName}),
		Resource: FunctionARN(region, accountID),
		Why:      "the reaper cannot delete the function it runs in",
	})}
	if role {
		out = append(out,
			at(OneShot{
				Name: JobRoleInline, Offset: RolePolicyOffset,
				Target:   sdkTarget("iam", "deleteRolePolicy"),
				Input:    jobInput(map[string]string{"RoleName": RoleName, "PolicyName": PolicyName}),
				Resource: RoleARN(accountID),
				Why:      "the reaper role is emptied after the function that runs as it is gone",
			}),
			at(OneShot{
				Name: JobRoleManaged, Offset: RolePolicyOffset,
				Target:   sdkTarget("iam", "detachRolePolicy"),
				Input:    jobInput(map[string]string{"RoleName": RoleName, "PolicyArn": BasicExecutionPolicyARN}),
				Resource: RoleARN(accountID),
				Why:      "the reaper role is emptied after the function that runs as it is gone",
			}),
			at(OneShot{
				Name: JobRole, Offset: RoleOffset,
				Target:   sdkTarget("iam", "deleteRole"),
				Input:    jobInput(map[string]string{"RoleName": RoleName}),
				Resource: RoleARN(accountID),
				Why:      "the reaper cannot delete the role it runs as",
			}))
	}
	return append(out, at(OneShot{
		Name: JobLogGroup, Offset: LogGroupOffset,
		Target:   sdkTarget("cloudwatchlogs", "deleteLogGroup"),
		Input:    jobInput(map[string]string{"LogGroupName": LogGroupName()}),
		Resource: LogGroupARN(region, accountID),
		Why:      "after the function's last log flush: deleted earlier, Lambda recreates it untagged",
	}))
}

// SchedulerAPI is the EventBridge Scheduler slice the one-shot jobs are
// created with, and that `spawn reaper teardown` deletes their group with.
// *scheduler.Client satisfies it.
type SchedulerAPI interface {
	CreateScheduleGroup(ctx context.Context, in *scheduler.CreateScheduleGroupInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleGroupOutput, error)
	CreateSchedule(ctx context.Context, in *scheduler.CreateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error)
	DeleteScheduleGroup(ctx context.Context, in *scheduler.DeleteScheduleGroupInput, optFns ...func(*scheduler.Options)) (*scheduler.DeleteScheduleGroupOutput, error)
}

// ScheduleInput is the CreateSchedule request for one job.
func ScheduleInput(o OneShot, invokeRoleARN string) *scheduler.CreateScheduleInput {
	return &scheduler.CreateScheduleInput{
		Name:                  awssdk.String(o.Name),
		GroupName:             awssdk.String(TeardownScheduleGroup),
		ScheduleExpression:    awssdk.String(o.Expression()),
		FlexibleTimeWindow:    &schedulertypes.FlexibleTimeWindow{Mode: schedulertypes.FlexibleTimeWindowModeOff},
		ActionAfterCompletion: schedulertypes.ActionAfterCompletionDelete,
		Description:           awssdk.String("self-removal of the self-hosted TTL reaper (spawn#1655): " + o.Why),
		Target: &schedulertypes.Target{
			Arn:     awssdk.String(o.Target),
			RoleArn: awssdk.String(invokeRoleARN),
			Input:   awssdk.String(o.Input),
		},
	}
}

func isConflict(err error) bool {
	var c *schedulertypes.ConflictException
	return errors.As(err, &c)
}

// CreateOneShots creates TeardownScheduleGroup, tagged, and then each job in
// it. Both are idempotent: a ConflictException (the group or the job already
// exists, from a run that crashed after creating it) counts as success, so a
// resumed removal carries on where it stopped. before, when set, runs before
// each job is created (the lease heartbeat); its error stops the creation.
func CreateOneShots(ctx context.Context, api SchedulerAPI, region, accountID, version string, jobs []OneShot,
	before func(ctx context.Context, step string) error) (created []string, err error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	if api == nil {
		return nil, errors.New("reaperdeploy: no Scheduler client, so the one-shot jobs cannot be created")
	}
	_, err = api.CreateScheduleGroup(ctx, &scheduler.CreateScheduleGroupInput{
		Name: awssdk.String(TeardownScheduleGroup),
		Tags: schedulertags.FromMap(scopedTags(version, region)),
	})
	if err != nil && !isConflict(err) {
		return nil, fmt.Errorf("create schedule group %s: %w", TeardownScheduleGroup, err)
	}
	invoke := TeardownInvokeRoleARN(accountID)
	for _, j := range jobs {
		if before != nil {
			if err := before(ctx, "create one-shot "+j.Name); err != nil {
				return created, err
			}
		}
		if _, err := api.CreateSchedule(ctx, ScheduleInput(j, invoke)); err != nil && !isConflict(err) {
			return created, fmt.Errorf("create one-shot teardown job %s: %w", j.Name, err)
		}
		created = append(created, j.Name)
	}
	return created, nil
}

// How a self-removal step is carried out.
const (
	// HowDirect: the reaper does it itself, in the run that plans it.
	HowDirect = "direct"
	// HowOneShot: a one-shot Scheduler job does it, after the run.
	HowOneShot = "one-shot"
	// HowKept: the plan leaves it, and Reason says why.
	HowKept = "kept"
)

// SelfRemovalStep is one line of the plan.
type SelfRemovalStep struct {
	// Step names what is done ("remove-target", "delete-rule",
	// "remove-permission", "create-schedule-group", "one-shot",
	// "delete-artifact-bucket").
	Step string
	How  string
	// Offset is when it happens, from the plan: zero for a direct step.
	Offset time.Duration
	// Target is the API call: "events:RemoveTargets", or a one-shot's
	// universal target ARN.
	Target   string
	Resource string
	Reason   string
	// Job is the one-shot, for a HowOneShot step.
	Job *OneShot
}

// ArtifactBucket is what the plan found in the reaper's artifact bucket.
type ArtifactBucket struct {
	Name   string
	Exists bool
	// Tagged: spawn:managed=true, positive evidence spawn made it.
	Tagged bool
	// Keys are its objects, all under ttl-reaper/ (a bucket holding anything
	// else is ErrRepurposedBucket).
	Keys []string
	// TagErr is why the tags could not be read, when they could not.
	TagErr error
}

// ErrRepurposedBucket: the artifact bucket holds an object spawn never wrote,
// so the reaper is entangled with someone's data and does not remove itself.
var ErrRepurposedBucket = errors.New("the artifact bucket holds objects spawn never wrote")

// inspectArtifactBucket reads the bucket's existence, tags and keys, and
// changes nothing. A bucket that does not exist is Exists=false with no
// error; one that cannot be read, or that holds anything outside
// artifactKeyPrefix, is an error.
func (d *Deployer) inspectArtifactBucket(ctx context.Context, bucket string) (ArtifactBucket, error) {
	b := ArtifactBucket{Name: bucket}
	if _, err := d.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: awssdk.String(bucket)}); err != nil {
		if isNoSuchBucket(err) {
			return b, nil
		}
		return b, fmt.Errorf("read bucket %s: %w", bucket, err)
	}
	b.Exists = true
	out, err := d.S3.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: awssdk.String(bucket)})
	if err != nil {
		b.TagErr = err
	} else {
		for _, t := range out.TagSet {
			if awssdk.ToString(t.Key) == "spawn:managed" && awssdk.ToString(t.Value) == "true" {
				b.Tagged = true
			}
		}
	}
	var token *string
	for {
		page, err := d.S3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: awssdk.String(bucket), ContinuationToken: token})
		if err != nil {
			return b, fmt.Errorf("list %s: %w", bucket, err)
		}
		for _, o := range page.Contents {
			k := awssdk.ToString(o.Key)
			if !strings.HasPrefix(k, artifactKeyPrefix) {
				return b, fmt.Errorf("%w: bucket %s holds %q", ErrRepurposedBucket, bucket, k)
			}
			b.Keys = append(b.Keys, k)
		}
		if page.NextContinuationToken == nil {
			break
		}
		token = page.NextContinuationToken
	}
	return b, nil
}

// isNoSuchBucket reports a HeadBucket that found no bucket: S3 answers 404
// (NotFound, NoSuchBucket) with no body.
func isNoSuchBucket(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchBucket":
			return true
		}
	}
	return false
}

// SelfRemovalInput is what PlanSelfRemoval is given.
type SelfRemovalInput struct {
	AccountID, Region string
	// Now is when the plan is made; the one-shot offsets count from it.
	Now time.Time
	// Regions are the account's enabled regions: a reaper deployed with the
	// same role in any but Region keeps the role. RegionsErr is why they
	// could not be listed, which keeps it too.
	Regions    []string
	RegionsErr error
	// LambdaIn is a Lambda client in a region, for that check. Nil, or an
	// error from it, keeps the role.
	LambdaIn func(region string) LambdaAPI
}

// SelfRemovalPlan is the self-hosted reaper's removal of itself, in the order
// it is carried out.
type SelfRemovalPlan struct {
	AccountID, Region string
	PlannedAt         time.Time
	// Version is the function's spawn:version, which the schedule group is
	// tagged with.
	Version string
	Bucket  ArtifactBucket
	// RoleKept says why the reaper's role is not removed, when it is not.
	RoleKept string
	Steps    []SelfRemovalStep
}

// OneShots are the plan's jobs, in order.
func (p *SelfRemovalPlan) OneShots() []OneShot {
	var out []OneShot
	for _, s := range p.Steps {
		if s.Job != nil {
			out = append(out, *s.Job)
		}
	}
	return out
}

// Count counts the steps carried out how.
func (p *SelfRemovalPlan) Count(how string) int {
	n := 0
	for _, s := range p.Steps {
		if s.How == how {
			n++
		}
	}
	return n
}

// PlanSelfRemoval computes the self-hosted reaper's removal of itself and
// changes nothing: it reads the function's tags (for the artifact bucket it
// was deployed from), the bucket, and, in the account's other enabled
// regions, whether a reaper there shares the role.
//
// The order is the order a reap would carry it out in (spawn#1657), chosen so
// a failure part-way leaves a reaper that runs again and resumes:
//
//  1. the schedule group and the one-shot jobs, first: if they cannot be
//     created, the rules still invoke the reaper, whose next run tries again
//     with the same job names;
//  2. the artifact bucket, emptied and deleted, when it is tagged
//     spawn:managed=true (a Lambda keeps its own copy of the code, so the
//     function does not need it);
//  3. last, the two rules' targets, the rules and the invoke permissions,
//     which stop the reaper being invoked again.
//
// It returns an error, and no plan, when the bucket cannot be read or holds
// an object spawn never wrote (ErrRepurposedBucket).
func (d *Deployer) PlanSelfRemoval(ctx context.Context, in SelfRemovalInput) (*SelfRemovalPlan, error) {
	if in.AccountID == "" || in.Region == "" {
		return nil, errors.New("reaperdeploy: self-removal needs the account and region")
	}
	bucket, version, err := d.ownArtifactBucket(ctx, in.AccountID, in.Region)
	if err != nil {
		return nil, err
	}
	b, err := d.inspectArtifactBucket(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("reaperdeploy: self-removal not planned: %w", err)
	}
	p := &SelfRemovalPlan{AccountID: in.AccountID, Region: in.Region, PlannedAt: in.Now.UTC(), Bucket: b, Version: version}

	role := true
	if others, err := otherReaperRegions(ctx, in); err != nil {
		role, p.RoleKept = false, "could not confirm no other region's reaper runs as it: "+err.Error()
	} else if len(others) > 0 {
		role, p.RoleKept = false, "the reaper in "+strings.Join(others, ", ")+" runs as it too"
	}

	// 1. The jobs.
	p.Steps = append(p.Steps, SelfRemovalStep{
		Step: "create-schedule-group", How: HowDirect, Target: "scheduler:CreateScheduleGroup",
		Resource: TeardownScheduleGroupARN(in.Region, in.AccountID),
		Reason:   "holds the one-shot jobs; residue once they have run (libs#78)",
	})
	for _, j := range OneShots(in.Region, in.AccountID, in.Now, role) {
		p.Steps = append(p.Steps, SelfRemovalStep{
			Step: "one-shot", How: HowOneShot, Offset: j.Offset, Target: j.Target, Resource: j.Resource,
			Reason: j.Why, Job: &j,
		})
	}
	if !role {
		p.Steps = append(p.Steps, SelfRemovalStep{
			Step: "delete-role", How: HowKept, Target: sdkTarget("iam", "deleteRole"),
			Resource: RoleARN(in.AccountID), Reason: p.RoleKept,
		})
	}

	// 2. The bucket.
	bs := SelfRemovalStep{Step: "delete-artifact-bucket", How: HowDirect, Target: "s3:DeleteBucket",
		Resource: "arn:aws:s3:::" + b.Name}
	switch {
	case !b.Exists:
		bs.How, bs.Reason = HowKept, "already gone"
	case !b.Tagged:
		why := "not tagged spawn:managed=true, so it cannot be confirmed as spawn's; 'spawn reaper teardown --force-artifacts' removes it"
		if b.TagErr != nil {
			why += " (tags unreadable: " + b.TagErr.Error() + ")"
		}
		bs.How, bs.Reason = HowKept, why
	default:
		bs.Reason = fmt.Sprintf("emptied (%d ttl-reaper/ object(s)) and deleted", len(b.Keys))
	}
	p.Steps = append(p.Steps, bs)

	// 3. The triggers, last.
	for _, rule := range []string{RuleName, InstanceEventRuleName} {
		arn := ruleARN(in.Region, in.AccountID, rule)
		p.Steps = append(p.Steps,
			SelfRemovalStep{Step: "remove-target", How: HowDirect, Target: "events:RemoveTargets", Resource: arn},
			SelfRemovalStep{Step: "delete-rule", How: HowDirect, Target: "events:DeleteRule", Resource: arn},
			SelfRemovalStep{Step: "remove-permission", How: HowDirect, Target: "lambda:RemovePermission",
				Resource: FunctionARN(in.Region, in.AccountID), Reason: "statement " + rule},
		)
	}
	return p, nil
}

// ownArtifactBucket is the bucket the function records it was deployed from
// (ArtifactBucketTag), or the default name for one deployed before that was
// recorded, as `spawn reaper teardown` resolves it; and the function's
// spawn:version, for the schedule group's tags.
func (d *Deployer) ownArtifactBucket(ctx context.Context, accountID, region string) (bucket, version string, err error) {
	out, err := d.Lambda.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: awssdk.String(FunctionName)})
	if err != nil {
		return "", "", fmt.Errorf("reaperdeploy: self-removal not planned: read the function's tags: %w", err)
	}
	version = out.Tags["spawn:version"]
	if b := out.Tags[ArtifactBucketTag]; b != "" {
		return b, version, nil
	}
	return DefaultBucketName(accountID, region), version, nil
}

// otherReaperRegions are the regions besides in.Region with a deployed
// reaper function, sorted.
func otherReaperRegions(ctx context.Context, in SelfRemovalInput) ([]string, error) {
	if in.RegionsErr != nil {
		return nil, fmt.Errorf("the enabled regions could not be listed: %w", in.RegionsErr)
	}
	if len(in.Regions) == 0 {
		return nil, errors.New("no enabled regions were listed")
	}
	var others []string
	for _, region := range in.Regions {
		if region == in.Region {
			continue
		}
		if in.LambdaIn == nil {
			return nil, errors.New("no Lambda client for the other regions")
		}
		api := in.LambdaIn(region)
		if api == nil {
			return nil, fmt.Errorf("no Lambda client for %s", region)
		}
		_, err := api.GetFunction(ctx, &lambda.GetFunctionInput{FunctionName: awssdk.String(FunctionName)})
		var nf *lambdatypes.ResourceNotFoundException
		switch {
		case err == nil:
			others = append(others, region)
		case errors.As(err, &nf):
		default:
			return nil, fmt.Errorf("%s: %w", region, err)
		}
	}
	sort.Strings(others)
	return others, nil
}

// TeardownInvokeTrustPolicy trusts EventBridge Scheduler acting for this
// account only (aws:SourceAccount, the confused-deputy guard Scheduler
// documents). No other principal, and no other account.
func TeardownInvokeTrustPolicy(accountID string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"scheduler.amazonaws.com"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":%q}}}]}`, accountID)
}

// policyStatement is one statement of a policy rendered here.
type policyStatement struct {
	Sid       string      `json:"Sid,omitempty"`
	Effect    string      `json:"Effect"`
	Action    []string    `json:"Action"`
	Resource  []string    `json:"Resource"`
	Condition interface{} `json:"Condition,omitempty"`
}

// serviceGuardStatement is the explicit Deny on anything tagged
// spore-host:role=service, spore.host's live service (spore-host#750). spawn
// never puts that tag on the self-hosted reaper (Tags), so it never denies
// what the plan deletes; it stops a later, wider grant reaching the service.
func serviceGuardStatement() policyStatement {
	return policyStatement{Sid: "NeverTheService", Effect: "Deny", Action: []string{"*"}, Resource: []string{"*"},
		Condition: map[string]map[string]string{"StringEquals": {"aws:ResourceTag/spore-host:role": "service"}}}
}

// boundaryPolicyARN is spawn's permissions boundary, the one permanent piece
// of a spore.host deployment (pkg/instancerole.BoundaryPolicyName at
// SporedPolicyPath; a test holds them equal).
func boundaryPolicyARN(accountID string) string {
	return fmt.Sprintf("arn:aws:iam::%s:policy/spore-host/spawn-instance-boundary", accountID)
}

// TeardownInvokeStatements are TeardownInvokePolicyName's statements: exactly
// what the one-shot jobs delete, by ARN, and nothing else. The function and
// its log group in any region (the role is account-wide, and a reaper may be
// deployed in more than one), the reaper's role emptied of its inline policy
// and its one managed policy, then deleted. Never the invoke role itself.
// Plus the explicit Denies on the service and on the boundary.
func TeardownInvokeStatements(accountID string) []policyStatement {
	logGroup := fmt.Sprintf("arn:aws:logs:*:%s:log-group:%s", accountID, LogGroupName())
	return []policyStatement{
		{Sid: "DeleteTheReaperFunction", Effect: "Allow", Action: []string{"lambda:DeleteFunction"},
			Resource: []string{fmt.Sprintf("arn:aws:lambda:*:%s:function:%s", accountID, FunctionName)}},
		// Both forms: CloudWatch Logs authorizes some actions against the
		// group's ARN and some against it with a trailing ":*".
		{Sid: "DeleteTheReaperLogGroup", Effect: "Allow", Action: []string{"logs:DeleteLogGroup"},
			Resource: []string{logGroup, logGroup + ":*"}},
		{Sid: "EmptyAndDeleteTheReaperRole", Effect: "Allow", Action: []string{"iam:DeleteRolePolicy", "iam:DeleteRole"},
			Resource: []string{RoleARN(accountID)}},
		{Sid: "DetachTheReaperLogsPolicy", Effect: "Allow", Action: []string{"iam:DetachRolePolicy"},
			Resource:  []string{RoleARN(accountID)},
			Condition: map[string]map[string]string{"ArnEquals": {"iam:PolicyARN": BasicExecutionPolicyARN}}},
		serviceGuardStatement(),
		{Sid: "NeverTheBoundary", Effect: "Deny", Action: []string{
			"iam:Create*", "iam:Delete*", "iam:Put*", "iam:Set*", "iam:Tag*", "iam:Untag*",
			"iam:Update*", "iam:Attach*", "iam:Detach*",
		}, Resource: []string{boundaryPolicyARN(accountID)}},
	}
}

// TeardownInvokePolicyDocument renders TeardownInvokeStatements.
func TeardownInvokePolicyDocument(accountID string) (string, error) {
	if accountID == "" {
		return "", errors.New("reaperdeploy: the teardown invoke policy needs the account")
	}
	b, err := json.Marshal(map[string]interface{}{"Version": "2012-10-17", "Statement": TeardownInvokeStatements(accountID)})
	if err != nil {
		return "", fmt.Errorf("reaperdeploy: render the teardown invoke policy: %w", err)
	}
	return string(b), nil
}

// ensureTeardownInvokeRole creates TeardownInvokeRoleName, tagged like the
// rest of the reaper (spore-host:managed-by=spawn, never spore-host:role),
// and writes its policy. At deploy time, so the self-removal never creates a
// role: by the time the account is idle it only names one that exists.
func (d *Deployer) ensureTeardownInvokeRole(ctx context.Context, opts Options) (bool, error) {
	doc, err := TeardownInvokePolicyDocument(opts.AccountID)
	if err != nil {
		return false, err
	}
	created := false
	if _, err := d.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: awssdk.String(TeardownInvokeRoleName)}); err != nil {
		var nse *iamtypes.NoSuchEntityException
		if !errors.As(err, &nse) {
			return false, fmt.Errorf("reaperdeploy: get role %s: %w", TeardownInvokeRoleName, err)
		}
		if _, err := d.IAM.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 awssdk.String(TeardownInvokeRoleName),
			AssumeRolePolicyDocument: awssdk.String(TeardownInvokeTrustPolicy(opts.AccountID)),
			Description:              awssdk.String("Runs the spawn reaper's one-shot self-removal jobs; may delete only the reaper's function, log group and role (spawn#1655)"),
			Tags:                     iamTags(opts.Version),
		}); err != nil {
			return false, fmt.Errorf("reaperdeploy: create role %s: %w", TeardownInvokeRoleName, err)
		}
		created = true
	}
	if _, err := d.IAM.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:       awssdk.String(TeardownInvokeRoleName),
		PolicyName:     awssdk.String(TeardownInvokePolicyName),
		PolicyDocument: awssdk.String(doc),
	}); err != nil {
		return created, fmt.Errorf("reaperdeploy: put role policy on %s: %w", TeardownInvokeRoleName, err)
	}
	return created, nil
}

// SelfRemovalStatements are what the self-hosted reaper's own role needs, on
// top of pkg/reaperiam's scan-self set, to plan its removal and (spawn#1657)
// hand the last steps to the one-shot jobs. Only `spawn reaper deploy`'s role
// carries them: the operated reaper and the member accounts' role never
// remove themselves, so they are not in reaperiam.ScanSelfStatements, which
// those share.
//
//   - lambda:GetFunction on the reaper function, in any region: its own tags
//     (the artifact bucket) and whether another region's reaper shares the
//     role;
//   - s3:ListBucket (HeadBucket, ListObjectsV2) and s3:GetBucketTagging on the
//     artifact bucket: the default spawn-reaper-artifacts-<account>-<region>
//     in any region, and bucket when it is another (--bucket);
//   - scheduler:CreateSchedule in TeardownScheduleGroup only, and
//     scheduler:CreateScheduleGroup and scheduler:TagResource on that group;
//   - iam:PassRole on TeardownInvokeRoleName only, to Scheduler only.
//
// None of them deletes anything. The reaper's role can delete neither its own
// function, nor its log group, nor itself (TestTheReaperCannotDeleteItself).
func SelfRemovalStatements(accountID, bucket string) []reaperiam.Statement {
	out := []reaperiam.Statement{
		{Effect: "Allow", Action: []string{"lambda:GetFunction"},
			Resource: fmt.Sprintf("arn:aws:lambda:*:%s:function:%s", accountID, FunctionName)},
		{Effect: "Allow", Action: []string{"s3:ListBucket", "s3:GetBucketTagging"},
			Resource: "arn:aws:s3:::" + DefaultBucketName(accountID, "*")},
	}
	if bucket != "" && !strings.HasPrefix(bucket, DefaultBucketName(accountID, "")) {
		out = append(out, reaperiam.Statement{Effect: "Allow", Action: []string{"s3:ListBucket", "s3:GetBucketTagging"},
			Resource: "arn:aws:s3:::" + bucket})
	}
	return append(out,
		reaperiam.Statement{Effect: "Allow", Action: []string{"scheduler:CreateSchedule"},
			Resource: fmt.Sprintf("arn:aws:scheduler:*:%s:schedule/%s/*", accountID, TeardownScheduleGroup)},
		reaperiam.Statement{Effect: "Allow", Action: []string{"scheduler:CreateScheduleGroup", "scheduler:TagResource"},
			Resource: fmt.Sprintf("arn:aws:scheduler:*:%s:schedule-group/%s", accountID, TeardownScheduleGroup)},
		reaperiam.Statement{Effect: "Allow", Action: []string{"iam:PassRole"},
			Resource:  TeardownInvokeRoleARN(accountID),
			Condition: map[string]map[string]string{"StringEquals": {"iam:PassedToService": "scheduler.amazonaws.com"}}},
	)
}

// PolicyDocument is the one inline policy `spawn reaper deploy` writes on
// the reaper's role: reaperiam's scan-self and own-account statements, and
// SelfRemovalStatements.
func PolicyDocument(accountID, bucket string) (string, error) {
	if accountID == "" {
		return "", errors.New("reaperdeploy: the reaper policy needs the account")
	}
	st := append(reaperiam.ScanSelfStatements(), reaperiam.OwnAccountStatements()...)
	st = append(st, SelfRemovalStatements(accountID, bucket)...)
	b, err := json.Marshal(map[string]interface{}{"Version": "2012-10-17", "Statement": st})
	if err != nil {
		return "", fmt.Errorf("reaperdeploy: marshal policy: %w", err)
	}
	return string(b), nil
}
