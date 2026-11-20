package aws

// What `spawn setup-account` provisions in an account, and how
// `spawn setup-account --remove` takes it away again.
//
// setup-account is the one admin step spawn has. It creates two
// customer-managed policies at /spore-host/:
//
//   - spawn-instance-boundary (iam_boundary.go), the permissions boundary
//     every role spawn creates is capped by;
//   - spawn-user-lifecycle (spawn#1666), which grants a CLI user the actions
//     on the shared-account lifecycle lease table (spore-host-lifecycle) that
//     spawn's creators, `spawn doctor` and `spawn reaper teardown
//     --if-idle-for` use. spore.host provisions the IAM a feature needs; it
//     used to be a statement an administrator copied into a policy by hand.
//     The administrator attaches it to the groups or roles spawn users sign in
//     with.
//
// These two are the only things spore.host keeps in an account while it is
// set up (owner decision, spore-host/spore-host#752). Both are tagged
// spore-host:managed-by=spawn and spore-host:permanent=true: no idle
// teardown, cleanup or reaper deletes them (footprintspawn classes both
// permanent, by name as well as tag, and neither name is a teardown pattern). They are removed
// only by an administrator running `spawn setup-account --remove`, which
// spawn#1656 adds. Neither is ever tagged spore-host:role=service, which
// marks spore.host's own live service.
//
// --remove refuses while any role or user still has the boundary as its
// permissions boundary (deleting it would uncap them), and in spore.host's
// infra account. It deletes each policy's non-default versions, then the
// policy; the user policy is first detached from whatever an administrator
// attached it to. With --offboard it also deletes the spore-portal-onboard
// role `spawn onboard` (or the portal's CloudFormation quick-create) made: the
// record that the account consented to the portal, so it goes only when asked.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	libtags "github.com/spore-host/libs/tags"
	"github.com/spore-host/libs/tags/iamtags"

	"github.com/spore-host/spawn/pkg/footprintspawn"
	"github.com/spore-host/spawn/pkg/lifecyclemark"
	"github.com/spore-host/spawn/pkg/sporetags"
)

// spawnUserLifecyclePolicyName is the user lifecycle policy's name at
// sporedPolicyPath. sporedPolicyPath + this is lifecyclemark.UserPolicy (a
// test holds the two equal).
const spawnUserLifecyclePolicyName = footprintspawn.UserLifecyclePolicyName

// SpawnUserLifecyclePolicyARN is the ARN of the policy `spawn setup-account`
// creates in account to grant CLI users the lifecycle lease actions.
func SpawnUserLifecyclePolicyARN(account string) string {
	return "arn:aws:iam::" + account + ":policy" + sporedPolicyPath + spawnUserLifecyclePolicyName
}

// SpawnInstanceBoundaryARN is the ARN of the spawn-instance permissions
// boundary in account.
func SpawnInstanceBoundaryARN(account string) string { return spawnInstanceBoundaryARN(account) }

// userLifecycleTableARN is the resource the user lifecycle policy grants on:
// the lease table in every region of account. The account is written in, not
// a policy variable (CreatePolicy rejects variables in a Resource, #1069).
func userLifecycleTableARN(account string) string {
	return "arn:aws:dynamodb:*:" + account + ":table/spore-host-lifecycle"
}

// spawnUserLifecycleDocument is the user lifecycle policy's document for
// account: lifecyclemark.UserActions on the lease table, nothing else.
func spawnUserLifecycleDocument(account string) string {
	type stmt struct {
		Sid      string   `json:"Sid"`
		Effect   string   `json:"Effect"`
		Action   []string `json:"Action"`
		Resource string   `json:"Resource"`
	}
	doc := struct {
		Version   string `json:"Version"`
		Statement []stmt `json:"Statement"`
	}{"2012-10-17", []stmt{{
		Sid:      "SporeHostLifecycleLease",
		Effect:   "Allow",
		Action:   lifecyclemark.UserActions(),
		Resource: userLifecycleTableARN(account),
	}}}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err) // a fixed struct of strings always marshals
	}
	return string(b)
}

func spawnUserLifecycleDescription() string {
	return "spawn: lets a spawn user mark the shared-account lifecycle lease (table spore-host-lifecycle), " +
		"so idle teardown can see the account in use. Created by `spawn setup-account`; attach it to spawn users' " +
		"groups or roles. Removed only by `spawn setup-account --remove` (spawn#1666)."
}

