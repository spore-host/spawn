package main

import (
	"context"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/reaperconfig"
)

// The per-task IAM role sweep (#1300): spawn-instance-<hash> roles and their
// instance profiles, which pkg/aws.CreateOrGetInstanceProfile creates one per
// distinct policy config and nothing else deletes. pkg/instancerole holds the
// rule and the delete; this file wires it to the reaper's accounts and its
// REAPER_INSTANCE_ROLES switch.
//
// IAM is global, so the sweep runs once per account per run, after the regional
// scans. A role is only deleted when no instance in ANY region enabled in the
// account (DescribeRegions, plus REAPER_REGIONS) has its profile attached — not
// only the regions the reaper scans, since a stopped instance in an unscanned
// region would otherwise come back with no credentials. If the region list or
// any region cannot be read, nothing in that account is deleted that run.
const (
	instanceRolesOff    = reaperconfig.ModeOff
	instanceRolesReport = reaperconfig.ModeReport
	instanceRolesReap   = reaperconfig.ModeReap
)

// iamAPI is the IAM slice the role sweep uses. Declared here, not only in
// pkg/instancerole, so pkg/reaperiam's drift gate discovers every IAM call the
// reaper makes and holds the three policies to granting them. It must keep at
// least instancerole.API's methods, or it would not compile as an argument to
// instancerole.Sweep.
type iamAPI interface {
	ListRoles(ctx context.Context, in *iam.ListRolesInput, optFns ...func(*iam.Options)) (*iam.ListRolesOutput, error)
	GetRole(ctx context.Context, in *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	GetInstanceProfile(ctx context.Context, in *iam.GetInstanceProfileInput, optFns ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
	ListInstanceProfilesForRole(ctx context.Context, in *iam.ListInstanceProfilesForRoleInput, optFns ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error)
	ListRolePolicies(ctx context.Context, in *iam.ListRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
	DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	ListAttachedRolePolicies(ctx context.Context, in *iam.ListAttachedRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	DetachRolePolicy(ctx context.Context, in *iam.DetachRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	RemoveRoleFromInstanceProfile(ctx context.Context, in *iam.RemoveRoleFromInstanceProfileInput, optFns ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error)
	DeleteInstanceProfile(ctx context.Context, in *iam.DeleteInstanceProfileInput, optFns ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error)
	DeleteRole(ctx context.Context, in *iam.DeleteRoleInput, optFns ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
}

// regionsAPI is the one call that lists the account's enabled regions, so the
// in-use check covers every region an instance could be in, not only the ones
// the reaper scans. Separate from ec2API so its fakes need not grow.
type regionsAPI interface {
	DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
}

// parseInstanceRoles maps REAPER_INSTANCE_ROLES onto a mode. Unset and any
// unrecognised value are 'report': this sweep is new deletion behaviour, so it
// starts report-only and deletes only once someone has read what it would do
// and set 'reap'.
func parseInstanceRoles(v string) string {
	if m, ok := reaperconfig.ParseMode(v); ok {
		return m
	}
	return instanceRolesReport
}

// reapInstanceRolesAccount runs the role sweep for one account.
func (r *reaper) reapInstanceRolesAccount(ctx context.Context, acct account, now time.Time, sum *Summary) {
	if acct.iamFor == nil || r.instanceRoles == instanceRolesOff {
		return
	}
	reap := r.instanceRoles == instanceRolesReap && !r.dryRun

	if acct.regionsFor == nil {
		log.Printf("instance-roles: %s: not swept, no way to list the account's enabled regions", acct.label)
		return
	}
	enabled, err := instancerole.EnabledRegions(ctx, acct.regionsFor(), r.regions...)
	if err != nil {
		log.Printf("instance-roles: %s: not swept, could not list enabled regions (%s): %v",
			acct.label, classifyScanError(err), err)
		if classifyScanError(err) != failureDenied {
			sum.Errors++
		}
		return
	}
	regions := make(map[string]instancerole.EC2Describer, len(enabled))
	for _, region := range enabled {
		regions[region] = acct.ec2For(region)
	}
	inUse, err := instancerole.ProfilesInUse(ctx, regions)
	if err != nil {
		// Fail closed: a role in use in the region we could not read would
		// look unused.
		log.Printf("instance-roles: %s: not swept, could not list instances (%s): %v",
			acct.label, classifyScanError(err), err)
		if classifyScanError(err) != failureDenied {
			sum.Errors++
		}
		return
	}

	res, err := instancerole.Sweep(ctx, acct.iamFor(), inUse, now, reap)
	if err != nil {
		cls := classifyScanError(err)
		log.Printf("instance-roles: %s: not swept (%s): %v", acct.label, cls, err)
		if cls == failureDenied {
			// The member role predates the #1300 grant: a standing
			// configuration fact, counted apart from Errors.
			sum.InstanceRolesAccountsDenied++
		} else {
			sum.Errors++
		}
		return
	}
	for _, d := range res.Decisions {
		switch {
		case d.Deleted:
			log.Printf("DELETED role %s and its instance profile in %s (created %s, last used %s)",
				d.Name, acct.label, stamp(d.Created), stamp(d.LastUsed))
		case d.Delete && d.Err != nil:
			log.Printf("instance-roles: %s: delete role %s failed: %v", acct.label, d.Name, d.Err)
			sum.Errors++
		case d.Delete:
			log.Printf("WOULD delete role %s and its instance profile in %s (created %s, last used %s; REAPER_INSTANCE_ROLES=%s, dry-run=%t)",
				d.Name, acct.label, stamp(d.Created), stamp(d.LastUsed), r.instanceRoles, r.dryRun)
		case d.Err != nil:
			log.Printf("instance-roles: %s: kept role %s: %s: %v", acct.label, d.Name, d.Reason, d.Err)
		}
	}
	scanned, deleted, would, skipped := res.Count()
	sum.InstanceRolesScanned += scanned
	sum.InstanceRolesDeleted += deleted
	sum.InstanceRolesWouldDelete += would
	sum.InstanceRolesSkipped += skipped
}

// stamp formats a time for the role log lines; "never" for zero.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
