package aws

import (
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/spore-host/libs/tags/iamtags"

	"github.com/spore-host/spawn/internal/sdkanswer"
)

// iamModel is an in-memory IAM (and STS) for the spore.host tag tests
// (#1652): enough of roles, instance profiles and managed policies, with
// their tags, for the role-setup paths to run through real SDK clients, so
// the tagtest Detector sees every call they make.
type iamModel struct {
	mu       sync.Mutex
	account  string
	roles    map[string]*modelRole
	profiles map[string]*modelProfile
	policies map[string]*modelPolicy // by ARN
	// denyTaggedCreatePolicy refuses a CreatePolicy that carries tags, as
	// IAM does for a principal without iam:TagPolicy.
	denyTaggedCreatePolicy bool
	calls                  []string
}

type modelRole struct {
	tags     map[string]string
	boundary string
	inline   map[string]string
	attached map[string]string // arn -> name
}

type modelProfile struct {
	tags  map[string]string
	roles []string
}

type modelPolicy struct {
	name, doc string
	tags      map[string]string
}

func newIAMModel() *iamModel {
	return &iamModel{account: "123456789012", roles: map[string]*modelRole{},
		profiles: map[string]*modelProfile{}, policies: map[string]*modelPolicy{}}
}

// seedBoundary makes the spawn-instance boundary exist, untagged, as an
// older spawn created it.
func (m *iamModel) seedBoundary() {
	m.policies[spawnInstanceBoundaryARN(m.account)] = &modelPolicy{name: spawnInstanceBoundaryName,
		doc: spawnInstanceBoundaryDocument, tags: map[string]string{}}
}

func (m *iamModel) called(op string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c == op {
			n++
		}
	}
	return n
}

func addTags(dst map[string]string, ts []iamtypes.Tag) {
	for k, v := range iamtags.ToMap(ts) {
		dst[k] = v
	}
}

func copyTags(m map[string]string) []iamtypes.Tag { return iamtags.FromMap(m) }

