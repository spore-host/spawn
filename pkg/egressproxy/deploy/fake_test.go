package deploy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	spawnaws "github.com/spore-host/spawn/pkg/aws"
)

// fakeEC2 is an in-memory EC2: security groups with rules, and instances.
type fakeEC2 struct {
	mu        sync.Mutex
	groups    []types.SecurityGroup
	instances []types.Instance
	calls     []string
	nextID    int

	runInputs  []*ec2.RunInstancesInput
	terminated []string
	deleted    []string
	revokedIn  []types.IpPermission
	revokedOut []types.IpPermission
	runErrs    []error
	// onDescribe, when set, edits an instance before each DescribeInstances
	// by ID returns it (a heartbeat appearing, say).
	onDescribe func(i *types.Instance, call int)
	describes  int
}

func (f *fakeEC2) call(name string) {
	f.calls = append(f.calls, name)
}

func (f *fakeEC2) count(name string) int {
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}

func (f *fakeEC2) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%04d", prefix, f.nextID)
}

func filterValues(fs []types.Filter, name string) []string {
	for _, x := range fs {
		if aws.ToString(x.Name) == name {
			return x.Values
		}
	}
	return nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("DescribeInstances")
	var out []types.Instance
	for i := range f.instances {
		inst := &f.instances[i]
		if len(in.InstanceIds) > 0 {
			if !contains(in.InstanceIds, aws.ToString(inst.InstanceId)) {
				continue
			}
			if f.onDescribe != nil {
				f.describes++
				f.onDescribe(inst, f.describes)
			}
		}
		if v := filterValues(in.Filters, "tag:"+ProxyTag); v != nil && !contains(v, tagMap(inst.Tags)[ProxyTag]) {
			continue
		}
		if v := filterValues(in.Filters, "instance-state-name"); v != nil && !contains(v, string(inst.State.Name)) {
			continue
		}
		if v := filterValues(in.Filters, "instance.group-id"); v != nil {
			ok := false
			for _, g := range inst.SecurityGroups {
				ok = ok || contains(v, aws.ToString(g.GroupId))
			}
			if !ok {
				continue
			}
		}
		out = append(out, *inst)
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: out}}}, nil
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("RunInstances")
	f.runInputs = append(f.runInputs, in)
	if len(f.runErrs) > 0 {
		err := f.runErrs[0]
		f.runErrs = f.runErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	ni := in.NetworkInterfaces[0]
	var sgs []types.GroupIdentifier
	for _, g := range ni.Groups {
		sgs = append(sgs, types.GroupIdentifier{GroupId: aws.String(g)})
	}
	inst := types.Instance{
		InstanceId:         aws.String(f.id("i")),
		State:              &types.InstanceState{Name: types.InstanceStateNamePending},
		PrivateIpAddress:   aws.String("10.0.9.9"),
		SubnetId:           ni.SubnetId,
		VpcId:              aws.String("vpc-1"),
		SecurityGroups:     sgs,
		IamInstanceProfile: &types.IamInstanceProfile{Arn: aws.String("arn:aws:iam::123456789012:instance-profile/" + aws.ToString(in.IamInstanceProfile.Name))},
		Tags:               in.TagSpecifications[0].Tags,
		Placement:          &types.Placement{AvailabilityZone: aws.String("us-west-2a")},
	}
	f.instances = append(f.instances, inst)
	return &ec2.RunInstancesOutput{Instances: []types.Instance{inst}}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("TerminateInstances")
	f.terminated = append(f.terminated, in.InstanceIds...)
	for i := range f.instances {
		if contains(in.InstanceIds, aws.ToString(f.instances[i].InstanceId)) {
			f.instances[i].State = &types.InstanceState{Name: types.InstanceStateNameShuttingDown}
		}
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) group(id string) *types.SecurityGroup {
	for i := range f.groups {
		if aws.ToString(f.groups[i].GroupId) == id {
			return &f.groups[i]
		}
	}
	return nil
}

func (f *fakeEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("DescribeSecurityGroups")
	var out []types.SecurityGroup
	for _, g := range f.groups {
		if len(in.GroupIds) > 0 && !contains(in.GroupIds, aws.ToString(g.GroupId)) {
			continue
		}
		if v := filterValues(in.Filters, "group-name"); v != nil && !contains(v, aws.ToString(g.GroupName)) {
			continue
		}
		if v := filterValues(in.Filters, "vpc-id"); v != nil && !contains(v, aws.ToString(g.VpcId)) {
			continue
		}
		out = append(out, g)
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: out}, nil
}

func (f *fakeEC2) CreateSecurityGroup(_ context.Context, in *ec2.CreateSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("CreateSecurityGroup")
	id := f.id("sg")
	f.groups = append(f.groups, types.SecurityGroup{
		GroupId: aws.String(id), GroupName: in.GroupName, VpcId: in.VpcId, Tags: in.TagSpecifications[0].Tags,
		// EC2's default for a new VPC group.
		IpPermissionsEgress: []types.IpPermission{{IpProtocol: aws.String("-1"), IpRanges: []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}},
	})
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String(id)}, nil
}

