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
// A role is also kept while a deferred launcher will still launch with it: an
// enabled schedule naming its profile (references.go, #1378).
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
//   - a role a user named with --iam-role. It does not carry the prefix.
//
// Pool workers (#1319). `spawn pool create` makes one role per pool, named
// spawn-instance-pool-<hash of the run id> (PoolWorkerRoleName), and `spawn
// pool drain` deletes it. A pool nobody drains used to keep its role forever,
// so pool roles now live inside NamePrefix and fall under the same rule: once
// no instance holds the profile and it has been unused for Grace, the pool has
// no workers left and the role goes. Decide additionally requires a pool role's
// spawn:pool-run-id tag to hash to its name. Roles made before #1319 are named
// spawn-pool-worker-<hash> (LegacyPoolWorkerPrefix); they are outside the
// reaper's IAM grants, so only `spawn cleanup --instance-roles` sweeps them
// (Options.LegacyPoolWorkers), under the same rule plus that exact name check.
//
// Untagged spawn-instance-* roles are kept. spawn has tagged these roles and
// profiles spawn:managed=true since the commit that introduced the
// spawn-instance-<hash> name (spore-host 52ae503, 2026-01-14, "Add IAM instance
// profile support"), so an untagged role with the prefix is not one spawn made
// and there is no legacy population to adopt by name.
package instancerole

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	// PoolWorkerPrefix names a pool's worker role and profile (#1319). It is
	// inside NamePrefix, so the reaper's existing spawn-instance-* grants and
	// this package's rule cover it without widening either.
	PoolWorkerPrefix = NamePrefix + "pool-"

	// LegacyPoolWorkerPrefix is the name pool roles had before #1319. Only
	// swept when Options.LegacyPoolWorkers is set, and only names of exactly
	// this prefix plus poolHashLen lowercase hex digits.
	LegacyPoolWorkerPrefix = "spawn-pool-worker-"

	// PoolRunIDTagKey is the tag `spawn pool create` writes on a pool's role,
	// its profile and its workers: the pool's run id.
	PoolRunIDTagKey = "spawn:pool-run-id"

	// poolHashLen is how many hex digits of sha256(run id) a pool role's name
	// carries. A role name is capped at 64 characters and a run id is not.
	poolHashLen = 16

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
	KeepPoolUnmatched   = "pool role's spawn:pool-run-id tag does not match its name"
)

// PoolWorkerRoleName is the role (and instance profile, same name) for one
// pool's workers: PoolWorkerPrefix + the first 16 hex digits of sha256(runID).
// One per pool, so each pool's inline policy can be scoped to its own queue
// (#1147). Defined here, not in cmd, so the name the CLI creates and the name
// this package deletes cannot drift apart.
func PoolWorkerRoleName(runID string) string {
	return PoolWorkerPrefix + poolHash(runID)
}

// LegacyPoolWorkerRoleName is the name PoolWorkerRoleName returned before
// #1319, for the pools created then.
func LegacyPoolWorkerRoleName(runID string) string {
	return LegacyPoolWorkerPrefix + poolHash(runID)
}

func poolHash(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return hex.EncodeToString(sum[:])[:poolHashLen]
}

