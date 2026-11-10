package aws

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// Versioned spored grant policies (#1558).
//
// The spored instance roles are shared. spored-instance-role serves every
// launch in the account that names no IAM, and a spawn-instance-<hash> role
// serves every launch with the same IAM flags. Every launch rewrote the role's
// spored grants with whatever the launching CLI carried, so in an account where
// more than one spawn version launches (CI on the latest, a laptop on an older
// Homebrew, a colleague), the grants flipped between versions. A newer CLI's
// grant disappeared from instances that were already running on it. Found
// live: a branch build added ec2:DescribeSpotPriceHistory to spored-policy, a
// released v0.161.0 launch rewrote spored-policy without it, and the running
// Spot instance's next price read got UnauthorizedOperation.
//
// The fix has three parts.
//
//  1. A policy name no released CLI writes. CLIs up to v0.161.0 overwrite
//     exactly "spored-policy" and "spored-baseline-policy" and touch nothing
//     else. This CLI writes its grants to "<name>-v<schema>" instead. IAM
//     grants the union of a role's inline policies, so when an old CLI
//     rewrites its own name afterwards it can only add to what this CLI wrote,
//     never take away. That is the only protection there can be against CLIs
//     already released, since they cannot be changed.
//  2. A schema version, carried in that name. sporedPolicySchema is bumped
//     whenever any spored grant document changes (a test pins the documents to
//     the schema, so the two cannot drift). A CLI that finds a NEWER schema on
//     the role writes nothing and warns: the newer CLI's document already
//     carries everything this one would add, except anything it retired on
//     purpose. A CLI at the same schema writes the same document. So no CLI
//     from this one on can remove a grant that a newer CLI added.
//  3. A named deleter for what is superseded. After writing its version, a
//     CLI deletes the unversioned policy and every older version from the
//     role. Leave no trace: they are spawn's and only spawn reads them. It is
//     safe because the version written covers them (see "Removing a grant").
//
// Removing a grant. Monotonic as this is, a removal still ships: drop the
// statement and bump sporedPolicySchema. The first CLI at the new schema
// writes its version and deletes the older ones, and from then on an older
// versioned CLI defers to it instead of writing the grant back. Two limits:
//
//   - A CLI released before #1558 still rewrites its unversioned policy on every
//     launch, and so puts the removed grant back until the next launch by a
//     CLI at the new schema deletes it again. Nothing in this CLI can stop that.
//     When the grant must be gone at once, because it is dangerous rather than
//     merely unneeded, retire it with an explicit Deny in the new version as
//     well: an explicit Deny wins over any Allow, and the versioned policy is
//     one no old CLI touches.
//   - A removal applies to instances that are already running, including ones
//     an older spored runs on. Remove a grant only when no supported spored
//     still needs it, or when taking it away from them is the point.
//
// Size. IAM caps a role's inline policies at 10,240 characters together
// (whitespace not counted), and the spored roles share that budget with
// everything else on them. On a lived-in spored-instance-role the inline copy
// this file describes did not fit (#1573), so the spored roles now carry their
// grants as a customer-managed policy, named the same way and versioned the
// same way; see iam_spored_managed.go. The inline write below is what they fall
// back to when the caller's IAM cannot create or attach one, and what
// SpawnSweepCrossAccountRole, a role spawn alone writes to, still uses. Where
// a role carries an older inline version, the older copies beneath it are
// deleted before this version is written and the newest one after, so a role
// never holds more than two inline copies while it runs.
// TestSporedRolesFitTheInlineLimitWhenMixed holds every role kind to the limit
// in that state.

// sporedPolicySchema is the version of the spored grant documents this CLI
// writes. Bump it whenever sporedDCVRolePolicy, the spored baseline, or the
// nested-spawn baseline changes, then update the pins in
// TestSporedPolicySchemaPinsTheDocuments. Same schema must mean the same
// document: otherwise an older CLI at the same schema would overwrite a newer
// one's grants, which is #1558 again.
const sporedPolicySchema = 1

// The unversioned policy names, which every CLI up to v0.161.0 overwrites on
// every launch. This CLI writes them only as the base of its versioned names
// and otherwise deletes them.
const (
	// sporedRolePolicyBase is the shared spored-instance-role's grant policy
	// (sporedDCVRolePolicy).
	sporedRolePolicyBase = "spored-policy"
	// sporedBaselinePolicyBase is the spored baseline that every
	// spawn-instance-* and nested-spawn role carries.
	sporedBaselinePolicyBase = "spored-baseline-policy"
)

// versionedPolicyName is base's policy name at schema.
func versionedPolicyName(base string, schema int) string {
	return fmt.Sprintf("%s-v%d", base, schema)
}