func samePerm(a, b types.IpPermission) bool {
	ra, rb := rulesOf([]types.IpPermission{a}), rulesOf([]types.IpPermission{b})
	return len(ra) == 1 && len(rb) == 1 && ra[0] == rb[0]
}

func addPerms(have []types.IpPermission, add []types.IpPermission) []types.IpPermission {
	for _, p := range add {
		for _, r := range rulesOf([]types.IpPermission{p}) {
			have = append(have, r.permission(""))
		}
	}
	return have
}

func removePerms(have []types.IpPermission, rm []types.IpPermission) []types.IpPermission {
	var out []types.IpPermission
	for _, h := range addPerms(nil, have) {
		gone := false
		for _, r := range addPerms(nil, rm) {
			gone = gone || samePerm(h, r)
		}
		if !gone {
			out = append(out, h)
		}
	}
	return out
}

func (f *fakeEC2) AuthorizeSecurityGroupIngress(_ context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("AuthorizeSecurityGroupIngress")
	g := f.group(aws.ToString(in.GroupId))
	g.IpPermissions = addPerms(g.IpPermissions, in.IpPermissions)
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func (f *fakeEC2) AuthorizeSecurityGroupEgress(_ context.Context, in *ec2.AuthorizeSecurityGroupEgressInput, _ ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupEgressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("AuthorizeSecurityGroupEgress")
	g := f.group(aws.ToString(in.GroupId))
	g.IpPermissionsEgress = addPerms(g.IpPermissionsEgress, in.IpPermissions)
	return &ec2.AuthorizeSecurityGroupEgressOutput{}, nil
}

func (f *fakeEC2) RevokeSecurityGroupIngress(_ context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("RevokeSecurityGroupIngress")
	f.revokedIn = append(f.revokedIn, in.IpPermissions...)
	g := f.group(aws.ToString(in.GroupId))
	g.IpPermissions = removePerms(g.IpPermissions, in.IpPermissions)
	return &ec2.RevokeSecurityGroupIngressOutput{}, nil
}

func (f *fakeEC2) RevokeSecurityGroupEgress(_ context.Context, in *ec2.RevokeSecurityGroupEgressInput, _ ...func(*ec2.Options)) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("RevokeSecurityGroupEgress")
	f.revokedOut = append(f.revokedOut, in.IpPermissions...)
	g := f.group(aws.ToString(in.GroupId))
	g.IpPermissionsEgress = removePerms(g.IpPermissionsEgress, in.IpPermissions)
	return &ec2.RevokeSecurityGroupEgressOutput{}, nil
}

func (f *fakeEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("DeleteSecurityGroup")
	id := aws.ToString(in.GroupId)
	// EC2 refuses a group another group's rule references.
	for _, g := range f.groups {
		for _, r := range rulesOf(g.IpPermissions) {
			if r.group == id && aws.ToString(g.GroupId) != id {
				return nil, errors.New("api error DependencyViolation: resource " + id + " has a dependent object")
			}
		}
	}
	var keep []types.SecurityGroup
	for _, g := range f.groups {
		if aws.ToString(g.GroupId) != id {
			keep = append(keep, g)
		}
	}
	f.groups = keep
	f.deleted = append(f.deleted, id)
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

// fakeProber answers the readiness probe.
type fakeProber struct {
	mu       sync.Mutex
	status   string
	err      error
	commands []string
}

func (p *fakeProber) RunShellScript(_ context.Context, _, _, command string, _ time.Duration) (*spawnaws.SSMRunResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.commands = append(p.commands, command)
	if p.err != nil {
		return nil, p.err
	}
	return &spawnaws.SSMRunResult{Status: p.status}, nil
}

// accessDenied is an EC2/SSM authorization failure.
var accessDenied = &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not allowed"}

// fakeClock counts sleeps instead of sleeping.
type fakeClock struct {
	now    time.Time
	sleeps int
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.sleeps++
	c.now = c.now.Add(d)
	return nil
}
