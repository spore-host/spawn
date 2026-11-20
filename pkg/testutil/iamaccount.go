package testutil

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	smithy "github.com/aws/smithy-go"
)

// IAMAccount is an in-memory IAM account holding customer-managed policies
// and roles, for the hermetic tests of `spawn setup-account` and its
// --remove (pkg/aws, cmd). It records every call in Calls, in order, as
// "<Op> <target>", and enforces the IAM rules those paths depend on: a policy
// with older versions or attachments cannot be deleted, a role with policies
// cannot be deleted, and a policy holds at most five versions.
//
// It deliberately does NOT refuse to delete a policy that is some role's
// permissions boundary: spawn's own guard is what must refuse, and a test
// that relied on the fake would prove nothing about it.
type IAMAccount struct {
	Account  string
	Policies map[string]*FakePolicy // by ARN
	Roles    map[string]*FakeRole   // by name
	Calls    []string
	// Err, when set for an op name ("CreatePolicy"), is returned by that op.
	Err map[string]error
	// OnCall runs at the start of every op, with its name.
	OnCall func(op string)
}

// FakePolicy is one customer-managed policy in an IAMAccount.
type FakePolicy struct {
	Name, Path   string
	Versions     []string          // oldest first
	Default      string            // the default version id
	Docs         map[string]string // version id -> document
	Tags         map[string]string
	Groups       []string // attached to
	Users        []string
	Roles        []string
	BoundaryOf   []string // roles with it as their permissions boundary
	BoundaryUser []string // users with it as their permissions boundary
}

// FakeRole is one role in an IAMAccount.
type FakeRole struct {
	ARN      string
	Tags     map[string]string
	Inline   []string
	Attached []string // managed policy ARNs
}

// NewIAMAccount is an empty account.
func NewIAMAccount(account string) *IAMAccount {
	return &IAMAccount{Account: account, Policies: map[string]*FakePolicy{}, Roles: map[string]*FakeRole{}, Err: map[string]error{}}
}

// AddPolicy puts a policy at arn with versions v1..vN (the last the default).
func (f *IAMAccount) AddPolicy(arn string, n int, tags map[string]string) *FakePolicy {
	name := arn[strings.LastIndex(arn, "/")+1:]
	p := &FakePolicy{Name: name, Docs: map[string]string{}, Tags: tags}
	for i := 1; i <= n; i++ {
		id := "v" + strconv.Itoa(i)
		p.Versions = append(p.Versions, id)
		p.Docs[id] = `{"Version":"2012-10-17","Statement":[]}`
		p.Default = id
	}
	f.Policies[arn] = p
	return p
}

// AddRole puts a role named name with tags.
func (f *IAMAccount) AddRole(name string, tags map[string]string) *FakeRole {
	r := &FakeRole{ARN: "arn:aws:iam::" + f.Account + ":role/" + name, Tags: tags}
	f.Roles[name] = r
	return r
}

// Called reports whether an op was called on target.
func (f *IAMAccount) Called(op, target string) bool {
	for _, c := range f.Calls {
		if c == op+" "+target {
			return true
		}
	}
	return false
}

// Index is the position of the first call op on target, or -1.
func (f *IAMAccount) Index(op, target string) int {
	for i, c := range f.Calls {
		if c == op+" "+target {
			return i
		}
	}
	return -1
}

func (f *IAMAccount) call(op, target string) error {
	f.Calls = append(f.Calls, op+" "+target)
	if f.OnCall != nil {
		f.OnCall(op)
	}
	return f.Err[op]
}

func noEntity(what string) error {
	return &types.NoSuchEntityException{Message: aws.String(what + " not found")}
}

func apiErr(code, msg string) error { return &smithy.GenericAPIError{Code: code, Message: msg} }