// IsLegacyPoolWorker reports whether name has exactly the shape of a pre-#1319
// pool role: LegacyPoolWorkerPrefix followed by 16 lowercase hex digits and
// nothing else.
func IsLegacyPoolWorker(name string) bool {
	h, ok := strings.CutPrefix(name, LegacyPoolWorkerPrefix)
	if !ok || len(h) != poolHashLen {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Options widen a sweep beyond the per-task roles. The zero value is what the
// reaper uses.
type Options struct {
	// LegacyPoolWorkers also sweeps pre-#1319 spawn-pool-worker-<hash> roles.
	// The reaper's IAM grants do not reach those names, so only the CLI,
	// running as a principal that created them, sets it.
	LegacyPoolWorkers bool
}

// poolRoleMatches reports whether a pool role's run-id tag hashes to its name
// (for either naming), so a hand-made role that happens to share a prefix is
// never taken for a pool's.
func poolRoleMatches(name string, tags map[string]string) bool {
	id := tags[PoolRunIDTagKey]
	return id != "" && (name == PoolWorkerRoleName(id) || name == LegacyPoolWorkerRoleName(id))
}

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
	InUse bool
	// ReferencedBy: the deferred launchers that will launch with the
	// same-name profile later (References.For), e.g. "schedule <id>" (#1378).
	ReferencedBy []string
	Created      time.Time
	LastUsed     time.Time // zero when IAM has no record of use
}

// Decide is THE predicate. It returns true only when every condition holds,
// and otherwise the first reason to keep the role. It never deletes a
// pre-#1319 pool role; DecideWith does, when opts allow it.
func Decide(f Facts, now time.Time) (bool, string) {
	return DecideWith(f, now, Options{})
}

// DecideWith is Decide with opts. The only difference opts make is whether a
// LegacyPoolWorkerPrefix name is considered at all; every other condition is
// the same for every role.
func DecideWith(f Facts, now time.Time, opts Options) (bool, string) {
	legacy := opts.LegacyPoolWorkers && IsLegacyPoolWorker(f.Name)
	pool := legacy || strings.HasPrefix(f.Name, PoolWorkerPrefix)
	switch {
	case f.Name == SharedRoleName || f.Name == SharedProfileName:
		return false, KeepShared
	case !legacy && !IsCandidate(f.Name):
		return false, KeepNotPerTask
	case pool && !poolRoleMatches(f.Name, f.RoleTags):
		return false, KeepPoolUnmatched
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
	case len(f.ReferencedBy) > 0:
		return false, KeepReferencedPrefix + strings.Join(f.ReferencedBy, ", ")
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
					// Legacy pool profiles are recorded too: a sweep with
					// Options.LegacyPoolWorkers must see them in use.
					if n := ProfileNameFromARN(aws.ToString(inst.IamInstanceProfile.Arn)); IsCandidate(n) || IsLegacyPoolWorker(n) {
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

// Result is a sweep's outcome. Scanned counts the roles examined.
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
// inUse (from ProfilesInUse), refs (from CollectReferences) and now, and, when
// reap is true, deletes those Decide allows. With reap false nothing is
// changed: the result says what reap would delete. References that
// CollectReferences did not produce are refused (ErrReferencesNotCollected)
// before anything is read.
func Sweep(ctx context.Context, api API, inUse map[string]bool, refs References, now time.Time, reap bool) (Result, error) {
	return SweepWith(ctx, api, inUse, refs, now, reap, Options{})
}

// SweepWith is Sweep with opts: with Options.LegacyPoolWorkers it also lists
// and decides (DecideWith) the pre-#1319 spawn-pool-worker-<hash> roles.
func SweepWith(ctx context.Context, api API, inUse map[string]bool, refs References, now time.Time, reap bool, opts Options) (Result, error) {
	var res Result
	if !refs.collected {
		return res, ErrReferencesNotCollected
	}
	p := iam.NewListRolesPaginator(api, &iam.ListRolesInput{})
	var names []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return res, fmt.Errorf("list roles: %w", err)
		}
		for _, role := range page.Roles {
			if n := aws.ToString(role.RoleName); IsCandidate(n) || (opts.LegacyPoolWorkers && IsLegacyPoolWorker(n)) {
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	for _, name := range names {
		d := Decision{Name: name}
		f, err := gather(ctx, api, name, inUse, refs)
		if err != nil {
			d.Err, d.Reason = err, "could not read the role"
			res.Decisions = append(res.Decisions, d)
			continue
		}
		d.Created, d.LastUsed = f.Created, f.LastUsed
		d.Delete, d.Reason = DecideWith(f, now, opts)
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
func gather(ctx context.Context, api API, name string, inUse map[string]bool, refs References) (Facts, error) {
	f := Facts{Name: name, InUse: inUse[name], ReferencedBy: refs.For(name)}
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
