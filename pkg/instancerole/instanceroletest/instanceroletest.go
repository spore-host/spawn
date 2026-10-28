// Package instanceroletest is one account's per-task roles and the schedules
// that name some of them, in memory, that `spawn cleanup --instance-roles` and
// the TTL reaper's tests both run (#1378). Each asserts it deletes exactly
// WantDeleted and reports exactly WantReferenced, so the CLI and the reaper are
// held to the same answer on the same account, not merely to the same package.
package instanceroletest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spore-host/spawn/pkg/instancerole"
	"github.com/spore-host/spawn/pkg/scheduler"
)

const (
	// Account is the scenario's account.
	Account = "111122223333"
	// Region is where the schedules' params are, and an enabled region.
	Region = "us-east-1"
	// OtherRegion is enabled too and holds the only instance using
	// "spawn-instance-busy".
	OtherRegion = "ap-south-1"
)

// The roles. Every one is spawn-tagged, three days old and last used three
// days ago, with its same-name profile, so only an instance or a schedule
// keeps it.
const (
	RoleIdle     = "spawn-instance-idle"     // nothing names it      -> deleted
	RoleDefaults = "spawn-instance-defaults" // sched-defaults' defaults
	RoleRow      = "spawn-instance-row"      // a row of sched-row
	RolePaused   = "spawn-instance-paused"   // only a paused schedule -> deleted
	RoleEnded    = "spawn-instance-ended"    // only a run-out schedule -> deleted
	RoleBusy     = "spawn-instance-busy"     // an instance in OtherRegion
)

// WantDeleted is what any correct sweep in reap mode deletes from the
// scenario, and what report mode says it would; WantReferenced is each role an
// enabled schedule keeps, with the reason the sweep must give.
var (
	WantDeleted    = []string{RoleEnded, RoleIdle, RolePaused}
	WantReferenced = map[string]string{
		RoleDefaults: "referenced by schedule sched-defaults",
		RoleRow:      "referenced by schedule sched-row",
	}
)

// Scenario is the account.
type Scenario struct {
	Now       time.Time
	IAM       *IAM
	EC2       map[string]*EC2
	Schedules *Schedules
}

// New builds the scenario.
func New() *Scenario {
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-72 * time.Hour)
	roles := map[string]time.Time{}
	for _, r := range []string{RoleIdle, RoleDefaults, RoleRow, RolePaused, RoleEnded, RoleBusy} {
		roles[r] = old
	}
	s := &Schedules{Objects: map[string][]byte{}}
	s.add("sched-defaults", params(RoleDefaults), nil)
	s.add("sched-row", params(instancerole.SharedProfileName, RoleRow), nil)
	s.add("sched-paused", params(RolePaused), func(r *scheduler.ScheduleRecord) { r.Status = string(scheduler.ScheduleStatusPaused) })
	s.add("sched-ended", params(RoleEnded), func(r *scheduler.ScheduleRecord) { r.EndAfter = now.Add(-time.Hour) })
	busy := "arn:aws:iam::" + Account + ":instance-profile/" + RoleBusy
	return &Scenario{
		Now: now,
		IAM: &IAM{Roles: roles},
		EC2: map[string]*EC2{
			Region:      {},
			OtherRegion: {Instances: []ec2types.Instance{{IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: aws.String(busy)}}}},
		},
		Schedules: s,
	}
}

// Regions are the scenario's enabled regions, sorted.
func (s *Scenario) Regions() []string { return []string{OtherRegion, Region} }

// EC2Clients is the per-region map instancerole.ProfilesInUse takes.
func (s *Scenario) EC2Clients() map[string]instancerole.EC2Describer {
	m := map[string]instancerole.EC2Describer{}
	for r, c := range s.EC2 {
		m[r] = c
	}
	return m
}

// Source is the schedules as a ReferenceSource, read through the same
// instancerole.ScheduleSource the CLI and the reaper build.
func (s *Scenario) Source() *instancerole.ScheduleSource {
	return &instancerole.ScheduleSource{
		DB: s.Schedules, Table: scheduler.TableName, Now: time.Now,
		Params: func(string) instancerole.ScheduleParamsGetter { return s.Schedules },
	}
}

func params(defaultsRole string, rowRoles ...string) []byte {
	rows := []map[string]interface{}{{"x": 0}}
	for i, r := range rowRoles {
		rows = append(rows, map[string]interface{}{"x": i + 1, "iam_role": r})
	}
	b, _ := json.Marshal(map[string]interface{}{
		"defaults": map[string]interface{}{"iam_role": defaultsRole, "instance_type": "t3.micro"},
		"params":   rows,
	})
	return b
}

// Schedules is the schedules table and the params bucket. Err makes every
// Scan fail.
type Schedules struct {
	mu      sync.Mutex
	Records []scheduler.ScheduleRecord
	Objects map[string][]byte // bucket/key
	Err     error
}