// parseVersionedPolicyName returns the schema of name if it is a versioned
// policy of base.
func parseVersionedPolicyName(base, name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, base+"-v")
	if !ok || rest == "" {
		return 0, false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	v, err := strconv.Atoi(rest)
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// sporedRolePolicyName is the policy this CLI gives spored-instance-role: the
// managed policy's name at sporedPolicyPath, and the inline name it falls back
// to.
func sporedRolePolicyName() string {
	return versionedPolicyName(sporedRolePolicyBase, sporedPolicySchema)
}

// sporedBaselinePolicyName is the policy this CLI gives every spawn-instance-*
// role: the managed policy's name, and the inline name it (and the nested-spawn
// role, whose managed policy is sporedNestedBaselinePolicyName) falls back to.
func sporedBaselinePolicyName() string {
	return versionedPolicyName(sporedBaselinePolicyBase, sporedPolicySchema)
}

// sporedNestedBaselinePolicyName is the managed policy this CLI attaches to
// the nested-spawn role.
func sporedNestedBaselinePolicyName() string {
	return versionedPolicyName(sporedNestedBaselinePolicyBase, sporedPolicySchema)
}

// rolePolicyAPI is the part of the IAM client putVersionedRolePolicy uses.
type rolePolicyAPI interface {
	iam.ListRolePoliciesAPIClient
	PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, optFns ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
}

// warnSporedPolicy prints a warning from putVersionedRolePolicy. A launch goes
// ahead after every one of them.
func warnSporedPolicy(msg string) {
	fmt.Fprintf(os.Stderr, "⚠️  %s\n", msg)
}

// putVersionedRolePolicy writes doc to role as base's policy at schema, unless
// the role already carries a newer schema, and deletes the policies it
// supersedes. See the comment at the top of this file.
//
// Only the write can fail the call. When the role's policies cannot be listed
// it still writes its version, which adds to the role and removes nothing, and
// it warns that it could not check for a newer one. A failed delete is a
// warning too: the grants it leaves are a subset of the ones written.
func putVersionedRolePolicy(ctx context.Context, api rolePolicyAPI, role, base, doc string, schema int, warn func(string)) error {
	names, err := listRolePolicyNames(ctx, api, role)
	return writeInlineVersion(ctx, api, role, base, doc, schema, names, err, warn)
}

// writeInlineVersion is putVersionedRolePolicy with the role's inline policy
// names already listed (names, or listErr if they could not be). The spored
// roles reach it only as the fallback of putSporedGrants (#1573), which has
// already deferred to a newer managed version on the role.
func writeInlineVersion(ctx context.Context, api rolePolicyAPI, role, base, doc string, schema int, names []string, listErr error, warn func(string)) error {
	mine := versionedPolicyName(base, schema)
	put := func() error {
		err := retryIAMWrite(func() error {
			_, e := api.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
				RoleName:       aws.String(role),
				PolicyName:     aws.String(mine),
				PolicyDocument: aws.String(doc),
			})
			return e
		})
		if err != nil {
			return fmt.Errorf("attach %s to %s: %w", mine, role, err)
		}
		return nil
	}

	if listErr != nil {
		warn(fmt.Sprintf("could not list the inline policies of IAM role %s (%v): writing %s without checking for a "+
			"newer spawn's grants or removing the ones it replaces. Grant iam:ListRolePolicies and "+
			"iam:DeleteRolePolicy on the role to let spawn keep it tidy (#1558).", role, listErr, mine))
		return put()
	}

	legacy := false
	var versions []int
	for _, n := range names {
		if n == base {
			legacy = true
			continue
		}
		if v, ok := parseVersionedPolicyName(base, n); ok {
			versions = append(versions, v)
		}
	}
	sort.Ints(versions)
	newest := 0
	if len(versions) > 0 {
		newest = versions[len(versions)-1]
	}

	if newest > schema {
		warnNewerSchema(warn, role, versionedPolicyName(base, newest), newest, schema)
		return nil
	}

	// What this write supersedes: the unversioned policy, and every version
	// below this one. When the role already carries a versioned policy, the
	// newest of those covers all the others, so they go first and the role
	// never holds more than two copies; the newest goes once this one is in
	// place. With no versioned policy yet, the unversioned one is the only
	// copy, and it must stay until this one is written.
	var before, after []string
	if newest > 0 {
		if legacy {
			before = append(before, base)
		}
		for _, v := range versions {
			if v < newest {
				before = append(before, versionedPolicyName(base, v))
			}
		}
		if newest < schema {
			after = append(after, versionedPolicyName(base, newest))
		}
	} else if legacy {
		after = append(after, base)
	}

	del := func(names []string) {
		for _, n := range names {
			if err := deleteRolePolicy(ctx, api, role, n); err != nil {
				warn(fmt.Sprintf("could not delete the superseded inline policy %s from IAM role %s (%v); %s "+
					"carries all of its grants, so it is only clutter. `aws iam delete-role-policy "+
					"--role-name %s --policy-name %s` removes it (#1558).", n, role, err, mine, role, n))
			}
		}
	}
	del(before)
	if err := put(); err != nil {
		return err
	}
	del(after)
	return nil
}

// listRolePolicyNames returns every inline policy name on role.
func listRolePolicyNames(ctx context.Context, api iam.ListRolePoliciesAPIClient, role string) ([]string, error) {
	var names []string
	p := iam.NewListRolePoliciesPaginator(api, &iam.ListRolePoliciesInput{RoleName: aws.String(role)})
	for p.HasMorePages() {
		var page *iam.ListRolePoliciesOutput
		err := retryIAMWrite(func() error {
			var e error
			page, e = p.NextPage(ctx)
			return e
		})
		if err != nil {
			return nil, err
		}
		names = append(names, page.PolicyNames...)
	}
	return names, nil
}

// deleteRolePolicy deletes role's inline policy name. Already gone is success:
// a concurrent launch got there first.
func deleteRolePolicy(ctx context.Context, api rolePolicyAPI, role, name string) error {
	err := retryIAMWrite(func() error {
		_, e := api.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
			RoleName:   aws.String(role),
			PolicyName: aws.String(name),
		})
		return e
	})
	if err != nil && isNoSuchEntity(err) {
		return nil
	}
	return err
}

// retryIAMWrite is retryIAM without its "already exists" success. retryIAM
// counts LimitExceeded as success, which is right for a role already attached
// to its instance profile and wrong for PutRolePolicy, where LimitExceeded
// means the role's inline policies are over IAM's size cap and the policy was
// NOT written.
func retryIAMWrite(fn func() error) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		if !isThrottle(err) && !isConcurrentModification(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return err
}
