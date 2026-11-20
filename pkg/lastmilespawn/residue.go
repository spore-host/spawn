package lastmilespawn

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/spore-host/libs/footprint"
	"github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/iamtags"
	"github.com/spore-host/libs/tags/schedulertags"

	fs "github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/reaperdeploy"
	"github.com/spore-host/spawn/pkg/teardown"
)

// TypeScheduleGroup is the footprint type of an EventBridge Scheduler
// schedule group.
const TypeScheduleGroup = "AWS::Scheduler::ScheduleGroup"

// Residue is spawn's residue in account: what the self-hosted reaper's
// self-removal (spawn#1655) leaves once its one-shot jobs have deleted the
// reaper's function, role and log group. That is the role the jobs ran as
// (reaperdeploy.TeardownInvokeRoleName, account-wide, made by `spawn reaper
// deploy`) and, in each region, the schedule group they ran from
// (reaperdeploy.TeardownScheduleGroup). Each group depends on the role, so a
// group that still holds a job keeps the role too.
//
// libs/lastmile judges them removable only once no spawn lifecycle item is
// left in any scope, so regions must be every region spawn can have used.
// The spore-host-lifecycle tables are not here: they are the suite's, and
// lastmile reports each empty one itself (CloseTableIfEmpty re-checks every
// tool's items).
func Residue(account string, regions []string) []footprint.Resource {
	managed := func() map[string]string { return map[string]string{tags.ManagedBy: string(fs.Tool)} }
	role := reaperdeploy.TeardownInvokeRoleARN(account)
	out := []footprint.Resource{{
		ARN: role, Type: fs.TypeRole, Region: fs.RegionGlobal, Name: reaperdeploy.TeardownInvokeRoleName,
		Ownership: footprint.OwnershipTagged, Class: footprint.ClassResidue, Tags: managed(),
	}}
	for _, r := range regions {
		out = append(out, footprint.Resource{
			ARN: reaperdeploy.TeardownScheduleGroupARN(r, account), Type: TypeScheduleGroup, Region: r,
			Name: reaperdeploy.TeardownScheduleGroup, Ownership: footprint.OwnershipTagged, Class: footprint.ClassResidue,
			DependsOn: []string{role}, Tags: managed(),
		})
	}
	return out
}

// Patterns are spawn's teardown patterns (footprintspawn.Patterns) and the
// residue's two exact names. pkg/teardown has no deleter for either (`spawn
// reaper teardown` removes them), so they are not in footprintspawn.Patterns,
// and only the last mile's plans, re-checked by ResidueChecker, use them.
func Patterns() []footprint.NamePattern {
	return append(append([]footprint.NamePattern(nil), fs.Patterns...),
		footprint.MustPattern(fs.TypeRole, regexp.QuoteMeta(reaperdeploy.TeardownInvokeRoleName)),
		footprint.MustPattern(TypeScheduleGroup, regexp.QuoteMeta(reaperdeploy.TeardownScheduleGroup)),
	)
}

// IAMAPI is the IAM read ResidueChecker makes. *iam.Client satisfies it.
type IAMAPI interface {
	GetRole(ctx context.Context, in *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
}

// SchedulerAPI are the Scheduler reads ResidueChecker makes.
// *scheduler.Client satisfies it.
type SchedulerAPI interface {
	GetScheduleGroup(ctx context.Context, in *scheduler.GetScheduleGroupInput, optFns ...func(*scheduler.Options)) (*scheduler.GetScheduleGroupOutput, error)
	ListTagsForResource(ctx context.Context, in *scheduler.ListTagsForResourceInput, optFns ...func(*scheduler.Options)) (*scheduler.ListTagsForResourceOutput, error)
	ListSchedules(ctx context.Context, in *scheduler.ListSchedulesInput, optFns ...func(*scheduler.Options)) (*scheduler.ListSchedulesOutput, error)
}

// ResidueChecker re-reads the residue from the account, as a Deleter's
// Describe does: an error wrapping footprint.ErrNotFound when it is gone, and
// one wrapping teardown.ErrInUse when it is in use. It reads only these few
// resources, never a whole footprint.
type ResidueChecker struct {
	Account   string
	IAM       IAMAPI
	Scheduler func(region string) SchedulerAPI

	iamLatch   Latch
	schedLatch Latch
}

// NewResidueChecker is a ResidueChecker over cfg's credentials.
func NewResidueChecker(cfg aws.Config, account string) *ResidueChecker {
	return &ResidueChecker{
		Account:    account,
		iamLatch:   Latch{What: "IAM read"},
		schedLatch: Latch{What: "Scheduler read"},
		IAM:        iam.NewFromConfig(cfg),
		Scheduler: func(region string) SchedulerAPI {
			c := cfg.Copy()
			c.Region = region
			return scheduler.NewFromConfig(c)
		},
	}
}

// Owns reports whether r is one of the residue's resources (by its ARN and
// type), which Describe re-checks.
func (c *ResidueChecker) Owns(r footprint.Resource) bool {
	switch r.Type {
	case fs.TypeRole:
		return r.ARN == reaperdeploy.TeardownInvokeRoleARN(c.Account)
	case TypeScheduleGroup:
		return r.Region != "" && r.ARN == reaperdeploy.TeardownScheduleGroupARN(r.Region, c.Account)
	}
	return false
}

// Describe re-reads r, one of the residue's resources.
func (c *ResidueChecker) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	switch {
	case !c.Owns(r):
		return footprint.Resource{}, fmt.Errorf("%s is not the reaper's teardown residue", r.ARN)
	case r.Type == fs.TypeRole:
		return c.describeRole(ctx, r)
	}
	return c.describeGroup(ctx, r)
}

