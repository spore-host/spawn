package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"

	"github.com/spore-host/spawn/pkg/sweep"
)

// SpawnSweepCrossAccountRole, provisioned by spawn (#1537).
//
// A detached parameter sweep (the default for `spawn launch --param-file`) is
// run by the spawn-sweep-orchestrator Lambda, which assumes this role in the
// account the members launch in (the compute account) for every launch, tag,
// egress-proxy repair and cancel. Until #1537 only a manual script created it
// (scripts/setup-sweep-cross-account-role.sh), and every policy change was
// another manual step per account. spawn now creates it on the first detached
// sweep, keeps its trust and policy current on every one after, and removes it
// with `spawn sweep teardown`.
//
// The trust. The role trusts one principal: the orchestrator's execution role,
// SpawnSweepOrchestratorRole, in the account whose orchestrator the CLI
// invokes (the infra credentials' account). That is the trust the script
// wrote. There is no ExternalId: the orchestrator's AssumeRole calls
// (lambda/sweep-orchestrator main.go createCrossAccountEC2Client and
// initializeRegionalOrchestrator, cancel.go assumeMemberClients) send none, so
// a trust that required one would refuse every sweep. Adding one is a change
// to the orchestrator, the sweep and schedule records and the CLI together,
// not something provisioning can do alone.
//
// The policy. Least privilege for what the orchestrator calls on these
// credentials, as internal/iamcoverage discovers it from the Lambda's code:
// TestEveryPrincipalCallIsGranted fails if a call is not granted, and
// TestSweepCrossAccountRoleGrantsOnlyWhatTheOrchestratorCalls if a grant is
// not called. It is written versioned, through putVersionedRolePolicy (#1558),
// so an older CLI or the old script can only add to it, never strip a newer
// grant.
//
// The boundary. The role must never exist in, or trust anything from,
// 942542972736, and the same holds for any Organizations management account:
// CheckSweepRoleAccounts refuses both before anything is created.

// SweepOrchestratorRoleName is the execution role of the spawn-sweep-orchestrator
// Lambda (scripts/setup-sweep-orchestrator-lambda-role.sh), the one principal
// SpawnSweepCrossAccountRole trusts.
const SweepOrchestratorRoleName = "SpawnSweepOrchestratorRole"

// sweepRolePolicyBase is the inline policy name the script wrote, and every
// spawn up to v0.161.0 relied on. spawn writes "<base>-v<schema>" instead and
// deletes this one once its version is in place.
const sweepRolePolicyBase = "SweepCrossAccountEC2Policy"

// sweepRolePolicySchema is the version of sweepCrossAccountPolicyTemplate. Bump
// it whenever the template changes, and update the pin and the golden file in
// sweep_role_test.go: at the same schema an older CLI would rewrite this one's
// document with its own (#1558).
const sweepRolePolicySchema = 1

// SweepRolePolicyName is the inline policy this spawn writes to
// SpawnSweepCrossAccountRole.
func SweepRolePolicyName() string {
	return versionedPolicyName(sweepRolePolicyBase, sweepRolePolicySchema)
}

// SweepRolePolicySchema is this spawn's sweep role policy schema.
func SweepRolePolicySchema() int { return sweepRolePolicySchema }

