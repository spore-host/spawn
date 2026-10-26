// Package instancerole decides when a per-task spawn-instance-<hash> IAM role
// and its instance profile are no longer needed, and removes them (#1300).
//
// pkg/aws.CreateOrGetInstanceProfile names a role and an instance profile
// spawn-instance-<hash of the policy config>. Every distinct set of input,
// output and results buckets mints a new pair, and before this package nothing
// ever deleted one, so a workflow fanned out across many bucket combinations
// walked an account towards the IAM role quota (1,000 by default). The owner's
// principle (spore-host#610) is that anything spore.host creates it removes once
// it is no longer needed; this package is the deleter.
//
// Two callers share it, so there is one definition of "no longer needed":
//
//   - the TTL reaper (lambda/ttl-reaper), once per member account per run, under
//     REAPER_INSTANCE_ROLES=off|report|reap;
//   - `spawn cleanup --instance-roles`, from a workstation, dry-run aware.
//
// A role and its profile are deleted only when ALL of Decide's conditions hold.
// The conditions are deliberately redundant: deleting a role an instance still
// uses removes that instance's credentials, and spored then cannot read its own
// tags, flush its task or terminate itself. Being wrong in the keep direction
// costs one IAM role; being wrong in the delete direction breaks a running
// workload. So every uncertainty resolves to keep.
//
// Not covered, by design:
//
//   - spored-instance-role / spored-instance-profile, the shared default
//     identity every IAM-flag-free launch uses. Not per-task, never deleted.
//   - spawn-pool-worker-<hash> roles, which `spawn pool drain` already removes
//     (cmd/pool.go removePoolWorkerIdentity).
//   - a role a user named with --iam-role. It does not carry the prefix.
//
// Untagged spawn-instance-* roles are kept. spawn has tagged these roles and
// profiles spawn:managed=true since the commit that introduced the
// spawn-instance-<hash> name (spore-host 52ae503, 2026-01-14, "Add IAM instance
// profile support"), so an untagged role with the prefix is not one spawn made
// and there is no legacy population to adopt by name.
package instancerole

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

const (
	// NamePrefix is the per-task role and instance-profile name prefix
	// (pkg/aws.PredictedInstanceRoleName). The IAM grants for the deletes are
	// scoped to exactly this prefix.
	NamePrefix = "spawn-instance-"

	// ManagedTagKey/ManagedTagValue mark a role or profile as spawn's. Literal,
	// not tagprefix-derived: pkg/aws.buildIAMTags writes this literal key, and
	// the IAM conditions on the deletes name it literally too.
	ManagedTagKey   = "spawn:managed"
	ManagedTagValue = "true"

	// SharedRoleName / SharedProfileName are the shared default identity
	// (pkg/aws.SporedRoleName / SporedInstanceProfileName, repeated here so the
	// reaper need not import pkg/aws). They never carry NamePrefix; IsCandidate
	// refuses them by name as well, so a future rename cannot make them
	// deletable by accident.
	SharedRoleName    = "spored-instance-role"
	SharedProfileName = "spored-instance-profile"

	// Grace is how old a role must be, and how long since IAM last saw it used,
	// before it may be deleted. A launch creates the role, then the profile,
	// then calls RunInstances; a day comfortably clears that window and any
	// launch retrying after a capacity error, and it means a role in use by a
	// running instance (whose credentials IAM refreshes, and whose spored
	// calls EC2 every minute) is never a candidate.
	Grace = 24 * time.Hour
)

// Reasons a role is kept. Exported so callers and tests can match them
// without parsing prose.
const (
	KeepNotPerTask      = "not a spawn-instance-* role"
	KeepShared          = "shared spored identity, never deleted"
	KeepUntagged        = "role not tagged spawn:managed=true"
	KeepProfileUntagged = "instance profile not tagged spawn:managed=true"
	KeepProfileYoung    = "instance profile younger than the 24h grace"
	KeepOtherProfiles   = "role is also in another instance profile"
	KeepInUse           = "instance profile attached to an instance"
	KeepNoCreateDate    = "role creation time unknown"
	KeepYoung           = "role younger than the 24h grace"
	KeepRecentlyUsed    = "role used within the 24h grace"
)