// userLifecycleTags are the user lifecycle policy's tags: the spore.host set,
// spore-host:permanent=true (permanent account setup, like the boundary it is
// provisioned beside) and a component naming it.
func userLifecycleTags() map[string]string {
	t := sporetags.Permanent("")
	t[libtags.Component] = "user-lifecycle"
	return t
}

// AccountSetupAPI is the IAM slice `spawn setup-account` uses: provisioning the
// boundary and the user lifecycle policy, and removing them and the
// spore-portal-onboard role. *iam.Client satisfies it.
type AccountSetupAPI interface {
	BoundaryAdminAPI
	TagPolicy(ctx context.Context, in *iam.TagPolicyInput, optFns ...func(*iam.Options)) (*iam.TagPolicyOutput, error)
	DeletePolicy(ctx context.Context, in *iam.DeletePolicyInput, optFns ...func(*iam.Options)) (*iam.DeletePolicyOutput, error)
	ListEntitiesForPolicy(ctx context.Context, in *iam.ListEntitiesForPolicyInput, optFns ...func(*iam.Options)) (*iam.ListEntitiesForPolicyOutput, error)
	DetachGroupPolicy(ctx context.Context, in *iam.DetachGroupPolicyInput, optFns ...func(*iam.Options)) (*iam.DetachGroupPolicyOutput, error)
	DetachUserPolicy(ctx context.Context, in *iam.DetachUserPolicyInput, optFns ...func(*iam.Options)) (*iam.DetachUserPolicyOutput, error)
	DetachRolePolicy(ctx context.Context, in *iam.DetachRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	GetRole(ctx context.Context, in *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	ListRolePolicies(ctx context.Context, in *iam.ListRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
	DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, optFns ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	ListAttachedRolePolicies(ctx context.Context, in *iam.ListAttachedRolePoliciesInput, optFns ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	DeleteRole(ctx context.Context, in *iam.DeleteRoleInput, optFns ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
}

// NewAccountSetupIAM is the IAM client `spawn setup-account` uses, from cfg.
func NewAccountSetupIAM(cfg aws.Config) AccountSetupAPI { return iam.NewFromConfig(cfg) }

// UserPolicyOutcome is what EnsureSpawnUserLifecyclePolicy found and did.
type UserPolicyOutcome struct {
	ARN       string
	Created   bool // the policy did not exist and was created
	Updated   bool // a new default version was written (the document had drifted)
	Pruned    int  // older versions deleted to stay under IAM's five-version cap
	Unchanged bool // the deployed document already matched this spawn's
	Tagged    bool // the existing policy was given the spore.host tags it lacked
}

// EnsureSpawnUserLifecyclePolicy creates the user lifecycle policy in account,
// or converges an existing one to this spawn's document and tags. Re-running
// it is a no-op once the policy is current.
func EnsureSpawnUserLifecyclePolicy(ctx context.Context, api AccountSetupAPI, account string, warn func(string)) (UserPolicyOutcome, error) {
	arn := SpawnUserLifecyclePolicyARN(account)
	want := spawnUserLifecycleDocument(account)
	out := UserPolicyOutcome{ARN: arn}
	const what = "the spawn user lifecycle policy"

	cur, err := api.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)})
	switch {
	case err == nil:
	case isNoSuchEntity(err):
		cerr := createPolicyTagged(ctx, func(in *iam.CreatePolicyInput) error {
			return retryIAMWrite(func() error {
				_, e := api.CreatePolicy(ctx, in)
				return e
			})
		}, &iam.CreatePolicyInput{
			PolicyName:     aws.String(spawnUserLifecyclePolicyName),
			Path:           aws.String(sporedPolicyPath),
			PolicyDocument: aws.String(want),
			Description:    aws.String(spawnUserLifecycleDescription()),
			Tags:           iamtags.FromMap(userLifecycleTags()),
		})
		switch {
		case cerr == nil:
			out.Created = true
			return out, nil
		case !isEntityAlreadyExists(cerr):
			return out, fmt.Errorf("create %s %s: %w", what, arn, cerr)
		}
		if cur, err = api.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)}); err != nil {
			return out, fmt.Errorf("read %s %s after a create race: %w", what, arn, err)
		}
	default:
		return out, fmt.Errorf("look up %s %s: %w", what, arn, err)
	}

	defaultVersion := "v1"
	if cur != nil && cur.Policy != nil {
		if add := sporetags.Converge(iamtags.ToMap(cur.Policy.Tags), userLifecycleTags()); len(add) > 0 {
			if _, terr := api.TagPolicy(ctx, &iam.TagPolicyInput{PolicyArn: aws.String(arn), Tags: iamtags.FromMap(add)}); terr != nil {
				warn(fmt.Sprintf("could not tag %s %s with the spore-host:* tags (iam:TagPolicy: %v); it grants as before", what, arn, terr))
			} else {
				out.Tagged = true
			}
		}
		if cur.Policy.DefaultVersionId != nil {
			defaultVersion = aws.ToString(cur.Policy.DefaultVersionId)
		}
	}
	gv, err := api.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{PolicyArn: aws.String(arn), VersionId: aws.String(defaultVersion)})
	if err != nil {
		return out, fmt.Errorf("read the current version of %s %s: %w", what, arn, err)
	}
	if gv.PolicyVersion != nil && samePolicyDocument(aws.ToString(gv.PolicyVersion.Document), want) {
		out.Unchanged = true
		return out, nil
	}
	if out.Pruned, err = prunePolicyVersions(ctx, api, arn, what, warn); err != nil {
		return out, err
	}
	err = retryIAMWrite(func() error {
		_, e := api.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{
			PolicyArn: aws.String(arn), PolicyDocument: aws.String(want), SetAsDefault: true,
		})
		return e
	})
	if err != nil {
		return out, fmt.Errorf("write a new version of %s %s: %w", what, arn, err)
	}
	out.Updated = true
	return out, nil
}