// sweepCrossAccountPolicyTemplate is SpawnSweepCrossAccountRole's inline
// policy, bound to the compute account by bindPolicyAccount. No policy
// variable appears in a Resource: PutRolePolicy rejects them (#1069).
//
// What each statement is for:
//
//   - The members: RunInstances, CreateTags on them, DescribeInstances and
//     DescribeAvailabilityZones while launching and cancelling,
//     TerminateInstances and CancelSpotInstanceRequests on cancel (#1349; a
//     Spot member's request is cancelled before its instance, as on every
//     spawn terminate path, #922). The members' requests are untagged, so the
//     cancel takes no spawn:managed condition; it is no wider than the
//     unconditioned TerminateInstances beside it.
//   - DeletePlacementGroup of spawn's own (spawn:managed) MPI groups (#254).
//   - iam:PassRole of the roles spawn creates, to EC2 only (#1323, #1331):
//     spored-instance-role, spawnd-role, spawn-instance-*. The nested-spawn
//     roles (spawn-instance-nested-*) are inside that family but denied
//     explicitly (#1372): such a role may itself call RunInstances and
//     iam:PassRole.
//   - The egress proxy (#1515): its two security groups re-created
//     (CreateSecurityGroup only of a group tagged spawn:managed=true, in any
//     VPC) and their rules reconciled (only on spawn:managed groups), and the
//     readiness probe (SendCommand of AWS-RunShellScript, only to instances
//     tagged spawn:managed=true and spawn:egress-proxy=vpc-*; ssm: keys on an
//     ssm: action, since ec2:ResourceTag there is an implicit deny), and
//     GetCommandInvocation, which has no resource-level ARN.
//   - iam:CreateServiceLinkedRole of the EC2 Spot role: RunInstances of a Spot
//     member creates AWSServiceRoleForEC2Spot in an account that has none.
const sweepCrossAccountPolicyTemplate = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "LaunchAndCancelMembers",
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeAvailabilityZones",
        "ec2:DescribeSecurityGroups",
        "ec2:CreateTags",
        "ec2:TerminateInstances",
        "ec2:CancelSpotInstanceRequests"
      ],
      "Resource": "*"
    },
    {
      "Sid": "DeleteManagedPlacementGroups",
      "Effect": "Allow",
      "Action": "ec2:DeletePlacementGroup",
      "Resource": "*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/spawn:managed": "true"
        }
      }
    },
    {
      "Sid": "PassSpawnInstanceRoles",
      "Effect": "Allow",
      "Action": "iam:PassRole",
      "Resource": [
        "arn:aws:iam::SPAWN-ACCOUNT-ID:role/spored-instance-role",
        "arn:aws:iam::SPAWN-ACCOUNT-ID:role/spawnd-role",
        "arn:aws:iam::SPAWN-ACCOUNT-ID:role/spawn-instance-*"
      ],
      "Condition": {
        "StringEquals": {
          "iam:PassedToService": "ec2.amazonaws.com"
        }
      }
    },
    {
      "Sid": "DenyPassNestedSpawnRoles",
      "Effect": "Deny",
      "Action": "iam:PassRole",
      "Resource": [
        "arn:aws:iam::SPAWN-ACCOUNT-ID:role/spawn-instance-nested-*"
      ]
    },
    {
      "Sid": "EgressProxyCreateManagedGroups",
      "Effect": "Allow",
      "Action": "ec2:CreateSecurityGroup",
      "Resource": "arn:aws:ec2:*:SPAWN-ACCOUNT-ID:security-group/*",
      "Condition": {
        "StringEquals": {
          "aws:RequestTag/spawn:managed": "true"
        }
      }
    },
    {
      "Sid": "EgressProxyCreateGroupsInVPC",
      "Effect": "Allow",
      "Action": "ec2:CreateSecurityGroup",
      "Resource": "arn:aws:ec2:*:SPAWN-ACCOUNT-ID:vpc/*"
    },
    {
      "Sid": "EgressProxyManageManagedGroupRules",
      "Effect": "Allow",
      "Action": [
        "ec2:AuthorizeSecurityGroupIngress",
        "ec2:AuthorizeSecurityGroupEgress",
        "ec2:RevokeSecurityGroupIngress",
        "ec2:RevokeSecurityGroupEgress"
      ],
      "Resource": "arn:aws:ec2:*:SPAWN-ACCOUNT-ID:security-group/*",
      "Condition": {
        "StringEquals": {
          "ec2:ResourceTag/spawn:managed": "true"
        }
      }
    },
    {
      "Sid": "EgressProxyProbeProxies",
      "Effect": "Allow",
      "Action": "ssm:SendCommand",
      "Resource": "arn:aws:ec2:*:SPAWN-ACCOUNT-ID:instance/*",
      "Condition": {
        "StringEquals": {
          "ssm:resourceTag/spawn:managed": "true"
        },
        "StringLike": {
          "ssm:resourceTag/spawn:egress-proxy": "vpc-*"
        }
      }
    },
    {
      "Sid": "EgressProxyProbeDocument",
      "Effect": "Allow",
      "Action": "ssm:SendCommand",
      "Resource": "arn:aws:ssm:*::document/AWS-RunShellScript"
    },
    {
      "Sid": "EgressProxyProbeResult",
      "Effect": "Allow",
      "Action": "ssm:GetCommandInvocation",
      "Resource": "*"
    },
    {
      "Sid": "CreateSpotServiceLinkedRole",
      "Effect": "Allow",
      "Action": "iam:CreateServiceLinkedRole",
      "Resource": "arn:aws:iam::SPAWN-ACCOUNT-ID:role/aws-service-role/spot.amazonaws.com/AWSServiceRoleForEC2Spot",
      "Condition": {
        "StringLike": {
          "iam:AWSServiceName": "spot.amazonaws.com"
        }
      }
    }
  ]
}`

// SweepCrossAccountPolicyDocument is SpawnSweepCrossAccountRole's inline
// policy for computeAccount, as spawn writes it.
func SweepCrossAccountPolicyDocument(computeAccount string) (string, error) {
	if !accountIDPattern.MatchString(computeAccount) {
		return "", fmt.Errorf("cannot bind the %s policy to account %q: not a 12-digit account ID", sweep.CrossAccountRoleName, computeAccount)
	}
	return bindPolicyAccount(sweepCrossAccountPolicyTemplate, computeAccount)
}

// SweepOrchestratorRoleARN is the orchestrator's execution role in
// orchestratorAccount: the principal SpawnSweepCrossAccountRole trusts.
func SweepOrchestratorRoleARN(orchestratorAccount string) string {
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", orchestratorAccount, SweepOrchestratorRoleName)
}

// SweepCrossAccountTrustDocument is SpawnSweepCrossAccountRole's trust policy:
// sts:AssumeRole by the orchestrator's execution role in orchestratorAccount,
// and nothing else. See the comment at the top of this file on the ExternalId.
func SweepCrossAccountTrustDocument(orchestratorAccount string) (string, error) {
	if !accountIDPattern.MatchString(orchestratorAccount) {
		return "", fmt.Errorf("cannot trust the sweep orchestrator in account %q: not a 12-digit account ID", orchestratorAccount)
	}
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Principal": map[string]any{"AWS": SweepOrchestratorRoleARN(orchestratorAccount)},
			"Action":    "sts:AssumeRole",
		}},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// --- the account boundary ---------------------------------------------------

// forbiddenTrustAccounts are accounts no SpawnSweepCrossAccountRole may be
// created in or trust, whatever the credentials allow. Hardcoded on purpose:
// a configurable deny is one a profile can switch off.
var forbiddenTrustAccounts = map[string]string{
	"942542972736": "an Amazon-internal account and the management account of AWS Organization " +
		"o-pygqyjjoym. No spore.host account may be trusted into it or out of it, and an ExternalId " +
		"or tag-gated least privilege does not change that: a trust created there once was " +
		"detected and force-denied in 12 minutes",
}

// sharedInfraAccount is spore.host's shared control-plane account. It runs
// the orchestrator and launches no sweep members, so a sweep role there means
// the credentials are aimed at the wrong account (the script refused it too).
const sharedInfraAccount = "966362334030"

// SweepRoleRefusedError is CheckSweepRoleAccounts' refusal.
type SweepRoleRefusedError struct {
	Account string
	Why     string
}

func (e *SweepRoleRefusedError) Error() string {
	return fmt.Sprintf("refusing to create or trust %s with account %s: it is %s", sweep.CrossAccountRoleName, e.Account, e.Why)
}

// ManagementAccountLookup returns the management account of the AWS
// Organization the caller's account belongs to, "" with no error when it
// belongs to none, and an error when it cannot tell (not permitted to ask).
type ManagementAccountLookup func(ctx context.Context) (string, error)

// CheckSweepRoleAccounts refuses SpawnSweepCrossAccountRole in computeAccount
// trusting the orchestrator in orchestratorAccount when either account is a
// forbidden one, or is the management account of computeAccount's
// Organization (a trust into or out of a management account reaches the whole
// organization). When the Organization cannot be read, it warns and allows:
// the hardcoded accounts are refused whatever Organizations says.
func CheckSweepRoleAccounts(ctx context.Context, computeAccount, orchestratorAccount string, mgmt ManagementAccountLookup, warn func(string)) error {
	for _, a := range []string{computeAccount, orchestratorAccount} {
		if why, bad := forbiddenTrustAccounts[a]; bad {
			return &SweepRoleRefusedError{Account: a, Why: why}
		}
	}
	if computeAccount == sharedInfraAccount {
		return &SweepRoleRefusedError{Account: computeAccount, Why: "spore.host's shared control-plane account, " +
			"which runs the sweep orchestrator and launches no sweep members: the compute credentials are " +
			"aimed at the wrong account"}
	}
	if mgmt == nil {
		return nil
	}
	m, err := mgmt(ctx)
	if err != nil {
		warn(fmt.Sprintf("could not tell whether account %s is an AWS Organizations management account (%v); "+
			"spawn never puts %s in one, and checks again on every detached sweep", computeAccount, err, sweep.CrossAccountRoleName))
		return nil
	}
	for _, a := range []string{computeAccount, orchestratorAccount} {
		if m != "" && a == m {
			return &SweepRoleRefusedError{Account: a, Why: "the management account of its AWS Organization. " +
				"A trust into or out of a management account reaches every account in the organization, " +
				"so spawn never creates one; launch detached sweeps from a member account"}
		}
	}
	return nil
}

// --- provisioning -------------------------------------------------------------

// SweepRoleIAM is the part of the IAM client the sweep role's provisioning,
// inspection and teardown use. *iam.Client satisfies it.
type SweepRoleIAM interface {
	rolePolicyAPI
	iam.ListAttachedRolePoliciesAPIClient
	GetRole(ctx context.Context, in *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(ctx context.Context, in *iam.CreateRoleInput, optFns ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	UpdateAssumeRolePolicy(ctx context.Context, in *iam.UpdateAssumeRolePolicyInput, optFns ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error)
	DetachRolePolicy(ctx context.Context, in *iam.DetachRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	DeleteRole(ctx context.Context, in *iam.DeleteRoleInput, optFns ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
}

// NewSweepRoleIAM is the IAM client for cfg's account.
func NewSweepRoleIAM(cfg aws.Config) SweepRoleIAM { return iam.NewFromConfig(cfg) }

// ErrSweepRoleUnknown says IAM would not say whether the role exists (the
// caller may not call iam:GetRole, or a throttle outlasted the retries).
var ErrSweepRoleUnknown = errors.New("could not look up " + sweep.CrossAccountRoleName)

// SweepRoleOutcome is what EnsureSweepCrossAccountRole found and did.
type SweepRoleOutcome struct {
	// Found: the role was there already (made by the script, or an earlier
	// spawn), and was adopted.
	Found bool
	// Created: spawn created the role.
	Created bool
	// TrustUpdated: the role's trust was not the one spawn writes, and was
	// replaced with it.
	TrustUpdated bool
	// PreviousTrust is the trust policy replaced, when TrustUpdated.
	PreviousTrust string
}

// EnsureSweepCrossAccountRole creates SpawnSweepCrossAccountRole in
// computeAccount (the account api acts in) trusting the orchestrator in
// orchestratorAccount, or adopts the one that is there, and converges its
// trust and policy to this spawn's. The caller has run CheckSweepRoleAccounts.
//
// An error from GetRole other than "no such role" wraps ErrSweepRoleUnknown.
// Any other error comes with the outcome so far: outcome.Found says whether
// the role exists all the same.
func EnsureSweepCrossAccountRole(ctx context.Context, api SweepRoleIAM, computeAccount, orchestratorAccount string, warn func(string)) (SweepRoleOutcome, error) {
	var out SweepRoleOutcome
	// Belt and braces: CheckSweepRoleAccounts is the caller's, and this is the
	// one function that creates the trust.
	for _, a := range []string{computeAccount, orchestratorAccount} {
		if why, bad := forbiddenTrustAccounts[a]; bad {
			return out, &SweepRoleRefusedError{Account: a, Why: why}
		}
	}
	trust, err := SweepCrossAccountTrustDocument(orchestratorAccount)
	if err != nil {
		return out, err
	}
	policy, err := SweepCrossAccountPolicyDocument(computeAccount)
	if err != nil {
		return out, err
	}
	role := sweep.CrossAccountRoleName
	principal := SweepOrchestratorRoleARN(orchestratorAccount)

	current, err := getRole(ctx, api, role)
	switch {
	case err == nil:
		out.Found = true
	case isNoSuchEntity(err):
		cerr := retryIAMWrite(func() error {
			_, e := api.CreateRole(ctx, &iam.CreateRoleInput{
				RoleName:                 aws.String(role),
				AssumeRolePolicyDocument: aws.String(trust),
				Description: aws.String("spawn: the sweep orchestrator launches, tags and cancels detached sweep " +
					"members here through this role. Created by spawn; `spawn sweep teardown` removes it."),
				Tags: []types.Tag{{Key: aws.String("spawn:managed"), Value: aws.String("true")}},
			})
			return e
		})
		switch {
		case cerr == nil:
			out.Created = true
		case strings.HasPrefix(iamErrorCode(cerr), "EntityAlreadyExists"):
			// A concurrent spawn created it: adopt it, as below.
			if current, err = getRole(ctx, api, role); err != nil {
				return out, fmt.Errorf("%w: %v", ErrSweepRoleUnknown, err)
			}
			out.Found = true
		default:
			return out, fmt.Errorf("create IAM role %s: %w", role, cerr)
		}
	default:
		return out, fmt.Errorf("%w: %v", ErrSweepRoleUnknown, err)
	}

	if out.Found {
		doc, derr := url.QueryUnescape(aws.ToString(current.AssumeRolePolicyDocument))
		if derr != nil {
			doc = aws.ToString(current.AssumeRolePolicyDocument)
		}
		if !SweepTrustMatches(doc, principal) {
			err := retryIAMWrite(func() error {
				_, e := api.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
					RoleName:       aws.String(role),
					PolicyDocument: aws.String(trust),
				})
				return e
			})
			if err != nil {
				return out, fmt.Errorf("replace the trust policy of IAM role %s: %w", role, err)
			}
			out.TrustUpdated = true
			out.PreviousTrust = doc
		}
	}

	if err := putVersionedRolePolicy(ctx, api, role, sweepRolePolicyBase, policy, sweepRolePolicySchema, warn); err != nil {
		return out, err
	}
	return out, nil
}

func getRole(ctx context.Context, api SweepRoleIAM, role string) (*types.Role, error) {
	var out *iam.GetRoleOutput
	err := retryIAMWrite(func() error {
		var e error
		out, e = api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
		return e
	})
	if err != nil {
		return nil, err
	}
	if out == nil || out.Role == nil {
		return &types.Role{RoleName: aws.String(role)}, nil
	}
	return out.Role, nil
}

// SweepTrustMatches reports whether trust (a decoded trust policy document)
// is the one spawn writes: one Allow of sts:AssumeRole by principal, with no
// condition and nothing else.
func SweepTrustMatches(trust, principal string) bool {
	var doc struct {
		Statement json.RawMessage
	}
	if json.Unmarshal([]byte(trust), &doc) != nil {
		return false
	}
	var stmts []map[string]json.RawMessage
	if json.Unmarshal(doc.Statement, &stmts) != nil {
		var one map[string]json.RawMessage
		if json.Unmarshal(doc.Statement, &one) != nil {
			return false
		}
		stmts = []map[string]json.RawMessage{one}
	}
	if len(stmts) != 1 {
		return false
	}
	st := stmts[0]
	for k := range st {
		switch k {
		case "Effect", "Principal", "Action", "Sid":
		default:
			return false // Condition, NotPrincipal, NotAction, ...
		}
	}
	var effect string
	if json.Unmarshal(st["Effect"], &effect) != nil || effect != "Allow" {
		return false
	}
	if one := stringOrOne(st["Action"]); one != "sts:AssumeRole" {
		return false
	}
	var p map[string]json.RawMessage
	if json.Unmarshal(st["Principal"], &p) != nil || len(p) != 1 {
		return false
	}
	return stringOrOne(p["AWS"]) == principal
}

// stringOrOne is raw as a string, or as the one element of a list; "" for
// anything else.
func stringOrOne(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var l []string
	if json.Unmarshal(raw, &l) == nil && len(l) == 1 {
		return l[0]
	}
	return ""
}

// --- inspection (spawn doctor) ------------------------------------------------

// SweepRoleState is what InspectSweepCrossAccountRole found.
type SweepRoleState struct {
	Exists bool
	// Schema is the newest versioned policy on the role (0: none).
	Schema int
	// Legacy: the role carries the unversioned policy the script wrote.
	Legacy bool
	// Principals are the AWS principals the trust names.
	Principals []string
}

// InspectSweepCrossAccountRole reads the role, its trust and the names of its
// inline policies. Exists is false, with no error, when there is no role.
func InspectSweepCrossAccountRole(ctx context.Context, api SweepRoleIAM) (SweepRoleState, error) {
	var st SweepRoleState
	role, err := getRole(ctx, api, sweep.CrossAccountRoleName)
	if isNoSuchEntity(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Exists = true
	doc, derr := url.QueryUnescape(aws.ToString(role.AssumeRolePolicyDocument))
	if derr != nil {
		doc = aws.ToString(role.AssumeRolePolicyDocument)
	}
	st.Principals = trustPrincipals(doc)
	names, err := listRolePolicyNames(ctx, api, sweep.CrossAccountRoleName)
	if err != nil {
		return st, err
	}
	for _, n := range names {
		if n == sweepRolePolicyBase {
			st.Legacy = true
		}
		if v, ok := parseVersionedPolicyName(sweepRolePolicyBase, n); ok && v > st.Schema {
			st.Schema = v
		}
	}
	return st, nil
}

// trustPrincipals is every AWS principal an Allow in trust names.
func trustPrincipals(trust string) []string {
	var doc struct {
		Statement []struct {
			Effect    string
			Principal json.RawMessage
		}
	}
	if json.Unmarshal([]byte(trust), &doc) != nil {
		return nil
	}
	var out []string
	for _, s := range doc.Statement {
		if s.Effect != "Allow" {
			continue
		}
		var p map[string]json.RawMessage
		if json.Unmarshal(s.Principal, &p) != nil {
			continue
		}
		var one string
		var many []string
		switch {
		case json.Unmarshal(p["AWS"], &one) == nil:
			out = append(out, one)
		case json.Unmarshal(p["AWS"], &many) == nil:
			out = append(out, many...)
		}
	}
	return out
}

// --- teardown ---------------------------------------------------------------

// TeardownSweepCrossAccountRole deletes SpawnSweepCrossAccountRole: every
// inline policy on it, every managed policy attached to it, then the role.
// The role is spawn's by name, so everything on it goes. It reports whether
// there was a role to delete; no role is success.
//
// It does not check for running sweeps: that is the caller's (spawn sweep
// teardown), which knows where the sweep records are.
func TeardownSweepCrossAccountRole(ctx context.Context, api SweepRoleIAM) (bool, error) {
	role := sweep.CrossAccountRoleName
	if _, err := getRole(ctx, api, role); err != nil {
		if isNoSuchEntity(err) {
			return false, nil
		}
		return false, fmt.Errorf("look up IAM role %s: %w", role, err)
	}
	names, err := listRolePolicyNames(ctx, api, role)
	if err != nil {
		return true, fmt.Errorf("list the inline policies of IAM role %s: %w", role, err)
	}
	for _, n := range names {
		if err := deleteRolePolicy(ctx, api, role, n); err != nil {
			return true, fmt.Errorf("delete inline policy %s from IAM role %s: %w", n, role, err)
		}
	}
	p := iam.NewListAttachedRolePoliciesPaginator(api, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(role)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return true, fmt.Errorf("list the managed policies attached to IAM role %s: %w", role, err)
		}
		for _, ap := range page.AttachedPolicies {
			err := retryIAMWrite(func() error {
				_, e := api.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(role), PolicyArn: ap.PolicyArn})
				return e
			})
			if err != nil && !isNoSuchEntity(err) {
				return true, fmt.Errorf("detach %s from IAM role %s: %w", aws.ToString(ap.PolicyArn), role, err)
			}
		}
	}
	err = retryIAMWrite(func() error {
		_, e := api.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(role)})
		return e
	})
	if err != nil && !isNoSuchEntity(err) {
		return true, fmt.Errorf("delete IAM role %s: %w", role, err)
	}
	return true, nil
}