// liveStates are the instance states in which an instance may still need its
// profile: anything not yet terminated.
var liveStates = []string{"pending", "running", "stopping", "stopped", "shutting-down"}

// IsCandidate reports whether name is a per-task role or profile this package
// may consider at all.
func IsCandidate(name string) bool {
	if name == SharedRoleName || name == SharedProfileName {
		return false
	}
	return strings.HasPrefix(name, NamePrefix)
}

// Facts is everything Decide needs about one role.
type Facts struct {
	Name     string
	RoleTags map[string]string
	// ProfileExists says whether an instance profile of the same name exists;
	// ProfileTags are its tags when it does.
	ProfileExists bool
	ProfileTags   map[string]string
	// ProfileCreated is the profile's CreateDate. A launch that finds the
	// role but not the profile (a delete that got as far as the profile)
	// recreates the profile and re-adds the role before RunInstances, while
	// the role is still old and long unused, and no instance is visible yet.
	// The profile's own age is what shows that launch is in progress (#1318).
	ProfileCreated time.Time
	// OtherProfiles are instance profiles, other than the same-name one, that
	// contain this role. spawn never creates such a pairing.
	OtherProfiles []string
	// InUse: some instance in a checked region, in a live state, has the
	// same-name profile attached.
	InUse    bool
	Created  time.Time
	LastUsed time.Time // zero when IAM has no record of use
}

// Decide is THE predicate. It returns true only when every condition holds,
// and otherwise the first reason to keep the role.
func Decide(f Facts, now time.Time) (bool, string) {
	switch {
	case f.Name == SharedRoleName || f.Name == SharedProfileName:
		return false, KeepShared
	case !IsCandidate(f.Name):
		return false, KeepNotPerTask
	case f.RoleTags[ManagedTagKey] != ManagedTagValue:
		return false, KeepUntagged
	case f.ProfileExists && f.ProfileTags[ManagedTagKey] != ManagedTagValue:
		return false, KeepProfileUntagged
	case f.ProfileExists && (f.ProfileCreated.IsZero() || now.Sub(f.ProfileCreated) < Grace):
		return false, KeepProfileYoung
	case len(f.OtherProfiles) > 0:
		return false, KeepOtherProfiles
	case f.InUse:
		return false, KeepInUse
	case f.Created.IsZero():
		return false, KeepNoCreateDate
	case now.Sub(f.Created) < Grace:
		return false, KeepYoung
	case !f.LastUsed.IsZero() && now.Sub(f.LastUsed) < Grace:
		return false, KeepRecentlyUsed
	}
	return true, ""
}

// EC2Describer is the one EC2 call ProfilesInUse makes.
type EC2Describer interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// ProfileNameFromARN returns the profile name at the end of an instance
// profile ARN (arn:aws:iam::123:instance-profile/[path/]name).
func ProfileNameFromARN(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// ProfilesInUse returns the names of the per-task instance profiles attached
// to any instance in a live state in any of the given regions.
//
// It fails on the first region it cannot read: an incomplete answer would make
// a role in use there look unused, so the caller must delete nothing. It
// filters only on the instance state and reads each instance's profile from
// the response, rather than asking EC2 to match the profile ARN: a filter that
// silently matched nothing would read as "no instance uses any role".
func ProfilesInUse(ctx context.Context, regions map[string]EC2Describer) (map[string]bool, error) {
	if len(regions) == 0 {
		return nil, errors.New("no regions to check for instances using the profiles")
	}
	names := make([]string, 0, len(regions))
	for r := range regions {
		names = append(names, r)
	}
	sort.Strings(names)
	inUse := map[string]bool{}
	for _, region := range names {
		p := ec2.NewDescribeInstancesPaginator(regions[region], &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{{Name: aws.String("instance-state-name"), Values: liveStates}},
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("%s: describe instances: %w", region, err)
			}
			for _, res := range page.Reservations {
				for _, inst := range res.Instances {
					if inst.IamInstanceProfile == nil {
						continue
					}
					if n := ProfileNameFromARN(aws.ToString(inst.IamInstanceProfile.Arn)); IsCandidate(n) {
						inUse[n] = true
					}
				}
			}
		}
	}
	return inUse, nil
}

