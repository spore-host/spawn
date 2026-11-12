package aws

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// The spawn-instance permissions boundary.
//
// Every role spawn creates (spored-instance-role, every spawn-instance-* role
// including the nested-spawn and app roles, the worker-pool roles, and the
// cross-account sweep role) is created with, or retrofitted onto, this
// customer-managed policy as its permissions boundary. A role's effective
// permissions are the intersection of whatever is written onto it and this
// boundary, so a caller who can write a role's policies — which spawn does by
// design, with caller-chosen documents (--iam-policy-file, the task staging
// policy, --iam-managed-policies) — still cannot give an instance a path to
// IAM administration or to another account.
//
// Shape. A boundary that allows "*" is no containment; one that enumerates
// only what spawn's own roles need breaks the user workloads that pass a broad
// --iam-policy-file (the whole point of that flag). So the ceiling is broad —
// Allow "*" on "*" — with explicit Denies for the escalation surface:
//
//   - IAM administration (creating or mutating users, roles, groups, policies,
//     access keys, login profiles, assume-role trust, and permissions
//     boundaries). iam:PassRole and iam:CreateServiceLinkedRole are NOT denied:
//     the nested-spawn role passes spawn's roles to EC2, and a first Spot or
//     FSx use creates a service-linked role. Read-only iam:Get*/List* are not
//     denied either.
//   - organizations:* and account:*, so an instance cannot touch the
//     organization or account-level settings.
//   - sts:AssumeRole into another account (aws:ResourceAccount !=
//     aws:PrincipalAccount), so an instance cannot be used as a pivot into
//     accounts the caller is trusted into. Same-account assume is left alone.
//
// Residual (stated in the IAM reference and the advisory): within the boundary
// a caller can still grant an instance data-plane access the caller does not
// hold themselves — e.g. S3 on a bucket their own identity cannot read, via
// --iam-policy-file. IAM cannot express "role ⊆ caller". What the boundary
// removes is IAM administration and account/organization/cross-account reach:
// even a role with AdministratorAccess attached, capped by this boundary,
// cannot create a user, attach a policy, edit its own boundary, or read the
// organization. An account that needs a tighter ceiling narrows this policy's
// resources or launches with --instance-profile.
//
// No policy variable appears in a Resource (PutRolePolicy/CreatePolicy reject
// them, #1069); the one variable, ${aws:PrincipalAccount}, is a Condition
// value, where it is accepted. Every Condition key belongs to its action's
// service or is a global key (aws:ResourceAccount, valid on sts:AssumeRole).
//
// Versioning. The boundary's name is fixed (it is named literally in the
// documented user baseline's PermissionsBoundary condition and its Deny on
// boundary edits), so it cannot be versioned in the name as the spored
// policies are. It is versioned in place with CreatePolicyVersion +
// SetDefaultPolicyVersion, which the documented user baseline denies on this
// ARN: only an admin running `spawn setup-account` changes it. IAM caps a
// policy at five versions; EnsureSpawnInstanceBoundary deletes the oldest
// non-default version before adding one. spawnInstanceBoundarySchema is bumped
// whenever the document changes so a stale boundary can be detected.

// spawnInstanceBoundaryName is the boundary policy's name at
// sporedPolicyPath. It is referenced literally in the documented user
// baseline, so it must not change and must not match the spored-* names the
// baseline lets a user create.
const spawnInstanceBoundaryName = "spawn-instance-boundary"

// spawnInstanceBoundarySchema is the version of spawnInstanceBoundaryDocument.
// Bump it whenever the document changes, and update the golden in
// iam_boundary_test.go. It is carried in the policy's Description so a
// deployed boundary's schema can be read without parsing the document.
const spawnInstanceBoundarySchema = 1

// spawnInstanceBoundaryDescription is the policy's Description; it carries the
// schema so an admin (and spawn) can see which version is deployed.
func spawnInstanceBoundaryDescription() string {
	return fmt.Sprintf("spawn: the permissions boundary (schema v%d) capping every role spawn creates. "+
		"Created and updated by `spawn setup-account`; `spawn cleanup --instance-roles` deletes it once no role uses it.",
		spawnInstanceBoundarySchema)
}