func (s *Schedules) add(id string, body []byte, adjust func(*scheduler.ScheduleRecord)) {
	rec := scheduler.ScheduleRecord{
		ScheduleID: id, Status: string(scheduler.ScheduleStatusActive), Region: Region,
		S3ParamsBucket: "spawn-schedules-" + Region, S3ParamsKey: "schedules/" + id + "/params.yaml",
	}
	if adjust != nil {
		adjust(&rec)
	}
	s.Records = append(s.Records, rec)
	s.Objects[rec.S3ParamsBucket+"/"+rec.S3ParamsKey] = body
}

// Scan implements instancerole.ScheduleScanner, one page.
func (s *Schedules) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return nil, s.Err
	}
	if aws.ToString(in.TableName) != scheduler.TableName {
		return nil, &dynamodbtypes.ResourceNotFoundException{Message: aws.String("no table " + aws.ToString(in.TableName))}
	}
	out := &dynamodb.ScanOutput{}
	for _, r := range s.Records {
		item, err := attributevalue.MarshalMap(r)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// GetObject implements instancerole.ScheduleParamsGetter.
func (s *Schedules) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, errors.New("NoSuchKey: " + aws.ToString(in.Key))
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

// EC2 is one region's live instances.
type EC2 struct{ Instances []ec2types.Instance }

// DescribeInstances implements instancerole.EC2Describer.
func (f *EC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: f.Instances}}}, nil
}

// IAM is the account's roles, each with its same-name profile. Deleted
// records the roles DeleteRole removed.
type IAM struct {
	mu      sync.Mutex
	Roles   map[string]time.Time // name -> created (and last used)
	Deleted []string
	Mutated []string
}

func tags() []iamtypes.Tag {
	return []iamtypes.Tag{{Key: aws.String(instancerole.ManagedTagKey), Value: aws.String(instancerole.ManagedTagValue)}}
}

// DeletedSorted is Deleted, sorted.
func (f *IAM) DeletedSorted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.Deleted...)
	sort.Strings(out)
	return out
}

func (f *IAM) mutate(op, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Mutated = append(f.Mutated, op+" "+name)
}

// ListRoles implements instancerole.API.
func (f *IAM) ListRoles(context.Context, *iam.ListRolesInput, ...func(*iam.Options)) (*iam.ListRolesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &iam.ListRolesOutput{}
	for n := range f.Roles {
		out.Roles = append(out.Roles, iamtypes.Role{RoleName: aws.String(n)})
	}
	return out, nil
}

// GetRole implements instancerole.API.
func (f *IAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Roles[aws.ToString(in.RoleName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no role")}
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{
		RoleName: in.RoleName, CreateDate: aws.Time(c), Tags: tags(),
		RoleLastUsed: &iamtypes.RoleLastUsed{LastUsedDate: aws.Time(c)},
	}}, nil
}

// GetInstanceProfile implements instancerole.API.
func (f *IAM) GetInstanceProfile(_ context.Context, in *iam.GetInstanceProfileInput, _ ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Roles[aws.ToString(in.InstanceProfileName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no profile")}
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
		InstanceProfileName: in.InstanceProfileName, Tags: tags(), CreateDate: aws.Time(c),
	}}, nil
}

// ListInstanceProfilesForRole implements instancerole.API.
func (f *IAM) ListInstanceProfilesForRole(_ context.Context, in *iam.ListInstanceProfilesForRoleInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return &iam.ListInstanceProfilesForRoleOutput{InstanceProfiles: []iamtypes.InstanceProfile{{InstanceProfileName: in.RoleName}}}, nil
}

// ListRolePolicies implements instancerole.API.
func (f *IAM) ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	return &iam.ListRolePoliciesOutput{}, nil
}

// DeleteRolePolicy implements instancerole.API.
func (f *IAM) DeleteRolePolicy(_ context.Context, in *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	f.mutate("DeleteRolePolicy", aws.ToString(in.RoleName))
	return &iam.DeleteRolePolicyOutput{}, nil
}

// ListAttachedRolePolicies implements instancerole.API.
func (f *IAM) ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}

// DetachRolePolicy implements instancerole.API.
func (f *IAM) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	f.mutate("DetachRolePolicy", aws.ToString(in.RoleName))
	return &iam.DetachRolePolicyOutput{}, nil
}

// RemoveRoleFromInstanceProfile implements instancerole.API.
func (f *IAM) RemoveRoleFromInstanceProfile(_ context.Context, in *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	f.mutate("RemoveRoleFromInstanceProfile", aws.ToString(in.RoleName))
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

// DeleteInstanceProfile implements instancerole.API.
func (f *IAM) DeleteInstanceProfile(_ context.Context, in *iam.DeleteInstanceProfileInput, _ ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	f.mutate("DeleteInstanceProfile", aws.ToString(in.InstanceProfileName))
	return &iam.DeleteInstanceProfileOutput{}, nil
}

// DeleteRole implements instancerole.API.
func (f *IAM) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	n := aws.ToString(in.RoleName)
	f.mutate("DeleteRole", n)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Roles, n)
	f.Deleted = append(f.Deleted, n)
	return &iam.DeleteRoleOutput{}, nil
}

// Joined is a sorted list as one comparable string.
func Joined(names []string) string {
	c := append([]string(nil), names...)
	sort.Strings(c)
	return strings.Join(c, ",")
}