func (m *iamModel) answer(service, op string, in any) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, op)
	if service == "STS" {
		return &sts.GetCallerIdentityOutput{Account: aws.String(m.account),
			Arn: aws.String("arn:aws:iam::" + m.account + ":user/u"), UserId: aws.String("AIDAEXAMPLEUSER")}, nil
	}
	if service != "IAM" {
		return nil, sdkanswer.Err("NotInModel")
	}
	switch v := in.(type) {
	case *iam.GetRoleInput:
		r, ok := m.roles[aws.ToString(v.RoleName)]
		if !ok {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		role := &iamtypes.Role{RoleName: v.RoleName, Arn: aws.String("arn:aws:iam::" + m.account + ":role/" + aws.ToString(v.RoleName)),
			Tags: copyTags(r.tags), AssumeRolePolicyDocument: aws.String(url.QueryEscape(`{"Version":"2012-10-17","Statement":[]}`))}
		if r.boundary != "" {
			role.PermissionsBoundary = &iamtypes.AttachedPermissionsBoundary{PermissionsBoundaryArn: aws.String(r.boundary)}
		}
		return &iam.GetRoleOutput{Role: role}, nil
	case *iam.CreateRoleInput:
		name := aws.ToString(v.RoleName)
		if _, ok := m.roles[name]; ok {
			return nil, sdkanswer.Err("EntityAlreadyExists")
		}
		r := &modelRole{tags: map[string]string{}, boundary: aws.ToString(v.PermissionsBoundary), inline: map[string]string{}, attached: map[string]string{}}
		addTags(r.tags, v.Tags)
		m.roles[name] = r
		return &iam.CreateRoleOutput{Role: &iamtypes.Role{RoleName: v.RoleName, Arn: aws.String("arn:aws:iam::" + m.account + ":role/" + name)}}, nil
	case *iam.PutRolePermissionsBoundaryInput:
		if r, ok := m.roles[aws.ToString(v.RoleName)]; ok {
			r.boundary = aws.ToString(v.PermissionsBoundary)
		}
	case *iam.TagRoleInput:
		r, ok := m.roles[aws.ToString(v.RoleName)]
		if !ok {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		addTags(r.tags, v.Tags)
	case *iam.PutRolePolicyInput:
		if r, ok := m.roles[aws.ToString(v.RoleName)]; ok {
			r.inline[aws.ToString(v.PolicyName)] = aws.ToString(v.PolicyDocument)
		}
	case *iam.DeleteRolePolicyInput:
		if r, ok := m.roles[aws.ToString(v.RoleName)]; ok {
			delete(r.inline, aws.ToString(v.PolicyName))
		}
	case *iam.GetRolePolicyInput:
		r, ok := m.roles[aws.ToString(v.RoleName)]
		if !ok || r.inline[aws.ToString(v.PolicyName)] == "" {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		return &iam.GetRolePolicyOutput{PolicyName: v.PolicyName, RoleName: v.RoleName,
			PolicyDocument: aws.String(r.inline[aws.ToString(v.PolicyName)])}, nil
	case *iam.ListRolePoliciesInput:
		out := &iam.ListRolePoliciesOutput{}
		if r, ok := m.roles[aws.ToString(v.RoleName)]; ok {
			for n := range r.inline {
				out.PolicyNames = append(out.PolicyNames, n)
			}
		}
		return out, nil
	case *iam.ListAttachedRolePoliciesInput:
		out := &iam.ListAttachedRolePoliciesOutput{}
		if r, ok := m.roles[aws.ToString(v.RoleName)]; ok {
			for arn, n := range r.attached {
				out.AttachedPolicies = append(out.AttachedPolicies, iamtypes.AttachedPolicy{PolicyArn: aws.String(arn), PolicyName: aws.String(n)})
			}
		}
		return out, nil
	case *iam.AttachRolePolicyInput:
		r, ok := m.roles[aws.ToString(v.RoleName)]
		if !ok {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		arn := aws.ToString(v.PolicyArn)
		r.attached[arn] = arn[strings.LastIndex(arn, "/")+1:]
	case *iam.DetachRolePolicyInput:
		if r, ok := m.roles[aws.ToString(v.RoleName)]; ok {
			delete(r.attached, aws.ToString(v.PolicyArn))
		}
	case *iam.GetPolicyInput:
		p, ok := m.policies[aws.ToString(v.PolicyArn)]
		if !ok {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		return &iam.GetPolicyOutput{Policy: &iamtypes.Policy{Arn: v.PolicyArn, PolicyName: aws.String(p.name),
			DefaultVersionId: aws.String("v1"), Tags: copyTags(p.tags)}}, nil
	case *iam.CreatePolicyInput:
		if m.denyTaggedCreatePolicy && len(v.Tags) > 0 {
			return nil, sdkanswer.Err("AccessDenied")
		}
		arn := "arn:aws:iam::" + m.account + ":policy" + aws.ToString(v.Path) + aws.ToString(v.PolicyName)
		if _, ok := m.policies[arn]; ok {
			return nil, sdkanswer.Err("EntityAlreadyExists")
		}
		p := &modelPolicy{name: aws.ToString(v.PolicyName), doc: aws.ToString(v.PolicyDocument), tags: map[string]string{}}
		addTags(p.tags, v.Tags)
		m.policies[arn] = p
		return &iam.CreatePolicyOutput{Policy: &iamtypes.Policy{Arn: aws.String(arn), PolicyName: v.PolicyName}}, nil
	case *iam.GetPolicyVersionInput:
		p, ok := m.policies[aws.ToString(v.PolicyArn)]
		if !ok {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		return &iam.GetPolicyVersionOutput{PolicyVersion: &iamtypes.PolicyVersion{Document: aws.String(url.QueryEscape(p.doc)),
			VersionId: aws.String("v1"), IsDefaultVersion: true}}, nil
	case *iam.TagPolicyInput:
		p, ok := m.policies[aws.ToString(v.PolicyArn)]
		if !ok {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		addTags(p.tags, v.Tags)
	case *iam.DeletePolicyInput:
		delete(m.policies, aws.ToString(v.PolicyArn))
	case *iam.GetInstanceProfileInput:
		p, ok := m.profiles[aws.ToString(v.InstanceProfileName)]
		if !ok {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		ip := &iamtypes.InstanceProfile{InstanceProfileName: v.InstanceProfileName, Tags: copyTags(p.tags)}
		for _, r := range p.roles {
			ip.Roles = append(ip.Roles, iamtypes.Role{RoleName: aws.String(r)})
		}
		return &iam.GetInstanceProfileOutput{InstanceProfile: ip}, nil
	case *iam.CreateInstanceProfileInput:
		name := aws.ToString(v.InstanceProfileName)
		if _, ok := m.profiles[name]; ok {
			return nil, sdkanswer.Err("EntityAlreadyExists")
		}
		p := &modelProfile{tags: map[string]string{}}
		addTags(p.tags, v.Tags)
		m.profiles[name] = p
	case *iam.TagInstanceProfileInput:
		p, ok := m.profiles[aws.ToString(v.InstanceProfileName)]
		if !ok {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		addTags(p.tags, v.Tags)
	case *iam.AddRoleToInstanceProfileInput:
		p, ok := m.profiles[aws.ToString(v.InstanceProfileName)]
		if !ok {
			return nil, sdkanswer.Err("NoSuchEntity")
		}
		if len(p.roles) > 0 {
			return nil, sdkanswer.Err("LimitExceeded")
		}
		p.roles = append(p.roles, aws.ToString(v.RoleName))
	case *iam.ListPolicyVersionsInput:
		return &iam.ListPolicyVersionsOutput{Versions: []iamtypes.PolicyVersion{{VersionId: aws.String("v1"), IsDefaultVersion: true}}}, nil
	case *iam.CreatePolicyVersionInput:
		if p, ok := m.policies[aws.ToString(v.PolicyArn)]; ok {
			p.doc = aws.ToString(v.PolicyDocument)
		}
	}
	return nil, nil
}

// stripSporeTags removes the spore-host:* tags from every resource, as if an
// older spawn had created them.
func (m *iamModel) stripSporeTags() {
	m.mu.Lock()
	defer m.mu.Unlock()
	strip := func(t map[string]string) {
		for k := range t {
			if strings.HasPrefix(k, "spore-host:") {
				delete(t, k)
			}
		}
	}
	for _, r := range m.roles {
		strip(r.tags)
	}
	for _, p := range m.profiles {
		strip(p.tags)
	}
	for _, p := range m.policies {
		strip(p.tags)
	}
}

// client is a Client whose IAM and STS calls the model answers, with
// installs (tagtest Detectors) on its config.
func (m *iamModel) client(t *testing.T, installs ...func(*aws.Config)) *Client {
	t.Helper()
	cfg := sdkanswer.Config("us-east-1", m.answer)
	for _, f := range installs {
		f(&cfg)
	}
	return NewClientFromConfig(cfg)
}