// spawnInstanceBoundaryARN is the boundary policy's ARN in account.
func spawnInstanceBoundaryARN(account string) string {
	return "arn:aws:iam::" + account + ":policy" + sporedPolicyPath + spawnInstanceBoundaryName
}

// spawnInstanceBoundaryDocument is the boundary's policy document. It takes no
// account: the Deny on cross-account assume compares aws:ResourceAccount to
// ${aws:PrincipalAccount} at evaluation time, so the same document caps a role
// in any account.
const spawnInstanceBoundaryDocument = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "AllowAllServicesAsTheCeiling",
      "Effect": "Allow",
      "Action": "*",
      "Resource": "*"
    },
    {
      "Sid": "DenyIAMAdministration",
      "Effect": "Deny",
      "Action": [
        "iam:CreateUser",
        "iam:CreateRole",
        "iam:CreateGroup",
        "iam:CreatePolicy",
        "iam:CreatePolicyVersion",
        "iam:SetDefaultPolicyVersion",
        "iam:DeletePolicy",
        "iam:DeletePolicyVersion",
        "iam:PutUserPolicy",
        "iam:PutRolePolicy",
        "iam:PutGroupPolicy",
        "iam:DeleteUserPolicy",
        "iam:DeleteRolePolicy",
        "iam:DeleteGroupPolicy",
        "iam:AttachUserPolicy",
        "iam:AttachRolePolicy",
        "iam:AttachGroupPolicy",
        "iam:DetachUserPolicy",
        "iam:DetachRolePolicy",
        "iam:DetachGroupPolicy",
        "iam:CreateInstanceProfile",
        "iam:DeleteInstanceProfile",
        "iam:AddRoleToInstanceProfile",
        "iam:RemoveRoleFromInstanceProfile",
        "iam:UpdateRole",
        "iam:UpdateAssumeRolePolicy",
        "iam:UpdateRoleDescription",
        "iam:PutRolePermissionsBoundary",
        "iam:PutUserPermissionsBoundary",
        "iam:DeleteRolePermissionsBoundary",
        "iam:DeleteUserPermissionsBoundary",
        "iam:CreateAccessKey",
        "iam:UpdateAccessKey",
        "iam:CreateLoginProfile",
        "iam:UpdateLoginProfile",
        "iam:ChangePassword",
        "iam:CreateServiceSpecificCredential",
        "iam:CreateSAMLProvider",
        "iam:UpdateSAMLProvider",
        "iam:CreateOpenIDConnectProvider",
        "iam:UpdateOpenIDConnectProviderThumbprint",
        "iam:DeleteRolePermissionsBoundary",
        "iam:TagRole",
        "iam:UntagRole",
        "iam:TagUser",
        "iam:UntagUser",
        "iam:TagPolicy",
        "iam:UntagPolicy"
      ],
      "Resource": "*"
    },
    {
      "Sid": "DenyOrganizations",
      "Effect": "Deny",
      "Action": "organizations:*",
      "Resource": "*"
    },
    {
      "Sid": "DenyAccountManagement",
      "Effect": "Deny",
      "Action": "account:*",
      "Resource": "*"
    },
    {
      "Sid": "DenyCrossAccountAssumeRole",
      "Effect": "Deny",
      "Action": "sts:AssumeRole",
      "Resource": "*",
      "Condition": {
        "StringNotEquals": {
          "aws:ResourceAccount": "${aws:PrincipalAccount}"
        }
      }
    }
  ]
}`

// BoundaryMissingError is returned when the spawn-instance boundary does not
// exist in the account and the caller may not create it (the documented user
// baseline denies creating spore-host/* policies other than spored-*). It
// names the exact command an admin runs.
type BoundaryMissingError struct {
	Account string
	Cause   error
}

func (e *BoundaryMissingError) Error() string {
	return fmt.Sprintf("the spawn permissions boundary %s does not exist in account %s and these credentials may not "+
		"create it (%v). spawn caps every role it creates with this boundary, so it must exist before a launch can "+
		"create or update an instance role. An administrator provisions it once per account with:\n\n"+
		"    spawn setup-account\n\n"+
		"run with credentials allowed to create IAM policies. Until then, launches that must create or update a role "+
		"are refused.", spawnInstanceBoundaryARN(e.Account), e.Account, e.Cause)
}

func (e *BoundaryMissingError) Unwrap() error { return e.Cause }

// boundaryLaunchAPI is the IAM slice the launch-time boundary check uses: read
// the boundary, create it if these are admin credentials, read a role and set
// its boundary. *iam.Client satisfies it.
type boundaryLaunchAPI interface {
	GetPolicy(ctx context.Context, in *iam.GetPolicyInput, optFns ...func(*iam.Options)) (*iam.GetPolicyOutput, error)
	CreatePolicy(ctx context.Context, in *iam.CreatePolicyInput, optFns ...func(*iam.Options)) (*iam.CreatePolicyOutput, error)
	GetRole(ctx context.Context, in *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	PutRolePermissionsBoundary(ctx context.Context, in *iam.PutRolePermissionsBoundaryInput, optFns ...func(*iam.Options)) (*iam.PutRolePermissionsBoundaryOutput, error)
}

// ensureBoundaryForLaunch makes sure the spawn-instance boundary exists in
// account before a launch creates or updates any role. Released spawn cannot
// change the boundary's document, so this only creates it when it is missing:
// an admin's first launch provisions it (iam:CreatePolicy allowed), and a
// non-admin whose create is refused gets a BoundaryMissingError naming the
// admin command. An existing boundary is used as-is; a stale one is warned
// about, since only `spawn setup-account` may upgrade it.
func ensureBoundaryForLaunch(ctx context.Context, api boundaryLaunchAPI, account string, warn func(string)) error {
	arn := spawnInstanceBoundaryARN(account)
	_, err := api.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)})
	switch {
	case err == nil:
		return nil
	case !isNoSuchEntity(err):
		return fmt.Errorf("look up the spawn permissions boundary %s: %w", arn, err)
	}
	// Missing: create the first version if these credentials may.
	cerr := retryIAMWrite(func() error {
		_, e := api.CreatePolicy(ctx, &iam.CreatePolicyInput{
			PolicyName:     aws.String(spawnInstanceBoundaryName),
			Path:           aws.String(sporedPolicyPath),
			PolicyDocument: aws.String(spawnInstanceBoundaryDocument),
			Description:    aws.String(spawnInstanceBoundaryDescription()),
		})
		return e
	})
	switch {
	case cerr == nil:
		return nil
	case isEntityAlreadyExists(cerr):
		// A concurrent launch or setup created it: use it.
		return nil
	case IsAccessDenied(cerr):
		return &BoundaryMissingError{Account: account, Cause: cerr}
	default:
		return fmt.Errorf("create the spawn permissions boundary %s: %w", arn, cerr)
	}
}

// ensureRoleBoundary retrofits the spawn-instance boundary onto an existing
// role, before any policy is written to it, so the role is never left with
// weaker containment while a launch updates it. It is a no-op when the role
// already carries the boundary. Setting the boundary is the one boundary write
// the documented user baseline allows (to this ARN only).
func ensureRoleBoundary(ctx context.Context, api boundaryLaunchAPI, role, account string) error {
	arn := spawnInstanceBoundaryARN(account)
	out, err := api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
	if err != nil {
		return fmt.Errorf("read IAM role %s before setting its permissions boundary: %w", role, err)
	}
	if out != nil && out.Role != nil && out.Role.PermissionsBoundary != nil &&
		aws.ToString(out.Role.PermissionsBoundary.PermissionsBoundaryArn) == arn {
		return nil
	}
	err = retryIAMWrite(func() error {
		_, e := api.PutRolePermissionsBoundary(ctx, &iam.PutRolePermissionsBoundaryInput{
			RoleName:            aws.String(role),
			PermissionsBoundary: aws.String(arn),
		})
		return e
	})
	if err != nil {
		return fmt.Errorf("set the permissions boundary on IAM role %s: %w", role, err)
	}
	return nil
}

// BoundaryAdminAPI is the IAM slice EnsureSpawnInstanceBoundary uses. It adds
// the version-management calls to boundaryLaunchAPI. *iam.Client satisfies it.
type BoundaryAdminAPI interface {
	GetPolicy(ctx context.Context, in *iam.GetPolicyInput, optFns ...func(*iam.Options)) (*iam.GetPolicyOutput, error)
	GetPolicyVersion(ctx context.Context, in *iam.GetPolicyVersionInput, optFns ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error)
	CreatePolicy(ctx context.Context, in *iam.CreatePolicyInput, optFns ...func(*iam.Options)) (*iam.CreatePolicyOutput, error)
	CreatePolicyVersion(ctx context.Context, in *iam.CreatePolicyVersionInput, optFns ...func(*iam.Options)) (*iam.CreatePolicyVersionOutput, error)
	ListPolicyVersions(ctx context.Context, in *iam.ListPolicyVersionsInput, optFns ...func(*iam.Options)) (*iam.ListPolicyVersionsOutput, error)
	DeletePolicyVersion(ctx context.Context, in *iam.DeletePolicyVersionInput, optFns ...func(*iam.Options)) (*iam.DeletePolicyVersionOutput, error)
}

// NewBoundaryAdminIAM is the IAM client EnsureSpawnInstanceBoundary uses, from
// cfg. It lets cmd/ provision the boundary without importing the IAM SDK.
func NewBoundaryAdminIAM(cfg aws.Config) BoundaryAdminAPI { return iam.NewFromConfig(cfg) }

// BoundaryOutcome is what EnsureSpawnInstanceBoundary found and did.
type BoundaryOutcome struct {
	ARN       string
	Created   bool // the policy did not exist and was created
	Updated   bool // a new default version was written (the document had drifted)
	Pruned    int  // older versions deleted to stay under IAM's five-version cap
	Unchanged bool // the deployed document already matched this spawn's
}

// EnsureSpawnInstanceBoundary creates the spawn-instance boundary in account,
// or converges an existing one to this spawn's document. It is the admin
// provisioning path (`spawn setup-account`); it needs iam:CreatePolicy /
// iam:CreatePolicyVersion / iam:SetDefaultPolicyVersion on the boundary ARN,
// which the documented user baseline denies on purpose.
//
// A new version is written only when the deployed default document differs
// from this spawn's. IAM caps a policy at five versions, so before adding one
// the oldest non-default version is deleted (SetAsDefault keeps the new one as
// the default). SetAsDefault is set on CreatePolicyVersion, so no separate
// SetDefaultPolicyVersion call is made.
func EnsureSpawnInstanceBoundary(ctx context.Context, api BoundaryAdminAPI, account string, warn func(string)) (BoundaryOutcome, error) {
	arn := spawnInstanceBoundaryARN(account)
	out := BoundaryOutcome{ARN: arn}

	cur, err := api.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)})
	switch {
	case err == nil:
	case isNoSuchEntity(err):
		cerr := retryIAMWrite(func() error {
			_, e := api.CreatePolicy(ctx, &iam.CreatePolicyInput{
				PolicyName:     aws.String(spawnInstanceBoundaryName),
				Path:           aws.String(sporedPolicyPath),
				PolicyDocument: aws.String(spawnInstanceBoundaryDocument),
				Description:    aws.String(spawnInstanceBoundaryDescription()),
			})
			return e
		})
		if cerr != nil && !isEntityAlreadyExists(cerr) {
			return out, fmt.Errorf("create the spawn permissions boundary %s: %w", arn, cerr)
		}
		if cerr == nil {
			out.Created = true
			return out, nil
		}
		// EntityAlreadyExists: a concurrent run created it; fall through to the
		// converge path by re-reading.
		if cur, err = api.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)}); err != nil {
			return out, fmt.Errorf("read the spawn permissions boundary %s after a create race: %w", arn, err)
		}
	default:
		return out, fmt.Errorf("look up the spawn permissions boundary %s: %w", arn, err)
	}

	// It exists: compare the default version's document.
	defaultVersion := "v1"
	if cur != nil && cur.Policy != nil && cur.Policy.DefaultVersionId != nil {
		defaultVersion = aws.ToString(cur.Policy.DefaultVersionId)
	}
	gv, err := api.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{PolicyArn: aws.String(arn), VersionId: aws.String(defaultVersion)})
	if err != nil {
		return out, fmt.Errorf("read the current version of the spawn permissions boundary %s: %w", arn, err)
	}
	if gv.PolicyVersion != nil && samePolicyDocument(aws.ToString(gv.PolicyVersion.Document), spawnInstanceBoundaryDocument) {
		out.Unchanged = true
		return out, nil
	}

	// The document drifted: write a new default version, pruning to stay under
	// the five-version cap first.
	pruned, err := pruneBoundaryVersions(ctx, api, arn, warn)
	if err != nil {
		return out, err
	}
	out.Pruned = pruned
	err = retryIAMWrite(func() error {
		_, e := api.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
			PolicyArn:      aws.String(arn),
			PolicyDocument: aws.String(spawnInstanceBoundaryDocument),
			SetAsDefault:   true,
		})
		return e
	})
	if err != nil {
		return out, fmt.Errorf("write a new version of the spawn permissions boundary %s: %w", arn, err)
	}
	out.Updated = true
	return out, nil
}

// boundaryVersionCap is IAM's limit on versions of one managed policy.
const boundaryVersionCap = 5

// pruneBoundaryVersions deletes the oldest non-default versions of the boundary
// so that a CreatePolicyVersion will not exceed IAM's five-version cap. It
// leaves room for one new version (deletes down to boundaryVersionCap-1), and
// never deletes the default version.
func pruneBoundaryVersions(ctx context.Context, api BoundaryAdminAPI, arn string, warn func(string)) (int, error) {
	lv, err := api.ListPolicyVersions(ctx, &iam.ListPolicyVersionsInput{PolicyArn: aws.String(arn)})
	if err != nil {
		return 0, fmt.Errorf("list the versions of the spawn permissions boundary %s: %w", arn, err)
	}
	var nonDefault []types.PolicyVersion
	for _, v := range lv.Versions {
		if !v.IsDefaultVersion {
			nonDefault = append(nonDefault, v)
		}
	}
	// Oldest first: version ids are "v<n>", ascending with age reversed, so
	// sort by the numeric suffix ascending.
	sort.Slice(nonDefault, func(i, j int) bool {
		return policyVersionNum(aws.ToString(nonDefault[i].VersionId)) < policyVersionNum(aws.ToString(nonDefault[j].VersionId))
	})
	pruned := 0
	for len(lv.Versions)-pruned >= boundaryVersionCap && pruned < len(nonDefault) {
		id := aws.ToString(nonDefault[pruned].VersionId)
		derr := retryIAMWrite(func() error {
			_, e := api.DeletePolicyVersion(ctx, &iam.DeletePolicyVersionInput{PolicyArn: aws.String(arn), VersionId: aws.String(id)})
			return e
		})
		if derr != nil {
			return pruned, fmt.Errorf("delete the superseded version %s of the spawn permissions boundary %s: %w", id, arn, derr)
		}
		warn(fmt.Sprintf("deleted the superseded version %s of the spawn permissions boundary to stay under IAM's %d-version limit", id, boundaryVersionCap))
		pruned++
	}
	return pruned, nil
}

// policyVersionNum is the numeric part of an IAM policy version id ("v7" -> 7).
func policyVersionNum(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "v"))
	if err != nil {
		return 0
	}
	return n
}