// PortalOnboardRoleName is the role the spore.host portal assumes in an
// onboarded account (`spawn onboard`, or the portal's CloudFormation
// quick-create).
const PortalOnboardRoleName = "spore-portal-onboard"

// portalOnboardPurpose is the Purpose tag both creators of the onboard role
// put on it. A role of that name without it is not spore.host's.
const portalOnboardPurpose = "spore-portal-onboard"

// cfnStackNameTag is the tag CloudFormation puts on a resource it created.
const cfnStackNameTag = "aws:cloudformation:stack-name"

// PolicyEntity is a user, group or role a managed policy is attached to, or
// is the permissions boundary of.
type PolicyEntity struct {
	Kind string // "user", "group" or "role"
	Name string
}

func (e PolicyEntity) String() string { return e.Kind + "/" + e.Name }

// PolicyRemoval is one of setup-account's policies as --remove found it.
type PolicyRemoval struct {
	ARN    string
	Exists bool
	// DefaultVersion is deleted with the policy; OlderVersions, oldest first,
	// are deleted before it (IAM refuses to delete a policy that has any).
	DefaultVersion string
	OlderVersions  []string
	// AttachedTo are the users, groups and roles it is attached to; each is
	// detached before the policy is deleted.
	AttachedTo []PolicyEntity
	// BoundaryOf are the roles and users that have it as their permissions
	// boundary. Any at all refuses the removal of the boundary.
	BoundaryOf []PolicyEntity
}

// RoleRemoval is the spore-portal-onboard role as --remove --offboard found it.
type RoleRemoval struct {
	Name           string
	ARN            string
	Exists         bool
	InlinePolicies []string // deleted first
	Attached       []string // managed policy ARNs, detached next
	Stack          string   // the CloudFormation stack that made it, when tagged
}

// AccountRemovalPlan is what `spawn setup-account --remove` would delete in an
// account, in order, and why it would refuse. It is built from reads only.
type AccountRemovalPlan struct {
	Account     string
	Offboard    bool
	UserPolicy  PolicyRemoval
	Boundary    PolicyRemoval
	OnboardRole *RoleRemoval // set only with Offboard
	// Refusals, when any, mean nothing is deleted.
	Refusals []string
}

// Empty reports whether there is nothing to delete.
func (p *AccountRemovalPlan) Empty() bool {
	return !p.UserPolicy.Exists && !p.Boundary.Exists && (p.OnboardRole == nil || !p.OnboardRole.Exists)
}