// RegionLister is the one EC2 call EnabledRegions makes.
type RegionLister interface {
	DescribeRegions(ctx context.Context, in *ec2.DescribeRegionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
}

// EnabledRegions returns every region enabled for the account (DescribeRegions
// with AllRegions=false: the default regions plus those opted in), sorted, with
// extra (e.g. the reaper's scanned regions) merged in. The in-use check must
// cover all of them: an instance's profile is account-wide, and a stopped
// instance in a region nobody scans would otherwise lose its role and come
// back with no credentials. An error, or an empty answer, means the caller
// must delete nothing.
func EnabledRegions(ctx context.Context, l RegionLister, extra ...string) ([]string, error) {
	out, err := l.DescribeRegions(ctx, &ec2.DescribeRegionsInput{AllRegions: aws.Bool(false)})
	if err != nil {
		return nil, fmt.Errorf("describe regions: %w", err)
	}
	seen := map[string]bool{}
	var regions []string
	add := func(r string) {
		if r != "" && !seen[r] {
			seen[r] = true
			regions = append(regions, r)
		}
	}
	for _, r := range out.Regions {
		add(aws.ToString(r.RegionName))
	}
	if len(regions) == 0 {
		return nil, errors.New("describe regions returned no regions")
	}
	for _, r := range extra {
		add(r)
	}
	sort.Strings(regions)
	return regions, nil
}

// API is the IAM slice Sweep uses. *iam.Client satisfies it, and the reaper's
// own iamAPI interface must declare at least these methods so its IAM grant
// stays discoverable by pkg/reaperiam's drift gate.
type API interface {
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

// NewClients builds the IAM client and one EC2 client for every region
// enabled in the account (EnabledRegions), from cfg, for a caller (the CLI)
// that should not construct SDK clients itself.
func NewClients(ctx context.Context, cfg aws.Config) (API, map[string]EC2Describer, error) {
	regions, err := EnabledRegions(ctx, ec2.NewFromConfig(cfg))
	if err != nil {
		return nil, nil, err
	}
	ec2s := make(map[string]EC2Describer, len(regions))
	for _, r := range regions {
		rc := cfg.Copy()
		rc.Region = r
		ec2s[r] = ec2.NewFromConfig(rc)
	}
	return iam.NewFromConfig(cfg), ec2s, nil
}

// Decision is the outcome for one role.
type Decision struct {
	Name     string
	Created  time.Time
	LastUsed time.Time
	// Delete: Decide said it may go. Deleted: it went. Err: the facts could not
	// be read, or the delete failed part-way (then Delete is true, Deleted false).
	Delete  bool
	Deleted bool
	Reason  string
	Err     error
}

// Result is a sweep's outcome. Scanned counts spawn-instance-* roles examined.
type Result struct {
	Decisions []Decision
}

// Count returns (scanned, deleted, wouldDelete, skipped). wouldDelete counts
// roles that qualified but were not deleted because reap was false; a failed
// delete counts as skipped.
func (r Result) Count() (scanned, deleted, wouldDelete, skipped int) {
	for _, d := range r.Decisions {
		scanned++
		switch {
		case d.Deleted:
			deleted++
		case d.Delete && d.Err == nil:
			wouldDelete++
		default:
			skipped++
		}
	}
	return
}

// Sweep lists the account's spawn-instance-* roles, decides each one against
// inUse (from ProfilesInUse) and now, and, when reap is true, deletes those
// Decide allows. With reap false nothing is changed: the result says what
// reap would delete.
func Sweep(ctx context.Context, api API, inUse map[string]bool, now time.Time, reap bool) (Result, error) {
	var res Result
	p := iam.NewListRolesPaginator(api, &iam.ListRolesInput{})
	var names []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return res, fmt.Errorf("list roles: %w", err)
		}
		for _, role := range page.Roles {
			if n := aws.ToString(role.RoleName); IsCandidate(n) {
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	for _, name := range names {
		d := Decision{Name: name}
		f, err := gather(ctx, api, name, inUse)
		if err != nil {
			d.Err, d.Reason = err, "could not read the role"
			res.Decisions = append(res.Decisions, d)
			continue
		}
		d.Created, d.LastUsed = f.Created, f.LastUsed
		d.Delete, d.Reason = Decide(f, now)
		if d.Delete && reap {
			if err := Delete(ctx, api, name, f.ProfileExists); err != nil {
				d.Err = err
			} else {
				d.Deleted = true
			}
		}
		res.Decisions = append(res.Decisions, d)
	}
	return res, nil
}

// gather reads the Facts for one role.
func gather(ctx context.Context, api API, name string, inUse map[string]bool) (Facts, error) {
	f := Facts{Name: name, InUse: inUse[name]}
	got, err := api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if err != nil {
		return f, fmt.Errorf("get role: %w", err)
	}
	if got.Role == nil {
		return f, errors.New("get role: empty response")
	}
	f.RoleTags = tagMap(got.Role.Tags)
	f.Created = aws.ToTime(got.Role.CreateDate)
	if lu := got.Role.RoleLastUsed; lu != nil {
		f.LastUsed = aws.ToTime(lu.LastUsedDate)
	}

	prof, err := api.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)})
	switch {
	case err == nil && prof.InstanceProfile != nil:
		f.ProfileExists = true
		f.ProfileTags = tagMap(prof.InstanceProfile.Tags)
		f.ProfileCreated = aws.ToTime(prof.InstanceProfile.CreateDate)
	case err != nil && !isNoSuchEntity(err):
		return f, fmt.Errorf("get instance profile: %w", err)
	}

	lp := iam.NewListInstanceProfilesForRolePaginator(api, &iam.ListInstanceProfilesForRoleInput{RoleName: aws.String(name)})
	for lp.HasMorePages() {
		page, err := lp.NextPage(ctx)
		if err != nil {
			return f, fmt.Errorf("list instance profiles for role: %w", err)
		}
		for _, ip := range page.InstanceProfiles {
			if n := aws.ToString(ip.InstanceProfileName); n != name {
				f.OtherProfiles = append(f.OtherProfiles, n)
			}
		}
	}
	return f, nil
}