func iamTags(m map[string]string) []types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []types.Tag
	for _, k := range keys {
		out = append(out, types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

func remove(s []string, v string) ([]string, bool) {
	for i, x := range s {
		if x == v {
			return append(s[:i:i], s[i+1:]...), true
		}
	}
	return s, false
}

// GetPolicy implements the IAM call.
func (f *IAMAccount) GetPolicy(_ context.Context, in *iam.GetPolicyInput, _ ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.call("GetPolicy", arn); err != nil {
		return nil, err
	}
	p, ok := f.Policies[arn]
	if !ok {
		return nil, noEntity("policy " + arn)
	}
	return &iam.GetPolicyOutput{Policy: &types.Policy{
		Arn: aws.String(arn), PolicyName: aws.String(p.Name), DefaultVersionId: aws.String(p.Default),
		AttachmentCount:               aws.Int32(int32(len(p.Groups) + len(p.Users) + len(p.Roles))),
		PermissionsBoundaryUsageCount: aws.Int32(int32(len(p.BoundaryOf) + len(p.BoundaryUser))),
		Tags:                          iamTags(p.Tags),
	}}, nil
}

// GetPolicyVersion implements the IAM call.
func (f *IAMAccount) GetPolicyVersion(_ context.Context, in *iam.GetPolicyVersionInput, _ ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.call("GetPolicyVersion", arn+" "+aws.ToString(in.VersionId)); err != nil {
		return nil, err
	}
	p, ok := f.Policies[arn]
	if !ok {
		return nil, noEntity("policy " + arn)
	}
	doc, ok := p.Docs[aws.ToString(in.VersionId)]
	if !ok {
		return nil, noEntity("version")
	}
	return &iam.GetPolicyVersionOutput{PolicyVersion: &types.PolicyVersion{Document: aws.String(doc), VersionId: in.VersionId}}, nil
}

// CreatePolicy implements the IAM call.
func (f *IAMAccount) CreatePolicy(_ context.Context, in *iam.CreatePolicyInput, _ ...func(*iam.Options)) (*iam.CreatePolicyOutput, error) {
	arn := "arn:aws:iam::" + f.Account + ":policy" + aws.ToString(in.Path) + aws.ToString(in.PolicyName)
	if err := f.call("CreatePolicy", arn); err != nil {
		return nil, err
	}
	if _, ok := f.Policies[arn]; ok {
		return nil, apiErr("EntityAlreadyExists", "policy exists")
	}
	tags := map[string]string{}
	for _, t := range in.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	p := f.AddPolicy(arn, 1, tags)
	p.Path = aws.ToString(in.Path)
	p.Docs["v1"] = aws.ToString(in.PolicyDocument)
	return &iam.CreatePolicyOutput{Policy: &types.Policy{Arn: aws.String(arn)}}, nil
}

// CreatePolicyVersion implements the IAM call.
func (f *IAMAccount) CreatePolicyVersion(_ context.Context, in *iam.CreatePolicyVersionInput, _ ...func(*iam.Options)) (*iam.CreatePolicyVersionOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.call("CreatePolicyVersion", arn); err != nil {
		return nil, err
	}
	p, ok := f.Policies[arn]
	if !ok {
		return nil, noEntity("policy " + arn)
	}
	if len(p.Versions) >= 5 {
		return nil, apiErr("LimitExceeded", "five versions")
	}
	hi := 0
	for _, v := range p.Versions {
		if n, _ := strconv.Atoi(strings.TrimPrefix(v, "v")); n > hi {
			hi = n
		}
	}
	id := "v" + strconv.Itoa(hi+1)
	p.Versions = append(p.Versions, id)
	p.Docs[id] = aws.ToString(in.PolicyDocument)
	if in.SetAsDefault {
		p.Default = id
	}
	return &iam.CreatePolicyVersionOutput{}, nil
}

// ListPolicyVersions implements the IAM call.
func (f *IAMAccount) ListPolicyVersions(_ context.Context, in *iam.ListPolicyVersionsInput, _ ...func(*iam.Options)) (*iam.ListPolicyVersionsOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.call("ListPolicyVersions", arn); err != nil {
		return nil, err
	}
	p, ok := f.Policies[arn]
	if !ok {
		return nil, noEntity("policy " + arn)
	}
	out := &iam.ListPolicyVersionsOutput{}
	for _, v := range p.Versions {
		out.Versions = append(out.Versions, types.PolicyVersion{VersionId: aws.String(v), IsDefaultVersion: v == p.Default})
	}
	return out, nil
}

// DeletePolicyVersion implements the IAM call.
func (f *IAMAccount) DeletePolicyVersion(_ context.Context, in *iam.DeletePolicyVersionInput, _ ...func(*iam.Options)) (*iam.DeletePolicyVersionOutput, error) {
	arn, id := aws.ToString(in.PolicyArn), aws.ToString(in.VersionId)
	if err := f.call("DeletePolicyVersion", arn+" "+id); err != nil {
		return nil, err
	}
	p, ok := f.Policies[arn]
	if !ok {
		return nil, noEntity("policy " + arn)
	}
	if id == p.Default {
		return nil, apiErr("DeleteConflict", "cannot delete the default version")
	}
	var found bool
	if p.Versions, found = remove(p.Versions, id); !found {
		return nil, noEntity("version " + id)
	}
	delete(p.Docs, id)
	return &iam.DeletePolicyVersionOutput{}, nil
}

// TagPolicy implements the IAM call.
func (f *IAMAccount) TagPolicy(_ context.Context, in *iam.TagPolicyInput, _ ...func(*iam.Options)) (*iam.TagPolicyOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.call("TagPolicy", arn); err != nil {
		return nil, err
	}
	p, ok := f.Policies[arn]
	if !ok {
		return nil, noEntity("policy " + arn)
	}
	if p.Tags == nil {
		p.Tags = map[string]string{}
	}
	for _, t := range in.Tags {
		p.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return &iam.TagPolicyOutput{}, nil
}

// DeletePolicy implements the IAM call: refused while the policy is attached
// or has a non-default version, as IAM does.
func (f *IAMAccount) DeletePolicy(_ context.Context, in *iam.DeletePolicyInput, _ ...func(*iam.Options)) (*iam.DeletePolicyOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.call("DeletePolicy", arn); err != nil {
		return nil, err
	}
	p, ok := f.Policies[arn]
	switch {
	case !ok:
		return nil, noEntity("policy " + arn)
	case len(p.Groups)+len(p.Users)+len(p.Roles) > 0:
		return nil, apiErr("DeleteConflict", "Cannot delete a policy attached to entities.")
	case len(p.Versions) > 1:
		return nil, apiErr("DeleteConflict", "This policy has more than one version.")
	}
	delete(f.Policies, arn)
	return &iam.DeletePolicyOutput{}, nil
}

// ListEntitiesForPolicy implements the IAM call, honouring PolicyUsageFilter.
func (f *IAMAccount) ListEntitiesForPolicy(_ context.Context, in *iam.ListEntitiesForPolicyInput, _ ...func(*iam.Options)) (*iam.ListEntitiesForPolicyOutput, error) {
	arn := aws.ToString(in.PolicyArn)
	if err := f.call("ListEntitiesForPolicy", arn+" "+string(in.PolicyUsageFilter)); err != nil {
		return nil, err
	}
	p, ok := f.Policies[arn]
	if !ok {
		return nil, noEntity("policy " + arn)
	}
	out := &iam.ListEntitiesForPolicyOutput{}
	if in.PolicyUsageFilter != types.PolicyUsageTypePermissionsBoundary {
		for _, g := range p.Groups {
			out.PolicyGroups = append(out.PolicyGroups, types.PolicyGroup{GroupName: aws.String(g)})
		}
		for _, u := range p.Users {
			out.PolicyUsers = append(out.PolicyUsers, types.PolicyUser{UserName: aws.String(u)})
		}
		for _, r := range p.Roles {
			out.PolicyRoles = append(out.PolicyRoles, types.PolicyRole{RoleName: aws.String(r)})
		}
	}
	if in.PolicyUsageFilter != types.PolicyUsageTypePermissionsPolicy {
		for _, r := range p.BoundaryOf {
			out.PolicyRoles = append(out.PolicyRoles, types.PolicyRole{RoleName: aws.String(r)})
		}
		for _, u := range p.BoundaryUser {
			out.PolicyUsers = append(out.PolicyUsers, types.PolicyUser{UserName: aws.String(u)})
		}
	}
	return out, nil
}

func (f *IAMAccount) detach(op string, arn, name string, from func(*FakePolicy) *[]string) error {
	if err := f.call(op, arn+" "+name); err != nil {
		return err
	}
	p, ok := f.Policies[arn]
	if !ok {
		return noEntity("policy " + arn)
	}
	var found bool
	if *from(p), found = remove(*from(p), name); !found {
		return noEntity("attachment")
	}
	return nil
}

// DetachGroupPolicy implements the IAM call.
func (f *IAMAccount) DetachGroupPolicy(_ context.Context, in *iam.DetachGroupPolicyInput, _ ...func(*iam.Options)) (*iam.DetachGroupPolicyOutput, error) {
	return &iam.DetachGroupPolicyOutput{}, f.detach("DetachGroupPolicy", aws.ToString(in.PolicyArn), aws.ToString(in.GroupName), func(p *FakePolicy) *[]string { return &p.Groups })
}

// DetachUserPolicy implements the IAM call.
func (f *IAMAccount) DetachUserPolicy(_ context.Context, in *iam.DetachUserPolicyInput, _ ...func(*iam.Options)) (*iam.DetachUserPolicyOutput, error) {
	return &iam.DetachUserPolicyOutput{}, f.detach("DetachUserPolicy", aws.ToString(in.PolicyArn), aws.ToString(in.UserName), func(p *FakePolicy) *[]string { return &p.Users })
}

// DetachRolePolicy implements the IAM call, for a policy in this account or a
// role's attachment to an AWS-managed one.
func (f *IAMAccount) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	arn, name := aws.ToString(in.PolicyArn), aws.ToString(in.RoleName)
	if r, ok := f.Roles[name]; ok {
		r.Attached, _ = remove(r.Attached, arn)
	}
	if _, ok := f.Policies[arn]; !ok {
		return &iam.DetachRolePolicyOutput{}, f.call("DetachRolePolicy", arn+" "+name)
	}
	return &iam.DetachRolePolicyOutput{}, f.detach("DetachRolePolicy", arn, name, func(p *FakePolicy) *[]string { return &p.Roles })
}

// GetRole implements the IAM call.
func (f *IAMAccount) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	name := aws.ToString(in.RoleName)
	if err := f.call("GetRole", name); err != nil {
		return nil, err
	}
	r, ok := f.Roles[name]
	if !ok {
		return nil, noEntity("role " + name)
	}
	return &iam.GetRoleOutput{Role: &types.Role{RoleName: aws.String(name), Arn: aws.String(r.ARN), Tags: iamTags(r.Tags)}}, nil
}

// ListRolePolicies implements the IAM call.
func (f *IAMAccount) ListRolePolicies(_ context.Context, in *iam.ListRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	name := aws.ToString(in.RoleName)
	if err := f.call("ListRolePolicies", name); err != nil {
		return nil, err
	}
	r, ok := f.Roles[name]
	if !ok {
		return nil, noEntity("role " + name)
	}
	return &iam.ListRolePoliciesOutput{PolicyNames: append([]string(nil), r.Inline...)}, nil
}

// DeleteRolePolicy implements the IAM call.
func (f *IAMAccount) DeleteRolePolicy(_ context.Context, in *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	name, pol := aws.ToString(in.RoleName), aws.ToString(in.PolicyName)
	if err := f.call("DeleteRolePolicy", name+" "+pol); err != nil {
		return nil, err
	}
	r, ok := f.Roles[name]
	if !ok {
		return nil, noEntity("role " + name)
	}
	var found bool
	if r.Inline, found = remove(r.Inline, pol); !found {
		return nil, noEntity("inline policy " + pol)
	}
	return &iam.DeleteRolePolicyOutput{}, nil
}

// ListAttachedRolePolicies implements the IAM call.
func (f *IAMAccount) ListAttachedRolePolicies(_ context.Context, in *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	name := aws.ToString(in.RoleName)
	if err := f.call("ListAttachedRolePolicies", name); err != nil {
		return nil, err
	}
	r, ok := f.Roles[name]
	if !ok {
		return nil, noEntity("role " + name)
	}
	out := &iam.ListAttachedRolePoliciesOutput{}
	for _, a := range r.Attached {
		out.AttachedPolicies = append(out.AttachedPolicies, types.AttachedPolicy{PolicyArn: aws.String(a)})
	}
	return out, nil
}

// DeleteRole implements the IAM call: refused while the role has policies.
func (f *IAMAccount) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	name := aws.ToString(in.RoleName)
	if err := f.call("DeleteRole", name); err != nil {
		return nil, err
	}
	r, ok := f.Roles[name]
	switch {
	case !ok:
		return nil, noEntity("role " + name)
	case len(r.Inline)+len(r.Attached) > 0:
		return nil, apiErr("DeleteConflict", "Cannot delete entity, must delete policies first.")
	}
	delete(f.Roles, name)
	return &iam.DeleteRoleOutput{}, nil
}