// GuardAccountRemoval refuses an account setup-account --remove must never
// run in: one it could not identify, and spore.host's infra account, which
// holds the live service.
func GuardAccountRemoval(account string) error {
	switch account {
	case "":
		return fmt.Errorf("refusing to remove spawn's account setup: the account could not be identified")
	case footprintspawn.InfraAccountID:
		return fmt.Errorf("refusing to remove spawn's account setup in %s: it is spore.host's infra account, which runs spore.host's own service", account)
	}
	return nil
}

// PlanAccountRemoval reads what `spawn setup-account --remove` would delete in
// account, and, with offboard, the spore-portal-onboard role too. It makes no
// change.
func PlanAccountRemoval(ctx context.Context, api AccountSetupAPI, account string, offboard bool) (*AccountRemovalPlan, error) {
	if err := GuardAccountRemoval(account); err != nil {
		return nil, err
	}
	p := &AccountRemovalPlan{Account: account, Offboard: offboard}
	var err error
	if p.UserPolicy, err = readPolicyRemoval(ctx, api, SpawnUserLifecyclePolicyARN(account)); err != nil {
		return nil, err
	}
	if p.Boundary, err = readPolicyRemoval(ctx, api, spawnInstanceBoundaryARN(account)); err != nil {
		return nil, err
	}
	if n := len(p.Boundary.BoundaryOf); n > 0 {
		p.Refusals = append(p.Refusals, fmt.Sprintf("the boundary %s is the permissions boundary of %d role(s) or user(s) (%s); "+
			"deleting it would leave them uncapped. Delete those roles first (`spawn teardown --plan` and `spawn cleanup --instance-roles` "+
			"show spawn's), or remove their boundary, then run this again", p.Boundary.ARN, n, entityList(p.Boundary.BoundaryOf)))
	}
	if len(p.UserPolicy.BoundaryOf) > 0 {
		p.Refusals = append(p.Refusals, fmt.Sprintf("the policy %s is the permissions boundary of %s; deleting it would widen what they may do. "+
			"Remove it as their boundary, then run this again", p.UserPolicy.ARN, entityList(p.UserPolicy.BoundaryOf)))
	}
	if len(p.Boundary.AttachedTo) > 0 {
		p.Refusals = append(p.Refusals, fmt.Sprintf("the boundary %s is attached as a permissions policy to %s; it is meant only as a boundary, "+
			"so spawn leaves that to you: detach it, then run this again", p.Boundary.ARN, entityList(p.Boundary.AttachedTo)))
	}
	if offboard {
		r, rerr := readRoleRemoval(ctx, api, PortalOnboardRoleName)
		if rerr != nil {
			return nil, rerr
		}
		p.OnboardRole = r
		if r.Exists && r.Stack != "" {
			p.Refusals = append(p.Refusals, fmt.Sprintf("the role %s was created by the CloudFormation stack %s (the portal's quick-create): "+
				"delete that stack instead (aws cloudformation delete-stack --stack-name %s), which also removes its phone-home function, "+
				"then run this again", r.Name, r.Stack, r.Stack))
		}
	}
	return p, nil
}

// readPolicyRemoval reads one policy: its versions, attachments and boundary
// uses. A missing policy is Exists false.
func readPolicyRemoval(ctx context.Context, api AccountSetupAPI, arn string) (PolicyRemoval, error) {
	pr := PolicyRemoval{ARN: arn}
	gp, err := api.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)})
	switch {
	case isNoSuchEntity(err):
		return pr, nil
	case err != nil:
		return pr, fmt.Errorf("read the policy %s: %w", arn, err)
	}
	pr.Exists = true
	if gp != nil && gp.Policy != nil {
		pr.DefaultVersion = aws.ToString(gp.Policy.DefaultVersionId)
	}
	vp := iam.NewListPolicyVersionsPaginator(api, &iam.ListPolicyVersionsInput{PolicyArn: aws.String(arn)})
	for vp.HasMorePages() {
		page, err := vp.NextPage(ctx)
		if err != nil {
			return pr, fmt.Errorf("list the versions of %s: %w", arn, err)
		}
		for _, v := range page.Versions {
			id := aws.ToString(v.VersionId)
			if v.IsDefaultVersion {
				pr.DefaultVersion = id
				continue
			}
			pr.OlderVersions = append(pr.OlderVersions, id)
		}
	}
	sort.Slice(pr.OlderVersions, func(i, j int) bool {
		return policyVersionNum(pr.OlderVersions[i]) < policyVersionNum(pr.OlderVersions[j])
	})
	if pr.AttachedTo, err = policyEntities(ctx, api, arn, types.PolicyUsageTypePermissionsPolicy); err != nil {
		return pr, err
	}
	if pr.BoundaryOf, err = policyEntities(ctx, api, arn, types.PolicyUsageTypePermissionsBoundary); err != nil {
		return pr, err
	}
	return pr, nil
}