// Delete removes the role name and, when profileExists, the instance profile
// of the same name, in the only order IAM accepts: take the role out of the
// profile, delete its inline policies, detach its managed policies, delete
// the profile, delete the role. It stops at the first failure, so a role is
// never left half-stripped while still attached to a profile. Something
// already gone counts as done, so a run that failed part-way finishes on the
// next one.
//
// The caller must have established, with Decide, that nothing uses the role.
func Delete(ctx context.Context, api API, name string, profileExists bool) error {
	role := aws.String(name)
	if profileExists {
		if _, err := api.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: role, RoleName: role,
		}); err != nil && !isNoSuchEntity(err) {
			return fmt.Errorf("remove role from instance profile: %w", err)
		}
	}

	ip := iam.NewListRolePoliciesPaginator(api, &iam.ListRolePoliciesInput{RoleName: role})
	for ip.HasMorePages() {
		page, err := ip.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list inline policies: %w", err)
		}
		for _, pol := range page.PolicyNames {
			if _, err := api.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
				RoleName: role, PolicyName: aws.String(pol),
			}); err != nil && !isNoSuchEntity(err) {
				return fmt.Errorf("delete inline policy %s: %w", pol, err)
			}
		}
	}

	ap := iam.NewListAttachedRolePoliciesPaginator(api, &iam.ListAttachedRolePoliciesInput{RoleName: role})
	for ap.HasMorePages() {
		page, err := ap.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list attached policies: %w", err)
		}
		for _, pol := range page.AttachedPolicies {
			if _, err := api.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
				RoleName: role, PolicyArn: pol.PolicyArn,
			}); err != nil && !isNoSuchEntity(err) {
				return fmt.Errorf("detach managed policy %s: %w", aws.ToString(pol.PolicyArn), err)
			}
		}
	}

	if profileExists {
		if _, err := api.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{
			InstanceProfileName: role,
		}); err != nil && !isNoSuchEntity(err) {
			return fmt.Errorf("delete instance profile: %w", err)
		}
	}

	if _, err := api.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: role}); err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("delete role: %w", err)
	}
	return nil
}

func tagMap(tags []iamtypes.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func isNoSuchEntity(err error) bool {
	var nse *iamtypes.NoSuchEntityException
	return errors.As(err, &nse)
}