func (c *ResidueChecker) describeRole(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	if c.IAM == nil {
		return footprint.Resource{}, errors.New("no IAM client")
	}
	if err := c.iamLatch.Err(); err != nil {
		return footprint.Resource{}, err
	}
	out, err := c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(reaperdeploy.TeardownInvokeRoleName)})
	var nse *iamtypes.NoSuchEntityException
	switch {
	case errors.As(err, &nse):
		return footprint.Resource{}, fmt.Errorf("%s: %w", r.ARN, footprint.ErrNotFound)
	case err != nil:
		c.iamLatch.Note(err)
		return footprint.Resource{}, fmt.Errorf("read role %s: %w", reaperdeploy.TeardownInvokeRoleName, err)
	}
	var t map[string]string
	if out != nil && out.Role != nil {
		t = iamtags.ToMap(out.Role.Tags)
	}
	live := residueLive(r, t)

	// The jobs that remove the reaper's own role run as this one, so it is
	// in use while that role is there: a reaper still deployed, or a
	// self-removal that has not finished.
	_, err = c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(reaperdeploy.RoleName)})
	switch {
	case err == nil:
		live.Class = footprint.ClassActive
		return live, fmt.Errorf("kept %s: %w: the self-hosted reaper's role %s still exists, and the jobs that remove it run as this role "+
			"('spawn reaper teardown' removes both)", r.Name, teardown.ErrInUse, reaperdeploy.RoleName)
	case errors.As(err, &nse):
		return live, nil
	}
	c.iamLatch.Note(err)
	return footprint.Resource{}, fmt.Errorf("read role %s: %w", reaperdeploy.RoleName, err)
}

func (c *ResidueChecker) describeGroup(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	if c.Scheduler == nil {
		return footprint.Resource{}, errors.New("no Scheduler client")
	}
	if err := c.schedLatch.Err(); err != nil {
		return footprint.Resource{}, err
	}
	api := c.Scheduler(r.Region)
	name := aws.String(reaperdeploy.TeardownScheduleGroup)
	_, err := api.GetScheduleGroup(ctx, &scheduler.GetScheduleGroupInput{Name: name})
	var nf *schedulertypes.ResourceNotFoundException
	switch {
	case errors.As(err, &nf):
		return footprint.Resource{}, fmt.Errorf("%s: %w", r.ARN, footprint.ErrNotFound)
	case err != nil:
		c.schedLatch.Note(err)
		return footprint.Resource{}, fmt.Errorf("read schedule group %s in %s: %w", reaperdeploy.TeardownScheduleGroup, r.Region, err)
	}
	tg, err := api.ListTagsForResource(ctx, &scheduler.ListTagsForResourceInput{ResourceArn: aws.String(r.ARN)})
	if err != nil {
		c.schedLatch.Note(err)
		return footprint.Resource{}, fmt.Errorf("read the tags of schedule group %s in %s: %w", reaperdeploy.TeardownScheduleGroup, r.Region, err)
	}
	live := residueLive(r, schedulertags.ToMap(tg.Tags))
	jobs, err := api.ListSchedules(ctx, &scheduler.ListSchedulesInput{GroupName: name, MaxResults: aws.Int32(1)})
	switch {
	case err != nil:
		c.schedLatch.Note(err)
		return footprint.Resource{}, fmt.Errorf("list the jobs in schedule group %s in %s: %w", reaperdeploy.TeardownScheduleGroup, r.Region, err)
	case len(jobs.Schedules) > 0:
		live.Class = footprint.ClassActive
		return live, fmt.Errorf("kept %s in %s: %w: it still holds the reaper's self-removal job %s",
			r.Name, r.Region, teardown.ErrInUse, aws.ToString(jobs.Schedules[0].Name))
	}
	return live, nil
}

// residueLive is r as the account lists it now: its live tags, and the
// ownership they give it.
func residueLive(r footprint.Resource, t map[string]string) footprint.Resource {
	live := r
	live.Tags = t
	live.Ownership = fs.OwnershipOf(r.Name, t)
	live.Class = footprint.ClassResidue
	return live
}

// ReportOnlyDeleter re-checks a plan as spawn's teardown would and deletes
// nothing: the residue through Residue, everything else through Plan
// (pkg/teardown.NewReportOnly, the deleter the reaper's account-idle sweep
// plans with). Delete always returns ErrReportOnly.
type ReportOnlyDeleter struct {
	Residue *ResidueChecker
	Plan    footprint.Deleter
}

// NewReportOnlyDeleter is ReportOnlyDeleter over cfg's credentials in
// account. RoleReferences are left uncollected (they are in the infra
// account), so no per-task role or profile is ever planned for deletion.
func NewReportOnlyDeleter(cfg aws.Config, account string, now func() time.Time) ReportOnlyDeleter {
	return ReportOnlyDeleter{
		Residue: NewResidueChecker(cfg, account),
		Plan:    teardown.NewReportOnly(cfg, teardown.Options{Account: account, Now: now}),
	}
}

// Describe re-reads r.
func (d ReportOnlyDeleter) Describe(ctx context.Context, r footprint.Resource) (footprint.Resource, error) {
	if d.Residue != nil && d.Residue.Owns(r) {
		return d.Residue.Describe(ctx, r)
	}
	if d.Plan == nil {
		return footprint.Resource{}, errors.New("no deleter to re-check with")
	}
	return d.Plan.Describe(ctx, r)
}

// Delete refuses.
func (ReportOnlyDeleter) Delete(context.Context, footprint.Resource) error { return ErrReportOnly }