// policyEntities lists the users, groups and roles that use the policy at
// arn as usage (attached, or as a permissions boundary).
func policyEntities(ctx context.Context, api AccountSetupAPI, arn string, usage types.PolicyUsageType) ([]PolicyEntity, error) {
	var out []PolicyEntity
	p := iam.NewListEntitiesForPolicyPaginator(api, &iam.ListEntitiesForPolicyInput{
		PolicyArn: aws.String(arn), PolicyUsageFilter: usage,
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list what uses the policy %s (%s): %w", arn, usage, err)
		}
		for _, g := range page.PolicyGroups {
			out = append(out, PolicyEntity{Kind: "group", Name: aws.ToString(g.GroupName)})
		}
		for _, u := range page.PolicyUsers {
			out = append(out, PolicyEntity{Kind: "user", Name: aws.ToString(u.UserName)})
		}
		for _, r := range page.PolicyRoles {
			out = append(out, PolicyEntity{Kind: "role", Name: aws.ToString(r.RoleName)})
		}
	}
	return out, nil
}

// readRoleRemoval reads the onboard role. A role of that name that is not
// tagged Purpose=spore-portal-onboard is not spore.host's, and is refused.
func readRoleRemoval(ctx context.Context, api AccountSetupAPI, name string) (*RoleRemoval, error) {
	r := &RoleRemoval{Name: name}
	gr, err := api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	switch {
	case isNoSuchEntity(err):
		return r, nil
	case err != nil:
		return nil, fmt.Errorf("read the role %s: %w", name, err)
	}
	r.Exists = true
	var t map[string]string
	if gr != nil && gr.Role != nil {
		r.ARN = aws.ToString(gr.Role.Arn)
		t = iamtags.ToMap(gr.Role.Tags)
	}
	if t["Purpose"] != portalOnboardPurpose {
		return nil, fmt.Errorf("a role named %s exists but is not tagged Purpose=%s, so spore.host did not create it; refusing to delete it", name, portalOnboardPurpose)
	}
	r.Stack = t[cfnStackNameTag]
	ip := iam.NewListRolePoliciesPaginator(api, &iam.ListRolePoliciesInput{RoleName: aws.String(name)})
	for ip.HasMorePages() {
		page, err := ip.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the inline policies of %s: %w", name, err)
		}
		r.InlinePolicies = append(r.InlinePolicies, page.PolicyNames...)
	}
	ap := iam.NewListAttachedRolePoliciesPaginator(api, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(name)})
	for ap.HasMorePages() {
		page, err := ap.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the managed policies attached to %s: %w", name, err)
		}
		for _, a := range page.AttachedPolicies {
			r.Attached = append(r.Attached, aws.ToString(a.PolicyArn))
		}
	}
	return r, nil
}

func entityList(es []PolicyEntity) string {
	s := ""
	for i, e := range es {
		if i > 0 {
			s += ", "
		}
		s += e.String()
	}
	return s
}

// ExecuteAccountRemoval deletes what plan lists, in order: the user lifecycle
// policy (detached from everything, its older versions, then it), with
// Offboard the spore-portal-onboard role (its inline policies, its managed
// attachments, then it), and last the boundary (its older versions, then it).
// It refuses a plan with Refusals, and re-reads the boundary's use just before
// deleting it: a role given the boundary since the plan was made refuses that
// step. progress is told each deletion.
func ExecuteAccountRemoval(ctx context.Context, api AccountSetupAPI, plan *AccountRemovalPlan, progress func(string)) error {
	if err := GuardAccountRemoval(plan.Account); err != nil {
		return err
	}
	if len(plan.Refusals) > 0 {
		return fmt.Errorf("refusing to remove spawn's account setup: %s", plan.Refusals[0])
	}
	if plan.UserPolicy.Exists {
		if err := deletePolicyRemoval(ctx, api, plan.UserPolicy, progress); err != nil {
			return err
		}
	}
	if plan.Offboard && plan.OnboardRole != nil && plan.OnboardRole.Exists {
		if err := deleteRoleRemoval(ctx, api, plan.OnboardRole, progress); err != nil {
			return err
		}
	}
	if !plan.Boundary.Exists {
		return nil
	}
	// The boundary goes last, after a fresh look at what uses it.
	now, err := readPolicyRemoval(ctx, api, plan.Boundary.ARN)
	if err != nil {
		return err
	}
	switch {
	case !now.Exists:
		return nil
	case len(now.BoundaryOf) > 0:
		return fmt.Errorf("refusing to delete the boundary %s: it is now the permissions boundary of %s, "+
			"and deleting it would leave them uncapped", now.ARN, entityList(now.BoundaryOf))
	case len(now.AttachedTo) > 0:
		return fmt.Errorf("refusing to delete the boundary %s: it is now attached to %s", now.ARN, entityList(now.AttachedTo))
	}
	return deletePolicyRemoval(ctx, api, now, progress)
}

// deletePolicyRemoval detaches pr from everything it is attached to, deletes
// its older versions oldest first, then deletes it.
func deletePolicyRemoval(ctx context.Context, api AccountSetupAPI, pr PolicyRemoval, progress func(string)) error {
	arn := aws.String(pr.ARN)
	for _, e := range pr.AttachedTo {
		var err error
		switch e.Kind {
		case "group":
			_, err = api.DetachGroupPolicy(ctx, &iam.DetachGroupPolicyInput{GroupName: aws.String(e.Name), PolicyArn: arn})
		case "user":
			_, err = api.DetachUserPolicy(ctx, &iam.DetachUserPolicyInput{UserName: aws.String(e.Name), PolicyArn: arn})
		case "role":
			_, err = api.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(e.Name), PolicyArn: arn})
		}
		if err != nil && !isNoSuchEntity(err) {
			return fmt.Errorf("detach %s from %s: %w", pr.ARN, e, err)
		}
		progress(fmt.Sprintf("detached %s from %s", pr.ARN, e))
	}
	for _, v := range pr.OlderVersions {
		id := v
		err := retryIAMWrite(func() error {
			_, e := api.DeletePolicyVersion(ctx, &iam.DeletePolicyVersionInput{PolicyArn: arn, VersionId: aws.String(id)})
			return e
		})
		if err != nil && !isNoSuchEntity(err) {
			return fmt.Errorf("delete version %s of %s: %w", id, pr.ARN, err)
		}
		progress(fmt.Sprintf("deleted version %s of %s", id, pr.ARN))
	}
	err := retryIAMWrite(func() error {
		_, e := api.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: arn})
		return e
	})
	if err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("delete %s: %w", pr.ARN, err)
	}
	progress(fmt.Sprintf("deleted %s", pr.ARN))
	return nil
}

// deleteRoleRemoval empties the onboard role of its policies, then deletes it.
func deleteRoleRemoval(ctx context.Context, api AccountSetupAPI, r *RoleRemoval, progress func(string)) error {
	name := aws.String(r.Name)
	for _, p := range r.InlinePolicies {
		if _, err := api.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: name, PolicyName: aws.String(p)}); err != nil && !isNoSuchEntity(err) {
			return fmt.Errorf("delete the inline policy %s of %s: %w", p, r.Name, err)
		}
		progress(fmt.Sprintf("deleted the inline policy %s of %s", p, r.Name))
	}
	for _, a := range r.Attached {
		if _, err := api.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: name, PolicyArn: aws.String(a)}); err != nil && !isNoSuchEntity(err) {
			return fmt.Errorf("detach %s from %s: %w", a, r.Name, err)
		}
		progress(fmt.Sprintf("detached %s from %s", a, r.Name))
	}
	err := retryIAMWrite(func() error {
		_, e := api.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: name})
		return e
	})
	if err != nil && !isNoSuchEntity(err) {
		return fmt.Errorf("delete the role %s: %w", r.Name, err)
	}
	progress(fmt.Sprintf("deleted the role %s", r.Name))
	return nil
}
